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

// TestL2SingleSegmentToken pins that providers whose type token is a single
// word (no underscore) — e.g. `hashicorp/external` exposing `data "external"`,
// or `hashicorp/http` exposing `data "http"` — resolve through the engine
// without being rejected as malformed tokens, AND go through the bridge
// BodyMapping so TF-side attribute names that don't reverse-derive from the
// Pulumi name (`program` → `programs`, or any explicit Customize rename) are
// translated before the engine validates the HCL body.
func TestL2SingleSegmentToken(t *testing.T) {
	t.Parallel()
	putest.RunCase(t, "l2_single_segment_token", putest.Case{
		Providers: []putest.Provider{
			{
				Name:    "single",
				Factory: providers.SingleSegmentProvider,
				Customize: func(_ *testing.T, info *tfbridge.ProviderInfo) {
					// Rename to a Pulumi name whose snake_case reversal
					// (`tagged_thing`, `my_input`) doesn't match the TF
					// name, so the body mapping is the only path that can
					// translate HCL's TF-style names into the Pulumi schema.
					info.DataSources["single"].Fields = map[string]*tfbridge.SchemaInfo{
						"tag_value": {Name: "taggedThing"},
					}
					info.Resources["single"].Fields = map[string]*tfbridge.SchemaInfo{
						"input_value": {Name: "myInput"},
					}
				},
			},
		},
		ExpectedOutputs: map[string]string{
			"data_answer":     "a-hi:v",
			"resource_result": "r-world",
		},
	})
}
