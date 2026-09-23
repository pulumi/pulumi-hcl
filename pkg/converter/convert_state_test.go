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

package converter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blang/semver"
	"github.com/hashicorp/hcl/v2"
	sdkschema "github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	"github.com/pulumi/pulumi-hcl/pkg/hcl/modulepath"
	"github.com/pulumi/pulumi-hcl/pkg/hcl/modules"
	"github.com/pulumi/pulumi-hcl/pkg/hcl/parser"
	"github.com/pulumi/pulumi-hcl/pkg/util/encryption"
	"github.com/pulumi/pulumi-hcl/tests/testutil/schemaloader"
	"github.com/pulumi/pulumi-hcl/vendored/addrs"
	"github.com/pulumi/pulumi-hcl/vendored/statefile"
	"github.com/pulumi/pulumi-hcl/vendored/states"
	"github.com/pulumi/pulumi-terraform-bridge/v3/pkg/tfbridge"
	shimv2 "github.com/pulumi/pulumi-terraform-bridge/v3/pkg/tfshim/sdk-v2"
	"github.com/pulumi/pulumi/pkg/v3/codegen/convert"
	"github.com/pulumi/pulumi/pkg/v3/codegen/schema"
	"github.com/pulumi/pulumi/pkg/v3/resource/plugin"
	"github.com/pulumi/pulumi/sdk/v3/go/common/apitype"
	"github.com/pulumi/pulumi/sdk/v3/go/common/util/rpcutil"
	"github.com/pulumi/pulumi/sdk/v3/go/common/workspace"
	"github.com/pulumi/pulumi/sdk/v3/go/property"
	pulumirpc "github.com/pulumi/pulumi/sdk/v3/proto/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

// fakeInfoSource is an in-memory bridge.ProviderInfoSource.
type fakeInfoSource struct {
	infos map[string]*tfbridge.ProviderInfo
}

func (f fakeInfoSource) GetProviderInfo(
	_ context.Context, tfProvider string, _ *workspace.PackageDescriptor,
) (*tfbridge.ProviderInfo, error) {
	return f.infos[tfProvider], nil
}

// roundtrip mirrors what the mapper delivers in production: the info is
// marshalled with its schema-only shim and unmarshalled back, so value
// translation runs against the same ProviderShim it would in the CLI.
func roundtrip(t *testing.T, info tfbridge.ProviderInfo) *tfbridge.ProviderInfo {
	t.Helper()
	b, err := json.Marshal(tfbridge.MarshalProviderInfo(&info))
	require.NoError(t, err)
	var m tfbridge.MarshallableProviderInfo
	require.NoError(t, json.Unmarshal(b, &m))
	return m.Unmarshal()
}

func exampleInfoSource(t *testing.T) fakeInfoSource {
	t.Helper()
	p := &sdkschema.Provider{
		ResourcesMap: map[string]*sdkschema.Resource{
			"example_resource": {
				Schema: map[string]*sdkschema.Schema{
					"input_one":    {Type: sdkschema.TypeString, Optional: true},
					"secret_sauce": {Type: sdkschema.TypeString, Optional: true, Sensitive: true},
					"computed_out": {Type: sdkschema.TypeString, Computed: true},
					"settings": {
						Type: sdkschema.TypeList, MaxItems: 1, Optional: true,
						Elem: &sdkschema.Resource{Schema: map[string]*sdkschema.Schema{
							"enabled": {Type: sdkschema.TypeBool, Optional: true},
						}},
					},
				},
			},
		},
	}
	return fakeInfoSource{
		infos: map[string]*tfbridge.ProviderInfo{
			"example": roundtrip(t, tfbridge.ProviderInfo{
				P:    shimv2.NewProvider(p),
				Name: "example",
				Resources: map[string]*tfbridge.ResourceInfo{
					"example_resource": {Tok: "example:index/resource:Resource"},
				},
			}),
		},
	}
}

// bridgeModuleFormat is the bridge's standard module format:
// "index/resource" is module "index", not a nested module.
const bridgeModuleFormat = "(.*)(?:/[^/]*)"

// examplePackageSpec is the Pulumi projection of exampleInfoSource's TF
// schema (renames, MaxItemsOne flattening, Sensitive → Secret).
func examplePackageSpec() schema.PackageSpec {
	str := schema.TypeSpec{Type: "string"}
	settings := schema.TypeSpec{Ref: "#/types/example:index/Settings:Settings"}
	return schema.PackageSpec{
		Name:    "example",
		Version: "1.0.0",
		Meta:    &schema.MetadataSpec{ModuleFormat: bridgeModuleFormat},
		Types: map[string]schema.ComplexTypeSpec{
			"example:index/Settings:Settings": {ObjectTypeSpec: schema.ObjectTypeSpec{
				Type: "object",
				Properties: map[string]schema.PropertySpec{
					"enabled": {TypeSpec: schema.TypeSpec{Type: "boolean"}},
				},
			}},
		},
		Resources: map[string]schema.ResourceSpec{
			"example:index/resource:Resource": {
				ObjectTypeSpec: schema.ObjectTypeSpec{Properties: map[string]schema.PropertySpec{
					"inputOne":    {TypeSpec: str},
					"secretSauce": {TypeSpec: str, Secret: true},
					"computedOut": {TypeSpec: str},
					"settings":    {TypeSpec: settings},
				}},
				InputProperties: map[string]schema.PropertySpec{
					"inputOne":    {TypeSpec: str},
					"secretSauce": {TypeSpec: str, Secret: true},
					"settings":    {TypeSpec: settings},
				},
			},
		},
	}
}

func exampleLoader(t *testing.T) schema.ReferenceLoader {
	t.Helper()
	return schemaloader.New(t, examplePackageSpec())
}

