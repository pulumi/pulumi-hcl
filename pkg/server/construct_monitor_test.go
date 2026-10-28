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

	"github.com/google/go-cmp/cmp"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource/plugin"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource/urn"
	"github.com/pulumi/pulumi/sdk/v3/go/property"
	pulumirpc "github.com/pulumi/pulumi/sdk/v3/proto/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/testing/protocmp"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/pulumi/pulumi-hcl/pkg/hcl/run"
)

// routingCaptureMonitorServer records the raw Invoke/Call requests so tests can
// assert on exactly what the construct-mode monitor forwards to the engine.
type routingCaptureMonitorServer struct {
	pulumirpc.UnimplementedResourceMonitorServer
	invokeReq   *pulumirpc.ResourceInvokeRequest
	callReq     *pulumirpc.ResourceCallRequest
	registerReq *pulumirpc.RegisterResourceRequest
}

func (s *routingCaptureMonitorServer) RegisterResource(
	_ context.Context, req *pulumirpc.RegisterResourceRequest,
) (*pulumirpc.RegisterResourceResponse, error) {
	s.registerReq = req
	return &pulumirpc.RegisterResourceResponse{Urn: "urn:child", Id: "id"}, nil
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
		Token:             "aws:index/getIamPolicyDocument:getIamPolicyDocument",
		Args:              property.NewMap(map[string]property.Value{"name": property.New("x")}),
		Provider:          "urn:pulumi:test::proj::pulumi:providers:aws::default::uuid",
		Version:           "1.2.3",
		PluginDownloadURL: "https://example.com/download",
		PackageRef:        "package-ref-uuid",
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
		Token:      "aws:index:Module/getOutput",
		Args:       property.NewMap(map[string]property.Value{"name": property.New("x")}),
		PackageRef: "package-ref-uuid",
	})
	require.NoError(t, err)

	args, err := structpb.NewStruct(map[string]any{"name": "x"})
	require.NoError(t, err)
	assert.Empty(t, cmp.Diff(&pulumirpc.ResourceCallRequest{
		Tok:  "aws:index:Module/getOutput",
		Args: args,
		ArgDependencies: map[string]*pulumirpc.ResourceCallRequest_ArgumentDependencies{
			"name": {},
		},
		PackageRef:        "package-ref-uuid",
		AcceptsByteString: true,
	}, capture.callReq, protocmp.Transform()))
}

// TestConstructMonitorForwardsResourceOptions guards against the construct-mode
// monitor dropping resource options when it registers a module's resources. A
// dropped replacement trigger, for example, makes a changed `triggers_replace`
// inside an `hcl_module` update the resource in place instead of replacing it.
// The resource's name, parent and aliases are moved under the component.
func TestConstructMonitorForwardsResourceOptions(t *testing.T) {
	t.Parallel()

	const componentURN = "urn:pulumi:test::proj::hcl:index:Module::m"
	capture := &routingCaptureMonitorServer{}
	m := newTestConstructMonitor(t, capture)
	m.componentName = "m"
	m.componentURN = componentURN
	retain := true

	_, err := m.RegisterResource(t.Context(), run.RegisterResourceRequest{
		Type:   "pulumi:index:Stash",
		Name:   "t",
		Custom: true,
		Aliases: []run.Alias{
			{URN: "urn:pulumi:test::proj::pulumi:index:Stash::old"},
			{Spec: &run.AliasSpec{Name: "old", NoParent: true}},
		},
		DeleteBeforeReplace:     true,
		DeleteBeforeReplaceDef:  true,
		CustomTimeouts:          &run.CustomTimeouts{Create: 60},
		ImportId:                "import-id",
		AdditionalSecretOutputs: []string{"output"},
		RetainOnDelete:          &retain,
		DeletedWith:             "urn:deleted-with",
		ReplaceWith:             []string{"urn:replace-with"},
		HideDiffs:               []property.Glob{property.GlobFromSegments(property.NewSegment("input"))},
		ReplaceOnChanges:        []property.Glob{property.GlobFromSegments(property.NewSegment("input"))},
		ReplacementTrigger:      property.New("b"),
		EnvVarMappings:          map[string]string{"A": "B"},
	})
	require.NoError(t, err)

	protect := false
	assert.Empty(t, cmp.Diff(&pulumirpc.RegisterResourceRequest{
		Type:   "pulumi:index:Stash",
		Name:   "m-t",
		Custom: true,
		Object: &structpb.Struct{},
		Parent: componentURN,
		Aliases: []*pulumirpc.Alias{
			{Alias: &pulumirpc.Alias_Urn{Urn: "urn:pulumi:test::proj::pulumi:index:Stash::old"}},
			{Alias: &pulumirpc.Alias_Spec_{Spec: &pulumirpc.Alias_Spec{
				Name:   "m-old",
				Parent: &pulumirpc.Alias_Spec_ParentUrn{ParentUrn: componentURN},
			}}},
		},
		Protect:                    &protect,
		Dependencies:               []string{},
		PropertyDependencies:       map[string]*pulumirpc.RegisterResourceRequest_PropertyDependencies{},
		AcceptSecrets:              true,
		AcceptResources:            true,
		AcceptsByteString:          true,
		SupportsPartialValues:      true,
		DeleteBeforeReplace:        true,
		DeleteBeforeReplaceDefined: true,
		CustomTimeouts:             &pulumirpc.RegisterResourceRequest_CustomTimeouts{Create: "1m0s"},
		ImportId:                   "import-id",
		AdditionalSecretOutputs:    []string{"output"},
		RetainOnDelete:             &retain,
		DeletedWith:                "urn:deleted-with",
		ReplaceWith:                []string{"urn:replace-with"},
		HideDiffs:                  []string{"input"},
		ReplaceOnChanges:           []string{"input"},
		ReplacementTrigger:         structpb.NewStringValue("b"),
		EnvVarMappings:             map[string]string{"A": "B"},
	}, capture.registerReq, protocmp.Transform()))
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

	return &constructResourceMonitor{client: pulumirpc.NewResourceMonitorClient(conn)}
}

