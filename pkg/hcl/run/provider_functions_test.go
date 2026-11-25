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

package run_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-go/tftypes"
	"github.com/pulumi/pulumi-hcl/pkg/hcl/parser"
	"github.com/pulumi/pulumi-hcl/pkg/hcl/run"
	"github.com/pulumi/pulumi-hcl/tests/testutil"
	"github.com/pulumi/pulumi-hcl/tests/testutil/schemaloader"
	"github.com/pulumi/pulumi-terraform-bridge/v3/pkg/tfbridge"
	"github.com/pulumi/pulumi-terraform-bridge/v3/pkg/tfbridge/info"
	shim "github.com/pulumi/pulumi-terraform-bridge/v3/pkg/tfshim"
	schemashim "github.com/pulumi/pulumi-terraform-bridge/v3/pkg/tfshim/schema"
	"github.com/pulumi/pulumi/pkg/v3/codegen/schema"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource/urn"
	"github.com/pulumi/pulumi/sdk/v3/go/common/workspace"
	"github.com/pulumi/pulumi/sdk/v3/go/property"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// providerFunctionPackage is a package spec with provider-defined functions —
// invokes with multiArgumentInputs — the shape the bridge generates:
//
//	concat_str(first string, second string|null) string
//	join_str(separator string, parts string...) string  (variadic)
//
// plus a resource to feed unknown values into calls during preview. The
// variadic-ness of join_str travels in the bridge mapping, not the schema:
// see providerFunctionInfoSource.
func providerFunctionPackage() schema.PackageSpec {
	return schema.PackageSpec{
		Name: "simple",
		Meta: &schema.MetadataSpec{ModuleFormat: `(.*)(?:/[^/]*)`},
		Resources: map[string]schema.ResourceSpec{
			"simple:index:Resource": {
				InputProperties: map[string]schema.PropertySpec{
					"value": {TypeSpec: schema.TypeSpec{Type: "string"}},
				},
				ObjectTypeSpec: schema.ObjectTypeSpec{
					Properties: map[string]schema.PropertySpec{
						"value": {TypeSpec: schema.TypeSpec{Type: "string"}},
					},
				},
			},
		},
		Functions: map[string]schema.FunctionSpec{
			"simple:index/concatStr:concatStr": {
				MultiArgumentInputs: []string{"first", "second"},
				Inputs: &schema.ObjectTypeSpec{
					Type: "object",
					Properties: map[string]schema.PropertySpec{
						"first":  {TypeSpec: schema.TypeSpec{Type: "string"}},
						"second": {TypeSpec: schema.TypeSpec{Type: "string"}},
					},
					Required: []string{"first"},
				},
				ReturnType: &schema.ReturnTypeSpec{TypeSpec: &schema.TypeSpec{Type: "string"}},
			},
			"simple:index/joinStr:joinStr": {
				MultiArgumentInputs: []string{"separator", "parts"},
				Inputs: &schema.ObjectTypeSpec{
					Type: "object",
					Properties: map[string]schema.PropertySpec{
						"separator": {TypeSpec: schema.TypeSpec{Type: "string"}},
						"parts": {TypeSpec: schema.TypeSpec{
							Type:  "array",
							Items: &schema.TypeSpec{Type: "string"},
						}},
					},
					Required: []string{"separator"},
				},
				ReturnType: &schema.ReturnTypeSpec{TypeSpec: &schema.TypeSpec{Type: "string"}},
			},
		},
	}
}

// concatInvokeHandler serves simple:index/concatStr:concatStr and
// simple:index/joinStr:joinStr, and falls through to the default data-source
// stub for other tokens.
func concatInvokeHandler(ctx context.Context, req run.InvokeRequest) (*run.InvokeResponse, error) {
	scalar := func(s string) *run.InvokeResponse {
		return &run.InvokeResponse{
			Return: property.NewMap(map[string]property.Value{"result": property.New(s)}),
		}
	}
	switch req.Token {
	case "simple:index/concatStr:concatStr":
		first := req.Args.Get("first").AsString()
		second := ""
		if v, ok := req.Args.GetOk("second"); ok && v.IsString() {
			second = v.AsString()
		}
		return scalar(first + second), nil
	case "simple:index/joinStr:joinStr":
		var parts []string
		for _, p := range req.Args.Get("parts").AsArray().All() {
			parts = append(parts, p.AsString())
		}
		return scalar(strings.Join(parts, req.Args.Get("separator").AsString())), nil
	default:
		return nil, fmt.Errorf("unexpected invoke %q", req.Token)
	}
}

// providerFunctionInfoSource serves the bridge mapping for the "simple"
// provider: join_str is variadic, a fact only the TF signature in the
// mapping's provider schema carries. concat_str is deliberately unmapped so
// it resolves through the convention path.
type providerFunctionInfoSource struct{}

