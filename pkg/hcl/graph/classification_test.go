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
	"cmp"
	"slices"
	"testing"

	"github.com/pulumi/pulumi-hcl/pkg/hcl/parser"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// blockDepsView is Node.Deps with static deps resolved back to node keys and
// every slice sorted, so tests can compare whole structs.
type blockDepsView struct {
	Static []string
	Whole  []string
	Narrow []InstanceKey
}

func depsView(t *testing.T, g *Graph, key string) blockDepsView {
	t.Helper()
	node, ok := g.seen[nk(key)]
	require.True(t, ok, "no node %q", key)
	require.NotNil(t, node.n.Deps, "node %q has no classified deps", key)
	bd := node.n.Deps
	view := blockDepsView{Narrow: slices.Clone(bd.Narrow)}
	for _, n := range bd.Static {
		view.Static = append(view.Static, g.keyByDagNode[n].String())
	}
	for _, w := range bd.Whole {
		view.Whole = append(view.Whole, w.String())
	}
	slices.Sort(view.Static)
	slices.Sort(view.Whole)
	slices.SortFunc(view.Narrow, func(a, b InstanceKey) int {
		return cmp.Or(cmp.Compare(a.Node.String(), b.Node.String()), cmp.Compare(a.Suffix, b.Suffix))
	})
	return view
}

func buildGraph(t *testing.T, src string) *Graph {
	t.Helper()
	config, diags := parser.NewParser().ParseSource("test.hcl", []byte(src))
	require.False(t, diags.HasErrors(), diags.Error())
	g, err := BuildFromConfig(config, nil, "")
	require.False(t, err.HasErrors())
	return g
}

func TestClassifyBodyRefs(t *testing.T) {
	t.Parallel()
	g := buildGraph(t, `
resource "order_resource" "a" {
  for_each = toset(["x", "y"])
  name     = each.key
}

resource "order_resource" "b" {
  name = order_resource.a["x"].result
}

resource "order_resource" "c" {
  name = "c-${order_resource.a[0].result}"
}

resource "order_resource" "wide" {
  name = join(",", order_resource.a[*].result)
}

variable "k" {
  type = string
}

resource "order_resource" "dyn" {
  name = order_resource.a[var.k].result
}
`)

	assert.Equal(t, blockDepsView{
		Narrow: []InstanceKey{{Node: nk("order_resource.a"), Suffix: `["x"]`}},
	}, depsView(t, g, "order_resource.b"))

	assert.Equal(t, blockDepsView{
		Narrow: []InstanceKey{{Node: nk("order_resource.a"), Suffix: `[0]`}},
	}, depsView(t, g, "order_resource.c"))

	assert.Equal(t, blockDepsView{
		Whole: []string{"order_resource.a"},
	}, depsView(t, g, "order_resource.wide"))

	// A dynamic index splits into two traversals: the whole resource and the
	// index variable.
	assert.Equal(t, blockDepsView{
		Static: []string{"var.k"},
		Whole:  []string{"order_resource.a"},
	}, depsView(t, g, "order_resource.dyn"))
}

func TestClassifyDependsOnAndMetaArgs(t *testing.T) {
	t.Parallel()
	g := buildGraph(t, `
resource "order_resource" "a" {
  for_each = toset(["x", "y"])
  name     = each.key
}

resource "order_resource" "b" {
  depends_on = [order_resource.a["x"]]
  name       = "b"
}

resource "order_resource" "c" {
  depends_on = [order_resource.a]
  name       = "c"
}

resource "order_resource" "d" {
  count = order_resource.a["x"].name == "x" ? 1 : 0
  name  = "d"
}
`)

	assert.Equal(t, blockDepsView{
		Narrow: []InstanceKey{{Node: nk("order_resource.a"), Suffix: `["x"]`}},
	}, depsView(t, g, "order_resource.b"))

	assert.Equal(t, blockDepsView{
		Whole: []string{"order_resource.a"},
	}, depsView(t, g, "order_resource.c"))

	assert.Equal(t, blockDepsView{
		Narrow: []InstanceKey{{Node: nk("order_resource.a"), Suffix: `["x"]`}},
	}, depsView(t, g, "order_resource.d"))
}

func TestClassifyWholeSubsumesNarrow(t *testing.T) {
	t.Parallel()
	g := buildGraph(t, `
resource "order_resource" "a" {
  for_each = toset(["x", "y"])
  name     = each.key
}

resource "order_resource" "b" {
  name = "${order_resource.a["x"].result}-${join(",", [for k, v in order_resource.a : v.result])}"
}
`)

	assert.Equal(t, blockDepsView{
		Whole: []string{"order_resource.a"},
	}, depsView(t, g, "order_resource.b"))
}

func TestClassifyDataRefs(t *testing.T) {
	t.Parallel()
	g := buildGraph(t, `
data "order_data" "d" {
  for_each = toset(["x", "y"])
  name     = each.key
}

resource "order_resource" "b" {
  name = data.order_data.d["x"].result
}

resource "order_resource" "a" {
  for_each = toset(["x"])
  name     = each.key
}

data "order_data" "narrow" {
  name = order_resource.a["x"].result
}
`)

	assert.Equal(t, blockDepsView{
		Narrow: []InstanceKey{{Node: nk("data.order_data.d"), Suffix: `["x"]`}},
	}, depsView(t, g, "order_resource.b"))

	assert.Equal(t, blockDepsView{
		Narrow: []InstanceKey{{Node: nk("order_resource.a"), Suffix: `["x"]`}},
	}, depsView(t, g, "data.order_data.narrow"))
}

