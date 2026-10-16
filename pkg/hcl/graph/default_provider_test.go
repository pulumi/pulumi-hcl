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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pulumi/pulumi-hcl/pkg/hcl/parser"
)

// TestPassThroughImplicitDefaultProvider covers a module call passing the
// implicit default provider — `providers = { simple = simple }` with no
// `provider "simple"` block anywhere, but with the provider declared in the
// root's `required_providers`. The reference names the implicit empty default
// configuration, so the graph must validate without an unresolved `simple`
// node.
func TestPassThroughImplicitDefaultProvider(t *testing.T) {
	t.Parallel()

	childSrc := []byte(`
resource "simple_resource" "r" {}
`)
	child, diags := parser.NewParser().ParseSource("child.hcl", childSrc)
	require.False(t, diags.HasErrors(), diags.Error())

	rootSrc := []byte(`
terraform {
  required_providers {
    simple = {
      source = "hashicorp/simple"
    }
  }
}

module "child" {
  source = "./child"
  providers = {
    simple = simple
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
}

// TestPassThroughUndeclaredProvider covers a module call passing
// `providers = { simple = simple }` where the root neither configures the
// provider nor declares it in `required_providers`: the reference must fail
// validation as a missing provider.
func TestPassThroughUndeclaredProvider(t *testing.T) {
	t.Parallel()

	childSrc := []byte(`
terraform {
  required_providers {
    simple = {
      source = "hashicorp/simple"
    }
  }
}

resource "simple_resource" "r" {}
`)
	child, diags := parser.NewParser().ParseSource("child.hcl", childSrc)
	require.False(t, diags.HasErrors(), diags.Error())

	rootSrc := []byte(`
module "child" {
  source = "./child"
  providers = {
    simple = simple
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

	errs := g.Validate()
	require.Len(t, errs, 1)
	assert.ErrorContains(t, errs[0], `missing provider provider["registry.opentofu.org/hashicorp/simple"]`)
}

// TestPassThroughParentWithoutProvider covers a mid-level module passing
// `providers = { simple = simple }` while having no configuration of its own:
// provider configurations are never inherited into a `providers` argument, so
// the reference must fail validation even though the root has a
// `provider "simple"` block.
func TestPassThroughParentWithoutProvider(t *testing.T) {
	t.Parallel()

	childSrc := []byte(`
resource "simple_resource" "r" {}
`)
	child, diags := parser.NewParser().ParseSource("child.hcl", childSrc)
	require.False(t, diags.HasErrors(), diags.Error())

	midSrc := []byte(`
module "child" {
  source = "./child"
  providers = {
    simple = simple
  }
}
`)
	mid, diags := parser.NewParser().ParseSource("mid.hcl", midSrc)
	require.False(t, diags.HasErrors(), diags.Error())

	rootSrc := []byte(`
provider "simple" {}

module "mid" {
  source = "./mid"
}
`)
	root, diags := parser.NewParser().ParseSource("root.hcl", rootSrc)
	require.False(t, diags.HasErrors(), diags.Error())

	loader := fakeModuleLoader{modules: map[string]*LoadedModule{
		"./mid":   {Config: mid, SourcePath: "./mid"},
		"./child": {Config: child, SourcePath: "./child"},
	}}

	g, err := BuildFromConfig(root, loader, ".")
	require.False(t, err.HasErrors())

	errs := g.Validate()
	require.Len(t, errs, 1)
	assert.ErrorContains(t, errs[0], `missing provider module.mid.provider["registry.opentofu.org/hashicorp/simple"]`)
}

// TestPassThroughChainedProviders covers a provider passed down two module
// levels, each hop naming the configuration it received from its own module
// call: the chain must validate.
func TestPassThroughChainedProviders(t *testing.T) {
	t.Parallel()

	childSrc := []byte(`
resource "simple_resource" "r" {}
`)
	child, diags := parser.NewParser().ParseSource("child.hcl", childSrc)
	require.False(t, diags.HasErrors(), diags.Error())

	midSrc := []byte(`
module "child" {
  source = "./child"
  providers = {
    simple = simple
  }
}
`)
	mid, diags := parser.NewParser().ParseSource("mid.hcl", midSrc)
	require.False(t, diags.HasErrors(), diags.Error())

	rootSrc := []byte(`
provider "simple" {}

module "mid" {
  source = "./mid"
  providers = {
    simple = simple
  }
}
`)
	root, diags := parser.NewParser().ParseSource("root.hcl", rootSrc)
	require.False(t, diags.HasErrors(), diags.Error())

	loader := fakeModuleLoader{modules: map[string]*LoadedModule{
		"./mid":   {Config: mid, SourcePath: "./mid"},
		"./child": {Config: child, SourcePath: "./child"},
	}}

	g, err := BuildFromConfig(root, loader, ".")
	require.False(t, err.HasErrors())

	assert.Empty(t, g.Validate())
	assert.True(t, g.HasDependents(nk("simple")))
}

// TestPassThroughAliasedProviderMissingBlock covers a module call passing an
// aliased provider that no block defines: aliased configurations are never
// implicit, so the reference must fail validation as a missing provider.
func TestPassThroughAliasedProviderMissingBlock(t *testing.T) {
	t.Parallel()

	childSrc := []byte(`
resource "simple_resource" "r" {}
`)
	child, diags := parser.NewParser().ParseSource("child.hcl", childSrc)
	require.False(t, diags.HasErrors(), diags.Error())

	rootSrc := []byte(`
terraform {
  required_providers {
    simple = {
      source = "hashicorp/simple"
    }
  }
}

module "child" {
  source = "./child"
  providers = {
    simple = simple.missing
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

	errs := g.Validate()
	require.Len(t, errs, 1)
	assert.ErrorContains(t, errs[0], `missing provider provider["registry.opentofu.org/hashicorp/simple"].missing`)
}

// TestBareProviderReferenceImplicitDefault covers a root resource with
// `provider = simple` and no `provider "simple"` block anywhere: the
// reference names the implicit empty default configuration, so the graph
// must validate without an unresolved `simple` node.
func TestBareProviderReferenceImplicitDefault(t *testing.T) {
	t.Parallel()

	rootSrc := []byte(`
resource "simple_resource" "r" {
  provider = simple
}
`)
	root, diags := parser.NewParser().ParseSource("root.hcl", rootSrc)
	require.False(t, diags.HasErrors(), diags.Error())

	g, err := BuildFromConfig(root, nil, ".")
	require.False(t, err.HasErrors())

	assert.Empty(t, g.Validate())
}

// TestBareProviderReferenceInherited covers an in-module resource with
// `provider = simple` where the default configuration is inherited from the
// root's `provider "simple"` block: the resource must be ordered after the
// root block.
func TestBareProviderReferenceInherited(t *testing.T) {
	t.Parallel()

	childSrc := []byte(`
resource "simple_resource" "r" {
  provider = simple
}
`)
	child, diags := parser.NewParser().ParseSource("child.hcl", childSrc)
	require.False(t, diags.HasErrors(), diags.Error())

	rootSrc := []byte(`
provider "simple" {}

module "child" {
  source = "./child"
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
	assert.Less(t, indexOf(order, "simple"), indexOf(order, "module.child.simple_resource.r"))
}

// TestBareProviderReferenceInheritedRenamedLocalName covers the same
// inheritance where the child names `hashicorp/simple` `myp`: inheritance is
// by fully-qualified address, so the resource must still be ordered after the
// root's `provider "simple"` block.
func TestBareProviderReferenceInheritedRenamedLocalName(t *testing.T) {
	t.Parallel()

	childSrc := []byte(`
terraform {
  required_providers {
    myp = {
      source = "hashicorp/simple"
    }
  }
}

resource "simple_resource" "r" {
  provider = myp
}
`)
	child, diags := parser.NewParser().ParseSource("child.hcl", childSrc)
	require.False(t, diags.HasErrors(), diags.Error())

	rootSrc := []byte(`
terraform {
  required_providers {
    simple = {
      source = "hashicorp/simple"
    }
  }
}

provider "simple" {}

module "child" {
  source = "./child"
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
	assert.Less(t, indexOf(order, "simple"), indexOf(order, "module.child.simple_resource.r"))
}