// exampleDescriptor mirrors what `pulumi install` writes to
// sdks/example/hcl.sdk.json for a dynamically bridged provider: the base
// terraform-provider plugin plus the parameterization that produces the
// example package from it.
func exampleDescriptor() workspace.PackageDescriptor {
	base := semver.MustParse("0.0.1")
	return workspace.PackageDescriptor{
		PluginDescriptor: workspace.PluginDescriptor{
			Name:              "terraform-provider",
			Kind:              apitype.ResourcePlugin,
			Version:           &base,
			PluginDownloadURL: "github://api.github.com/pulumi/pulumi-terraform-provider",
		},
		Parameterization: &workspace.Parameterization{
			Name:    "example",
			Version: semver.MustParse("6.0.0"),
			Value:   []byte(`{"remote":{"url":"registry.terraform.io/acme/example","version":"6.0.0"}}`),
		},
	}
}

// stateV4 wraps a `{"resources": [...]}` fragment in the v4 envelope the
// state-file parser requires.
func stateV4(t *testing.T, fragment string) []byte {
	t.Helper()
	var doc map[string]json.RawMessage
	require.NoError(t, json.Unmarshal([]byte(fragment), &doc))
	doc["version"] = json.RawMessage(`4`)
	doc["terraform_version"] = json.RawMessage(`"1.12.0"`)
	doc["lineage"] = json.RawMessage(`"00000000-0000-0000-0000-000000000000"`)
	doc["serial"] = json.RawMessage(`1`)
	full, err := json.Marshal(doc)
	require.NoError(t, err)
	return full
}

func parseState(t *testing.T, fragment string) *states.State {
	t.Helper()
	f, err := statefile.Read(bytes.NewReader(stateV4(t, fragment)), encryption.StateEncryptionDisabled())
	require.NoError(t, err)
	return f.State
}

func TestConvertTFState_EmitsParameterizedImport(t *testing.T) {
	t.Parallel()

	state := parseState(t, `{
		"resources": [
			{
				"mode": "managed", "type": "example_resource", "name": "b",
				"provider": "provider[\"registry.terraform.io/acme/example\"]",
				"instances": [ { "attributes": { "id": "res-1" } } ]
			}
		]
	}`)

	descriptors := map[string]workspace.PackageDescriptor{"example": exampleDescriptor()}
	resp := convertTFState(t.Context(), exampleInfoSource(t), exampleLoader(t), descriptors, nil, state)

	require.Empty(t, resp.Diagnostics)
	require.Len(t, resp.Resources, 1)

	got := resp.Resources[0]
	assert.Equal(t, "example:index/resource:Resource", got.Type)
	assert.Equal(t, "b", got.Name)
	assert.Equal(t, "res-1", got.ID)
	assert.Equal(t, "6.0.0", got.Version, "the parameterized package's version")
	assert.Equal(t, "github://api.github.com/pulumi/pulumi-terraform-provider", got.PluginDownloadURL)
	require.NotNil(t, got.Parameterization)
	assert.Equal(t, "terraform-provider", got.Parameterization.PluginName)
	assert.Equal(t, "0.0.1", got.Parameterization.PluginVersion)
	assert.JSONEq(t, `{"remote":{"url":"registry.terraform.io/acme/example","version":"6.0.0"}}`,
		string(got.Parameterization.Value))
}

func TestConvertTFState_SkipsUnimportable(t *testing.T) {
	t.Parallel()

	state := parseState(t, `{
		"resources": [
			{
				"mode": "data", "type": "example_data", "name": "ubuntu",
				"provider": "provider[\"registry.terraform.io/acme/example\"]",
				"instances": [ { "attributes": { "id": "ami-123" } } ]
			},
			{
				"module": "module.vpc", "mode": "managed", "type": "example_resource", "name": "nested",
				"provider": "provider[\"registry.terraform.io/acme/example\"]",
				"instances": [ { "attributes": { "id": "nested-bucket" } } ]
			},
			{
				"mode": "managed", "type": "unknown_widget", "name": "g",
				"provider": "provider[\"registry.terraform.io/acme/unknown\"]",
				"instances": [ { "attributes": { "id": "widget-1" } } ]
			}
		]
	}`)

	resp := convertTFState(t.Context(), exampleInfoSource(t), exampleLoader(t), nil, nil, state)

	// The data source skips silently; the other two each warn.
	assert.Empty(t, resp.Resources)
	assert.Len(t, resp.Diagnostics, 2)
	for _, d := range resp.Diagnostics {
		assert.Equal(t, hcl.DiagWarning, d.Severity)
	}
}

// TestConvertTFState_MissingID pins the ID a resource with no `id` attribute
// imports under: the sentinel the bridge computes for it, not an empty ID,
// which the engine reads as a deleted resource.
func TestConvertTFState_MissingID(t *testing.T) {
	t.Parallel()

	state := parseState(t, `{
		"resources": [
			{
				"mode": "managed", "type": "example_resource", "name": "no_id",
				"provider": "provider[\"registry.terraform.io/acme/example\"]",
				"instances": [ { "attributes": { "input_one": "hello" } } ]
			}
		]
	}`)

	resp := convertTFState(t.Context(), exampleInfoSource(t), exampleLoader(t), nil, nil, state)

	require.Empty(t, resp.Diagnostics)
	require.Len(t, resp.Resources, 1)
	assert.Equal(t, "missing ID", resp.Resources[0].ID)
	assert.Equal(t, property.New("hello"), resp.Resources[0].Inputs.AsMap()["inputOne"])
}

func TestConvertTFState_CountAndForEach(t *testing.T) {
	t.Parallel()

	// Emitted names must match the runtime's buildResourceName.
	state := parseState(t, `{
		"resources": [
			{
				"mode": "managed", "type": "example_resource", "name": "counted",
				"provider": "provider[\"registry.terraform.io/acme/example\"]",
				"instances": [
					{ "index_key": 0, "attributes": { "id": "bucket-0" } },
					{ "index_key": 1, "attributes": { "id": "bucket-1" } }
				]
			},
			{
				"mode": "managed", "type": "example_resource", "name": "byregion",
				"provider": "provider[\"registry.terraform.io/acme/example\"]",
				"instances": [
					{ "index_key": "east", "attributes": { "id": "bucket-east" } }
				]
			}
		]
	}`)

	resp := convertTFState(t.Context(), exampleInfoSource(t), exampleLoader(t), nil, nil, state)

	require.Empty(t, resp.Diagnostics)
	names := make(map[string]string, len(resp.Resources))
	for _, r := range resp.Resources {
		names[r.Name] = r.ID
	}
	assert.Equal(t, map[string]string{
		"counted[0]":       "bucket-0",
		"counted[1]":       "bucket-1",
		`byregion["east"]`: "bucket-east",
	}, names)
}

