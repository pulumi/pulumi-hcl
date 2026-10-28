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

// A change to `triggers_replace` on a `terraform_data` inside a component
// replaces the resource. The resource lowers to a Stash, whose `output` keeps
// the value it was created with: after a replacement it holds the new value,
// after an in-place update it still holds the old one.
func TestTriggersReplace(t *testing.T) {
	t.Parallel()
	mlctest.RunCase(t, "triggers_replace", mlctest.Case{
		AssertState: func(t *testing.T, resources []apitype.ResourceV3) {
			var stash map[string]any
			for _, r := range resources {
				if r.Type == "pulumi:index:Stash" {
					stash = r.Outputs
				}
			}
			assert.Equal(t, map[string]any{
				"input":  map[string]any{"type": `"string"`, "value": "b"},
				"output": map[string]any{"type": `"string"`, "value": "b"},
			}, stash)
		},
	})
}
