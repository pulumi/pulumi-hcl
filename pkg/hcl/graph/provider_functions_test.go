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
	"fmt"
	"testing"

	"github.com/hashicorp/hcl/v2"
	"github.com/pulumi/pulumi-hcl/pkg/hcl/ast"
	"github.com/pulumi/pulumi-hcl/pkg/hcl/parser"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// walkOrder and indexOf live in provisioner_reference_test.go.

func TestProviderFunctionCallOrdersResourceAfterProvider(t *testing.T) {
	t.Parallel()
	src := []byte(`
provider "simple" {}

resource "simple_thing" "x" {
  arn = provider::simple::parse_arn(var.name)
}

variable "name" {
  type = string
}
`)
	config, diags := parser.NewParser().ParseSource("test.hcl", src)
	require.False(t, diags.HasErrors(), diags.Error())

	g, err := BuildFromConfig(config, nil, "")
	require.False(t, err.HasErrors())

	assert.True(t, g.HasDependents(nk("simple")))
	assert.Empty(t, g.Validate())

	order := walkOrder(t, g)
	assert.Less(t, indexOf(order, "simple"), indexOf(order, "simple_thing.x"))
}

// TestProviderFunctionCallWithoutProviderBlockBuildsCleanly covers a
// provider-defined function call whose provider has no local `provider`
// block: the engine falls back to the package's default provider at
// runtime, so the graph must build without creating a node for it or
// erroring in Validate.
func TestProviderFunctionCallWithoutProviderBlockBuildsCleanly(t *testing.T) {
	t.Parallel()
	src := []byte(`
resource "simple_thing" "x" {
  arn = provider::simple::parse_arn(var.name)
}

variable "name" {
  type = string
}
`)
	config, diags := parser.NewParser().ParseSource("test.hcl", src)
	require.False(t, diags.HasErrors(), diags.Error())

	g, err := BuildFromConfig(config, nil, "")
	require.False(t, err.HasErrors())

	assert.Empty(t, g.Validate())
	assert.False(t, g.HasDependents(nk("simple")))
	_, ok := g.seen[nk("simple")]
	assert.False(t, ok, "no node should be created for an undeclared provider")
}

// TestProviderFunctionSelfReferenceDoesNotCycle covers a provider block whose
// own configuration calls one of its own provider's functions: providerDeps
// filters the resulting self-edge, so this must not be reported as a cycle.
func TestProviderFunctionSelfReferenceDoesNotCycle(t *testing.T) {
	t.Parallel()
	src := []byte(`
provider "simple" {
  token = provider::simple::compute_token()
}

resource "simple_thing" "x" {}
`)
	config, diags := parser.NewParser().ParseSource("test.hcl", src)
	require.False(t, diags.HasErrors(), diags.Error())

	g, err := BuildFromConfig(config, nil, "")
	require.False(t, err.HasErrors())
	assert.Empty(t, g.Validate())
}

func TestLocalValueProviderFunctionOrdersAfterProviderBlock(t *testing.T) {
	t.Parallel()
	src := []byte(`
provider "simple" {}

locals {
  arn = provider::simple::parse_arn(var.name)
}

variable "name" {
  type = string
}
`)
	config, diags := parser.NewParser().ParseSource("test.hcl", src)
	require.False(t, diags.HasErrors(), diags.Error())

	g, err := BuildFromConfig(config, nil, "")
	require.False(t, err.HasErrors())

	assert.True(t, g.HasDependents(nk("simple")))
	assert.Empty(t, g.Validate())

	order := walkOrder(t, g)
	assert.Less(t, indexOf(order, "simple"), indexOf(order, "local.arn"))
}

// fakeModuleLoader resolves module sources to pre-parsed configs from a fixed
// map, so a test can inline a child module without a filesystem fixture.
type fakeModuleLoader struct {
	modules map[string]*LoadedModule
	diags   map[string]hcl.Diagnostics
}

func (f fakeModuleLoader) LoadModule(module *ast.Module, _ string) (*LoadedModule, hcl.Diagnostics) {
	m, ok := f.modules[module.Source]
	if !ok {
		return nil, hcl.Diagnostics{{Severity: hcl.DiagError, Summary: fmt.Sprintf("no module %q", module.Source), Subject: module.DeclRange.Ptr()}}
	}
	return m, f.diags[module.Source]
}

// TestModuleResourceProviderFunctionInheritsRootProvider covers a
// provider-defined function call inside a child module resource, where
// neither the child module nor the module call declares the provider: the
// call must route to the nearest ancestor's `provider` block (the root's),
// ordering the child resource after it.
func TestModuleResourceProviderFunctionInheritsRootProvider(t *testing.T) {
	t.Parallel()

	childSrc := []byte(`
variable "name" {
  type = string
}

resource "simple_thing" "y" {
  arn = provider::simple::parse_arn(var.name)
}
`)
	child, diags := parser.NewParser().ParseSource("child.hcl", childSrc)
	require.False(t, diags.HasErrors(), diags.Error())

	rootSrc := []byte(`
provider "simple" {}

module "child" {
  source = "./child"
  name   = "hi"
}
`)
	root, diags := parser.NewParser().ParseSource("root.hcl", rootSrc)
	require.False(t, diags.HasErrors(), diags.Error())

	loader := fakeModuleLoader{modules: map[string]*LoadedModule{
		"./child": {Config: child, SourcePath: "./child"},
	}}

	g, err := BuildFromConfig(root, loader, ".")
	require.False(t, err.HasErrors())

	assert.Empty(t, g.Validate())
	assert.True(t, g.HasDependents(nk("simple")))

	order := walkOrder(t, g)
	assert.Less(t, indexOf(order, "simple"), indexOf(order, "module.child.simple_thing.y"))
}

// TestModuleResourceProviderFunctionUsesPassThroughProvider covers a
// provider-defined function call inside a child module resource, where the
// module call passes the provider through under an alias
// (`providers = { simple = simple.aliased }`) and the child declares no
// provider block of its own: the call must route to the parent-scope aliased
// provider block, not any un-aliased default.
func TestModuleResourceProviderFunctionUsesPassThroughProvider(t *testing.T) {
	t.Parallel()

	childSrc := []byte(`
variable "name" {
  type = string
}

resource "simple_thing" "y" {
  arn = provider::simple::parse_arn(var.name)
}
`)
	child, diags := parser.NewParser().ParseSource("child.hcl", childSrc)
	require.False(t, diags.HasErrors(), diags.Error())

	rootSrc := []byte(`
provider "simple" {
  alias = "aliased"
}

module "child" {
  source = "./child"
  name   = "hi"
  providers = {
    simple = simple.aliased
  }
}
`)
	root, diags := parser.NewParser().ParseSource("root.hcl", rootSrc)
	require.False(t, diags.HasErrors(), diags.Error())

	loader := fakeModuleLoader{modules: map[string]*LoadedModule{
		"./child": {Config: child, SourcePath: "./child"},
	}}

	g, err := BuildFromConfig(root, loader, ".")
	require.False(t, err.HasErrors())

	assert.Empty(t, g.Validate())
	assert.True(t, g.HasDependents(nk("simple.aliased")))

	order := walkOrder(t, g)
	assert.Less(t, indexOf(order, "simple.aliased"), indexOf(order, "module.child.simple_thing.y"))
}
