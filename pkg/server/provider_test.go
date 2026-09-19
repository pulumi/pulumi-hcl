// Copyright 2026, Pulumi Corporation.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package server

import (
	"context"
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"

	"github.com/google/go-cmp/cmp"
	pulumiSchema "github.com/pulumi/pulumi/pkg/v3/codegen/schema"
	pulumirpc "github.com/pulumi/pulumi/sdk/v3/proto/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/testing/protocmp"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/structpb"
)

// TestNewLocalProvider drives the locally-born provider — the RunPlugin path —
// end to end over the raw gRPC surface. The loader/mapper address points at an
// unused port: both clients dial lazily, and a provider-free module never uses
// them.
func TestNewLocalProvider(t *testing.T) {
	t.Parallel()

	dir, err := filepath.Abs(filepath.Join("testdata", "module-one-var"))
	require.NoError(t, err)

	prov, err := NewLocalProvider(t.Context(), dir, "127.0.0.1:1")
	require.NoError(t, err)

	// An address-free handshake, as the RunPlugin flow sends, must succeed.
	_, err = prov.Handshake(t.Context(), &pulumirpc.ProviderHandshakeRequest{})
	require.NoError(t, err)

	info, err := prov.GetPluginInfo(t.Context(), &emptypb.Empty{})
	require.NoError(t, err)
	require.Equal(t, "0.0.0-dev", info.Version)

	out, err := prov.GetSchema(t.Context(), &pulumirpc.GetSchemaRequest{})
	require.NoError(t, err)
	var spec pulumiSchema.PackageSpec
	require.NoError(t, json.Unmarshal([]byte(out.Schema), &spec))
	require.Equal(t, "module-one-var", spec.Name)
	require.Nil(t, spec.Parameterization)
	res, ok := spec.Resources["module-one-var:index:Module"]
	require.True(t, ok, "schema should declare the typed component")
	require.True(t, res.IsComponent)
	require.Equal(t, "string", res.InputProperties["name"].Type)
	require.Equal(t, "string", res.Properties["greeting"].Type)

	mon, _, endpoint := serveMonitor(t)
	inputs, err := structpb.NewStruct(map[string]any{"name": "world"})
	require.NoError(t, err)
	resp, err := prov.Construct(t.Context(), &pulumirpc.ConstructRequest{
		Type:                "module-one-var:index:Module",
		Name:                "greet",
		Project:             "proj",
		Stack:               "test",
		Organization:        "acme",
		MonitorEndpoint:     endpoint,
		Inputs:              inputs,
		ReplaceWith:         []string{"urn:pulumi:test::proj::pkg:index:Other::sibling"},
		ReplacementTrigger:  structpb.NewStringValue("trigger"),
		AcceptsOutputValues: true,
	})
	require.NoError(t, err)
	require.Equal(t, "hello world", resp.State.Fields["greeting"].GetStringValue())

	component := mon.registeredType("module-one-var:index:Module")
	require.NotNil(t, component, "the component itself must be registered")
	require.Equal(t, []string{"urn:pulumi:test::proj::pkg:index:Other::sibling"}, component.ReplaceWith)
	require.Equal(t, structpb.NewStringValue("trigger"), component.ReplacementTrigger)
}

// captureMonitor records registrations without invoking bound hooks, so a
// forwarding test can bind hook names no program ever registered.
type captureMonitor struct {
	pulumirpc.UnimplementedResourceMonitorServer
	mu         sync.Mutex
	registered []*pulumirpc.RegisterResourceRequest
}

// RegisterPackage answers every registration with one fixed ref; these tests
// exercise request forwarding, not package identity.
func (*captureMonitor) RegisterPackage(
	context.Context, *pulumirpc.RegisterPackageRequest,
) (*pulumirpc.RegisterPackageResponse, error) {
	return &pulumirpc.RegisterPackageResponse{Ref: "package-ref"}, nil
}

func (s *captureMonitor) RegisterResource(
	_ context.Context, req *pulumirpc.RegisterResourceRequest,
) (*pulumirpc.RegisterResourceResponse, error) {
	s.mu.Lock()
	s.registered = append(s.registered, req)
	s.mu.Unlock()
	return &pulumirpc.RegisterResourceResponse{
		Urn:    "urn:pulumi:test::proj::" + req.Type + "::" + req.Name,
		Object: req.Object,
	}, nil
}

func (s *captureMonitor) RegisterResourceHook(
	_ context.Context, _ *pulumirpc.RegisterResourceHookRequest,
) (*emptypb.Empty, error) {
	return &emptypb.Empty{}, nil
}

func (s *captureMonitor) RegisterResourceOutputs(
	_ context.Context, _ *pulumirpc.RegisterResourceOutputsRequest,
) (*emptypb.Empty, error) {
	return &emptypb.Empty{}, nil
}

// registeredType returns the recorded registration for the given type token.
func (s *captureMonitor) registeredType(typ string) *pulumirpc.RegisterResourceRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, req := range s.registered {
		if req.Type == typ {
			return req
		}
	}
	return nil
}

