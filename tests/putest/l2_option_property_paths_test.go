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

package putest_test

import (
	"testing"

	"github.com/pulumi/pulumi-hcl/tests/testutil/putest"
	"github.com/pulumi/pulumi-hcl/tests/testutil/tfcompat/providers"
	"github.com/pulumi/pulumi-hcl/tests/testutil/tfexec"
	"github.com/stretchr/testify/assert"
)

// TestL2OptionPropertyPaths writes resource option paths that end in a map
// key, one of them inside a MaxItems=1 block. Only a change to a named key
// replaces the resource, so the round trip must keep each whole path.
func TestL2OptionPropertyPaths(t *testing.T) {
	t.Parallel()
	putest.RunCase(t, "l2_option_property_paths", putest.Case{
		Providers: []putest.Provider{
			{Name: "collections", Factory: providers.CollectionsProvider},
		},
		AssertOps: func(t *testing.T, ops []tfexec.Op) {
			thing := func(a, b, x, y, summary string) map[string]any {
				return map[string]any{
					"tags": map[string]any{"a": a, "b": b},
					"metadata": []any{map[string]any{
						"labels":    map[string]any{"x": x, "y": y},
						"selectors": []any{},
					}},
					"ports":   []any{},
					"rule":    []any{},
					"zones":   []any{},
					"summary": summary,
				}
			}
			summary := func(a, b, x, y string) string {
				return "tags=[a=" + a + ",b=" + b + "]|labels=[x=" + x + ",y=" + y + "]"
			}
			created := func(a, b, x, y string) tfexec.Op {
				return tfexec.Op{
					Kind:    tfexec.OpCreate,
					Type:    "collections_thing",
					Inputs:  thing(a, b, x, y, ""),
					Outputs: thing(a, b, x, y, summary(a, b, x, y)),
				}
			}
			assert.Equal(t, []tfexec.Op{
				created("1", "1", "1", "1"),
				created("2", "2", "1", "2"),
				created("2", "2", "2", "2"),
				{
					Kind:    tfexec.OpUpdate,
					Type:    "collections_thing",
					Inputs:  thing("1", "2", "1", "2", summary("1", "1", "1", "1")),
					Outputs: thing("1", "2", "1", "2", summary("1", "1", "1", "1")),
				},
				{
					Kind:   tfexec.OpDelete,
					Type:   "collections_thing",
					Inputs: thing("1", "2", "1", "2", summary("1", "1", "1", "1")),
				},
				{
					Kind:   tfexec.OpDelete,
					Type:   "collections_thing",
					Inputs: thing("2", "2", "1", "2", summary("2", "2", "1", "2")),
				},
			}, ops)
		},
	})
}