// callDepsMonitorServer answers every Call the way the engine answers
// getBoth(a = pa.text, b = pb.text): ReturnDependencies holds the coarse
// union for `wrapper`, and the output-value return holds per-leaf deps.
type callDepsMonitorServer struct {
	pulumirpc.UnimplementedResourceMonitorServer
	callReq *pulumirpc.ResourceCallRequest
}

func (s *callDepsMonitorServer) Call(
	_ context.Context, req *pulumirpc.ResourceCallRequest,
) (*pulumirpc.CallResponse, error) {
	s.callReq = req
	ret, err := plugin.MarshalProperties(resource.PropertyMap{
		"wrapper": resource.NewProperty(resource.PropertyMap{
			"a": resource.NewProperty(resource.Output{
				Element: resource.NewProperty("pa"), Known: true, Dependencies: []resource.URN{"urn:pa"},
			}),
			"b": resource.NewProperty(resource.Output{
				Element: resource.NewProperty("pb"), Known: true, Dependencies: []resource.URN{"urn:pb"},
			}),
		}),
	}, plugin.MarshalOptions{KeepOutputValues: req.AcceptOutputValues})
	if err != nil {
		return nil, err
	}
	return &pulumirpc.CallResponse{
		Return: ret,
		ReturnDependencies: map[string]*pulumirpc.CallResponse_ReturnDependencies{
			"wrapper": {Urns: []string{"urn:pa", "urn:pb"}},
		},
	}, nil
}