// A provider whose name does not prefix its resource types (google-beta
// owning google_* resources) must resolve via the state's provider address.
func TestConvertTFState_ProviderNamePrefixMismatch(t *testing.T) {
	t.Parallel()

	state := parseState(t, `{
		"resources": [
			{
				"mode": "managed", "type": "confounding_provider_resource", "name": "sadness",
				"provider": "provider[\"registry.terraform.io/example/confounding-beta\"]",
				"instances": [ { "attributes": { "id": "c-1" } } ]
			}
		]
	}`)

	src := fakeInfoSource{infos: map[string]*tfbridge.ProviderInfo{
		"confounding-beta": roundtrip(t, tfbridge.ProviderInfo{
			P: shimv2.NewProvider(&sdkschema.Provider{
				ResourcesMap: map[string]*sdkschema.Resource{
					"confounding_provider_resource": {Schema: map[string]*sdkschema.Schema{
						"name": {Type: sdkschema.TypeString, Optional: true},
					}},
				},
			}),
			Name: "confounding-beta",
			Resources: map[string]*tfbridge.ResourceInfo{
				"confounding_provider_resource": {Tok: "confounding:index/resource:Resource"},
			},
		}),
	}}
	loader := schemaloader.New(t, schema.PackageSpec{
		Name:    "confounding",
		Version: "1.0.0",
		Meta:    &schema.MetadataSpec{ModuleFormat: bridgeModuleFormat},
		Resources: map[string]schema.ResourceSpec{
			"confounding:index/resource:Resource": {ObjectTypeSpec: schema.ObjectTypeSpec{
				Properties: map[string]schema.PropertySpec{
					"name": {TypeSpec: schema.TypeSpec{Type: "string"}},
				},
			}},
		},
	})
	resp := convertTFState(t.Context(), src, loader, nil, nil, state)

	require.Empty(t, resp.Diagnostics)
	require.Len(t, resp.Resources, 1)
	assert.Equal(t, "confounding:index/resource:Resource", resp.Resources[0].Type)
	assert.Equal(t, "c-1", resp.Resources[0].ID)
}

// TestConvertTFState_SuppliesValues pins the outputs-supplied import shape:
// bridge renames and MaxItemsOne flattening, schema-sensitive fields and
// dynamically-marked paths as secrets, and inputs reduced to the schema's
// input subset.
func TestConvertTFState_SuppliesValues(t *testing.T) {
	t.Parallel()

	state := parseState(t, `{
		"resources": [
			{
				"mode": "managed", "type": "example_resource", "name": "b",
				"provider": "provider[\"registry.terraform.io/acme/example\"]",
				"instances": [ {
					"attributes": {
						"id": "res-1",
						"input_one": "hello",
						"secret_sauce": "hunter2",
						"computed_out": "made-up-by-the-cloud",
						"settings": [ { "enabled": true } ],
						"timeouts": { "create": "10m" }
					},
					"sensitive_attributes": [
						[ { "type": "get_attr", "value": "input_one" } ],
						[
							{ "type": "get_attr", "value": "settings" },
							{ "type": "index", "value": { "value": 0, "type": "number" } },
							{ "type": "get_attr", "value": "enabled" }
						]
					]
				} ]
			}
		]
	}`)

	resp := convertTFState(t.Context(), exampleInfoSource(t), exampleLoader(t), nil, nil, state)
	require.Empty(t, resp.Diagnostics)
	require.Len(t, resp.Resources, 1)
	got := resp.Resources[0]

	outs := got.Outputs.AsMap()
	assert.Equal(t, property.New("res-1"), outs["id"])
	assert.Equal(t, property.New("hello").WithSecret(true), outs["inputOne"],
		"dynamically-marked sensitive path")
	assert.Equal(t, property.New("hunter2").WithSecret(true), outs["secretSauce"],
		"schema-sensitive field")
	assert.Equal(t, property.New("made-up-by-the-cloud"), outs["computedOut"])
	require.True(t, outs["settings"].IsMap(), "MaxItemsOne block flattens to an object")
	assert.NotContains(t, outs, "timeouts", "SDKv2's timeouts state attribute is dropped")
	assert.Equal(t, property.New(true).WithSecret(true), outs["settings"].AsMap().Get("enabled"),
		"the sensitive path's index step drops with the MaxItemsOne flattening")

	ins := got.Inputs.AsMap()
	assert.Contains(t, ins, "inputOne")
	assert.Contains(t, ins, "secretSauce")
	assert.Contains(t, ins, "settings")
	assert.NotContains(t, ins, "computedOut", "computed-only fields are not inputs")
	assert.NotContains(t, ins, "id")
}

