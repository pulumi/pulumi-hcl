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

package graph

import (
	"context"
	"testing"

	"github.com/hashicorp/hcl/v2"
	"github.com/pulumi/pulumi-hcl/pkg/hcl/modulepath"
	"github.com/pulumi/pulumi-hcl/pkg/hcl/parser"
	"github.com/pulumi/pulumi/pkg/v3/util/pdag"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zclconf/go-cty/cty"
)

func TestBuildFromConfig(t *testing.T) {
	t.Parallel()
	src := []byte(`
variable "name" {
  type = string
}

locals {
  greeting = "Hello, ${var.name}"
}

resource "aws_instance" "web" {
  ami = local.greeting
}

output "instance_id" {
  value = aws_instance.web.id
}
`)

	p := parser.NewParser()
	config, diags := p.ParseSource("test.hcl", src)
	require.Empty(t, diags)

	g, err := BuildFromConfig(config, nil, "")
	require.False(t, err.HasErrors())

	nodes := g.seen
	require.Len(t, nodes, 7, "4 explicit nodes + 3 builtins")

	// Verify dependencies
	localNode := g.seen[nk("local.greeting")].n
	if localNode == nil {
		t.Fatal("Expected local.greeting node")
	}

	// Verify topological sort works
	var sorted []string
	errWalk := g.dag.Walk(t.Context(), func(_ context.Context, n dagNode) error {
		sorted = append(sorted, n.desc.String())
		return nil
	}, pdag.MaxProcs(1))
	require.NoError(t, errWalk)

	positions := make(map[string]int)
	for i, node := range sorted {
		positions[node] = i
	}

	if positions["var.name"] >= positions["local.greeting"] {
		t.Error("var.name should come before local.greeting")
	}
	if positions["local.greeting"] >= positions["aws_instance.web"] {
		t.Error("local.greeting should come before aws_instance.web")
	}
	if positions["aws_instance.web"] >= positions["output.instance_id"] {
		t.Error("aws_instance.web should come before output.instance_id")
	}
}

// TestBuildFromConfigSelfModuleCycle: a module whose source resolves back to
// the root directory must fail with a cycle error instead of recursing forever.
func TestBuildFromConfigSelfModuleCycle(t *testing.T) {
	t.Parallel()
	src := []byte(`
module "self" {
  source = "./."
}
`)
	config, diags := parser.NewParser().ParseSource("main.tf", src)
	require.False(t, diags.HasErrors(), diags.Error())

	loader := fakeModuleLoader{modules: map[string]*LoadedModule{
		"./.": {Config: config, SourcePath: "/proj"},
	}}

	_, err := BuildFromConfig(config, loader, "/proj")
	require.Len(t, err, 1)
	assert.NotNil(t, err[0].Subject)
	require.Equal(t, err[0].Summary,
		`module "self" source "./." creates a module cycle: /proj -> /proj`)
}

// TestBuildFromConfigIndirectModuleCycle: a cycle through an intermediate
// module (a -> b -> a) must fail, while the same module reused as a sibling
// stays legal (covered by the a-under-root and a-under-b duplication here
// erroring only on the ancestor repeat).
func TestBuildFromConfigIndirectModuleCycle(t *testing.T) {
	t.Parallel()
	rootSrc := []byte(`
module "a" {
  source = "./a"
}
`)
	aSrc := []byte(`
module "b" {
  source = "./b"
}
`)
	bSrc := []byte(`
module "a" {
  source = "./a"
}
`)
	p := parser.NewParser()
	root, diags := p.ParseSource("main.tf", rootSrc)
	require.False(t, diags.HasErrors(), diags.Error())
	a, diags := p.ParseSource("a.tf", aSrc)
	require.False(t, diags.HasErrors(), diags.Error())
	b, diags := p.ParseSource("b.tf", bSrc)
	require.False(t, diags.HasErrors(), diags.Error())

	loader := fakeModuleLoader{modules: map[string]*LoadedModule{
		"./a": {Config: a, SourcePath: "/proj/a"},
		"./b": {Config: b, SourcePath: "/proj/b"},
	}}

	_, err := BuildFromConfig(root, loader, "/proj")
	require.Len(t, err, 1)
	assert.NotNil(t, err[0].Subject)
	require.Equal(t, err[0].Summary,
		``+
			`module "a" source "./a" creates a module cycle: /proj -> /proj/a -> /proj/b -> /proj/a`)
}

