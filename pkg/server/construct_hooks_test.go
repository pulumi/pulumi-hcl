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
	"fmt"
	"path/filepath"
	"sync"
	"testing"

	p "github.com/pulumi/pulumi-go-provider"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource"
	"github.com/pulumi/pulumi/sdk/v3/go/property"
	pulumirpc "github.com/pulumi/pulumi/sdk/v3/proto/go"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/pulumi/pulumi-hcl/pkg/hcl/modules"
)

// hookInvokingMonitor is a ResourceMonitor test double that invokes a resource's
// bound hooks over the provider's callback server as the engine does around a
// step — a hook error fails the registration — so tests observe a condition
// actually firing, not merely being registered.
type hookInvokingMonitor struct {
	pulumirpc.UnimplementedResourceMonitorServer

	mu    sync.Mutex
	hooks map[string]*pulumirpc.Callback
	// deferredDeletes holds before_delete invocations recorded during
	// RegisterResource; runDeletes fires them later, as the engine deletes the
	// component's children during `destroy --run-program`.
	deferredDeletes []func(context.Context) error
	// registered records every RegisterResource request.
	registered []*pulumirpc.RegisterResourceRequest
}

// RegisterPackage answers every registration with one fixed ref; these tests
// exercise request forwarding, not package identity.
func (*hookInvokingMonitor) RegisterPackage(
	context.Context, *pulumirpc.RegisterPackageRequest,
) (*pulumirpc.RegisterPackageResponse, error) {
	return &pulumirpc.RegisterPackageResponse{Ref: "package-ref"}, nil
}

// registeredType returns the recorded registration for the given type token.
func (s *hookInvokingMonitor) registeredType(typ string) *pulumirpc.RegisterResourceRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, req := range s.registered {
		if req.Type == typ {
			return req
		}
	}
	return nil
}

func newHookInvokingMonitor() *hookInvokingMonitor {
	return &hookInvokingMonitor{hooks: map[string]*pulumirpc.Callback{}}
}

func (s *hookInvokingMonitor) runDeletes(ctx context.Context) error {
	for _, d := range s.deferredDeletes {
		if err := d(ctx); err != nil {
			return err
		}
	}
	return nil
}

func (s *hookInvokingMonitor) RegisterResourceHook(
	_ context.Context, req *pulumirpc.RegisterResourceHookRequest,
) (*emptypb.Empty, error) {
	s.mu.Lock()
	s.hooks[req.Name] = req.Callback
	s.mu.Unlock()
	return &emptypb.Empty{}, nil
}

func (s *hookInvokingMonitor) RegisterResource(
	ctx context.Context, req *pulumirpc.RegisterResourceRequest,
) (*pulumirpc.RegisterResourceResponse, error) {
	s.mu.Lock()
	s.registered = append(s.registered, req)
	s.mu.Unlock()

	// Chain the parent's qualified type the way the engine's generateURN
	// does; the dispatcher keys instance entries by that URN.
	parentChain := ""
	if p := resource.URN(req.Parent); p != "" && p.QualifiedType() != resource.RootStackType {
		parentChain = string(p.QualifiedType()) + "$"
	}
	urn := "urn:pulumi:test::proj::" + parentChain + req.Type + "::" + req.Name

	// The engine echoes checked inputs back as outputs for a resource with no
	// provider diff; enough for a postcondition's `self` to resolve.
	hookReq := &pulumirpc.ResourceHookRequest{
		Urn:        urn,
		Id:         req.Name,
		Name:       req.Name,
		Type:       req.Type,
		NewInputs:  req.Object,
		NewOutputs: req.Object,
	}

	for _, name := range req.GetHooks().GetBeforeCreate() {
		if err := s.invokeHook(ctx, name, hookReq); err != nil {
			return nil, err
		}
	}
	for _, name := range req.GetHooks().GetAfterCreate() {
		if err := s.invokeHook(ctx, name, hookReq); err != nil {
			return nil, err
		}
	}

	// before_delete fires when the engine later deletes this resource, not now;
	// record it against the prior state (`self` reads old outputs).
	deleteReq := &pulumirpc.ResourceHookRequest{
		Urn:        urn,
		Id:         req.Name,
		Name:       req.Name,
		Type:       req.Type,
		OldInputs:  req.Object,
		OldOutputs: req.Object,
	}
	for _, name := range req.GetHooks().GetBeforeDelete() {
		s.mu.Lock()
		s.deferredDeletes = append(s.deferredDeletes, func(ctx context.Context) error {
			return s.invokeHook(ctx, name, deleteReq)
		})
		s.mu.Unlock()
	}

	return &pulumirpc.RegisterResourceResponse{Urn: urn, Object: req.Object}, nil
}

func (s *hookInvokingMonitor) RegisterResourceOutputs(
	_ context.Context, _ *pulumirpc.RegisterResourceOutputsRequest,
) (*emptypb.Empty, error) {
	return &emptypb.Empty{}, nil
}