// TestConvertTFState_RenamedID pins the dynamic-bridge import shape (#512):
// the TF id must project onto the bridge's renamed id property — and only
// there — or the provider's first Diff sees a null id and replaces.
func TestConvertTFState_RenamedID(t *testing.T) {
	t.Parallel()

	state := parseState(t, `{
		"resources": [
			{
				"mode": "managed", "type": "acme_thing", "name": "b",
				"provider": "provider[\"registry.terraform.io/acme/acme\"]",
				"instances": [ { "attributes": { "id": "t-1", "name": "hello" } } ]
			}
		]
	}`)

	src := fakeInfoSource{infos: map[string]*tfbridge.ProviderInfo{
		"acme": roundtrip(t, tfbridge.ProviderInfo{
			P: shimv2.NewProvider(&sdkschema.Provider{
				ResourcesMap: map[string]*sdkschema.Resource{
					"acme_thing": {Schema: map[string]*sdkschema.Schema{
						"id":   {Type: sdkschema.TypeString, Optional: true, Computed: true},
						"name": {Type: sdkschema.TypeString, Optional: true},
					}},
				},
			}),
			Name: "acme",
			Resources: map[string]*tfbridge.ResourceInfo{
				"acme_thing": {
					Tok: "acme:index/thing:Thing",
					// The rename fixID installs for every dynamic resource.
					Fields: map[string]*tfbridge.SchemaInfo{"id": {Name: "thingId"}},
				},
			},
		}),
	}}
	str := schema.TypeSpec{Type: "string"}
	loader := schemaloader.New(t, schema.PackageSpec{
		Name:    "acme",
		Version: "1.0.0",
		Meta:    &schema.MetadataSpec{ModuleFormat: bridgeModuleFormat},
		Resources: map[string]schema.ResourceSpec{
			"acme:index/thing:Thing": {
				ObjectTypeSpec: schema.ObjectTypeSpec{Properties: map[string]schema.PropertySpec{
					"thingId": {TypeSpec: str},
					"name":    {TypeSpec: str},
				}},
				InputProperties: map[string]schema.PropertySpec{
					"thingId": {TypeSpec: str},
					"name":    {TypeSpec: str},
				},
			},
		},
	})

	resp := convertTFState(t.Context(), src, loader, nil, nil, state)
	require.Empty(t, resp.Diagnostics)
	require.Len(t, resp.Resources, 1)
	got := resp.Resources[0]

	assert.Equal(t, "t-1", got.ID)
	outs := got.Outputs.AsMap()
	ins := got.Inputs.AsMap()
	assert.Equal(t, property.New("t-1"), outs["thingId"],
		"the TF id projects onto the renamed property")
	assert.NotContains(t, outs, "id",
		"native dynamic-bridge state carries no plain id output")
	assert.Equal(t, property.New("hello"), outs["name"])
	assert.Contains(t, ins, "name")
	assert.NotContains(t, ins, "thingId",
		"the renamed id is provider-populated, never a program input")
}

// TestConvertTFState_TerraformData pins the builtin's import shape: Stash
// imports carrying the runtime's {type, value} wrapper encoding, an explicit
// null input when absent, and no triggers_replace (the engine's replacement
// trigger records on the first up).
func TestConvertTFState_TerraformData(t *testing.T) {
	t.Parallel()

	state := parseState(t, `{
		"resources": [
			{
				"mode": "managed", "type": "terraform_data", "name": "d",
				"provider": "provider[\"terraform.io/builtin/terraform\"]",
				"instances": [ {
					"attributes": {
						"id": "aaaa-bbbb",
						"input": {"value": ["x", "y"], "type": ["set", "string"]},
						"output": {"value": ["x", "y"], "type": ["set", "string"]},
						"triggers_replace": {"value": "v1", "type": "string"}
					}
				} ]
			},
			{
				"mode": "managed", "type": "terraform_data", "name": "trigger_only",
				"provider": "provider[\"terraform.io/builtin/terraform\"]",
				"instances": [ {
					"attributes": {
						"id": "cccc-dddd",
						"input": null,
						"output": null,
						"triggers_replace": {"value": "v2", "type": "string"}
					}
				} ]
			}
		]
	}`)

	resp := convertTFState(t.Context(), fakeInfoSource{}, nil, nil, nil, state)
	require.Empty(t, resp.Diagnostics)
	require.Len(t, resp.Resources, 2)

	got := resp.Resources[0]
	assert.Equal(t, "pulumi:index:Stash", got.Type)
	assert.Equal(t, "d", got.Name)
	assert.Equal(t, "aaaa-bbbb", got.ID)
	assert.Empty(t, got.Version)
	assert.Nil(t, got.Parameterization, "the engine's builtin provider serves Stash; no plugin identity")
	wrapped := property.New(map[string]property.Value{
		"type":  property.New(`["set","string"]`),
		"value": property.New([]property.Value{property.New("x"), property.New("y")}),
	})
	assert.Equal(t, property.NewMap(map[string]property.Value{"input": wrapped}), *got.Inputs,
		"the cty type survives in the wrapper; triggers_replace is not an input")
	assert.Equal(t, property.NewMap(map[string]property.Value{"input": wrapped, "output": wrapped}), *got.Outputs)

	trigger := resp.Resources[1]
	assert.Equal(t, "cccc-dddd", trigger.ID)
	assert.Equal(t, property.NewMap(map[string]property.Value{"input": property.New(property.Null)}), *trigger.Inputs,
		"an absent input imports as the explicit null the runtime registers")
}

// TestConvertTFState_ValueFallbacks pins the degradations: instances whose
// values cannot be translated still import by id, with a warning.
func TestConvertTFState_ValueFallbacks(t *testing.T) {
	t.Parallel()

	state := parseState(t, `{
		"resources": [
			{
				"mode": "managed", "type": "example_resource", "name": "drifted",
				"provider": "provider[\"registry.terraform.io/acme/example\"]",
				"instances": [ { "schema_version": 5, "attributes": { "id": "old-schema", "input_one": "hello" } } ]
			},
			{
				"mode": "managed", "type": "example_resource", "name": "legacy",
				"provider": "provider[\"registry.terraform.io/acme/example\"]",
				"instances": [ { "attributes_flat": { "id": "flatmap" } } ]
			}
		]
	}`)

	resp := convertTFState(t.Context(), exampleInfoSource(t), exampleLoader(t), nil, nil, state)
	require.Len(t, resp.Resources, 2)
	byID := map[string]plugin.ResourceImport{}
	for _, r := range resp.Resources {
		byID[r.ID] = r
	}

	// Attributes at a version the provider does not map are still imported:
	// they carry their own schema version, and the provider upgrades them.
	drifted := byID["old-schema"].Outputs.AsMap()
	assert.Equal(t, property.New("hello"), drifted["inputOne"])
	assert.Equal(t, property.New(`{"schema_version":"5"}`), drifted["__meta"],
		"the state's version travels with its values")

	// Flatmap attributes have no translation at all.
	assert.Nil(t, byID["flatmap"].Outputs, "pre-0.12 state imports by id only")
	assert.Nil(t, byID["flatmap"].Inputs)

	require.Len(t, resp.Diagnostics, 1)
	assert.Equal(t, "Importing without values", resp.Diagnostics[0].Summary)
}