// TestBuildFromConfigSiblingModuleReuse: the same module included twice as
// siblings is not a cycle and must build.
func TestBuildFromConfigSiblingModuleReuse(t *testing.T) {
	t.Parallel()
	rootSrc := []byte(`
module "x" {
  source = "./child"
}

module "y" {
  source = "./child"
}
`)
	childSrc := []byte(`
resource "simple_resource" "r" {
  input_one = "a"
}
`)
	p := parser.NewParser()
	root, diags := p.ParseSource("main.tf", rootSrc)
	require.False(t, diags.HasErrors(), diags.Error())
	child, diags := p.ParseSource("child.tf", childSrc)
	require.False(t, diags.HasErrors(), diags.Error())

	loader := fakeModuleLoader{modules: map[string]*LoadedModule{
		"./child": {Config: child, SourcePath: "/proj/child"},
	}}

	_, err := BuildFromConfig(root, loader, "/proj")
	require.False(t, err.HasErrors())
}

func TestVariableValidationResourceRefSplitsNode(t *testing.T) {
	t.Parallel()
	// gate's validation references a resource, so the rules move to a
	// validation node that owns "var.gate" while an internal value node
	// evaluates the value. An InjectAfter barrier on variable nodes (as the
	// runtime adds before walking) must then order value -> barrier ->
	// resource -> validation without a cycle.
	src := []byte(`
resource "simple_resource" "r" {
  input_one = "a"
}

variable "gate" {
  type    = string
  default = "a"

  validation {
    condition     = simple_resource.r.result == "${var.gate}-false"
    error_message = "failed"
  }
}

output "gate" {
  value = var.gate
}
`)

	p := parser.NewParser()
	config, diags := p.ParseSource("test.hcl", src)
	require.Empty(t, diags)

	g, err := BuildFromConfig(config, nil, "")
	require.False(t, err.HasErrors())

	assert.Equal(t, NodeTypeVariableValidation, g.seen[nk("var.gate")].n.Type)
	assert.Equal(t, NodeTypeVariable, g.seen[nk("var.gate!value")].n.Type)

	barrierKey := "barrier"
	require.NoError(t, g.InjectAfter(func(context.Context) error { return nil }, func(n *Node) bool {
		return n.Type == NodeTypeVariable
	}))

	var sorted []string
	errWalk := g.dag.Walk(t.Context(), func(_ context.Context, n dagNode) error {
		key := n.desc.String()
		if n.exec != nil {
			key = barrierKey
		}
		sorted = append(sorted, key)
		return nil
	}, pdag.MaxProcs(1))
	require.NoError(t, errWalk)

	positions := make(map[string]int)
	for i, key := range sorted {
		positions[key] = i
	}
	assert.Less(t, positions["var.gate!value"], positions[barrierKey])
	assert.Less(t, positions[barrierKey], positions["simple_resource.r"])
	assert.Less(t, positions["simple_resource.r"], positions["var.gate"])
	assert.Less(t, positions["var.gate"], positions["output.gate"])
}

func TestVariableValidationSelfRefKeepsSingleNode(t *testing.T) {
	t.Parallel()
	src := []byte(`
variable "name" {
  type = string

  validation {
    condition     = length(var.name) > 0
    error_message = "empty"
  }
}
`)

	p := parser.NewParser()
	config, diags := p.ParseSource("test.hcl", src)
	require.Empty(t, diags)

	g, err := BuildFromConfig(config, nil, "")
	require.False(t, err.HasErrors())

	assert.Equal(t, NodeTypeVariable, g.seen[nk("var.name")].n.Type)
	assert.NotContains(t, g.seen, nk("var.name!value"))
}