func (providerFunctionInfoSource) GetProviderInfo(
	_ context.Context, tfProvider string, _ *workspace.PackageDescriptor,
) (*tfbridge.ProviderInfo, error) {
	if tfProvider != "simple" {
		return nil, fmt.Errorf("unknown provider %q", tfProvider)
	}
	return &tfbridge.ProviderInfo{
		Name: "simple",
		P: (&schemashim.Provider{
			Functions: map[string]shim.Function{
				"join_str": {
					Parameters: []shim.FunctionParameter{
						{Name: "separator", Type: tftypes.String},
					},
					VariadicParameter: &shim.FunctionParameter{
						Name: "parts",
						Type: tftypes.String,
					},
					Return: tftypes.String,
				},
			},
		}).Shim(),
		Functions: map[string]*info.Function{
			"join_str": {Tok: "simple:index/joinStr:joinStr"},
		},
	}, nil
}

func TestEngine_ProviderFunctionCall(t *testing.T) {
	t.Parallel()

	src := []byte(`
variable "name" {
  type    = string
  default = "world"
}

resource "simple_resource" "r" {
  value = provider::simple::concat_str("hello-", var.name)
}

output "joined" {
  value = provider::simple::join_str("-", "a", "b", "c")
}
`)

	p := parser.NewParser()
	config, diags := p.ParseSource("test.hcl", src)
	require.Empty(t, diags)

	mock := &testutil.MockResourceMonitor{InvokeHandler: concatInvokeHandler}
	engine := newTestEngine(t, config, &run.EngineOptions{
		ModuleLoader:       testModuleLoader(t),
		ProjectName:        "test-project",
		StackName:          "dev",
		ResourceMonitor:    mock,
		WorkDir:            t.TempDir(),
		RootDir:            t.TempDir(),
		SchemaLoader:       schemaloader.New(t, providerFunctionPackage()),
		ProviderInfoSource: providerFunctionInfoSource{},
	})
	require.NoError(t, engine.Run(t.Context()))

	require.Len(t, mock.InvokedFunctions, 2)
	byToken := map[string]run.InvokeRequest{}
	for _, req := range mock.InvokedFunctions {
		byToken[req.Token] = req
	}
	assert.Equal(t, property.NewMap(map[string]property.Value{
		"first":  property.New("hello-"),
		"second": property.New("world"),
	}), byToken["simple:index/concatStr:concatStr"].Args)
	assert.Equal(t, property.NewMap(map[string]property.Value{
		"separator": property.New("-"),
		"parts": property.New([]property.Value{
			property.New("a"), property.New("b"), property.New("c"),
		}),
	}), byToken["simple:index/joinStr:joinStr"].Args)

	require.Len(t, mock.RegisteredResources, 2)
	assert.Equal(t, property.NewMap(map[string]property.Value{
		"value": property.New("hello-world"),
	}), mock.RegisteredResources[1].Inputs)

	assert.Equal(t, property.New("a-b-c"), mock.StackOutputs.Get("joined"))
}

func TestEngine_ProviderFunctionUnknownArgumentDuringPreview(t *testing.T) {
	t.Parallel()

	// The function's argument is another resource's id, unknown during
	// preview: the provider call must be skipped and the dependent input
	// registered as computed.
	src := []byte(`
resource "simple_resource" "a" {
  value = "seed"
}

resource "simple_resource" "b" {
  value = provider::simple::concat_str("id-", simple_resource.a.id)
}
`)

	p := parser.NewParser()
	config, diags := p.ParseSource("test.hcl", src)
	require.Empty(t, diags)

	mock := &testutil.MockResourceMonitor{
		DryRun:        true,
		InvokeHandler: concatInvokeHandler,
		RegisterResourceHandler: func(_ context.Context, req run.RegisterResourceRequest) (*run.RegisterResourceResponse, error) {
			return &run.RegisterResourceResponse{
				URN:     urn.URN("urn:pulumi:test::project::" + req.Type + "::" + req.Name),
				ID:      "", // unknown id during preview
				Outputs: req.Inputs,
			}, nil
		},
	}
	engine := newTestEngine(t, config, &run.EngineOptions{
		ModuleLoader:    testModuleLoader(t),
		ProjectName:     "test-project",
		StackName:       "dev",
		ResourceMonitor: mock,
		WorkDir:         t.TempDir(),
		RootDir:         t.TempDir(),
		DryRun:          true,
		SchemaLoader:    schemaloader.New(t, providerFunctionPackage()),
	})
	require.NoError(t, engine.Run(t.Context()))

	assert.Empty(t, mock.InvokedFunctions)

	require.Len(t, mock.RegisteredResources, 3)
	assert.True(t, mock.RegisteredResources[2].Inputs.Get("value").IsComputed())
}

