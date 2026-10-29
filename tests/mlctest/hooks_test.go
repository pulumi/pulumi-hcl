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

package mlctest_test

import (
	"testing"

	"github.com/pulumi/pulumi-hcl/tests/testutil/mlctest"
	"github.com/pulumi/pulumi/sdk/v3/go/common/apitype"
	"github.com/stretchr/testify/assert"
)

// Two instances of one component each register the precondition,
// postcondition, and provisioner hooks of their `terraform_data`. The engine
// names hooks per deployment, so each instance must register distinct names.
func TestHooks(t *testing.T) {
	t.Parallel()
	mlctest.RunCase(t, "hooks", mlctest.Case{
		AssertState: func(t *testing.T, resources []apitype.ResourceV3) {
			stashes := map[string]any{}
			for _, r := range resources {
				if r.Type == "pulumi:index:Stash" {
					stashes[r.URN.Name()] = r.Outputs["output"]
				}
			}
			assert.Equal(t, map[string]any{
				"a-t": map[string]any{"type": `"string"`, "value": "a"},
				"b-t": map[string]any{"type": `"string"`, "value": "b"},
			}, stashes)
		},
	})
}