// A resource whose provider declares a schema version imports its values like
// any other, and records the version those values are at, so the provider
// upgrades them only if they are behind the schema it now serves.
func TestConvertTFState_VersionedResource(t *testing.T) {
	t.Parallel()

	info := roundtrip(t, tfbridge.ProviderInfo{
		P: shimv2.NewProvider(&sdkschema.Provider{
			ResourcesMap: map[string]*sdkschema.Resource{
				"example_resource": {
					SchemaVersion: 1,
					Schema: map[string]*sdkschema.Schema{
						"input_one": {Type: sdkschema.TypeString, Optional: true},
					},
				},
			},
		}),
		Name: "example",
		Resources: map[string]*tfbridge.ResourceInfo{
			"example_resource": {Tok: "example:index/resource:Resource"},
		},
	})
	state := parseState(t, `{
		"resources": [
			{
				"mode": "managed", "type": "example_resource", "name": "b",
				"provider": "provider[\"registry.terraform.io/acme/example\"]",
				"instances": [ { "schema_version": 1, "attributes": { "id": "res-1", "input_one": "hello" } } ]
			}
		]
	}`)

	src := fakeInfoSource{infos: map[string]*tfbridge.ProviderInfo{"example": info}}
	resp := convertTFState(t.Context(), src, exampleLoader(t), nil, nil, state)
	assert.Empty(t, resp.Diagnostics)
	require.Len(t, resp.Resources, 1)

	outs := resp.Resources[0].Outputs.AsMap()
	assert.Equal(t, property.New("hello"), outs["inputOne"])
	assert.Equal(t, property.New(`{"schema_version":"1"}`), outs["__meta"])
}

// TestConvertTFState_ModuleResources pins the module import shape: a component
// per module instance (including ancestors the state does not name), children
// parented and dot-named as the runtime registers them.
func TestConvertTFState_ModuleResources(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "main.tf"), []byte(`
		module "outer" { source = "./outer" }
	`), 0o600))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "outer"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "outer", "main.tf"), []byte(`
		module "inner" { source = "./inner" }
	`), 0o600))

	state := parseState(t, `{
		"resources": [
			{
				"module": "module.outer.module.inner", "mode": "managed",
				"type": "example_resource", "name": "r",
				"provider": "provider[\"registry.terraform.io/acme/example\"]",
				"instances": [ { "attributes": { "id": "res-1" } } ]
			},
			{
				"module": "module.absent", "mode": "managed",
				"type": "example_resource", "name": "gone",
				"provider": "provider[\"registry.terraform.io/acme/example\"]",
				"instances": [ { "attributes": { "id": "res-2" } } ]
			}
		]
	}`)

	resp := convertTFState(t.Context(), exampleInfoSource(t), exampleLoader(t), nil, mustModuleSources(t, dir), state)
	require.Len(t, resp.Diagnostics, 1)
	assert.Equal(t, "Skipped module resource", resp.Diagnostics[0].Summary,
		"a module the program does not declare has no component to import under")

	require.Len(t, resp.Resources, 3)
	outer, inner, child := resp.Resources[0], resp.Resources[1], resp.Resources[2]

	assert.Equal(t, "components:index:Outer", outer.Type)
	assert.Equal(t, "outer", outer.Name)
	assert.True(t, outer.IsComponent)
	assert.Empty(t, outer.Parent)
	assert.Empty(t, outer.ID, "components have no id")

	assert.Equal(t, "components:index:Inner", inner.Type, "the enclosing module holds no resources of its own")
	assert.Equal(t, "outer.inner", inner.Name)
	assert.Equal(t, "outer", inner.Parent)

	assert.Equal(t, "example:index/resource:Resource", child.Type)
	assert.Equal(t, "outer.inner.r", child.Name)
	assert.Equal(t, "outer.inner", child.Parent)
	assert.Equal(t, "res-1", child.ID)
}

// TestConvertTFState_SiblingModuleCalls covers two calls in one module:
// emitting their shared ancestor once must not stop the second call's own
// component being emitted. A dangling Parent fails the import outright.
func TestConvertTFState_SiblingModuleCalls(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "main.tf"), []byte(`
		module "outer" { source = "./outer" }
	`), 0o600))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "outer", "leaf"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "outer", "main.tf"), []byte(`
		module "a" { source = "./leaf" }
		module "b" { source = "./leaf" }
	`), 0o600))

	state := parseState(t, `{
		"resources": [
			{
				"module": "module.outer.module.a", "mode": "managed",
				"type": "example_resource", "name": "r",
				"provider": "provider[\"registry.terraform.io/acme/example\"]",
				"instances": [ { "attributes": { "id": "res-a" } } ]
			},
			{
				"module": "module.outer.module.b", "mode": "managed",
				"type": "example_resource", "name": "r",
				"provider": "provider[\"registry.terraform.io/acme/example\"]",
				"instances": [ { "attributes": { "id": "res-b" } } ]
			}
		]
	}`)

	resp := convertTFState(t.Context(), exampleInfoSource(t), exampleLoader(t), nil, mustModuleSources(t, dir), state)
	require.Empty(t, resp.Diagnostics)

	byName := map[string]plugin.ResourceImport{}
	for _, r := range resp.Resources {
		byName[r.Name] = r
	}
	assert.Len(t, byName, 5)
	for _, r := range resp.Resources {
		if r.Parent != "" {
			assert.Containsf(t, byName, r.Parent, "%q is parented to a resource not in the response", r.Name)
		}
	}

	// Both calls share a source, so both components carry the same type.
	assert.Equal(t, "components:index:Leaf", byName["outer.a"].Type)
	assert.Equal(t, "components:index:Leaf", byName["outer.b"].Type)
	assert.Equal(t, "outer", byName["outer.a"].Parent)
	assert.Equal(t, "outer", byName["outer.b"].Parent)
	assert.Equal(t, "res-a", byName["outer.a.r"].ID)
	assert.Equal(t, "res-b", byName["outer.b.r"].ID)
}

