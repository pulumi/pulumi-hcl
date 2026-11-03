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

// TestL2SameNameDeclarations checks that a variable and a resource may share a
// name. HCL scopes names per kind, but PCL declares both in one namespace, so
// the converter must give one of them another PCL name and keep every
// reference pointing at the right one.
func TestL2SameNameDeclarations(t *testing.T) {
	t.Parallel()
	hcltest.RunCase(t, "l2_same_name_declarations", hcltest.Case{
		Providers: []hcltest.Provider{
			{Name: "simple", Factory: providers.SimpleProvider},
		},
		ExpectedOutputs: map[string]string{
			"variable": "v",
			"resource": "v-false",
		},
	})
}
