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

package mlctest_test

import (
	"testing"

	"github.com/pulumi/pulumi-hcl/tests/testutil/mlctest"
	"github.com/pulumi/pulumi-hcl/tests/testutil/tfcompat/providers"
)

// A component selects between a resource and its data source, whose reference
// types do not unify (their `timeouts` objects differ). The conditional types
// as a union: an attribute both members share types as that attribute, and
// the union itself types as any.
func TestConditionalUnion(t *testing.T) {
	t.Parallel()
	mlctest.RunCase(t, "conditional_union", mlctest.Case{
		Providers: []mlctest.Provider{
			{Name: "timeoutable", Factory: providers.TimeoutableProvider},
		},
		ExpectedOutputs: map[string]string{
			"result": "hello-done",
			"thing":  `{"id":"timeoutable-id","input_one":"hello","resource_id":"timeoutable-id","result":"hello-done"}`,
		},
	})
}
