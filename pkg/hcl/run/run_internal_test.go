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

package run

import (
	"fmt"
	"testing"

	"github.com/hashicorp/hcl/v2"
	"github.com/pulumi/pulumi-hcl/pkg/hcl/ast"
	"github.com/pulumi/pulumi-hcl/pkg/hcl/bridge"
	"github.com/pulumi/pulumi-hcl/pkg/hcl/eval"
	"github.com/pulumi/pulumi-hcl/pkg/hcl/graph"
	"github.com/pulumi/pulumi-hcl/pkg/hcl/modulepath"
	"github.com/pulumi/pulumi-hcl/pkg/hcl/parser"
	"github.com/pulumi/pulumi/pkg/v3/codegen/schema"
	"github.com/pulumi/pulumi/pkg/v3/util/pdag"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource"
	"github.com/pulumi/pulumi/sdk/v3/go/property"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zclconf/go-cty/cty"
)

func TestProviderRefFromCty(t *testing.T) {
	t.Parallel()

	providerOutputs := cty.ObjectVal(map[string]cty.Value{
		"urn": cty.StringVal("urn:pulumi:dev::p::pulumi:providers:aws::aws-west"),
		"id":  cty.StringVal("provider-id-123"),
	})

	callResult := eval.MarkResourceReference(
		cty.ObjectVal(map[string]cty.Value{"id": cty.StringVal("provider-id-123")}),
		resource.URN("urn:pulumi:dev::p::pulumi:providers:aws::aws-west"),
	)

	nonProviderResource := cty.ObjectVal(map[string]cty.Value{
		"name": cty.StringVal("my-bucket"),
		"tags": cty.MapValEmpty(cty.String),
	})

	tests := []struct {
		name    string
		val     cty.Value
		want    string
		wantErr string
	}{
		{
			name: "provider resource outputs",
			val:  providerOutputs,
			want: "urn:pulumi:dev::p::pulumi:providers:aws::aws-west::provider-id-123",
		},
		{
			name: "call result with __ref",
			val:  callResult,
			want: "urn:pulumi:dev::p::pulumi:providers:aws::aws-west::provider-id-123",
		},
		{
			name:    "string value",
			val:     cty.StringVal("aws.west"),
			wantErr: "provider value must be an object, got string",
		},
		{
			name:    "non-provider resource (object without urn/id or __ref)",
			val:     nonProviderResource,
			wantErr: "provider value is not a resource reference",
		},
		{
			name:    "null value",
			val:     cty.NullVal(cty.DynamicPseudoType),
			wantErr: "provider value is null",
		},
		{
			name:    "unknown value",
			val:     cty.UnknownVal(cty.DynamicPseudoType),
			wantErr: "provider value is not yet known",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := providerRefFromCty(tt.val)
			if tt.wantErr != "" {
				assert.EqualError(t, err, tt.wantErr)
				return
			}
			assert.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestConditionResultToBool(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		val     cty.Value
		want    bool
		wantErr string
	}{
		{name: "bool true", val: cty.True, want: true},
		{name: "bool false", val: cty.False, want: false},
		// OpenTofu converts the result to bool, so the strings "true"/"false"
		// are valid condition results.
		{name: "string true", val: cty.StringVal("true"), want: true},
		{name: "string false", val: cty.StringVal("false"), want: false},
		{
			name:    "number is not convertible to bool",
			val:     cty.NumberIntVal(1),
			wantErr: "condition must be a boolean: bool required, but have number",
		},
		{
			name:    "non-bool string is not convertible",
			val:     cty.StringVal("yes"),
			wantErr: "condition must be a boolean: a bool is required",
		},
		{
			name:    "null is rejected",
			val:     cty.NullVal(cty.Bool),
			wantErr: "condition must return either true or false, not null",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := conditionResultToBool(tt.val)
			if tt.wantErr != "" {
				assert.EqualError(t, err, tt.wantErr)
				return
			}
			assert.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

// ignoreChangesSchema is shared by the attribute-path translation tests: a
// Pulumi schema where the bridge applied the default snake→camel rename
// (including a nested object property) plus a mapping with an explicit rename.
func ignoreChangesSchema() (*bridge.BodyMapping, []*schema.Property) {
	props := []*schema.Property{
		{Name: "inputOne"},
		{Name: "tags"},
		{Name: "networkConfig", Type: &schema.ArrayType{ElementType: &schema.ObjectType{
			Properties: []*schema.Property{{Name: "subnetId"}},
		}}},
	}
	mapping := &bridge.BodyMapping{Fields: map[string]*bridge.FieldMapping{
		"weird_name": {TFName: "weird_name", PulumiName: "niceName"},
		"network_config": {TFName: "network_config", PulumiName: "networkConfig", TFBlock: true, Nested: &bridge.BodyMapping{Fields: map[string]*bridge.FieldMapping{
			"subnet_id": {TFName: "subnet_id", PulumiName: "subnetId"},
		}}},
	}}
	return mapping, props
}

func TestTranslateAttrPathTraversal(t *testing.T) {
	t.Parallel()

	// attr builds a relative traversal of attribute-name segments.
	attr := func(names ...string) hcl.Traversal {
		tr := make(hcl.Traversal, len(names))
		for i, n := range names {
			tr[i] = hcl.TraverseAttr{Name: n}
		}
		return tr
	}
	strIndex := func(base hcl.Traversal, key string) hcl.Traversal {
		return append(base, hcl.TraverseIndex{Key: cty.StringVal(key)})
	}

	mapping, props := ignoreChangesSchema()

	tests := []struct {
		name      string
		traversal hcl.Traversal
		mapping   *bridge.BodyMapping
		props     []*schema.Property
		want      string
		wantErr   string
	}{
		{
			name:      "convention rename via schema props",
			traversal: attr("input_one"),
			props:     props,
			want:      "inputOne",
		},
		{
			name:      "camelCase name is rejected, requiring the TF name",
			traversal: attr("inputOne"),
			props:     props,
			wantErr:   `unknown property "inputOne"`,
		},
		{
			name:      "unknown name with a schema is rejected",
			traversal: attr("not_a_property"),
			props:     props,
			wantErr:   `unknown property "not_a_property"`,
		},
		{
			name:      "explicit rename via bridge mapping",
			traversal: attr("weird_name"),
			mapping:   mapping,
			want:      "niceName",
		},
		{
			name:      "map key passes through unchanged",
			traversal: strIndex(attr("tags"), "input_one"),
			props:     props,
			want:      "tags.input_one",
		},
		{
			name:      "nested block field renamed via mapping",
			traversal: attr("network_config", "subnet_id"),
			mapping:   mapping,
			want:      "networkConfig.subnetId",
		},
		{
			name:      "nested object field renamed via schema props",
			traversal: attr("network_config", "subnet_id"),
			props:     props,
			want:      "networkConfig.subnetId",
		},
		{
			name:      "unknown name with no schema passes through",
			traversal: attr("input_one"),
			want:      "input_one",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			glob, err := translateAttrPathTraversal(tt.traversal, tt.mapping, tt.props)
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			text, err := glob.MarshalText()
			require.NoError(t, err)
			assert.Equal(t, tt.want, string(text))
		})
	}
}

// TestIgnoreChangesApplies covers the OpenTofu-style existence check that
// decides whether an ignore_changes entry is forwarded to the engine: the path
// (minus a trailing map key) must resolve inside the evaluated inputs.
func TestIgnoreChangesApplies(t *testing.T) {
	t.Parallel()

	attr := func(names ...string) hcl.Traversal {
		tr := make(hcl.Traversal, len(names))
		for i, n := range names {
			tr[i] = hcl.TraverseAttr{Name: n}
		}
		return tr
	}
	numIndex := func(base hcl.Traversal, i int) hcl.Traversal {
		return append(base, hcl.TraverseIndex{Key: cty.NumberIntVal(int64(i))})
	}
	strIndex := func(base hcl.Traversal, key string) hcl.Traversal {
		return append(base, hcl.TraverseIndex{Key: cty.StringVal(key)})
	}

	mapping := &bridge.BodyMapping{Fields: map[string]*bridge.FieldMapping{
		"note": {TFName: "note", PulumiName: "note"},
		"tags": {TFName: "tags", PulumiName: "tags"},
		"settings": {
			TFName: "settings", PulumiName: "settings", TFBlock: true, MaxItemsOne: true,
			Nested: &bridge.BodyMapping{Fields: map[string]*bridge.FieldMapping{
				"mode": {TFName: "mode", PulumiName: "mode"},
			}},
		},
		"rules": {
			TFName: "rules", PulumiName: "rules", TFBlock: true,
			Nested: &bridge.BodyMapping{Fields: map[string]*bridge.FieldMapping{
				"port": {TFName: "port", PulumiName: "port"},
			}},
		},
	}}

	tests := []struct {
		name      string
		traversal hcl.Traversal
		inputs    property.Map
		want      bool
	}{
		{
			name:      "leaf attribute present",
			traversal: attr("note"),
			inputs:    property.NewMap(map[string]property.Value{"note": property.New("x")}),
			want:      true,
		},
		{
			name:      "leaf attribute unset decodes as null and still applies",
			traversal: attr("note"),
			inputs:    property.Map{},
			want:      true,
		},
		{
			name:      "singular block removed un-ignores its attributes",
			traversal: append(numIndex(attr("settings"), 0), hcl.TraverseAttr{Name: "mode"}),
			inputs:    property.NewMap(map[string]property.Value{"note": property.New("y")}),
			want:      false,
		},
		{
			name:      "singular block present",
			traversal: attr("settings", "mode"),
			inputs: property.NewMap(map[string]property.Value{
				"settings": property.New(map[string]property.Value{"mode": property.New("a")}),
			}),
			want: true,
		},
		{
			name:      "list block index in range",
			traversal: append(numIndex(attr("rules"), 0), hcl.TraverseAttr{Name: "port"}),
			inputs: property.NewMap(map[string]property.Value{
				"rules": property.New([]property.Value{
					property.New(map[string]property.Value{"port": property.New(80.0)}),
				}),
			}),
			want: true,
		},
		{
			name:      "list block index out of range",
			traversal: append(numIndex(attr("rules"), 1), hcl.TraverseAttr{Name: "port"}),
			inputs: property.NewMap(map[string]property.Value{
				"rules": property.New([]property.Value{
					property.New(map[string]property.Value{"port": property.New(80.0)}),
				}),
			}),
			want: false,
		},
		{
			name:      "trailing map key may be absent",
			traversal: strIndex(attr("tags"), "missing"),
			inputs:    property.NewMap(map[string]property.Value{"tags": property.New(map[string]property.Value{})}),
			want:      true,
		},
		{
			name:      "trailing map key with map itself unset",
			traversal: strIndex(attr("tags"), "k"),
			inputs:    property.Map{},
			want:      true,
		},
		{
			name:      "non-trailing map key must exist",
			traversal: append(strIndex(attr("tags"), "missing"), hcl.TraverseAttr{Name: "x"}),
			inputs:    property.NewMap(map[string]property.Value{"tags": property.New(map[string]property.Value{})}),
			want:      false,
		},
		{
			name:      "unknown value cannot be disproven",
			traversal: attr("settings", "mode"),
			inputs:    property.NewMap(map[string]property.Value{"settings": property.New(property.Computed)}),
			want:      true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, ignoreChangesApplies(tt.traversal, mapping, nil, tt.inputs))
		})
	}
}

// TestTranslateSecretOutputName covers the single-name translation used by
// additional_secret_outputs: a TF-convention name is translated to its Pulumi
// name, a name already in Pulumi form passes through, and a multi-segment path
// is rejected.
func TestTranslateSecretOutputName(t *testing.T) {
	t.Parallel()

	// attr builds a relative traversal of attribute-name segments, matching what
	// the parser produces for additional_secret_outputs entries.
	attr := func(names ...string) hcl.Traversal {
		tr := make(hcl.Traversal, len(names))
		for i, n := range names {
			tr[i] = hcl.TraverseAttr{Name: n}
		}
		return tr
	}

	mapping, props := ignoreChangesSchema()

	tests := []struct {
		name      string
		traversal hcl.Traversal
		mapping   *bridge.BodyMapping
		props     []*schema.Property
		want      string
		wantErr   string
	}{
		{name: "snake_case translated via schema props", traversal: attr("input_one"), props: props, want: "inputOne"},
		{name: "camelCase name is rejected", traversal: attr("inputOne"), props: props, wantErr: `unknown property "inputOne"`},
		{name: "explicit rename via bridge mapping", traversal: attr("weird_name"), mapping: mapping, want: "niceName"},
		{name: "unknown name with no schema passes through", traversal: attr("input_one"), want: "input_one"},
		{
			name:      "multi-segment path rejected",
			traversal: attr("network_config", "subnet_id"),
			props:     props,
			wantErr:   `invalid additional_secret_outputs entry "network_config.subnet_id": expected a single top-level property name`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := translateSecretOutputName(tt.traversal, tt.mapping, tt.props)
			if tt.wantErr != "" {
				require.EqualError(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

// TestMovedFromModuleInits pins the ordering that keeps a moved-out resource's
// alias intact: the resource's cell must wait for the init of every
// still-configured module a chain of moved blocks names as a prior home, or
// resolveMovedAliases races processModuleInit for the prior module's component
// URN and silently drops the alias when it wins (#586).
func TestMovedFromModuleInits(t *testing.T) {
	t.Parallel()

	parse := func(name, src string) *ast.Config {
		config, diags := parser.NewParser().ParseSource(name, []byte(src))
		require.False(t, diags.HasErrors(), diags.Error())
		return config
	}

	child := parse("mod.hcl", `resource "simple_resource" "kept" { input_one = "k" }`)
	root := parse("root.hcl", `
module "b" { source = "./mod" }
module "c" { source = "./mod" }

resource "simple_resource" "r" { input_one = "x" }

# Chained move: module.c.r -> module.b.moved_r -> simple_resource.r, composed
# with the module.a -> module.b call rename from the flaky case. The resource
# must wait for both prior modules' inits; the whole-call rename adds none.
moved {
  from = module.a
  to   = module.b
}

moved {
  from = module.c.simple_resource.r
  to   = module.b.simple_resource.moved_r
}

moved {
  from = module.b.simple_resource.moved_r
  to   = simple_resource.r
}
`)

	g, err := graph.BuildFromConfig(root, fakeModuleLoader{modules: map[string]*graph.LoadedModule{
		"./mod": {Config: child, SourcePath: "./mod"},
	}}, ".")
	require.False(t, err.HasErrors())
	require.Empty(t, g.Validate())

	var resNode *graph.Node
	for _, node := range g.ExpandableNodes() {
		if node.ModuleInfo == nil && node.Type == graph.NodeTypeResource {
			resNode = node
		}
	}
	require.NotNil(t, resNode, "no root resource node")

	initNode := func(name string) pdag.Node {
		init, ok := g.KeyNode(graph.NodeKey{
			Module: modulepath.Root().Append(modulepath.NewStep(name)),
			ID:     "__init__",
		})
		require.True(t, ok, "no init node for module.%s", name)
		return init
	}

	e := &Engine{graph: g}
	assert.ElementsMatch(t, []pdag.Node{initNode("b"), initNode("c")}, e.movedFromModuleInits(resNode))

	// The kept resource inside the module is no moved target: no ordering.
	var childNode *graph.Node
	for _, node := range g.ExpandableNodes() {
		if node.ModuleInfo != nil && node.Type == graph.NodeTypeResource {
			childNode = node
		}
	}
	require.NotNil(t, childNode, "no child resource node")
	assert.Empty(t, e.movedFromModuleInits(childNode))
}

type fakeModuleLoader struct {
	modules map[string]*graph.LoadedModule
}

func (f fakeModuleLoader) LoadModule(module *ast.Module, _ string) (*graph.LoadedModule, hcl.Diagnostics) {
	m, ok := f.modules[module.Source]
	if !ok {
		return nil, hcl.Diagnostics{{Severity: hcl.DiagError, Summary: fmt.Sprintf("no module %q", module.Source), Subject: module.DeclRange.Ptr()}}
	}
	return m, nil
}
