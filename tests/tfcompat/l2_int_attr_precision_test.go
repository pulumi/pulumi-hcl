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

	"github.com/pulumi-labs/pulumi-hcl/tests/testutil/tfcompat"
	"github.com/pulumi-labs/pulumi-hcl/tests/testutil/tfcompat/providers"
)

// TestL2IntAttrPrecision asserts that a large integer literal assigned to a
// TypeInt resource attribute reaches the provider with full precision. OpenTofu
// delivers 9007199254740993 (2^53+1) exactly; pulumi-hcl coerces the value
// through a float64 and the provider receives 9007199254740992 instead.
func TestL2IntAttrPrecision(t *testing.T) {
	t.Parallel()
	tfcompat.RunCase(t, "l2_int_attr_precision", tfcompat.Case{
		Providers: []tfcompat.Provider{
			{Name: "collections", Factory: providers.CollectionsProvider},
		},
	})
}