func TestForcedCreateBeforeDestroy(t *testing.T) {
	t.Parallel()
	// c declares create_before_destroy and depends on b, which depends on a, so
	// the behaviour propagates back to b and a. d is independent and unaffected.
	src := []byte(`
resource "aws_instance" "a" {
  ami = "x"
}

resource "aws_instance" "b" {
  ami = aws_instance.a.id
}

resource "aws_instance" "c" {
  ami = aws_instance.b.id
  lifecycle {
    create_before_destroy = true
  }
}

resource "aws_instance" "d" {
  ami = "y"
}
`)

	p := parser.NewParser()
	config, diags := p.ParseSource("test.hcl", src)
	require.Empty(t, diags)

	g, err := BuildFromConfig(config, nil, "")
	require.False(t, err.HasErrors())

	assert.Equal(t, map[NodeKey]bool{
		nk("aws_instance.a"): true,
		nk("aws_instance.b"): true,
		nk("aws_instance.c"): true,
	}, g.ForcedCreateBeforeDestroy())
}

func TestForcedCreateBeforeDestroyOverridesExplicitFalse(t *testing.T) {
	t.Parallel()
	// The dependency explicitly sets create_before_destroy = false, but a
	// dependent has it true. The behaviour is forced onto the dependency
	// anyway, matching tofu's auto-upgrade (a CBD node depending on a non-CBD
	// one would otherwise create a cycle).
	src := []byte(`
resource "aws_instance" "dep" {
  ami = "x"
  lifecycle {
    create_before_destroy = false
  }
}

resource "aws_instance" "user" {
  ami = aws_instance.dep.id
  lifecycle {
    create_before_destroy = true
  }
}
`)

	p := parser.NewParser()
	config, diags := p.ParseSource("test.hcl", src)
	require.Empty(t, diags)

	g, err := BuildFromConfig(config, nil, "")
	require.False(t, err.HasErrors())

	assert.Equal(t, map[NodeKey]bool{
		nk("aws_instance.dep"):  true,
		nk("aws_instance.user"): true,
	}, g.ForcedCreateBeforeDestroy())
}

func TestValidate(t *testing.T) {
	t.Parallel()
	g := NewGraph()

	// Missing dependency
	_, i := g.newNode(nk("NonExistent"))
	diags := g.AddNode(&Node{Key: nk("A"), Type: NodeTypeLocal}, []pdag.Node{i})
	require.False(t, diags.HasErrors(), "%s", diags)

	errors := g.Validate()
	if len(errors) != 1 {
		t.Errorf("Expected 1 error, got %d", len(errors))
	}
}

// TestValidateUnknownNodeReportsSourceLocation covers
// https://github.com/pulumi/pulumi-hcl/issues/153: a reference to a
// node that does not exist must report the source location of the offending
// traversal, not just the unknown key.
func TestValidateUnknownNodeReportsSourceLocation(t *testing.T) {
	t.Parallel()
	src := []byte(`resource "aws_s2_bucket" "example" {
  bucket = "${resource.fuzz.bucket}"
}
`)

	p := parser.NewParser()
	config, diags := p.ParseSource("test.hcl", src)
	require.Empty(t, diags)

	g, err := BuildFromConfig(config, nil, "")
	require.False(t, err.HasErrors())

	errs := g.Validate()
	require.Len(t, errs, 1)

	var diag *hcl.Diagnostic
	require.ErrorAs(t, errs[0], &diag)
	require.NotNil(t, diag.Subject, "diagnostic must include a source location")
	assert.Equal(t, &hcl.Diagnostic{
		Severity: hcl.DiagError,
		Summary:  `unknown node "resource.fuzz"`,
		Subject: &hcl.Range{
			Filename: "test.hcl",
			Start:    hcl.Pos{Line: 2, Column: 15, Byte: 51},
			End:      hcl.Pos{Line: 2, Column: 35, Byte: 71},
		},
	}, diag)
}