// TestConstructForwardsResourceHooks guards against dropping the consumer's
// hook binding on the component: the engine sends it on the ConstructRequest
// and expects the provider to re-attach it to the component's own
// RegisterResourceRequest, where it flows into state and fires around the
// component's lifecycle steps.
// https://github.com/pulumi/pulumi-hcl/issues/542
func TestConstructForwardsResourceHooks(t *testing.T) {
	t.Parallel()

	dir, err := filepath.Abs(filepath.Join("testdata", "module-one-var"))
	require.NoError(t, err)

	prov, err := NewLocalProvider(t.Context(), dir, "127.0.0.1:1")
	require.NoError(t, err)
	_, err = prov.Handshake(t.Context(), &pulumirpc.ProviderHandshakeRequest{})
	require.NoError(t, err)

	mon := &captureMonitor{}
	endpoint := serveResourceMonitor(t, mon)
	inputs, err := structpb.NewStruct(map[string]any{"name": "world"})
	require.NoError(t, err)
	_, err = prov.Construct(t.Context(), &pulumirpc.ConstructRequest{
		Type:            "module-one-var:index:Module",
		Name:            "greet",
		Project:         "proj",
		Stack:           "test",
		MonitorEndpoint: endpoint,
		Inputs:          inputs,
		ResourceHooks: &pulumirpc.ConstructRequest_ResourceHooksBinding{
			BeforeCreate: []string{"notify-start"},
			AfterCreate:  []string{"notify-done"},
			BeforeUpdate: []string{"before-update"},
			AfterUpdate:  []string{"after-update"},
			BeforeDelete: []string{"prevent-destroy"},
			AfterDelete:  []string{"after-delete"},
			OnError:      []string{"on-error"},
		},
		AcceptsOutputValues: true,
	})
	require.NoError(t, err)

	component := mon.registeredType("module-one-var:index:Module")
	require.NotNil(t, component, "the component itself must be registered")
	assert.Empty(t, cmp.Diff(&pulumirpc.RegisterResourceRequest_ResourceHooksBinding{
		BeforeCreate: []string{"notify-start"},
		AfterCreate:  []string{"notify-done"},
		BeforeUpdate: []string{"before-update"},
		AfterUpdate:  []string{"after-update"},
		BeforeDelete: []string{"prevent-destroy"},
		AfterDelete:  []string{"after-delete"},
		OnError:      []string{"on-error"},
	}, component.Hooks, protocmp.Transform()))
}

// TestNewLocalProviderMultiComponent verifies the local package serves one
// component per consumable directory and dispatches Construct on the request's
// type token.
func TestNewLocalProviderMultiComponent(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	pkgDir := filepath.Join(base, "mypkg")
	require.NoError(t, os.Rename(writeMultiComponentModule(t, true), pkgDir))

	prov, err := NewLocalProvider(t.Context(), pkgDir, "127.0.0.1:1")
	require.NoError(t, err)

	out, err := prov.GetSchema(t.Context(), &pulumirpc.GetSchemaRequest{})
	require.NoError(t, err)
	var spec pulumiSchema.PackageSpec
	require.NoError(t, json.Unmarshal([]byte(out.Schema), &spec))
	require.Equal(t, "mypkg", spec.Name)
	require.Equal(t,
		[]string{"mypkg:greeter:Module", "mypkg:index:Module", "mypkg:user-data:Module"},
		slices.Sorted(maps.Keys(spec.Resources)))

	_, _, endpoint := serveMonitor(t)
	inputs, err := structpb.NewStruct(map[string]any{"who": "world"})
	require.NoError(t, err)
	resp, err := prov.Construct(t.Context(), &pulumirpc.ConstructRequest{
		Type:                "mypkg:greeter:Module",
		Name:                "g",
		Project:             "proj",
		Stack:               "test",
		MonitorEndpoint:     endpoint,
		Inputs:              inputs,
		AcceptsOutputValues: true,
	})
	require.NoError(t, err)
	require.Equal(t, "hi world", resp.State.Fields["hello"].GetStringValue())

	_, err = prov.Construct(t.Context(), &pulumirpc.ConstructRequest{
		Type:                "mypkg:nope:Module",
		Name:                "x",
		MonitorEndpoint:     endpoint,
		AcceptsOutputValues: true,
	})
	require.ErrorContains(t, err, `unknown resource type: "mypkg:nope:Module"`)
}

// TestNewLocalProviderSubmodulesOnly verifies a local package whose root holds
// no .tf files — previously a hard failure — serves its submodule components.
func TestNewLocalProviderSubmodulesOnly(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	pkgDir := filepath.Join(base, "mypkg")
	require.NoError(t, os.Rename(writeMultiComponentModule(t, false), pkgDir))

	prov, err := NewLocalProvider(t.Context(), pkgDir, "127.0.0.1:1")
	require.NoError(t, err)

	out, err := prov.GetSchema(t.Context(), &pulumirpc.GetSchemaRequest{})
	require.NoError(t, err)
	var spec pulumiSchema.PackageSpec
	require.NoError(t, json.Unmarshal([]byte(out.Schema), &spec))
	require.Equal(t, "mypkg", spec.Name)
	require.Equal(t, "0.0.0-dev", spec.Version)
	require.Equal(t,
		[]string{"mypkg:greeter:Module", "mypkg:user-data:Module"},
		slices.Sorted(maps.Keys(spec.Resources)))
}
