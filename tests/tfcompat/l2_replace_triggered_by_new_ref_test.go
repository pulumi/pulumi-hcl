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

// TestL2ReplaceTriggeredByNewRef adds, in one operation, both a new
// `trigger` resource and a `replace_triggered_by` entry on the existing
// `dependent` resource referencing the new trigger's computed attribute.
// Both paths must plan and perform the same operations on `dependent` —
// whether that is a replacement (the referenced value goes from nothing to a
// value) or no change at all, tofu and pulumi must agree, and each path's
// plan must agree with its own apply.
func TestL2ReplaceTriggeredByNewRef(t *testing.T) {
	t.Parallel()
	tfcompat.RunCase(t, "l2_replace_triggered_by_new_ref", tfcompat.Case{
		Providers: []tfcompat.Provider{
			{Name: "simple", Factory: providers.SimpleProvider},
		},
	})
}
