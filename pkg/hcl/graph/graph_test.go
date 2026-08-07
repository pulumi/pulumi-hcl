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
	require.NoError(t, err)

	nodes := g.seen
	require.Len(t, nodes, 7, "4 explicit nodes + 3 builtins")

	// Verify dependencies
	localNode := g.seen[nk("local.greeting")].n
	if localNode == nil {
		t.Fatal("Expected local.greeting node")
	}

	// Verify topological sort works
	var sorted []string
	err = g.dag.Walk(t.Context(), func(_ context.Context, n dagNode) error {
		sorted = append(sorted, n.desc.String())
		return nil
	}, pdag.MaxProcs(1))
	require.NoError(t, err)

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
	require.NoError(t, err)

	assert.Equal(t, NodeTypeVariableValidation, g.seen[nk("var.gate")].n.Type)
	assert.Equal(t, NodeTypeVariable, g.seen[nk("var.gate!value")].n.Type)

	barrierKey := "barrier"
	require.NoError(t, g.InjectAfter(func(context.Context) error { return nil }, func(n *Node) bool {
		return n.Type == NodeTypeVariable
	}))

	var sorted []string
	err = g.dag.Walk(t.Context(), func(_ context.Context, n dagNode) error {
		key := n.desc.String()
		if n.exec != nil {
			key = barrierKey
		}
		sorted = append(sorted, key)
		return nil
	}, pdag.MaxProcs(1))
	require.NoError(t, err)

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
	require.NoError(t, err)

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
	require.NoError(t, err)

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
	require.NoError(t, err)

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
	err := g.AddNode(&Node{Key: nk("A"), Type: NodeTypeLocal}, []pdag.Node{i})
	require.NoError(t, err)

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
	require.NoError(t, err)

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
	require.NoError(t, g.AddNode(&Node{Key: nk("local.a")}, nil))
	aIdx, ok := g.KeyNode(nk("local.a"))
	require.True(t, ok)
	require.NoError(t, g.AddNode(&Node{Key: nk("local.b")}, []pdag.Node{aIdx}))
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
