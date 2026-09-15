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
	"testing"

	"github.com/blang/semver"
	"github.com/google/go-cmp/cmp"
	"github.com/pulumi/pulumi/sdk/v3/go/common/workspace"
	"github.com/pulumi/pulumi/sdk/v3/go/property"
	pulumirpc "github.com/pulumi/pulumi/sdk/v3/proto/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/testing/protocmp"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/pulumi/pulumi-hcl/pkg/hcl/pkgid"
	"github.com/pulumi/pulumi-hcl/pkg/hcl/run"
)

// routingCaptureMonitorServer records the raw Invoke/Call requests so tests can
// assert on exactly what the construct-mode monitor forwards to the engine.
type routingCaptureMonitorServer struct {
	pulumirpc.UnimplementedResourceMonitorServer
	invokeReq *pulumirpc.ResourceInvokeRequest
	callReq   *pulumirpc.ResourceCallRequest
}

func (s *routingCaptureMonitorServer) RegisterPackage(
	context.Context, *pulumirpc.RegisterPackageRequest,
) (*pulumirpc.RegisterPackageResponse, error) {
	return &pulumirpc.RegisterPackageResponse{Ref: "package-ref-uuid"}, nil
}

func (s *routingCaptureMonitorServer) Invoke(
	_ context.Context, req *pulumirpc.ResourceInvokeRequest,
) (*pulumirpc.ResourceInvokeResponse, error) {
	s.invokeReq = req
	return &pulumirpc.ResourceInvokeResponse{}, nil
}

func (s *routingCaptureMonitorServer) Call(
	_ context.Context, req *pulumirpc.ResourceCallRequest,
) (*pulumirpc.CallResponse, error) {
	s.callReq = req
	return &pulumirpc.CallResponse{}, nil
}

// TestConstructMonitorForwardsInvokeRouting guards against the construct-mode
// monitor dropping provider-routing fields from data-source invokes. Without
// PackageRef the engine resolves dynamically-bridged tokens (e.g.
// aws:index/getIamPolicyDocument) against the default provider for the package
// name, which does not serve them, and module execution fails with "Invoke not
// found".
func TestConstructMonitorForwardsInvokeRouting(t *testing.T) {
	t.Parallel()

	capture := &routingCaptureMonitorServer{}
	m := newTestConstructMonitor(t, capture)

	_, err := m.Invoke(t.Context(), run.InvokeRequest{
		Token:    "aws:index/getIamPolicyDocument:getIamPolicyDocument",
		Args:     property.NewMap(map[string]property.Value{"name": property.New("x")}),
		Provider: "urn:pulumi:test::proj::pulumi:providers:aws::default::uuid",
		Package:  testPackageIdentity(t, m),
	})
	require.NoError(t, err)

	args, err := structpb.NewStruct(map[string]any{"name": "x"})
	require.NoError(t, err)
	assert.Empty(t, cmp.Diff(&pulumirpc.ResourceInvokeRequest{
		Tok:               "aws:index/getIamPolicyDocument:getIamPolicyDocument",
		Args:              args,
		Provider:          "urn:pulumi:test::proj::pulumi:providers:aws::default::uuid",
		Version:           "1.2.3",
		PluginDownloadURL: "https://example.com/download",
		PackageRef:        "package-ref-uuid",
		AcceptResources:   true,
		AcceptsByteString: true,
	}, capture.invokeReq, protocmp.Transform()))
}

// TestConstructMonitorForwardsCallRouting is the Call analogue of
// TestConstructMonitorForwardsInvokeRouting.
func TestConstructMonitorForwardsCallRouting(t *testing.T) {
	t.Parallel()

	capture := &routingCaptureMonitorServer{}
	m := newTestConstructMonitor(t, capture)

	_, err := m.Call(t.Context(), run.CallRequest{
		Token:   "aws:index:Module/getOutput",
		Args:    property.NewMap(map[string]property.Value{"name": property.New("x")}),
		Package: testPackageIdentity(t, m),
	})
	require.NoError(t, err)

	args, err := structpb.NewStruct(map[string]any{"name": "x"})
	require.NoError(t, err)
	assert.Empty(t, cmp.Diff(&pulumirpc.ResourceCallRequest{
		Tok:               "aws:index:Module/getOutput",
		Args:              args,
		Version:           "1.2.3",
		PluginDownloadURL: "https://example.com/download",
		PackageRef:        "package-ref-uuid",
		AcceptsByteString: true,
	}, capture.callReq, protocmp.Transform()))
}

// testPackageIdentity registers aws@1.2.3 from https://example.com/download
// through m; the capture server answers every registration with
// "package-ref-uuid".
func testPackageIdentity(t *testing.T, m *constructResourceMonitor) pkgid.Identity {
	t.Helper()
	v := semver.MustParse("1.2.3")
	ident, err := m.RegisterPackage(t.Context(), workspace.PackageDescriptor{
		PluginDescriptor: workspace.PluginDescriptor{
			Name: "aws", Version: &v, PluginDownloadURL: "https://example.com/download",
		},
	})
	require.NoError(t, err)
	return ident
}

// serveResourceMonitor serves the given monitor implementation over gRPC for
// the duration of the test and returns its endpoint.
func serveResourceMonitor(t *testing.T, srv pulumirpc.ResourceMonitorServer) string {
	t.Helper()

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	grpcServer := grpc.NewServer()
	pulumirpc.RegisterResourceMonitorServer(grpcServer, srv)
	go func() { _ = grpcServer.Serve(lis) }()
	t.Cleanup(grpcServer.Stop)
	return lis.Addr().String()
}

// newTestConstructMonitor serves the given monitor implementation over gRPC and
// returns a constructResourceMonitor connected to it.
func newTestConstructMonitor(t *testing.T, srv pulumirpc.ResourceMonitorServer) *constructResourceMonitor {
	t.Helper()

	conn, err := grpc.NewClient(serveResourceMonitor(t, srv), grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	client := pulumirpc.NewResourceMonitorClient(conn)
	return &constructResourceMonitor{client: client, packages: pkgid.NewRegistrar(client)}
}
