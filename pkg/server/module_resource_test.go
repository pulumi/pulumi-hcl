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
	"net"
	"path/filepath"
	"testing"

	p "github.com/pulumi/pulumi-go-provider"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource"
	"github.com/pulumi/pulumi/sdk/v3/go/property"
	pulumirpc "github.com/pulumi/pulumi/sdk/v3/proto/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/pulumi/pulumi-hcl/pkg/hcl/modules"
	"github.com/pulumi/pulumi-hcl/pkg/hcl/parser"
	"github.com/pulumi/pulumi-hcl/pkg/hcl/resolve"
)

// stubResolver is a non-nil PackageResolverClient whose methods are never
// reached by the validation tests below.
type stubResolver struct {
	pulumirpc.PackageResolverClient
}

func TestModuleConstructValidation(t *testing.T) {
	t.Parallel()

	urn := resource.URN("urn:pulumi:test::proj::hcl:index:Module::mod")

	t.Run("before handshake", func(t *testing.T) {
		t.Parallel()
		// resolver is nil until a successful Handshake.
		_, err := (&moduleProvider{}).construct(t.Context(), p.ConstructRequest{
			Urn:    urn,
			Inputs: property.NewMap(map[string]property.Value{"source": property.New("./mod")}),
		})
		require.EqualError(t, err, "construct called before a successful handshake")
	})

	t.Run("missing source", func(t *testing.T) {
		t.Parallel()
		_, err := (&moduleProvider{resolver: stubResolver{}}).construct(t.Context(), p.ConstructRequest{
			Urn:    urn,
			Inputs: property.NewMap(nil),
		})
		require.EqualError(t, err, `module requires a plain string "source" input`)
	})

	t.Run("non-string source", func(t *testing.T) {
		t.Parallel()
		_, err := (&moduleProvider{resolver: stubResolver{}}).construct(t.Context(), p.ConstructRequest{
			Urn:    urn,
			Inputs: property.NewMap(map[string]property.Value{"source": property.New(42.0)}),
		})
		require.EqualError(t, err, `module requires a plain string "source" input`)
	})

	t.Run("non-map inputs", func(t *testing.T) {
		t.Parallel()
		// source is valid, but inputs is the wrong shape and must be rejected
		// rather than silently dropped.
		_, err := (&moduleProvider{resolver: stubResolver{}}).construct(t.Context(), p.ConstructRequest{
			Urn: urn,
			Inputs: property.NewMap(map[string]property.Value{
				"source": property.New("./mod"),
				"inputs": property.New("not a map"),
			}),
		})
		require.EqualError(t, err, `module "inputs" input must be a map`)
	})

	t.Run("non-string version", func(t *testing.T) {
		t.Parallel()
		// version is optional, but when present it must be a plain string.
		_, err := (&moduleProvider{resolver: stubResolver{}}).construct(t.Context(), p.ConstructRequest{
			Urn: urn,
			Inputs: property.NewMap(map[string]property.Value{
				"source":  property.New("./mod"),
				"version": property.New(42.0),
			}),
		})
		require.EqualError(t, err, `module "version" input must be a plain string`)
	})
}

func TestModuleConstructRejectsUnknownInput(t *testing.T) {
	t.Parallel()

	dir, err := filepath.Abs(filepath.Join("testdata", "module-one-var"))
	require.NoError(t, err)

	// The module declares only "name"; any other input must be rejected rather
	// than silently dropped. The check runs after the module loads but before any
	// provider resolution, so the stub resolver is never called.
	tests := []struct {
		name    string
		inputs  map[string]property.Value
		wantErr string
	}{
		{
			name:    "single unknown",
			inputs:  map[string]property.Value{"name": property.New("ada"), "bogus": property.New("x")},
			wantErr: "module has no variables declared for input: bogus",
		},
		{
			name:    "multiple unknown reported sorted",
			inputs:  map[string]property.Value{"zeta": property.New("z"), "alpha": property.New("a")},
			wantErr: "module has no variables declared for inputs: alpha, zeta",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			m := &moduleProvider{moduleLoader: modules.NewLoader(modules.LiveResolver(t.Context())), resolver: stubResolver{}}
			_, err := m.construct(t.Context(), p.ConstructRequest{
				Urn: resource.URN("urn:pulumi:test::proj::hcl:index:Module::mod"),
				Inputs: property.NewMap(map[string]property.Value{
					"source": property.New(dir),
					"inputs": property.New(property.NewMap(tt.inputs)),
				}),
			})
			require.EqualError(t, err, tt.wantErr)
		})
	}
}

