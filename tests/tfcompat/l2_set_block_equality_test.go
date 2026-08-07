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

package tfcompat_test

import (
	"testing"

	"github.com/pulumi/pulumi-hcl/tests/testutil/tfcompat"
	"github.com/pulumi/pulumi-hcl/tests/testutil/tfcompat/providers"
)

// TestL2SetBlockEquality exercises the runtime type of a repeating TypeSet
// nested block (`filter`). OpenTofu materializes it as a cty set, so
// `filter == toset([...same elements...])` is true (set equality is
// content-based and order-independent). pulumi-hcl materializes the set block
// as an ordered tuple, so the same comparison is a tuple-vs-set type mismatch
// and yields false — diverging on both the same-order and reordered outputs.
func TestL2SetBlockEquality(t *testing.T) {
	t.Parallel()
	// Correct behavior pinned in tests/putest/l2_block_projection_test.go.
	t.Skip("TODO[https://github.com/pulumi/pulumi-hcl/issues/509]: dynamic bridge materializes the set block as a tuple, so == toset(...) is false")
	tfcompat.RunCase(t, "l2_set_block_equality", tfcompat.Case{
		Providers: []tfcompat.Provider{
			{Name: "blocky", Factory: providers.BlockyProvider},
		},
	})
}