func TestResourceName(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "b", resourceName("b", addrs.NoKey))
	assert.Equal(t, "b[0]", resourceName("b", addrs.IntKey(0)))
	assert.Equal(t, "b[2]", resourceName("b", addrs.IntKey(2)))
	assert.Equal(t, `b["east"]`, resourceName("b", addrs.StringKey("east")))
	assert.Equal(t, "my-bucket.v2", resourceName("my-bucket.v2", addrs.NoKey), "no sanitisation")
}

func TestIDAttr(t *testing.T) {
	t.Parallel()
	id, ok := idAttr(&states.ResourceInstanceObjectSrc{AttrsJSON: []byte(`{"id": "x"}`)})
	assert.True(t, ok)
	assert.Equal(t, "x", id)

	_, ok = idAttr(&states.ResourceInstanceObjectSrc{AttrsJSON: []byte(`{"input_one": "hello"}`)})
	assert.False(t, ok, "no id attribute")

	_, ok = idAttr(&states.ResourceInstanceObjectSrc{AttrsJSON: []byte(`{"id": ""}`)})
	assert.False(t, ok, "empty id")

	_, ok = idAttr(&states.ResourceInstanceObjectSrc{AttrsJSON: []byte(`{"id": 123}`)})
	assert.False(t, ok, "non-string id")

	id, ok = idAttr(&states.ResourceInstanceObjectSrc{AttrsFlat: map[string]string{"id": "flat"}})
	assert.True(t, ok, "pre-0.12 flatmap attributes")
	assert.Equal(t, "flat", id)
}

// TestPackageDescriptorsFromResolver pins the pre-install path: with no SDKs
// on disk, the program's providers resolve through the engine's package
// resolver, exactly as `pulumi install` resolves them.
func TestPackageDescriptorsFromResolver(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "main.tf"), []byte(`
		terraform {
			required_providers {
				example = { source = "acme/example", version = "6.0.0" }
			}
		}
	`), 0o600))

	got, _, err := testPackageDescriptors(t, &hclConverter{projectDir: dir}, serveResolver(t))
	require.NoError(t, err)

	desc, ok := got["example"]
	require.True(t, ok, "the program's provider resolves without an installed SDK")
	assert.Equal(t, "terraform-provider", desc.Name)
	require.NotNil(t, desc.Parameterization)
	assert.Equal(t, "example", desc.Parameterization.Name)
	assert.Equal(t, "6.0.0", desc.Parameterization.Version.String())
}

// fakeResolver stands in for the engine's package resolver: it echoes the
// requested parameterization back, as the terraform-provider plugin does.
type fakeResolver struct {
	pulumirpc.UnimplementedPackageResolverServer
	t *testing.T
	// respond overrides the echo, so a test can fail one resolution.
	respond func(*pulumirpc.PackageSpec) (*pulumirpc.PackageDependency, error)
}

func (r fakeResolver) ResolvePackage(
	_ context.Context, spec *pulumirpc.PackageSpec,
) (*pulumirpc.PackageDependency, error) {
	if r.respond != nil {
		return r.respond(spec)
	}
	// assert, never require: this runs on a gRPC handler goroutine, where
	// t.FailNow would be an illegal runtime.Goexit.
	assert.Equal(r.t, "terraform-provider", spec.Source)
	if !assert.NotEmpty(r.t, spec.Parameters, "the provider's source is the first parameter") {
		return nil, errors.New("no parameters")
	}
	source, version := spec.Parameters[0], ""
	if len(spec.Parameters) > 1 {
		version = spec.Parameters[1]
	}
	name := source[strings.LastIndex(source, "/")+1:]
	return &pulumirpc.PackageDependency{
		Kind:    string(apitype.ResourcePlugin),
		Name:    "terraform-provider",
		Version: "0.0.1",
		Parameterization: &pulumirpc.PackageParameterization{
			Name:    name,
			Version: version,
			Value:   fmt.Appendf(nil, `{"remote":{"url":%q,"version":%q}}`, source, version),
		},
	}, nil
}

func TestConvertStateArgValidation(t *testing.T) {
	t.Parallel()

	_, err := New().ConvertState(t.Context(), &plugin.ConvertStateRequest{
		Args: []string{"state.json"},
	})
	assert.ErrorContains(t, err, "missing mapper target")

	_, err = New().ConvertState(t.Context(), &plugin.ConvertStateRequest{
		MapperTarget: "127.0.0.1:1",
		Args:         []string{"state.json"},
	})
	assert.ErrorContains(t, err, "missing loader address")

	_, err = New().ConvertState(t.Context(), &plugin.ConvertStateRequest{
		MapperTarget:   "127.0.0.1:1",
		LoaderTarget:   "127.0.0.1:1",
		ResolverTarget: "127.0.0.1:1",
	})
	assert.ErrorContains(t, err, "expected exactly one argument")

	_, err = New().ConvertState(t.Context(), &plugin.ConvertStateRequest{
		MapperTarget:   "127.0.0.1:1",
		LoaderTarget:   "127.0.0.1:1",
		ResolverTarget: "127.0.0.1:1",
		Args:           []string{"a", "b"},
	})
	assert.ErrorContains(t, err, "expected exactly one argument")
}

// TestPackageDescriptorsWithoutResolver pins the older-engine path: with no
// resolver address the import still lands under whatever `pulumi install`
// wrote, exactly as it did before the resolver existed.
func TestPackageDescriptorsWithoutResolver(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "main.tf"), nil, 0o600))
	writeInstalledSDK(t, dir, "example", exampleDescriptor())

	got, _, err := testPackageDescriptors(t, &hclConverter{projectDir: dir}, "")
	require.NoError(t, err)

	desc, ok := got["example"]
	require.True(t, ok)
	require.NotNil(t, desc.Parameterization)
	assert.Equal(t, "example", desc.Parameterization.Name)
}