func TestResourceExpander(t *testing.T) {
	t.Parallel()

	node := &Node{
		Key:  nk("aws_instance.web"),
		Type: NodeTypeResource,
	}

	t.Run("single instance", func(t *testing.T) {
		t.Parallel()
		result := NewResourceExpander().Expand(node)
		if !result.IsSingle {
			t.Error("Expected single instance")
		}
		if len(result.Instances) != 1 {
			t.Errorf("Expected 1 instance, got %d", len(result.Instances))
		}
		if result.Instances[0].Key.String() != "aws_instance.web" {
			t.Errorf("Unexpected key: %s", result.Instances[0].Key)
		}
	})

	t.Run("count expansion", func(t *testing.T) {
		t.Parallel()
		expander := NewResourceExpander()
		expander.SetCount(nk("aws_instance.web"), 3)
		result := expander.Expand(node)
		if result.IsSingle {
			t.Error("Should not be single instance")
		}
		if len(result.Instances) != 3 {
			t.Errorf("Expected 3 instances, got %d", len(result.Instances))
		}
		for i, inst := range result.Instances {
			expectedKey := "aws_instance.web[" + string(rune('0'+i)) + "]"
			if inst.Key.String() != expectedKey {
				t.Errorf("Instance %d: expected key %s, got %s", i, expectedKey, inst.Key)
			}
			if inst.Index == nil || *inst.Index != i {
				t.Errorf("Instance %d: expected index %d", i, i)
			}
		}
	})

	t.Run("count zero", func(t *testing.T) {
		t.Parallel()
		expander := NewResourceExpander()
		expander.SetCount(nk("aws_instance.zero"), 0)
		zeroNode := &Node{Key: nk("aws_instance.zero"), Type: NodeTypeResource}
		result := expander.Expand(zeroNode)
		if result.IsSingle {
			t.Error("Should not be single instance")
		}
		if len(result.Instances) != 0 {
			t.Errorf("Expected 0 instances, got %d", len(result.Instances))
		}
	})

	t.Run("for_each expansion", func(t *testing.T) {
		t.Parallel()
		expander := NewResourceExpander()
		expander.SetForEach(nk("aws_instance.each"), map[string]cty.Value{
			"a": cty.StringVal("value_a"),
			"b": cty.StringVal("value_b"),
		})
		eachNode := &Node{Key: nk("aws_instance.each"), Type: NodeTypeResource}
		result := expander.Expand(eachNode)
		if result.IsSingle {
			t.Error("Should not be single instance")
		}
		if len(result.Instances) != 2 {
			t.Errorf("Expected 2 instances, got %d", len(result.Instances))
		}

		// Verify keys are set
		for _, inst := range result.Instances {
			if inst.EachKey == nil {
				t.Error("EachKey should be set")
			}
			if inst.EachValue == nil {
				t.Error("EachValue should be set")
			}
		}
	})
}

func TestModuleAddress(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "", ModuleAddress(modulepath.Root()))

	p := modulepath.Root().Append(modulepath.NewStep("a"))
	assert.Equal(t, "module.a", ModuleAddress(p))

	keyed := modulepath.Root().
		Append(modulepath.NewIndexedStep("a", 2)).
		Append(modulepath.NewKeyedStep("b", "k"))
	assert.Equal(t, `module.a[2].module.b["k"]`, ModuleAddress(keyed))

	assert.Equal(t, "module.a.aws_instance.web",
		NodeKey{Module: p, ID: "aws_instance.web"}.String())
	assert.Equal(t, "aws_instance.web",
		NodeKey{ID: "aws_instance.web"}.String())
}

func TestCycleErrorNamesNodes(t *testing.T) {
	t.Parallel()
	g := NewGraph()
	require.Empty(t, g.AddNode(&Node{Key: nk("local.a")}, nil))
	aIdx, ok := g.KeyNode(nk("local.a"))
	require.True(t, ok)
	require.Empty(t, g.AddNode(&Node{Key: nk("local.b")}, []pdag.Node{aIdx}))
	bIdx, ok := g.KeyNode(nk("local.b"))
	require.True(t, ok)

	assert.EqualError(t, g.Order(bIdx, aIdx),
		"dependency cycle: local.b -> local.a -> local.b")
}

