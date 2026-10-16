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

	"github.com/pulumi/pulumi-hcl/pkg/hcl/ast"
	"github.com/pulumi/pulumi-hcl/pkg/hcl/modulepath"
	"github.com/pulumi/pulumi-hcl/pkg/hcl/parser"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A removed block targeting a resource inside a child module errors during
// inlining when the child still declares that resource, and builds cleanly
// once the resource is gone.
func TestRemovedResourceInModule(t *testing.T) {
	t.Parallel()

	rootSrc := []byte(`
module "m" {
  source = "./child"
}

removed {
  from = module.m.simple_resource.a
  lifecycle {
    destroy = true
  }
}
`)
	rootConfig, diags := parser.NewParser().ParseSource("root.hcl", rootSrc)
	require.False(t, diags.HasErrors(), diags.Error())

	stillExists, diags := parser.NewParser().ParseSource("child.hcl", []byte(`
resource "simple_resource" "a" {}
`))
	require.False(t, diags.HasErrors(), diags.Error())

	_, err := BuildFromConfig(rootConfig, fakeModuleLoader{modules: map[string]*LoadedModule{
		"./child": {Config: stillExists, SourcePath: "./child"},
	}}, "")
	require.Len(t, err, 1)
	assert.NotNil(t, err[0].Subject)
	assert.Equal(t, err[0].Summary,
		"removed block for module.m.simple_resource.a: this resource block still exists in the configuration")

	gone, diags := parser.NewParser().ParseSource("child.hcl", []byte(`
resource "simple_resource" "b" {}
`))
	require.False(t, diags.HasErrors(), diags.Error())

	_, err = BuildFromConfig(rootConfig, fakeModuleLoader{modules: map[string]*LoadedModule{
		"./child": {Config: gone, SourcePath: "./child"},
	}}, "")
	assert.False(t, err.HasErrors())
}

// A child module's removed block inlines with its relative address rewritten
// to be root-relative, provisioners intact.
func TestRemovedDeclaredInChildModule(t *testing.T) {
	t.Parallel()

	rootConfig, diags := parser.NewParser().ParseSource("root.hcl", []byte(`
module "m" {
  source = "./child"
}
`))
	require.False(t, diags.HasErrors(), diags.Error())

	childConfig, diags := parser.NewParser().ParseSource("child.hcl", []byte(`
removed {
  from = simple_resource.a
  lifecycle {
    destroy = true
  }
  provisioner "local-exec" {
    when    = destroy
    command = "true"
  }
}
`))
	require.False(t, diags.HasErrors(), diags.Error())

	g, err := BuildFromConfig(rootConfig, fakeModuleLoader{modules: map[string]*LoadedModule{
		"./child": {Config: childConfig, SourcePath: "./child"},
	}}, "")
	require.False(t, err.HasErrors())

	require.Len(t, g.Removed(), 1)
	assert.Equal(t, ast.TargetAddr{
		Module: modulepath.Root().Append(modulepath.NewStep("m")),
		Type:   "simple_resource",
		Name:   "a",
	}, g.Removed()[0].From)
	assert.True(t, g.Removed()[0].Destroy)
	require.Len(t, g.Removed()[0].Provisioners, 1)
}

// A removed block whose root-level target is still declared errors at graph
// build.
func TestRemovedRootTargetStillExists(t *testing.T) {
	t.Parallel()

	resourceConfig, diags := parser.NewParser().ParseSource("root.hcl", []byte(`
resource "simple_resource" "a" {}

removed {
  from = simple_resource.a
  lifecycle {
    destroy = true
  }
}
`))
	require.False(t, diags.HasErrors(), diags.Error())

	_, err := BuildFromConfig(resourceConfig, fakeModuleLoader{}, "")
	require.Len(t, err, 1)
	assert.NotNil(t, err[0].Subject)
	assert.Equal(t, err[0].Summary,
		"removed block for simple_resource.a: this resource block still exists in the configuration")

	moduleConfig, diags := parser.NewParser().ParseSource("root.hcl", []byte(`
module "child" {
  source = "./child"
}

removed {
  from = module.child
  lifecycle {
    destroy = true
  }
}
`))
	require.False(t, diags.HasErrors(), diags.Error())

	childConfig, diags := parser.NewParser().ParseSource("child.hcl", []byte(``))
	require.False(t, diags.HasErrors(), diags.Error())

	_, err = BuildFromConfig(moduleConfig, fakeModuleLoader{modules: map[string]*LoadedModule{
		"./child": {Config: childConfig, SourcePath: "./child"},
	}}, "")
	require.Len(t, err, 1)
	assert.NotNil(t, err[0].Subject)
	assert.Equal(t, err[0].Summary,
		"removed block for module.child: this module block still exists in the configuration")
}

