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

func TestL2OptionalComputedBlockUnset(t *testing.T) {
	t.Parallel()
	// Correct behavior pinned in tests/putest/l2_block_projection_test.go.
	t.Skip("TODO[https://github.com/pulumi/pulumi-hcl/issues/508]: dynamic bridge reads the unset TypeSet block as null, not the empty set")
	tfcompat.RunCase(t, "l2_optional_computed_block_unset", tfcompat.Case{
		Providers: []tfcompat.Provider{
			{Name: "optcomp", Factory: providers.OptCompBlockProvider},
		},
	})
}