func TestCycleErrorLabelsExpansionNodes(t *testing.T) {
	t.Parallel()
	g := NewGraph()
	b := g.NewBlockExpansion(nk("pfx_res.a"), true, func(context.Context) error { return nil })

	assert.EqualError(t, b.DependOn(b.Complete()),
		"dependency cycle: pfx_res.a (completion) -> pfx_res.a (expand) -> pfx_res.a (completion)")
}

// buildWithModules parses root and the given module sources and builds the
// graph, failing the test on a parse error.
func buildWithModules(t *testing.T, root string, modules map[string]string) (*Graph, hcl.Diagnostics) {
	t.Helper()
	p := parser.NewParser()
	config, diags := p.ParseSource("main.tf", []byte(root))
	require.False(t, diags.HasErrors(), diags.Error())
	loader := fakeModuleLoader{modules: map[string]*LoadedModule{}}
	for source, src := range modules {
		mod, diags := p.ParseSource(source+"/main.tf", []byte(src))
		require.False(t, diags.HasErrors(), diags.Error())
		loader.modules[source] = &LoadedModule{Config: mod, SourcePath: "/proj/" + source}
	}
	return BuildFromConfig(config, loader, "/proj")
}

// TestDependsOnUndeclaredModuleOutput: depends_on entries in a module call, a
// resource and a data source that name an undeclared output of a declared
// module call add no dependency, and each entry warns
// (https://github.com/pulumi/pulumi-hcl/issues/685).
func TestDependsOnUndeclaredModuleOutput(t *testing.T) {
	t.Parallel()
	g, diags := buildWithModules(t, `module "m" {
  source = "./m"
}
module "n" {
  source     = "./n"
  depends_on = [module.m.missing]
}
resource "simple_resource" "r" {
  depends_on = [module.m.missing]
}
data "simple_data" "d" {
  depends_on = [module.m.missing]
}
`, map[string]string{
		"./m": `resource "simple_resource" "slow" {}`,
		"./n": ``,
	})
	warning := func(line, byteOffset int) *hcl.Diagnostic {
		return &hcl.Diagnostic{
			Severity: hcl.DiagWarning,
			Summary:  "Reference to undeclared output value",
			Detail:   `module.m declares no output named "missing", so this depends_on entry adds no dependency.`,
			Subject: &hcl.Range{
				Filename: "main.tf",
				Start:    hcl.Pos{Line: line, Column: 17, Byte: byteOffset},
				End:      hcl.Pos{Line: line, Column: 33, Byte: byteOffset + 16},
			},
		}
	}
	assert.Equal(t, hcl.Diagnostics{warning(6, 82), warning(9, 151), warning(12, 212)}, diags)
	assert.Empty(t, g.Validate())

	_, exists := g.seen[NodeKey{Module: modulepath.Root().Append(modulepath.NewStep("m")), ID: "output.missing"}]
	assert.False(t, exists, "a dropped depends_on entry must not create a node")
}

// TestDependsOnUndeclaredModuleOutputAlsoReferenced: an undeclared module
// output stays an error when an expression also references it.
func TestDependsOnUndeclaredModuleOutputAlsoReferenced(t *testing.T) {
	t.Parallel()
	g, diags := buildWithModules(t, `module "m" {
  source = "./m"
}
resource "simple_resource" "r" {
  depends_on = [module.m.missing]
}
output "o" {
  value = module.m.missing
}
`, map[string]string{"./m": ``})
	require.False(t, diags.HasErrors(), diags.Error())

	errs := g.Validate()
	require.Len(t, errs, 1)
	var diag *hcl.Diagnostic
	require.ErrorAs(t, errs[0], &diag)
	assert.Equal(t, "Unsupported attribute", diag.Summary)
	assert.Equal(t, `This object does not have an attribute named "missing".`, diag.Detail)
}

