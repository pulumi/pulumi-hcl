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

	"github.com/hashicorp/hcl/v2"
	"github.com/pulumi/pulumi-hcl/pkg/hcl/ast"
	"github.com/pulumi/pulumi-hcl/pkg/hcl/parser"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildFromConfigDiagnostics(t *testing.T) {
	t.Parallel()
	child, childDiags := parser.NewParser().ParseSource("child.hcl", []byte(`terraform {
  backend "local" {}
}`))
	require.False(t, childDiags.HasErrors())
	require.Len(t, childDiags, 1)
	root, rootDiags := parser.NewParser().ParseSource("root.hcl", []byte(`terraform {
  required_version = ">= 1.0"
}
module "a" {
  source = "./child"
}
module "b" {
  source = "./child"
}`))
	require.False(t, rootDiags.HasErrors())
	require.Len(t, rootDiags, 1)
	g, diags := BuildFromConfig(root, fakeModuleLoader{
		modules: map[string]*LoadedModule{"./child": {Config: child, SourcePath: "./child"}},
		diags:   map[string]hcl.Diagnostics{"./child": childDiags},
	}, ".")
	require.NotNil(t, g)
	// Root parsing belongs to the caller; shared child diagnostics are reported once.
	assert.Equal(t, childDiags, diags)
}

type diagnosticModuleLoader struct{ diags hcl.Diagnostics }

func (l diagnosticModuleLoader) LoadModule(_ *ast.Module, _ string) (*LoadedModule, hcl.Diagnostics) {
	return nil, l.diags
}

func TestModuleLoadDiagnostics(t *testing.T) {
	t.Parallel()
	root := parse(t, "root.hcl", `module "child" {
  source = "./child"
}`)
	subject := hcl.Range{Filename: "child.hcl", Start: hcl.Pos{Line: 3, Column: 2}, End: hcl.Pos{Line: 3, Column: 5}}
	for _, subject := range []*hcl.Range{&subject, nil} {
		original := &hcl.Diagnostic{Severity: hcl.DiagError, Summary: "Invalid child configuration", Subject: subject}
		g, diags := BuildFromConfig(root, diagnosticModuleLoader{hcl.Diagnostics{original}}, ".")
		require.Nil(t, g)
		require.Len(t, diags, 1)
		assert.Same(t, original, diags[0])
		assert.Equal(t, subject, diags[0].Subject)
	}
	require.Panics(t, func() { _, _ = BuildFromConfig(root, nil, ".") })
}

func TestDependencyCycleDiagnostic(t *testing.T) {
	t.Parallel()
	config := parse(t, "cycle.hcl", `provider "test" {
  value = test_resource.example.id
}
resource "test_resource" "example" {}`)
	g, diags := BuildFromConfig(config, nil, ".")
	require.Nil(t, g)
	require.True(t, diags.HasErrors())
	require.NotNil(t, diags[0].Subject)
	assert.Equal(t, "cycle.hcl", diags[0].Subject.Filename)
	assert.Contains(t, diags[0].Summary, "cycle")
}

func TestBuildDiagnosticsPreserveWarningsOnNestedFailure(t *testing.T) {
	t.Parallel()
	root := parse(t, "root.hcl", `module "child" {
  source = "./child"
}`)
	child, childDiags := parser.NewParser().ParseSource("child.hcl", []byte(`terraform {
  backend "local" {}
}
module "self" {
  source = "./child"
}`))
	require.False(t, childDiags.HasErrors())
	require.Len(t, childDiags, 1)
	g, diags := BuildFromConfig(root, fakeModuleLoader{
		modules: map[string]*LoadedModule{"./child": {Config: child, SourcePath: "./child"}},
		diags:   map[string]hcl.Diagnostics{"./child": childDiags},
	}, ".")
	require.Nil(t, g)
	require.Len(t, diags, 2)
	assert.Same(t, childDiags[0], diags[0])
	assert.Equal(t, hcl.DiagError, diags[1].Severity)
	assert.Contains(t, diags[1].Summary, "module cycle")
	assert.Equal(t, child.Modules["self"].DeclRange.Ptr(), diags[1].Subject)
}