func TestEngine_ProviderFunctionRoutesThroughProviderBlock(t *testing.T) {
	t.Parallel()

	src := []byte(`
provider "simple" {}

resource "simple_resource" "r" {
  value = provider::simple::concat_str("a", "b")
}
`)

	p := parser.NewParser()
	config, diags := p.ParseSource("test.hcl", src)
	require.Empty(t, diags)

	mock := &testutil.MockResourceMonitor{InvokeHandler: concatInvokeHandler}
	engine := newTestEngine(t, config, &run.EngineOptions{
		ModuleLoader:    testModuleLoader(t),
		ProjectName:     "test-project",
		StackName:       "dev",
		ResourceMonitor: mock,
		WorkDir:         t.TempDir(),
		RootDir:         t.TempDir(),
		SchemaLoader:    schemaloader.New(t, providerFunctionPackage()),
	})
	require.NoError(t, engine.Run(t.Context()))

	require.Len(t, mock.InvokedFunctions, 1)
	assert.Equal(t,
		"urn:pulumi:test::project::pulumi:providers:simple::simple::simple-id",
		mock.InvokedFunctions[0].Provider)
}

func TestEngine_ProviderFunctionFailureNamesFunction(t *testing.T) {
	t.Parallel()

	src := []byte(`
resource "simple_resource" "r" {
  value = provider::simple::concat_str("a", "b")
}
`)

	p := parser.NewParser()
	config, diags := p.ParseSource("test.hcl", src)
	require.Empty(t, diags)

	mock := &testutil.MockResourceMonitor{
		InvokeHandler: func(context.Context, run.InvokeRequest) (*run.InvokeResponse, error) {
			return &run.InvokeResponse{Failures: []string{`"first" must not be empty`}}, nil
		},
	}
	engine := newTestEngine(t, config, &run.EngineOptions{
		ModuleLoader:    testModuleLoader(t),
		ProjectName:     "test-project",
		StackName:       "dev",
		ResourceMonitor: mock,
		WorkDir:         t.TempDir(),
		RootDir:         t.TempDir(),
		SchemaLoader:    schemaloader.New(t, providerFunctionPackage()),
	})
	err := engine.Run(t.Context())
	require.Error(t, err)
	assert.ErrorContains(t, err, "provider::simple::concat_str")
	assert.ErrorContains(t, err, `"first" must not be empty`)
}

func TestEngine_ProviderFunctionSensitiveArgument(t *testing.T) {
	t.Parallel()

	src := []byte(`
variable "secret" {
  type      = string
  default   = "hush"
  sensitive = true
}

resource "simple_resource" "r" {
  value = provider::simple::concat_str("k-", var.secret)
}
`)

	p := parser.NewParser()
	config, diags := p.ParseSource("test.hcl", src)
	require.Empty(t, diags)

	mock := &testutil.MockResourceMonitor{InvokeHandler: concatInvokeHandler}
	engine := newTestEngine(t, config, &run.EngineOptions{
		ModuleLoader:    testModuleLoader(t),
		ProjectName:     "test-project",
		StackName:       "dev",
		ResourceMonitor: mock,
		WorkDir:         t.TempDir(),
		RootDir:         t.TempDir(),
		SchemaLoader:    schemaloader.New(t, providerFunctionPackage()),
	})
	require.NoError(t, engine.Run(t.Context()))

	// The provider sees the plain value; the sensitivity re-attaches to the
	// call's result.
	require.Len(t, mock.InvokedFunctions, 1)
	assert.Equal(t, property.NewMap(map[string]property.Value{
		"first":  property.New("k-"),
		"second": property.New("hush"),
	}), mock.InvokedFunctions[0].Args)

	require.Len(t, mock.RegisteredResources, 2)
	assert.Equal(t, property.New("k-hush").WithSecret(true),
		mock.RegisteredResources[1].Inputs.Get("value"))
}

func TestEngine_ProviderFunctionUnknownName(t *testing.T) {
	t.Parallel()

	src := []byte(`
resource "simple_resource" "r" {
  value = provider::simple::does_not_exist("a")
}
`)

	p := parser.NewParser()
	config, diags := p.ParseSource("test.hcl", src)
	require.Empty(t, diags)

	mock := &testutil.MockResourceMonitor{}
	engine := newTestEngine(t, config, &run.EngineOptions{
		ModuleLoader:    testModuleLoader(t),
		ProjectName:     "test-project",
		StackName:       "dev",
		ResourceMonitor: mock,
		WorkDir:         t.TempDir(),
		RootDir:         t.TempDir(),
		SchemaLoader:    schemaloader.New(t, providerFunctionPackage()),
	})
	err := engine.Run(t.Context())
	require.Error(t, err)
	assert.ErrorContains(t, err, "does_not_exist")
}