func TestClassifyInModule(t *testing.T) {
	t.Parallel()
	childSrc := []byte(`
resource "order_resource" "a" {
  for_each = toset(["x", "y"])
  name     = each.key
}

resource "order_resource" "b" {
  name = order_resource.a["x"].result
}
`)
	childConfig, diags := parser.NewParser().ParseSource("child.hcl", childSrc)
	require.False(t, diags.HasErrors(), diags.Error())

	rootSrc := []byte(`
module "m" {
  source = "./child"
}
`)
	rootConfig, diags := parser.NewParser().ParseSource("root.hcl", rootSrc)
	require.False(t, diags.HasErrors(), diags.Error())

	g, err := BuildFromConfig(rootConfig, fakeModuleLoader{modules: map[string]*LoadedModule{
		"./child": {Config: childConfig, SourcePath: "./child"},
	}}, "")
	require.False(t, err.HasErrors())

	assert.Equal(t, blockDepsView{
		Static: []string{"module.m.__init__"},
		Narrow: []InstanceKey{{Node: nk("module.m.order_resource.a"), Suffix: `["x"]`}},
	}, depsView(t, g, "module.m.order_resource.b"))
}

// A data source inside a module must intern under the "data."-prefixed key
// that references and the module completion node resolve to.
func TestClassifyDataSourceInModule(t *testing.T) {
	t.Parallel()
	childSrc := []byte(`
data "order_data" "d" {
  name = "d"
}

resource "order_resource" "a" {
  name = data.order_data.d.result
}
`)
	childConfig, diags := parser.NewParser().ParseSource("child.hcl", childSrc)
	require.False(t, diags.HasErrors(), diags.Error())

	rootSrc := []byte(`
module "m" {
  source = "./child"
}
`)
	rootConfig, diags := parser.NewParser().ParseSource("root.hcl", rootSrc)
	require.False(t, diags.HasErrors(), diags.Error())

	g, err := BuildFromConfig(rootConfig, fakeModuleLoader{modules: map[string]*LoadedModule{
		"./child": {Config: childConfig, SourcePath: "./child"},
	}}, "")
	require.False(t, err.HasErrors())
	require.Empty(t, g.Validate())

	assert.Equal(t, NodeTypeDataSource, g.seen[nk("module.m.data.order_data.d")].n.Type)
	assert.Equal(t, blockDepsView{
		Static: []string{"module.m.__init__"},
		Whole:  []string{"module.m.data.order_data.d"},
	}, depsView(t, g, "module.m.order_resource.a"))

	order := walkOrder(t, g)
	assert.Less(t, indexOf(order, "module.m.data.order_data.d"), indexOf(order, "module.m.order_resource.a"))
}

// A root local classifies its value's references and carries no block-level
// edge to the resources it reads, so the engine's instance-level wiring alone
// orders its evaluation.
func TestClassifyRootLocal(t *testing.T) {
	t.Parallel()
	g := buildGraph(t, `
resource "order_resource" "a" {
  for_each = toset(["x", "y"])
  name     = each.key
}

locals {
  picked = order_resource.a["x"].result
}

resource "order_resource" "b" {
  name = "b-${local.picked}"
}
`)

	assert.Equal(t, blockDepsView{
		Narrow: []InstanceKey{{Node: nk("order_resource.a"), Suffix: `["x"]`}},
	}, depsView(t, g, "local.picked"))

	// No block-level edge from the resource to the local: the local's only
	// build-time prerequisites are its static deps (none here).
	local, ok := g.seen[nk("local.picked")]
	require.True(t, ok)
	for pred := range g.dag.Predecessors(local.i) {
		assert.NotEqual(t, "order_resource.a", g.keyByDagNode[pred])
	}
}

// create_before_destroy still propagates through a root local even though the
// local has no block-level edge to the resource it reads.
func TestForcedCreateBeforeDestroyThroughClassifiedLocal(t *testing.T) {
	t.Parallel()
	g := buildGraph(t, `
resource "order_resource" "a" {
  for_each = toset(["x", "y"])
  name     = each.key
}

locals {
  picked = order_resource.a["x"].result
}

resource "order_resource" "b" {
  name = "b-${local.picked}"
  lifecycle {
    create_before_destroy = true
  }
}
`)

	assert.Equal(t, map[NodeKey]bool{
		nk("order_resource.a"): true,
		nk("order_resource.b"): true,
	}, g.ForcedCreateBeforeDestroy())
}

// A narrow reference still creates the block-level graph edge, so completion
// ordering, Validate, and cycle detection stay block-granular.
func TestClassifyKeepsBlockEdges(t *testing.T) {
	t.Parallel()
	g := buildGraph(t, `
resource "order_resource" "a" {
  for_each = toset(["x", "y"])
  name     = each.key
}

resource "order_resource" "b" {
  name = order_resource.a["x"].result
}
`)

	order := walkOrder(t, g)
	assert.Less(t, indexOf(order, "order_resource.a"), indexOf(order, "order_resource.b"))
}
