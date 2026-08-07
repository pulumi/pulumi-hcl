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
	"github.com/pulumi/pulumi-terraform-bridge/v3/pkg/tfbridge"
)

// TestL2PFMaxItemsOneAttr sets a plugin-framework list-nested attribute that
// a MaxItemsOne override flattens to a single Pulumi object. TF assigns the
// field with attribute syntax (`settings = [{...}]`) — there is no block form
// for a PF attribute — so the evaluated value reaches input conversion as a
// single-element tuple that must collapse to the flattened object.
func TestL2PFMaxItemsOneAttr(t *testing.T) {
	t.Parallel()
	putest.RunCase(t, "l2_pf_max_items_one_attr", putest.Case{
		Providers: []putest.Provider{pfxFlatMaxItemsOne()},
		ExpectedOutputs: map[string]string{
			"id": "pfx-flat-id",
		},
	})
}

// TestL2PFMaxItemsOneAttrOutput reads the flattened field back: in TF the
// attribute stays a list, so `settings` must re-expand to a single-element
// list on output and `settings[0]` / `length(settings)` must resolve.
func TestL2PFMaxItemsOneAttrOutput(t *testing.T) {
	t.Parallel()
	putest.RunCase(t, "l2_pf_max_items_one_attr_output", putest.Case{
		Providers: []putest.Provider{pfxFlatMaxItemsOne()},
		ExpectedOutputs: map[string]string{
			"settings": `[{"enabled":true}]`,
			"enabled":  "true",
			"count":    "1",
		},
	})
}

// pfxFlatMaxItemsOne is the PFX provider with pfx_flat's `settings`
// list-nested attribute flattened to a single Pulumi object.
func pfxFlatMaxItemsOne() putest.Provider {
	return putest.Provider{
		Name:      "pfx",
		PFFactory: providers.PFXProvider,
		Customize: func(_ *testing.T, info *tfbridge.ProviderInfo) {
			info.Resources["pfx_flat"].Fields = map[string]*tfbridge.SchemaInfo{
				"settings": {MaxItemsOne: tfbridge.True()},
			}
		},
	}
}