// Two provisioner-carrying removed blocks for one address in one
// configuration are rejected at graph build.
func TestRemovedDuplicateProvisionersSameConfig(t *testing.T) {
	t.Parallel()

	config, diags := parser.NewParser().ParseSource("root.hcl", []byte(`
removed {
  from = simple_resource.a
  lifecycle {
    destroy = true
  }
  provisioner "local-exec" {
    when    = destroy
    command = "true"
  }
}

removed {
  from = simple_resource.a
  lifecycle {
    destroy = true
  }
  provisioner "local-exec" {
    when    = destroy
    command = "false"
  }
}
`))
	require.False(t, diags.HasErrors(), diags.Error())

	_, err := BuildFromConfig(config, fakeModuleLoader{}, "")
	require.Len(t, err, 1)
	assert.NotNil(t, err[0].Subject)
	assert.Equal(t, err[0].Summary,
		"duplicate removed block for simple_resource.a: a removed block with provisioners for this address was already declared at root.hcl:2,1-8; declare all of the address's provisioners in one removed block")
}

// A child module's removed block targeting its own nested module errors when
// the grandchild config still declares the target.
func TestRemovedDeclaredInChildModuleNestedStillExists(t *testing.T) {
	t.Parallel()

	rootConfig, diags := parser.NewParser().ParseSource("root.hcl", []byte(`
module "m" {
  source = "./child"
}
`))
	require.False(t, diags.HasErrors(), diags.Error())

	childConfig, diags := parser.NewParser().ParseSource("child.hcl", []byte(`
module "n" {
  source = "./grand"
}

removed {
  from = module.n.simple_resource.a
  lifecycle {
    destroy = true
  }
}
`))
	require.False(t, diags.HasErrors(), diags.Error())

	grandConfig, diags := parser.NewParser().ParseSource("grand.hcl", []byte(`
resource "simple_resource" "a" {}
`))
	require.False(t, diags.HasErrors(), diags.Error())

	_, err := BuildFromConfig(rootConfig, fakeModuleLoader{modules: map[string]*LoadedModule{
		"./child": {Config: childConfig, SourcePath: "./child"},
		"./grand": {Config: grandConfig, SourcePath: "./grand"},
	}}, "")
	require.Len(t, err, 1)
	assert.NotNil(t, err[0].Subject)
	assert.Equal(t, err[0].Summary,
		"removed block for module.m.module.n.simple_resource.a: this resource block still exists in the configuration")
}

// Provisioner-carrying removed blocks for one address declared in two
// different configurations are rejected: two provisioner sets for one orphan
// would be ambiguous at destroy time.
func TestRemovedDuplicateProvisionersAcrossConfigs(t *testing.T) {
	t.Parallel()

	rootConfig, diags := parser.NewParser().ParseSource("root.hcl", []byte(`
module "m" {
  source = "./child"
}

removed {
  from = module.m.simple_resource.a
  lifecycle {
    destroy = true
  }
  provisioner "local-exec" {
    when    = destroy
    command = "true"
  }
}
`))
	require.False(t, diags.HasErrors(), diags.Error())

	childConfig, diags := parser.NewParser().ParseSource("child.hcl", []byte(`
removed {
  from = simple_resource.a
  lifecycle {
    destroy = true
  }
  provisioner "local-exec" {
    when    = destroy
    command = "false"
  }
}
`))
	require.False(t, diags.HasErrors(), diags.Error())

	_, err := BuildFromConfig(rootConfig, fakeModuleLoader{modules: map[string]*LoadedModule{
		"./child": {Config: childConfig, SourcePath: "./child"},
	}}, "")
	require.Len(t, err, 1)
	assert.NotNil(t, err[0].Subject)
	assert.Equal(t, err[0].Summary,
		"duplicate removed block for module.m.simple_resource.a: a removed block with provisioners for this address was already declared at root.hcl:6,1-8; declare all of the address's provisioners in one removed block")
}

// A removed block targeting a nested module errors when that module is still
// declared; the depth-one case is already rejected at parse time.
func TestRemovedNestedModuleStillExists(t *testing.T) {
	t.Parallel()

	rootSrc := []byte(`
module "m" {
  source = "./child"
}

removed {
  from = module.m.module.n
  lifecycle {
    destroy = true
  }
}
`)
	rootConfig, diags := parser.NewParser().ParseSource("root.hcl", rootSrc)
	require.False(t, diags.HasErrors(), diags.Error())

	childConfig, diags := parser.NewParser().ParseSource("child.hcl", []byte(`
module "n" {
  source = "./grand"
}
`))
	require.False(t, diags.HasErrors(), diags.Error())

	grandConfig, diags := parser.NewParser().ParseSource("grand.hcl", []byte(`
resource "simple_resource" "a" {}
`))
	require.False(t, diags.HasErrors(), diags.Error())

	_, err := BuildFromConfig(rootConfig, fakeModuleLoader{modules: map[string]*LoadedModule{
		"./child": {Config: childConfig, SourcePath: "./child"},
		"./grand": {Config: grandConfig, SourcePath: "./grand"},
	}}, "")
	require.Len(t, err, 1)
	assert.NotNil(t, err[0].Subject)
	assert.Equal(t, err[0].Summary,
		"removed block for module.m.module.n: this module block still exists in the configuration")
}