// TestPackageDescriptorsUnresolvableProvider pins that a provider the resolver
// cannot reach — served locally, or behind a registry that is down — does not
// sink the import: it falls back to what `pulumi install` wrote, and says so
// when there is nothing to fall back to.
func TestPackageDescriptorsUnresolvableProvider(t *testing.T) {
	t.Parallel()

	program := `
		terraform {
			required_providers {
				nowhere = { source = "hashicorp/nowhere", version = "1.0.0" }
			}
		}
		resource "nowhere_thing" "a" {}
	`
	target := serveResolver(t, func(*pulumirpc.PackageSpec) (*pulumirpc.PackageDependency, error) {
		return nil, errors.New("registry does not have a provider named hashicorp/nowhere")
	})

	installedDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(installedDir, "main.tf"), []byte(program), 0o600))
	writeInstalledSDK(t, installedDir, "nowhere", exampleDescriptor())

	got, diags, err := testPackageDescriptors(t, &hclConverter{projectDir: installedDir}, target)
	require.NoError(t, err, "an unresolvable provider must not fail the conversion")
	assert.Empty(t, diags, "the installed descriptor covers it, so there is nothing to report")
	assert.Contains(t, got, "nowhere")

	bareDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(bareDir, "main.tf"), []byte(program), 0o600))

	got, diags, err = testPackageDescriptors(t, &hclConverter{projectDir: bareDir}, target)
	require.NoError(t, err)
	assert.Empty(t, got)
	require.Len(t, diags, 1, "nothing to import under, so the degradation is reported")
	assert.Equal(t, hcl.DiagWarning, diags[0].Severity)
}

// TestPackageDescriptorsKeyByPackageName pins the key space: a provider whose
// module-local name differs from its source's type is still found under the
// package name, which is how the state file names it.
func TestPackageDescriptorsKeyByPackageName(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "main.tf"), []byte(`
		terraform {
			required_providers {
				randy = { source = "hashicorp/random", version = "6.0.0" }
			}
		}
		resource "randy_uuid" "a" {}
	`), 0o600))

	got, _, err := testPackageDescriptors(t, &hclConverter{projectDir: dir}, serveResolver(t))
	require.NoError(t, err)

	desc, ok := got["random"]
	require.True(t, ok, "keyed by package name, not by the local name %q", "randy")
	require.NotNil(t, desc.Parameterization)
	assert.Equal(t, "random", desc.Parameterization.Name)
}

// TestProgramDirFromWorkingDirectory pins what the plugin binary does:
// ConvertState carries no source directory, so New() must find the project
// from the process's working directory.
//
//nolint:paralleltest // t.Chdir cannot be used in a parallel test.
func TestProgramDirFromWorkingDirectory(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "Pulumi.yaml"), "name: import-test\nruntime: hcl\n")
	writeFile(t, filepath.Join(dir, "main.tf"), `
		terraform {
			required_providers {
				example = { source = "acme/example", version = "6.0.0" }
			}
		}
	`)

	t.Chdir(dir)
	got, _, err := testPackageDescriptors(t, &hclConverter{}, serveResolver(t))
	require.NoError(t, err)

	desc, ok := got["example"]
	require.True(t, ok, "the project resolves from the working directory")
	assert.Equal(t, "terraform-provider", desc.Name)
}

// testPackageDescriptors mirrors ConvertState's prelude: locate the program,
// parse it, and hand packageDescriptors the shared loader.
func testPackageDescriptors(
	t *testing.T, c *hclConverter, resolverTarget string,
) (map[string]workspace.PackageDescriptor, hcl.Diagnostics, error) {
	t.Helper()
	dir, err := c.programDir()
	require.NoError(t, err)
	config, diags := parser.NewParser().ParseDirectory(dir)
	require.False(t, diags.HasErrors(), diags)
	loader := modules.NewLoader(modules.LiveResolver(t.Context()))
	return packageDescriptors(t.Context(), resolverTarget, dir, config, loader)
}

// serveResolver runs fakeResolver on a local port and returns its target,
// optionally with respond standing in for the echo.
func serveResolver(
	t *testing.T, respond ...func(*pulumirpc.PackageSpec) (*pulumirpc.PackageDependency, error),
) string {
	t.Helper()
	resolver := fakeResolver{t: t}
	if len(respond) > 0 {
		resolver.respond = respond[0]
	}
	cancel := make(chan bool)
	handle, err := rpcutil.ServeWithOptions(rpcutil.ServeOptions{
		Cancel: cancel,
		Init: func(srv *grpc.Server) error {
			pulumirpc.RegisterPackageResolverServer(srv, resolver)
			return nil
		},
	})
	require.NoError(t, err)
	t.Cleanup(func() { close(cancel); <-handle.Done })
	return fmt.Sprintf("127.0.0.1:%d", handle.Port)
}

// writeInstalledSDK writes the sdks/<name>/hcl.sdk.json that `pulumi install`
// leaves behind for a dynamically bridged provider.
func writeInstalledSDK(t *testing.T, dir, name string, desc workspace.PackageDescriptor) {
	t.Helper()
	data, err := json.Marshal(desc)
	require.NoError(t, err)
	sdkDir := filepath.Join(dir, "sdks", name)
	require.NoError(t, os.MkdirAll(sdkDir, 0o755))
	writeFile(t, filepath.Join(sdkDir, "hcl.sdk.json"), string(data))
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
}

// ecosystemAssertingMapper is a convert.Mapper with a fixed mapping for
// "random" that asserts the converter's wire contract: mappings are requested
// for the terraform ecosystem, hinted at the parameterized base plugin.
type ecosystemAssertingMapper struct{ t *testing.T }

