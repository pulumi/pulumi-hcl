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

package pcltest_test

import (
	"testing"

	"github.com/pulumi/pulumi-hcl/tests/testutil/pcltest"
	"github.com/pulumi/pulumi-hcl/tests/testutil/tfcompat/providers"
)

// TestL2MaxItemsOneRef covers a PCL reference into a TypeList MaxItems=1
// block, which HCL reads with [0].
func TestL2MaxItemsOneRef(t *testing.T) {
	t.Parallel()
	pcltest.RunCase(t, "l2_max_items_one_ref", pcltest.Case{
		Providers: []pcltest.Provider{
			{Name: "blocky", Factory: providers.BlockyProvider},
		},
		ExpectedOutputs: map[string]string{
			"mode": "a",
		},
	})
}
