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

// renamedProviderCustomize applies the full set of non-default Pulumi renames
// the bridge mapping is expected to walk: provider config (scalar + nested
// block), resource (scalar input/output + nested-block input/output), and
// data source (nested-block input + scalar output + nested-block output).
// Sharing this customizer across the renamed-* cases keeps the schema in one
// place — every test exercises the same provider info, just from a different
// angle.
func renamedProviderCustomize(_ *testing.T, info *tfbridge.ProviderInfo) {
	info.Config = map[string]*tfbridge.SchemaInfo{
		"endpoint": {Name: "host"},
		"default_options": {
			Name: "defaults",
			Elem: &tfbridge.SchemaInfo{
				Fields: map[string]*tfbridge.SchemaInfo{
					"retries": {Name: "retryCount"},
				},
			},
		},
	}
	info.Resources["renamed_thing"].Fields = map[string]*tfbridge.SchemaInfo{
		"function_name": {Name: "name"},
		"settings": {
			Name: "config",
			Elem: &tfbridge.SchemaInfo{
				Fields: map[string]*tfbridge.SchemaInfo{
					"window_size": {Name: "sizeWindow"},
				},
			},
		},
	}
	info.DataSources["renamed_lookup"].Fields = map[string]*tfbridge.SchemaInfo{
		"filter": {
			Name: "lookupFilter",
			Elem: &tfbridge.SchemaInfo{
				Fields: map[string]*tfbridge.SchemaInfo{
					"kind": {Name: "kindLabel"},
				},
			},
		},
		"upstream": {Name: "source"},
		"result": {
			Name: "outcome",
			Elem: &tfbridge.SchemaInfo{
				Fields: map[string]*tfbridge.SchemaInfo{
					"tag": {Name: "label"},
				},
			},
		},
	}
}

// renamedCase is the standard provider list for these tests.
func renamedCase(expectedOutputs map[string]string) putest.Case {
	return putest.Case{
		Providers: []putest.Provider{
			{
				Name:      "renamed",
				Factory:   providers.RenamedProvider,
				Customize: renamedProviderCustomize,
			},
		},
		ExpectedOutputs: expectedOutputs,
	}
}

// TestL2RenamedOutputs covers resource inputs + outputs, both scalar and
// nested-block, when the bridge applies non-default Pulumi renames.
//
// Resource inputs exercised:
//   - `function_name` (TF) → `name` (Pulumi) — required scalar.
//   - `settings { window_size }` (TF block) → `config: { sizeWindow }` (Pulumi
//     flattened object) — block rename + nested field rename.
//
// Resource outputs exercised: the same fields read back, plus the unrenamed
// `arn` to confirm default-cased outputs still work alongside renames.
func TestL2RenamedOutputs(t *testing.T) {
	t.Parallel()
	putest.RunCase(t, "l2_renamed_outputs", renamedCase(map[string]string{
		"fn_name": "alpha",
		"arn":     "arn:test:alpha",
		"window":  "5",
	}))
}

// TestL2RenamedDataSourceOutputs covers data-source scalar output renames.
// The data source input `query` is unrenamed; the output `upstream` (TF) is
// surfaced under the TF name even though the bridge renames it to `source`.
func TestL2RenamedDataSourceOutputs(t *testing.T) {
	t.Parallel()
	putest.RunCase(t, "l2_renamed_data_source_outputs", renamedCase(map[string]string{
		"matched": "hit:lambda:",
		"from":    "registry",
	}))
}

// TestL2RenamedProviderConfig covers provider-config input renames, both
// scalar (`endpoint` → `host`) and nested-block (`default_options { retries }`
// → `defaults: { retryCount }`). The provider echoes its received config into
// computed resource outputs (`provider_endpoint`, `provider_retries`) so the
// test asserts the renamed inputs reached the upstream Configure call intact.
func TestL2RenamedProviderConfig(t *testing.T) {
	t.Parallel()
	putest.RunCase(t, "l2_renamed_provider_config", renamedCase(map[string]string{
		"endpoint_seen": "https://example.test/api",
		"retries_seen":  "3",
	}))
}

// TestL2RenamedDataSourceInputs covers data-source nested-block input renames:
// `filter { kind }` (TF) → `lookupFilter: { kindLabel }` (Pulumi). The data
// source echoes the filter back into `matched`, so the assertion fires only
// when the renamed nested input was forwarded through the bridge.
func TestL2RenamedDataSourceInputs(t *testing.T) {
	t.Parallel()
	putest.RunCase(t, "l2_renamed_data_source_inputs", renamedCase(map[string]string{
		"matched": "hit:lambda:trigger",
	}))
}

// TestL2RenamedDataSourceNestedOutputs covers nested-block output renames on
// data sources: `result { tag }` (TF) → `outcome: { label }` (Pulumi). The
// engine must project the computed output back under TF names so the source's
// `result[0].tag` traversal resolves.
func TestL2RenamedDataSourceNestedOutputs(t *testing.T) {
	t.Parallel()
	putest.RunCase(t, "l2_renamed_data_source_nested_outputs", renamedCase(map[string]string{
		"tag": "tag-for-lambda",
	}))
}
