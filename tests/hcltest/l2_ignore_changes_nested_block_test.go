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

package hcltest_test

import (
	"testing"

	"github.com/pulumi/pulumi-hcl/tests/testutil/hcltest"
	"github.com/pulumi/pulumi-hcl/tests/testutil/tfcompat/providers"
)

// TestL2IgnoreChangesNestedBlock writes `ignore_changes` into a MaxItems=1
// block as `settings[0].mode`. The bridge flattens the block to one object,
// so PCL must name the path `settings.mode`, and HCL must get the [0] back.
// Stage 1 changes only the ignored attribute, so `mode` stays "a".
func TestL2IgnoreChangesNestedBlock(t *testing.T) {
	t.Parallel()
	hcltest.RunCase(t, "l2_ignore_changes_nested_block", hcltest.Case{
		Providers: []hcltest.Provider{
			{Name: "blocky", Factory: providers.BlockyProvider},
		},
		ExpectedOutputs: map[string]string{
			"mode": "a",
		},
	})
}