// TestUndeclaredReference: a reference to an undeclared object names the kind
// of the object, the same way from an expression and from a depends_on entry.
func TestUndeclaredReference(t *testing.T) {
	t.Parallel()
	type message struct{ Summary, Detail string }
	for _, tc := range []struct {
		name    string
		root    string
		modules map[string]string
		want    message
	}{
		{
			name: "variable from an expression",
			root: `resource "simple_resource" "r" {
  input = var.nope
}`,
			want: message{
				"Reference to undeclared input variable",
				`An input variable with the name "nope" has not been declared. This variable can be declared with a variable "nope" {} block.`,
			},
		},
		{
			name: "variable from depends_on with a suggestion",
			root: `variable "nopee" {}
resource "simple_resource" "r" {
  depends_on = [var.nope]
}`,
			want: message{
				"Reference to undeclared input variable",
				`An input variable with the name "nope" has not been declared. Did you mean "nopee"?`,
			},
		},
		{
			name: "local from depends_on",
			root: `resource "simple_resource" "r" {
  depends_on = [local.nope]
}`,
			want: message{"Reference to undeclared local value", `A local value with the name "nope" has not been declared.`},
		},
		{
			name: "local from an expression with a suggestion",
			root: `locals {
  nopee = 1
}
resource "simple_resource" "r" {
  input = local.nope
}`,
			want: message{
				"Reference to undeclared local value",
				`A local value with the name "nope" has not been declared. Did you mean "nopee"?`,
			},
		},
		{
			name: "managed resource from an expression",
			root: `resource "simple_resource" "r" {
  input = simple_resource.nope.id
}`,
			want: message{
				"Reference to undeclared resource",
				`There is no managed resource "simple_resource" "nope" definition in the root module.`,
			},
		},
		{
			name: "managed resource from depends_on that names a data resource",
			root: `data "simple_data" "d" {}
resource "simple_resource" "r" {
  depends_on = [simple_data.d]
}`,
			want: message{
				"Reference to undeclared resource",
				"There is no managed resource \"simple_data\" \"d\" definition in the root module.\n\nDid you mean the data resource data.simple_data.d?",
			},
		},
		{
			name: "data resource from an expression that names a managed resource",
			root: `resource "simple_resource" "x" {}
resource "simple_resource" "r" {
  input = data.simple_resource.x.id
}`,
			want: message{
				"Reference to undeclared resource",
				"There is no data resource \"simple_resource\" \"x\" definition in the root module.\n\nDid you mean the resource resource simple_resource.x?",
			},
		},
		{
			name: "module call from depends_on",
			root: `resource "simple_resource" "r" {
  depends_on = [module.nope]
}`,
			want: message{"Reference to undeclared module", `No module call named "nope" is declared in the root module.`},
		},
		{
			name: "managed resource in a child module",
			root: `module "a" {
  source = "./a"
}`,
			modules: map[string]string{"./a": `resource "simple_resource" "r" {
  input = simple_resource.nope.id
}`},
			want: message{
				"Reference to undeclared resource",
				`There is no managed resource "simple_resource" "nope" definition in module.a.`,
			},
		},
		{
			name: "module call output in a child module with a suggestion",
			root: `module "a" {
  source = "./a"
}`,
			modules: map[string]string{
				"./a": `module "inner" {
  source = "./inner"
}
resource "simple_resource" "r" {
  input = module.innr.x
}`,
				"./inner": ``,
			},
			want: message{
				"Reference to undeclared module",
				`No module call named "innr" is declared in module.a. Did you mean "inner"?`,
			},
		},
		{
			// The key of a provider configuration has the same shape as the
			// key of a managed resource.
			name: "provider configuration",
			root: `resource "simple_resource" "r" {
  provider = simple.nope
}`,
			want: message{Summary: `unknown node "simple.nope"`},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			g, diags := buildWithModules(t, tc.root, tc.modules)
			require.False(t, diags.HasErrors(), diags.Error())

			errs := g.Validate()
			require.Len(t, errs, 1)
			var diag *hcl.Diagnostic
			require.ErrorAs(t, errs[0], &diag)
			assert.Equal(t, tc.want, message{diag.Summary, diag.Detail})
		})
	}
}
