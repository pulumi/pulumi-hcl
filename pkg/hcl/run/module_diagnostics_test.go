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
	"os"
	"path/filepath"
	"testing"

	"github.com/pulumi/pulumi-hcl/pkg/hcl/graph"
	"github.com/pulumi/pulumi-hcl/pkg/hcl/modules"
	"github.com/pulumi/pulumi-hcl/pkg/hcl/parser"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildPreservesModuleParserDiagnostics(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	filename := filepath.Join(dir, "main.tf")
	require.NoError(t, os.WriteFile(filename, []byte(`output "unfinished" {`), 0o600))
	root, diags := parser.NewParser().ParseSource("root.hcl", []byte(`module "child" {
 source = "./child"
}`))
	require.False(t, diags.HasErrors(), "%s", diags)
	loader := modules.NewLoader(func(_, _, _ string) (string, string, error) {
		return dir, "", nil
	})
	_, expected := parser.NewParser().ParseDirectory(dir)
	require.True(t, expected.HasErrors())
	g, actual := graph.BuildFromConfig(root, &moduleLoaderAdapter{loader}, ".")
	require.Nil(t, g)
	assert.Equal(t, expected, actual)
	require.NotNil(t, actual[0].Subject)
	assert.Equal(t, filename, actual[0].Subject.Filename)
}

func TestBuildPreservesCachedModuleWarnings(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "main.tf"), []byte(`terraform {
  backend "local" {}
}`), 0o600))
	root, diags := parser.NewParser().ParseSource("root.hcl", []byte(`module "a" {
  source = "./child"
}
module "b" {
  source = "./child"
}`))
	require.False(t, diags.HasErrors(), "%s", diags)
	loader := modules.NewLoader(func(_, _, _ string) (string, string, error) {
		return dir, "", nil
	})
	_, expected := parser.NewParser().ParseDirectory(dir)
	require.Len(t, expected, 1)
	require.False(t, expected.HasErrors())
	for range 2 {
		g, actual := graph.BuildFromConfig(root, &moduleLoaderAdapter{loader}, ".")
		require.NotNil(t, g)
		assert.Equal(t, expected, actual)
	}
}

func TestBuildLocatesModuleResolutionFailure(t *testing.T) {
	t.Parallel()
	root, diags := parser.NewParser().ParseSource("root.hcl", []byte(`module "child" {
 source = "./child"
}`))
	require.False(t, diags.HasErrors(), "%s", diags)
	loader := modules.NewLoader(func(_, _, _ string) (string, string, error) {
		return "", "", fmt.Errorf("module directory is unavailable")
	})
	g, actual := graph.BuildFromConfig(root, &moduleLoaderAdapter{loader}, ".")
	require.Nil(t, g)
	require.Len(t, actual, 1)
	assert.Contains(t, actual[0].Detail, "module directory is unavailable")
	assert.Equal(t, root.Modules["child"].DeclRange.Ptr(), actual[0].Subject)
}
