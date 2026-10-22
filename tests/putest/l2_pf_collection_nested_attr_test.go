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

// TestL2PFCollectionNestedAttr pins the correct behavior of the linked-in
// bridge for the skipped tfcompat twin: plugin-framework list- and set-nested
// attributes take attribute syntax, and the round trip writes them back as
// arguments, not blocks. Delete it when the tfcompat twin is re-enabled.
func TestL2PFCollectionNestedAttr(t *testing.T) {
	t.Parallel()
	putest.RunCase(t, "l2_pf_collection_nested_attr", putest.Case{
		SkipRunPCL: "TODO[https://github.com/pulumi/pulumi-hcl/issues/710]: The PCL invoke does not use the configured `pfx` provider.",
		Providers: []putest.Provider{
			{Name: "pfx", PFFactory: providers.PFXProvider},
		},
		ExpectedOutputs: map[string]string{
			"settings":   `[{"enabled":true},{"enabled":false}]`,
			"names":      "alpha,beta",
			"registries": "oci://a.example,oci://b.example",
		},
	})
}
