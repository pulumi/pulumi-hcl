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
	"strings"
	"testing"

	"github.com/pulumi/pulumi/pkg/v3/util/pdag"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pulumi/pulumi-hcl/pkg/hcl/modulepath"
	"github.com/pulumi/pulumi-hcl/pkg/hcl/parser"
)

// nk parses a rendered node key ("module.m.output.o") back into its typed
// form, so tests can keep asserting against reference-syntax strings.
func nk(s string) NodeKey {
	p := modulepath.Root()
	for {
		rest, ok := strings.CutPrefix(s, "module.")
		if !ok {
			break
		}
		name, id, ok := strings.Cut(rest, ".")
		if !ok {
			break
		}
		p = p.Append(modulepath.NewStep(name))
		s = id
	}
	return NodeKey{Module: p, ID: s}
}

// walkOrder returns the DAG node keys in the single-worker Walk visitation
// order, so a test can assert that one node is scheduled before another.
func walkOrder(t *testing.T, g *Graph) []string {
	t.Helper()
	var order []string
	err := g.dag.Walk(t.Context(), func(_ context.Context, n dagNode) error {
		order = append(order, n.desc.String())
		return nil
	}, pdag.MaxProcs(1))
	require.NoError(t, err)
	return order
}

func indexOf(order []string, key string) int {
	for i, k := range order {
		if k == key {
			return i
		}
	}
	return -1
}

func TestProvisionerReferenceOrdersResourceAfterReferent(t *testing.T) {
	t.Parallel()
	src := []byte(`
resource "simple_resource" "upstream" {
  input_one = "from-upstream"
}

resource "simple_resource" "dependent" {
  input_one = "dependent"

  provisioner "local-exec" {
    command = "echo ${simple_resource.upstream.result}"
  }
}
`)
	config, diags := parser.NewParser().ParseSource("test.hcl", src)
	require.False(t, diags.HasErrors(), diags.Error())

	g, err := BuildFromConfig(config, nil, "")
	require.NoError(t, err)

	assert.True(t, g.HasDependents(nk("simple_resource.upstream")))

	order := walkOrder(t, g)
	assert.Less(t, indexOf(order, "simple_resource.upstream"),
		indexOf(order, "simple_resource.dependent"))
}

func TestConnectionReferenceOrdersResourceAfterReferent(t *testing.T) {
	t.Parallel()
	src := []byte(`
resource "simple_resource" "upstream" {
  input_one = "10.0.0.1"
}

resource "simple_resource" "dependent" {
  input_one = "dependent"

  connection {
    type = "ssh"
    host = simple_resource.upstream.result
  }

  provisioner "remote-exec" {
    inline = ["echo hello"]
  }
}
`)
	config, diags := parser.NewParser().ParseSource("test.hcl", src)
	require.False(t, diags.HasErrors(), diags.Error())

	g, err := BuildFromConfig(config, nil, "")
	require.NoError(t, err)

	assert.True(t, g.HasDependents(nk("simple_resource.upstream")))
}
