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

package server

import (
	"maps"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pulumi/pulumi-hcl/pkg/hcl/ast"
	"github.com/pulumi/pulumi-hcl/pkg/hcl/parser"
)

// A configuration whose only mention of a provider is a removed block still
// requires that provider: the block's provisioners resolve the removed
// resource's schema at run time.
func TestCollectRequirementsRemovedBlock(t *testing.T) {
	t.Parallel()

	config, diags := parser.NewParser().ParseSource("main.tf", []byte(`
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

	tf, pulumiPkgs, aliases, err := collectRequirements(t.Context(), nil, config, "")
	require.NoError(t, err)
	assert.Equal(t, []string{"registry.opentofu.org/hashicorp/simple"}, slices.Sorted(maps.Keys(tf)))
	assert.Equal(t, map[string]*ast.RequiredProvider{"simple": nil}, aliases)
	assert.Empty(t, pulumiPkgs)
}

// A removed block whose target sits under a gone module call is the config's
// only mention of the provider; the module-prefixed address still yields the
// requirement.
func TestCollectRequirementsRemovedBlockModulePrefixed(t *testing.T) {
	t.Parallel()

	config, diags := parser.NewParser().ParseSource("main.tf", []byte(`
removed {
  from = module.gone.simple_resource.a

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

	tf, pulumiPkgs, aliases, err := collectRequirements(t.Context(), nil, config, "")
	require.NoError(t, err)
	assert.Equal(t, []string{"registry.opentofu.org/hashicorp/simple"}, slices.Sorted(maps.Keys(tf)))
	assert.Equal(t, map[string]*ast.RequiredProvider{"simple": nil}, aliases)
	assert.Empty(t, pulumiPkgs)
}