func (m ecosystemAssertingMapper) GetMapping(
	_ context.Context, provider string, hint *convert.MapperPackageHint, ecosystem string,
) ([]byte, error) {
	assert.Equal(m.t, "terraform", ecosystem, "the converter must request terraform mappings")
	if provider != "random" {
		return nil, nil
	}
	if assert.NotNil(m.t, hint, "the descriptor must hint the mapper") {
		assert.Equal(m.t, "terraform-provider", hint.PluginName)
		assert.NotNil(m.t, hint.Parameterization)
	}
	return json.Marshal(tfbridge.MarshalProviderInfo(&tfbridge.ProviderInfo{
		P: shimv2.NewProvider(&sdkschema.Provider{
			ResourcesMap: map[string]*sdkschema.Resource{
				"random_uuid": {Schema: map[string]*sdkschema.Schema{
					"result": {Type: sdkschema.TypeString, Computed: true},
				}},
			},
		}),
		Name: "random",
		Resources: map[string]*tfbridge.ResourceInfo{
			"random_uuid": {Tok: "random:index/randomUuid:RandomUuid"},
		},
	}))
}

// TestConvertStateViaMapper drives the full converter entry point — gRPC mapper dialing
// and state-file parsing — against a real in-process mapper server.
func TestConvertStateViaMapper(t *testing.T) {
	t.Parallel()

	cancel := make(chan bool)
	handle, err := rpcutil.ServeWithOptions(rpcutil.ServeOptions{
		Cancel: cancel,
		Init: func(srv *grpc.Server) error {
			convert.MapperRegistration(convert.NewMapperServer(ecosystemAssertingMapper{t}))(srv)
			loader := schemaloader.New(t, schema.PackageSpec{
				Name:    "random",
				Version: "6.0.0",
				Meta:    &schema.MetadataSpec{ModuleFormat: bridgeModuleFormat},
				Resources: map[string]schema.ResourceSpec{
					"random:index/randomUuid:RandomUuid": {ObjectTypeSpec: schema.ObjectTypeSpec{
						Properties: map[string]schema.PropertySpec{
							"result": {TypeSpec: schema.TypeSpec{Type: "string"}},
						},
					}},
				},
			})
			schema.LoaderRegistration(schema.NewLoaderServer(loader))(srv)
			pulumirpc.RegisterPackageResolverServer(srv, fakeResolver{t: t})
			return nil
		},
	})
	require.NoError(t, err)
	t.Cleanup(func() { close(cancel); <-handle.Done })
	target := fmt.Sprintf("127.0.0.1:%d", handle.Port)

	dir := t.TempDir()
	statePath := filepath.Join(dir, "terraform.tfstate")
	require.NoError(t, os.WriteFile(statePath, stateV4(t, `{
		"resources": [
			{
				"mode": "managed", "type": "random_uuid", "name": "example",
				"provider": "provider[\"registry.opentofu.org/hashicorp/random\"]",
				"instances": [ { "attributes": { "id": "aabbccdd-0011-2233-4455-66778899aabb" } } ]
			}
		]
	}`), 0o600))

	require.NoError(t, os.WriteFile(filepath.Join(dir, "main.tf"), []byte(`
		terraform {
			required_providers {
				random = { source = "hashicorp/random", version = "6.0.0" }
			}
		}
	`), 0o600))

	// The project's sdks/ descriptor marks "random" as dynamically bridged.
	desc := exampleDescriptor()
	desc.Parameterization.Name = "random"
	writeInstalledSDK(t, dir, "random", desc)

	resp, err := NewInDir(dir).ConvertState(t.Context(), &plugin.ConvertStateRequest{
		MapperTarget:   target,
		LoaderTarget:   target,
		ResolverTarget: target,
		Args:           []string{statePath},
	})
	require.NoError(t, err)
	require.Empty(t, resp.Diagnostics)
	require.Len(t, resp.Resources, 1)
	got := resp.Resources[0]
	assert.Equal(t, "random:index/randomUuid:RandomUuid", got.Type)
	assert.Equal(t, "example", got.Name)
	assert.Equal(t, "aabbccdd-0011-2233-4455-66778899aabb", got.ID)
	assert.Equal(t, "6.0.0", got.Version)
	require.NotNil(t, got.Parameterization)
	assert.Equal(t, "terraform-provider", got.Parameterization.PluginName)

	// State-file error paths share the same entry point.
	_, err = NewInDir(dir).ConvertState(t.Context(), &plugin.ConvertStateRequest{
		MapperTarget:   target,
		LoaderTarget:   target,
		ResolverTarget: target,
		Args:           []string{filepath.Join(dir, "does-not-exist.tfstate")},
	})
	assert.ErrorContains(t, err, "reading state file")

	badPath := filepath.Join(dir, "bad.tfstate")
	require.NoError(t, os.WriteFile(badPath, []byte("not json"), 0o600))
	_, err = NewInDir(dir).ConvertState(t.Context(), &plugin.ConvertStateRequest{
		MapperTarget:   target,
		LoaderTarget:   target,
		ResolverTarget: target,
		Args:           []string{badPath},
	})
	assert.ErrorContains(t, err, "parsing state file")
}

func mustModuleSources(t *testing.T, dir string) map[modulepath.Path]string {
	t.Helper()
	config, diags := parser.NewParser().ParseDirectory(dir)
	require.False(t, diags.HasErrors(), diags)
	return moduleSources(t.Context(), modules.NewLoader(modules.LiveResolver(t.Context())), config, dir)
}

// TestModuleSourcesDottedLabels covers a dot in a block label, which a dotted
// call-path string cannot tell apart from nesting.
func TestModuleSourcesDottedLabels(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	for _, sub := range []string{"dotted", "a", "nested"} {
		require.NoError(t, os.MkdirAll(filepath.Join(dir, sub), 0o755))
	}
	require.NoError(t, os.WriteFile(filepath.Join(dir, "main.tf"), []byte(`
		module "a.b" { source = "./dotted" }
		module "a"   { source = "./a" }
	`), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a", "main.tf"), []byte(`
		module "b" { source = "../nested" }
	`), 0o600))

	root := modulepath.Root()
	assert.Equal(t, map[modulepath.Path]string{
		root.Append(modulepath.NewStep("a.b")):                               "./dotted",
		root.Append(modulepath.NewStep("a")):                                 "./a",
		root.Append(modulepath.NewStep("a")).Append(modulepath.NewStep("b")): "../nested",
	}, mustModuleSources(t, dir))
}