// invokeHook dials the provider-hosted callback server and runs the named hook,
// surfacing a hook-reported error as a registration failure.
func (s *hookInvokingMonitor) invokeHook(
	ctx context.Context, name string, req *pulumirpc.ResourceHookRequest,
) error {
	s.mu.Lock()
	cb, ok := s.hooks[name]
	s.mu.Unlock()
	if !ok {
		return fmt.Errorf("hook %q was bound but never registered", name)
	}

	conn, err := grpc.NewClient(cb.Target, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return fmt.Errorf("dialing callback server: %w", err)
	}
	defer func() { _ = conn.Close() }()

	reqBytes, err := proto.Marshal(req)
	if err != nil {
		return err
	}
	resp, err := pulumirpc.NewCallbacksClient(conn).Invoke(ctx, &pulumirpc.CallbackInvokeRequest{
		Token:   cb.Token,
		Request: reqBytes,
	})
	if err != nil {
		return err
	}
	var hookResp pulumirpc.ResourceHookResponse
	if err := proto.Unmarshal(resp.Response, &hookResp); err != nil {
		return err
	}
	if hookResp.Error != "" {
		return fmt.Errorf("%s", hookResp.Error)
	}
	return nil
}

// serveMonitor starts the hook-invoking monitor and returns a module provider
// wired to it. One provider spans a test's construct and delete phases so its
// callback server outlives any single Construct call.
func serveMonitor(t *testing.T) (*hookInvokingMonitor, *moduleProvider, string) {
	t.Helper()
	mon := newHookInvokingMonitor()
	endpoint := serveResourceMonitor(t, mon)

	m := &moduleProvider{
		moduleLoader: modules.NewLoader(modules.LiveResolver(t.Context())),
		resolver:     stubResolver{},
	}
	return mon, m, endpoint
}

func construct(
	t *testing.T, m *moduleProvider, endpoint, testdataDir string, inputs map[string]property.Value,
) error {
	t.Helper()
	dir, err := filepath.Abs(filepath.Join("testdata", testdataDir))
	require.NoError(t, err)
	_, err = m.construct(t.Context(), p.ConstructRequest{
		Urn:             resource.URN("urn:pulumi:test::proj::hcl:index:Module::mod"),
		MonitorEndpoint: endpoint,
		Inputs: property.NewMap(map[string]property.Value{
			"source": property.New(dir),
			"inputs": property.New(property.NewMap(inputs)),
		}),
	})
	return err
}

func constructModule(t *testing.T, testdataDir string, expected string) error {
	t.Helper()
	_, m, endpoint := serveMonitor(t)
	return construct(t, m, endpoint, testdataDir, map[string]property.Value{
		"expected": property.New(expected),
	})
}

// TestModuleConstructResourcePrecondition asserts a resource `precondition`
// inside a constructed module is registered and fired: satisfied passes, a
// violated one blocks construct with the configured message.
func TestModuleConstructResourcePrecondition(t *testing.T) {
	t.Parallel()

	t.Run("pass", func(t *testing.T) {
		t.Parallel()
		require.NoError(t, constructModule(t, "module-precondition", "ok"))
	})

	t.Run("fail", func(t *testing.T) {
		t.Parallel()
		err := constructModule(t, "module-precondition", "not-ok")
		require.ErrorContains(t, err, "PRECONDITION_IN_MODULE")
	})
}

// TestModuleConstructResourcePostcondition asserts a resource `postcondition`
// inside a constructed module fires against the resource's outputs (`self`):
// satisfied passes, a violated one blocks construct with the configured message.
func TestModuleConstructResourcePostcondition(t *testing.T) {
	t.Parallel()

	t.Run("pass", func(t *testing.T) {
		t.Parallel()
		require.NoError(t, constructModule(t, "module-postcondition", "ok"))
	})

	t.Run("fail", func(t *testing.T) {
		t.Parallel()
		err := constructModule(t, "module-postcondition", "not-ok")
		require.ErrorContains(t, err, "POSTCONDITION_IN_MODULE")
	})
}

// TestModuleConstructDestroyProvisioner models the two phases of `destroy
// --run-program`: Construct runs the create provisioner, then the child is
// deleted later, firing the destroy provisioner after Construct returned. The
// delete hook only reaches a live callback server because the provider hosts it.
func TestModuleConstructDestroyProvisioner(t *testing.T) {
	t.Parallel()

	markerDir := t.TempDir()
	mon, m, endpoint := serveMonitor(t)

	require.NoError(t, construct(t, m, endpoint, "module-destroy-provisioner",
		map[string]property.Value{"marker_dir": property.New(markerDir)}))
	require.FileExists(t, filepath.Join(markerDir, "created"),
		"create-time provisioner should run during Construct")

	// Model the engine deleting the component's child after Construct returned.
	require.NoError(t, mon.runDeletes(t.Context()))
	require.FileExists(t, filepath.Join(markerDir, "destroyed"),
		"destroy-time provisioner should run when the child is deleted post-Construct")
}

// TestModuleProviderReleasesHooks asserts the provider-owned callback server is
// created lazily by a construct that registers hooks and released on cancel.
func TestModuleProviderReleasesHooks(t *testing.T) {
	t.Parallel()

	_, m, endpoint := serveMonitor(t)
	require.NoError(t, construct(t, m, endpoint, "module-precondition",
		map[string]property.Value{"expected": property.New("ok")}))
	require.NotNil(t, m.hooks.cbs, "registering a hook should start the callback server")

	require.NoError(t, m.cancel(t.Context()))
	require.Nil(t, m.hooks.cbs, "cancel should release the callback server")
}
