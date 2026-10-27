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

package putest_test

import (
	"testing"

	"github.com/pulumi/pulumi-hcl/tests/testutil/putest"
	"github.com/pulumi/pulumi-hcl/tests/testutil/tfcompat/providers"
)

// TestL2MapKeyAttribute writes a map key as an attribute (`tags.foo_bar`) in
// `ignore_changes` and in a reference. A map key is data, not a property
// name, so the round trip must keep it as `foo_bar`.
func TestL2MapKeyAttribute(t *testing.T) {
	t.Parallel()
	putest.RunCase(t, "l2_map_key_attribute", putest.Case{
		Providers: []putest.Provider{
			{Name: "collections", Factory: providers.CollectionsProvider},
		},
		ExpectedOutputs: map[string]string{"foo_bar": "x"},
	})
}
