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

// TestL2PFCollectionNestedAttr assigns plugin-framework list- and set-nested
// attributes with attribute syntax (`name = [{...}]`) in a provider
// configuration, a resource and a data source. Attribute syntax is the only
// valid syntax for them: they are attributes, not blocks.
//
// https://github.com/pulumi/pulumi-hcl/issues/687
func TestL2PFCollectionNestedAttr(t *testing.T) {
	t.Parallel()
	// Correct behavior pinned in tests/putest/l2_pf_collection_nested_attr_test.go.
	t.Skip("TODO[https://github.com/pulumi/pulumi-hcl/issues/687]: terraform-provider encodes list- and set-nested attributes as blocks")
	tfcompat.RunCase(t, "l2_pf_collection_nested_attr", tfcompat.Case{
		Providers: []tfcompat.Provider{
			{Name: "pfx", PFFactory: providers.PFXProvider},
		},
	})
}
