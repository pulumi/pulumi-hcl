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

// A reference to an object that is not declared fails with the same message
// on both runtimes, whether an expression or a depends_on entry makes it.
func TestL2UndeclaredReference(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, expectErr string }{
		{"l2_undeclared_resource", `There is no managed resource "order_resource" "nope" definition in the root`},
		{"l2_undeclared_data_source", `There is no data resource "order_data" "nope" definition in the root`},
		{"l2_undeclared_variable", `An input variable with the name "nope" has not been declared.`},
		{"l2_undeclared_variable_suggestion", `An input variable with the name "nope" has not been declared. Did you mean`},
		{"l2_undeclared_local", `A local value with the name "nope" has not been declared. Did you`},
		{"l2_undeclared_module_call", `No module call named "nope" is declared in the root module.`},
		{"l2_undeclared_module_output", `This object does not have an attribute named "nope".`},
		{"l2_undeclared_resource_in_module", `There is no managed resource "order_resource" "n" definition in module.a.`},
		{"l2_undeclared_resource_data_hint", `Did you mean the data resource data.order_data.d?`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tfcompat.RunCase(t, tc.name, tfcompat.Case{
				Providers: []tfcompat.Provider{
					{Name: "order", Factory: providers.OrderProvider},
				},
				Stages: []tfcompat.Stage{{ExpectErr: tc.expectErr}},
			})
		})
	}
}
