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
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/pulumi/pulumi-hcl/pkg/hcl/modules"
)

// captureMonitorServer is a minimal in-memory ResourceMonitor: it echoes each
// registration back so the engine's component registration resolves to a URN.
// Construct returns the module's outputs directly (from resmon.outputs), so the
// server need not retain them.
type captureMonitorServer struct {
	pulumirpc.UnimplementedResourceMonitorServer
}

// RegisterPackage answers every registration with one fixed ref; these tests
// exercise request forwarding, not package identity.
func (*captureMonitorServer) RegisterPackage(
	context.Context, *pulumirpc.RegisterPackageRequest,
) (*pulumirpc.RegisterPackageResponse, error) {
	return &pulumirpc.RegisterPackageResponse{Ref: "package-ref"}, nil
}

func (s *captureMonitorServer) RegisterResource(
	_ context.Context, req *pulumirpc.RegisterResourceRequest,
) (*pulumirpc.RegisterResourceResponse, error) {
	return &pulumirpc.RegisterResourceResponse{
		Urn:    "urn:pulumi:test::proj::" + req.Type + "::" + req.Name,
		Object: req.Object,
	}, nil
}

func (s *captureMonitorServer) RegisterResourceOutputs(
	_ context.Context, _ *pulumirpc.RegisterResourceOutputsRequest,
) (*emptypb.Empty, error) {
	return &emptypb.Empty{}, nil
}

// TestModuleConstructSecretFlow drives construct directly with a secret input
// and asserts the secret flows through the dynamic MLC boundary: the value
// reaches the module intact, arrives marked sensitive (observable via
// issensitive), and a value derived from it comes back out marked secret.
func TestModuleConstructSecretFlow(t *testing.T) {
	t.Parallel()

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	srv := grpc.NewServer()
	pulumirpc.RegisterResourceMonitorServer(srv, &captureMonitorServer{})
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	dir, err := filepath.Abs(filepath.Join("testdata", "module-sensitive"))
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
				"password": property.New("hunter2").WithSecret(true),
			})),
		}),
	})
	require.NoError(t, err)

	outputs, ok := resp.State.GetOk("outputs")
	require.True(t, ok, "construct response should expose an outputs map")
	out := outputs.AsMap()

	connection, ok := out.GetOk("connection")
	require.True(t, ok, "module should expose a connection output")
	require.Equal(t, "conn:hunter2", connection.AsString(),
		"the secret input value should reach the module intact")
	require.True(t, connection.Secret(),
		"a value derived from the secret should come back out marked secret")

	sensitive, ok := out.GetOk("password_is_sensitive")
	require.True(t, ok, "module should expose a password_is_sensitive output")
	require.Equal(t, true, sensitive.AsBool(),
		"the secret input should arrive sensitive inside the module")
}
