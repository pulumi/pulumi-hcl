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

// TestL2MaxItemsOneOne covers `one()` of TypeList and TypeSet MaxItems=1
// blocks, set and absent.
func TestL2MaxItemsOneOne(t *testing.T) {
	t.Parallel()
	hcltest.RunCase(t, "l2_max_items_one_one", hcltest.Case{
		Providers: []hcltest.Provider{
			{Name: "blocky", Factory: providers.BlockyProvider},
		},
		ExpectedOutputs: map[string]string{
			"list_one":       "a",
			"list_one_unset": "true",
			"set_one":        "b",
			"set_one_unset":  "true",
		},
	})
}
