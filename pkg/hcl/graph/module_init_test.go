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
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pulumi/pulumi-hcl/pkg/hcl/ast"
	"github.com/pulumi/pulumi-hcl/pkg/hcl/modulepath"
	"github.com/pulumi/pulumi-hcl/pkg/hcl/parser"
)

func parse(t *testing.T, name, src string) *ast.Config {
	t.Helper()
	config, diags := parser.NewParser().ParseSource(name, []byte(src))
	require.False(t, diags.HasErrors(), diags.Error())
	return config
}

// TestModuleInitAwaitsItsArguments pins the ordering the component
// registration needs: init evaluates the call's arguments to report them as
// inputs, so it must follow whatever they read. Without the edge init races
// the value and the argument is dropped with no error.
func TestModuleInitAwaitsItsArguments(t *testing.T) {
	t.Parallel()

	child := parse(t, "child.hcl", `variable "v" { type = string }`)
	root := parse(t, "root.hcl", `
locals {
  computed = "hello"
}

module "child" {
  source = "./child"
  v      = local.computed
}
`)

	g, err := BuildFromConfig(root, fakeModuleLoader{modules: map[string]*LoadedModule{
		"./child": {Config: child, SourcePath: "./child"},
	}}, ".")
	require.False(t, err.HasErrors())
	require.Empty(t, g.Validate())

	initIdx, ok := g.KeyNode(NodeKey{
		Module: modulepath.Root().Append(modulepath.NewStep("child")),
		ID:     "__init__",
	})
	require.True(t, ok, "no init node for module.child")
	localIdx, ok := g.KeyNode(NodeKey{Module: modulepath.Root(), ID: "local.computed"})
	require.True(t, ok, "no node for local.computed")

	assert.Contains(t, slices.Collect(g.dag.Predecessors(initIdx)), localIdx,
		"module init does not depend on the local its argument reads")
}

// TestMutuallyDependentModulesBuild covers the case the ordering above cannot
// have: two calls each passing the other's output. The dependency runs from
// one module's outputs to the other's variables, not between the calls, so the
// graph must still build — one of the two init edges is refused, and the same
// one every time, or the component that keeps its argument would vary per run.
func TestMutuallyDependentModulesBuild(t *testing.T) {
	t.Parallel()

	side := func(name string) *ast.Config {
		return parse(t, name+".hcl", `
variable "input" {
  type = bool
}
# out comes from the resource that does not read var.input, so the two calls
# depend on each other without either module depending on its own argument.
resource "simple_resource" "independent" {
  value = true
}
resource "simple_resource" "dependent" {
  value = ! var.input
}
output "out" {
  value = simple_resource.independent.value
}
`)
	}
	root := parse(t, "root.hcl", `
module "first" {
  source = "./first"
  input  = module.second.out
}
module "second" {
  source = "./second"
  input  = module.first.out
}
`)
	loader := fakeModuleLoader{modules: map[string]*LoadedModule{
		"./first":  {Config: side("first"), SourcePath: "./first"},
		"./second": {Config: side("second"), SourcePath: "./second"},
	}}

	var first []string
	for range 8 {
		g, err := BuildFromConfig(root, loader, ".")
		require.False(t, err.HasErrors(), "mutually dependent modules must not cycle")
		require.Empty(t, g.Validate())

		var gated []string
		for _, name := range []string{"first", "second"} {
			init, ok := g.KeyNode(NodeKey{
				Module: modulepath.Root().Append(modulepath.NewStep(name)),
				ID:     "__init__",
			})
			require.True(t, ok)
			if len(slices.Collect(g.dag.Predecessors(init))) > 0 {
				gated = append(gated, name)
			}
		}
		if first == nil {
			first = gated
			continue
		}
		assert.Equal(t, first, gated, "which call keeps its argument varies between builds")
	}
}