func TestRequirementSpecs(t *testing.T) {
	t.Parallel()

	const src = `terraform {
  required_providers {
    null = {
      source  = "hashicorp/null"
      version = "~> 3.2"
    }
    random = {
      source = "pulumi/random"
    }
  }
}

# pulumi_stash uses the builtin "pulumi" provider (skipped); aws_s3_bucket
# references an undeclared provider that defaults to "hashicorp/aws".
resource "pulumi_stash" "s" {}

resource "aws_s3_bucket" "b" {}
`
	cfg, diags := parser.NewParser().ParseSource("main.tf", []byte(src))
	require.False(t, diags.HasErrors(), "diags: %v", diags)

	got := RequirementSpecs(t.Context(), nil, cfg, "")

	assert.Equal(t, []resolve.Request{
		{Alias: "aws", Spec: &pulumirpc.PackageSpec{
			Source:     "terraform-provider",
			Version:    bridgePackageVersion,
			Parameters: []string{"hashicorp/aws"},
		}},
		{Alias: "null", Spec: &pulumirpc.PackageSpec{
			Source:     "terraform-provider",
			Version:    bridgePackageVersion,
			Parameters: []string{"hashicorp/null", "~> 3.2"},
		}},
		{Alias: "random", Spec: &pulumirpc.PackageSpec{Source: "random"}},
	}, got)
}

// refMonitorServer adds getResource support to captureMonitorServer so a
// construct test can resolve a resource passed in by reference.
type refMonitorServer struct {
	captureMonitorServer
}

func (s *refMonitorServer) Invoke(
	_ context.Context, req *pulumirpc.ResourceInvokeRequest,
) (*pulumirpc.ResourceInvokeResponse, error) {
	if req.Tok != "pulumi:pulumi:getResource" {
		return &pulumirpc.ResourceInvokeResponse{}, nil
	}
	ret, err := structpb.NewStruct(map[string]any{
		"state": map[string]any{"value": "from-handler"},
	})
	if err != nil {
		return nil, err
	}
	return &pulumirpc.ResourceInvokeResponse{Return: ret}, nil
}

// TestModuleConstructResourceReferenceInput drives construct with a whole
// resource passed by reference as a module input and asserts the module can read
// the referenced resource's fields: the runtime fetches its state (via
// getResource) so `var.handler.value` resolves to the resource's value, not just
// its id.
func TestModuleConstructResourceReferenceInput(t *testing.T) {
	t.Parallel()

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	srv := grpc.NewServer()
	pulumirpc.RegisterResourceMonitorServer(srv, &refMonitorServer{})
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	dir, err := filepath.Abs(filepath.Join("testdata", "module-resource-ref"))
	require.NoError(t, err)

	m := &moduleProvider{
		moduleLoader: modules.NewLoader(modules.LiveResolver(t.Context())),
		resolver:     stubResolver{},
	}

	resp, err := m.construct(t.Context(), p.ConstructRequest{
		Urn:             resource.URN("urn:pulumi:test::proj::hcl:index:Module::mod"),
		MonitorEndpoint: lis.Addr().String(),
		Inputs: property.NewMap(map[string]property.Value{
			"source": property.New(dir),
			"inputs": property.New(property.NewMap(map[string]property.Value{
				"handler": property.New(property.ResourceReference{
					URN: "urn:pulumi:test::proj::aws:lambda/function:Function::fn",
					ID:  property.New("fn-id"),
				}),
			})),
		}),
	})
	require.NoError(t, err)

	outputs, ok := resp.State.GetOk("outputs")
	require.True(t, ok, "construct response should expose an outputs map")
	out := outputs.AsMap()

	value, ok := out.GetOk("handler_value")
	require.True(t, ok, "module should expose a handler_value output")
	require.Equal(t, "from-handler", value.AsString(),
		"reading a field off the passed-in reference should resolve the referenced resource's state")

	id, ok := out.GetOk("handler_id")
	require.True(t, ok, "module should expose a handler_id output")
	require.Equal(t, "fn-id", id.AsString(),
		"the reference's own id should remain readable")
}
