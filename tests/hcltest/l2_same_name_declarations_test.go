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

// TestL2SameNameDeclarations declares a variable, a local, a provider, two
// resources of different types, a data block and a module, all with the same
// name. PCL has one namespace for these declarations, so the round trip must
// give each a unique PCL name and keep each reference on its declaration.
func TestL2SameNameDeclarations(t *testing.T) {
	t.Parallel()
	hcltest.RunCase(t, "l2_same_name_declarations", hcltest.Case{
		Providers: []hcltest.Provider{
			{Name: "simple", Factory: providers.SimpleProvider},
			{Name: "blocky", Factory: providers.BlockyProvider},
		},
		ExpectedOutputs: map[string]string{
			"r":             "c-c-p-v-local-false-child|c-p-v-local-false-child",
			"variable":      "v",
			"local":         "v-local",
			"resource":      "p-v-local",
			"other_type":    "blocky-single-id",
			"data":          "p-v-local-false",
			"passed":        "p-passed",
			"nested_passed": "c-passed",
		},
	})
}