func TestCallMethodDependencies(t *testing.T) {
	t.Parallel()

	req := run.CallRequest{
		Token: "pick-call:index:Picker/getBoth",
		Args: property.NewMap(map[string]property.Value{
			"a": property.New("pa").WithDependencies([]urn.URN{"urn:pa"}),
			"b": property.New("pb").WithDependencies([]urn.URN{"urn:pb"}),
		}),
	}
	argDeps := map[string]*pulumirpc.ResourceCallRequest_ArgumentDependencies{
		"a": {Urns: []string{"urn:pa"}},
		"b": {Urns: []string{"urn:pb"}},
	}

	t.Run("output values", func(t *testing.T) {
		t.Parallel()
		srv := &callDepsMonitorServer{}
		m := newTestConstructMonitor(t, srv)

		ret, _, err := callMethod(t.Context(), m.client, req, true, plugin.MarshalOptions{})
		require.NoError(t, err)

		args, err := plugin.MarshalProperties(resource.PropertyMap{
			"a": resource.NewProperty(resource.Output{
				Element: resource.NewProperty("pa"), Known: true, Dependencies: []resource.URN{"urn:pa"},
			}),
			"b": resource.NewProperty(resource.Output{
				Element: resource.NewProperty("pb"), Known: true, Dependencies: []resource.URN{"urn:pb"},
			}),
		}, plugin.MarshalOptions{KeepOutputValues: true})
		require.NoError(t, err)
		assert.Empty(t, cmp.Diff(&pulumirpc.ResourceCallRequest{
			Tok:                "pick-call:index:Picker/getBoth",
			Args:               args,
			ArgDependencies:    argDeps,
			AcceptsByteString:  true,
			AcceptOutputValues: true,
		}, srv.callReq, protocmp.Transform()))

		assert.Equal(t, property.NewMap(map[string]property.Value{
			"wrapper": property.New(property.NewMap(map[string]property.Value{
				"a": property.New("pa").WithDependencies([]urn.URN{"urn:pa"}),
				"b": property.New("pb").WithDependencies([]urn.URN{"urn:pb"}),
			})),
		}), ret)
	})

	t.Run("dependency maps", func(t *testing.T) {
		t.Parallel()
		srv := &callDepsMonitorServer{}
		m := newTestConstructMonitor(t, srv)

		ret, _, err := callMethod(t.Context(), m.client, req, false, plugin.MarshalOptions{})
		require.NoError(t, err)

		args, err := structpb.NewStruct(map[string]any{"a": "pa", "b": "pb"})
		require.NoError(t, err)
		assert.Empty(t, cmp.Diff(&pulumirpc.ResourceCallRequest{
			Tok:               "pick-call:index:Picker/getBoth",
			Args:              args,
			ArgDependencies:   argDeps,
			AcceptsByteString: true,
		}, srv.callReq, protocmp.Transform()))

		assert.Equal(t, property.NewMap(map[string]property.Value{
			"wrapper": property.New(property.NewMap(map[string]property.Value{
				"a": property.New("pa"),
				"b": property.New("pb"),
			})).WithDependencies([]urn.URN{"urn:pa", "urn:pb"}),
		}), ret)
	})
}

// deploymentInfoMonitorServer answers GetDeploymentInfo with errs, one per
// call, and with info after errs runs out.
type deploymentInfoMonitorServer struct {
	pulumirpc.UnimplementedResourceMonitorServer
	errs  []error
	info  *pulumirpc.DeploymentInfo
	calls int
}

func (s *deploymentInfoMonitorServer) GetDeploymentInfo(
	context.Context, *emptypb.Empty,
) (*pulumirpc.DeploymentInfo, error) {
	s.calls++
	if len(s.errs) > 0 {
		err := s.errs[0]
		s.errs = s.errs[1:]
		return nil, err
	}
	return s.info, nil
}

func TestCallOutputValuesProbe(t *testing.T) {
	t.Parallel()

	supported := &pulumirpc.DeploymentInfo{SupportedFeatures: []pulumirpc.ResourceMonitorFeature{
		pulumirpc.ResourceMonitorFeature_RESOURCE_MONITOR_FEATURE_CALL_OUTPUT_VALUES,
	}}

	t.Run("an error is returned and cached", func(t *testing.T) {
		t.Parallel()
		srv := &deploymentInfoMonitorServer{
			errs: []error{status.Error(codes.Unavailable, "try again")},
			info: supported,
		}
		m := newTestConstructMonitor(t, srv)
		var p callOutputValuesProbe

		_, err := p.get(t.Context(), m.client)
		assert.EqualError(t, err, "checking monitor features: rpc error: code = Unavailable desc = try again")

		_, err = p.get(t.Context(), m.client)
		assert.EqualError(t, err, "checking monitor features: rpc error: code = Unavailable desc = try again")
		assert.Equal(t, 1, srv.calls)
	})

	t.Run("an unimplemented RPC means not supported", func(t *testing.T) {
		t.Parallel()
		srv := &deploymentInfoMonitorServer{
			errs: []error{status.Error(codes.Unimplemented, "unimplemented")},
			info: supported,
		}
		m := newTestConstructMonitor(t, srv)
		var p callOutputValuesProbe

		got, err := p.get(t.Context(), m.client)
		require.NoError(t, err)
		assert.False(t, got)

		got, err = p.get(t.Context(), m.client)
		require.NoError(t, err)
		assert.False(t, got)
		assert.Equal(t, 1, srv.calls)
	})
}
