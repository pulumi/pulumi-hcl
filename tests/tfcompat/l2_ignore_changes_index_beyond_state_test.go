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

// TestL2IgnoreChangesIndexBeyondState exercises an indexed `ignore_changes`
// path whose index exists in the new configuration but not in the prior
// state. Stage 0 creates `zones = ["a"]`, so `zones[1]` is absent from state;
// stage 1 appends an element. OpenTofu skips an ignore_changes entry that
// cannot be resolved against the prior value, so the appended element is
// applied and `zones` becomes ["a", "z"].
func TestL2IgnoreChangesIndexBeyondState(t *testing.T) {
	t.Parallel()
	tfcompat.RunCase(t, "l2_ignore_changes_index_beyond_state", tfcompat.Case{
		Providers: []tfcompat.Provider{
			{Name: "collections", Factory: providers.CollectionsProvider},
		},
	})
}
