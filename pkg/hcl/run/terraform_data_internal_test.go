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

package run

import (
	"testing"

	"github.com/pulumi/pulumi/sdk/v3/go/common/resource/archive"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource/asset"
	"github.com/pulumi/pulumi/sdk/v3/go/property"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zclconf/go-cty/cty"

	"github.com/pulumi/pulumi-hcl/pkg/hcl/eval"
	"github.com/pulumi/pulumi-hcl/pkg/hcl/packages"
)

func TestLowerTerraformDataInputs(t *testing.T) {
	t.Parallel()

	null := property.New(property.Null)

	tests := []struct {
		name        string
		inputs      property.Map
		opts        ResourceOptions
		wantInputs  property.Map
		wantTrigger property.Value
	}{
		{
			name: "triggers_replace present",
			inputs: property.NewMap(map[string]property.Value{
				"input":            property.New("a"),
				"triggers_replace": property.New("x"),
			}),
			wantInputs: property.NewMap(map[string]property.Value{
				"input": property.New("a"),
			}),
			wantTrigger: property.New([]property.Value{property.New("x"), null}),
		},
		{
			name: "triggers_replace absent still registers a non-null trigger",
			inputs: property.NewMap(map[string]property.Value{
				"input": property.New("a"),
			}),
			wantInputs: property.NewMap(map[string]property.Value{
				"input": property.New("a"),
			}),
			wantTrigger: property.New([]property.Value{null, null}),
		},
		{
			name:   "omitted input is materialized as null",
			inputs: property.Map{},
			wantInputs: property.NewMap(map[string]property.Value{
				"input": null,
			}),
			wantTrigger: property.New([]property.Value{null, null}),
		},
		{
			name: "replace_triggered_by trigger is preserved alongside triggers_replace",
			inputs: property.NewMap(map[string]property.Value{
				"input":            property.New("a"),
				"triggers_replace": property.New("x"),
			}),
			opts: ResourceOptions{ReplacementTrigger: property.New("lifecycle")},
			wantInputs: property.NewMap(map[string]property.Value{
				"input": property.New("a"),
			}),
			wantTrigger: property.New([]property.Value{
				property.New("x"), property.New("lifecycle"),
			}),
		},
		{
			name: "ignored triggers_replace holds a null trigger slot",
			inputs: property.NewMap(map[string]property.Value{
				"input":            property.New("a"),
				"triggers_replace": property.New("x"),
			}),
			opts: ResourceOptions{IgnoreChanges: []property.Glob{
				property.GlobFromSegments(property.NewSegment("triggers_replace")),
			}},
			wantInputs: property.NewMap(map[string]property.Value{
				"input": property.New("a"),
			}),
			wantTrigger: property.New([]property.Value{null, null}),
		},
		{
			name: "ignore_changes = all nulls the trigger slot too",
			inputs: property.NewMap(map[string]property.Value{
				"input":            property.New("a"),
				"triggers_replace": property.New("x"),
			}),
			opts: ResourceOptions{IgnoreChanges: []property.Glob{
				property.GlobFromSegments(property.Splat),
			}},
			wantInputs: property.NewMap(map[string]property.Value{
				"input": property.New("a"),
			}),
			wantTrigger: property.New([]property.Value{null, null}),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := lowerTerraformDataInputs(packages.TerraformDataType, tt.inputs, &tt.opts)
			assert.Equal(t, tt.wantInputs, got)
			assert.Equal(t, tt.wantTrigger, tt.opts.ReplacementTrigger)
		})
	}

	t.Run("other types are untouched", func(t *testing.T) {
		t.Parallel()

		inputs := property.NewMap(map[string]property.Value{
			"triggers_replace": property.New("x"),
		})
		opts := ResourceOptions{}
		got := lowerTerraformDataInputs("random_pet", inputs, &opts)
		assert.Equal(t, inputs, got)
		assert.Equal(t, ResourceOptions{}, opts)
	})

	t.Run("input ignore_changes collapse to the whole attribute; triggers_replace ones drop", func(t *testing.T) {
		t.Parallel()

		glob := func(s string) property.Glob {
			var g property.Glob
			require.NoError(t, g.UnmarshalText([]byte(s)))
			return g
		}
		opts := ResourceOptions{IgnoreChanges: []property.Glob{
			glob("input.k"), glob("input[0]"), glob("input"),
			glob("triggers_replace"), glob("triggers_replace.k"),
			property.GlobFromSegments(property.Splat), glob("other"),
		}}
		lowerTerraformDataInputs(packages.TerraformDataType, property.Map{}, &opts)
		assert.Equal(t, []property.Glob{
			glob("input"), glob("input"), glob("input"),
			property.GlobFromSegments(property.Splat), glob("other"),
		}, opts.IgnoreChanges)
	})
}

func TestLowerTerraformDataOutputs(t *testing.T) {
	t.Parallel()

	null := property.New(property.Null)
	inputs := property.NewMap(map[string]property.Value{
		"input":            property.New("a"),
		"triggers_replace": property.New("x"),
	})
	opts := ResourceOptions{}
	lowerTerraformDataInputs(packages.TerraformDataType, inputs, &opts)

	outputs := property.NewMap(map[string]property.Value{
		"input": property.New("a"),
	})
	got := lowerTerraformDataOutputs(packages.TerraformDataType, outputs, &opts)
	assert.Equal(t, property.NewMap(map[string]property.Value{
		"input":            property.New("a"),
		"output":           property.New("a"),
		"triggers_replace": property.New("x"),
	}), got)

	t.Run("absent triggers_replace echoes null", func(t *testing.T) {
		t.Parallel()

		opts := ResourceOptions{}
		lowerTerraformDataInputs(packages.TerraformDataType, property.Map{}, &opts)
		got := lowerTerraformDataOutputs(packages.TerraformDataType, property.Map{}, &opts)
		assert.Equal(t, property.NewMap(map[string]property.Value{
			"output":           null,
			"triggers_replace": null,
		}), got)
	})
}

func TestWrapTerraformDataInputs(t *testing.T) {
	t.Parallel()

	strSet := cty.SetVal([]cty.Value{cty.StringVal("a"), cty.StringVal("b")})
	arr := property.New([]property.Value{property.New("a"), property.New("b")})
	wrapped := property.New(map[string]property.Value{
		"type":  property.New(`["set","string"]`),
		"value": arr,
	})

	tests := []struct {
		name      string
		resType   string
		inputs    property.Map
		evaluated map[string]cty.Value
		want      property.Map
	}{
		{
			name:    "input and triggers_replace are wrapped",
			resType: packages.TerraformDataType,
			inputs: property.NewMap(map[string]property.Value{
				"input":            arr,
				"triggers_replace": arr,
			}),
			evaluated: map[string]cty.Value{"input": strSet, "triggers_replace": strSet},
			want: property.NewMap(map[string]property.Value{
				"input":            wrapped,
				"triggers_replace": wrapped,
			}),
		},
		{
			name:      "unevaluated inputs are untouched",
			resType:   packages.TerraformDataType,
			inputs:    property.NewMap(map[string]property.Value{"input": arr}),
			evaluated: map[string]cty.Value{},
			want:      property.NewMap(map[string]property.Value{"input": arr}),
		},
		{
			name:      "other types are untouched",
			resType:   "random_pet",
			inputs:    property.NewMap(map[string]property.Value{"input": arr}),
			evaluated: map[string]cty.Value{"input": strSet},
			want:      property.NewMap(map[string]property.Value{"input": arr}),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := wrapTerraformDataInputs(tt.resType, tt.inputs, tt.evaluated)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestWrapTerraformDataInputsAssets(t *testing.T) {
	t.Parallel()

	textAsset, err := asset.FromText("hello")
	require.NoError(t, err)
	assetsArchive, err := archive.FromAssets(map[string]any{"a": textAsset})
	require.NoError(t, err)
	assetVal := cty.CapsuleVal(eval.AssetCapsuleType, textAsset)

	tests := []struct {
		name  string
		attr  string
		value cty.Value
		prop  property.Value
	}{
		{"asset in input", "input", assetVal, property.New(textAsset)},
		{"asset in triggers_replace", "triggers_replace", assetVal, property.New(textAsset)},
		{"archive", "input", cty.CapsuleVal(eval.ArchiveCapsuleType, assetsArchive), property.New(assetsArchive)},
		{"nested asset", "input", cty.ListVal([]cty.Value{assetVal}), property.New([]property.Value{property.New(textAsset)})},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			inputs := property.NewMap(map[string]property.Value{tt.attr: tt.prop})
			got := wrapTerraformDataInputs(packages.TerraformDataType, inputs, map[string]cty.Value{tt.attr: tt.value})

			want := property.NewMap(map[string]property.Value{tt.attr: property.New(map[string]property.Value{
				"type":  property.New(`"dynamic"`),
				"value": tt.prop,
			})})
			assert.Equal(t, want, got)
		})
	}
}

func TestUnwrapTerraformDataOutputs(t *testing.T) {
	t.Parallel()

	strSet := cty.SetVal([]cty.Value{cty.StringVal("a"), cty.StringVal("b")})
	arr := property.New([]property.Value{property.New("a"), property.New("b")})
	wrap := func(typeJSON string, v property.Value) property.Value {
		return property.New(map[string]property.Value{
			"type":  property.New(typeJSON),
			"value": v,
		})
	}
	// The generically re-expanded stand-in the unwrap must replace; its exact
	// shape is irrelevant to the property-level wrapper detection.
	reexpanded := cty.ObjectVal(map[string]cty.Value{"any": cty.True})

	tests := []struct {
		name    string
		resType string
		outputs map[string]cty.Value
		props   property.Map
		want    map[string]cty.Value
		wantErr string
	}{
		{
			name:    "wrappers unbox on input, output, and triggers_replace",
			resType: packages.TerraformDataType,
			outputs: map[string]cty.Value{
				"input":            reexpanded,
				"output":           reexpanded,
				"triggers_replace": reexpanded,
			},
			props: property.NewMap(map[string]property.Value{
				"input":            wrap(`["set","string"]`, arr),
				"output":           wrap(`["set","string"]`, arr),
				"triggers_replace": wrap(`["set","bool"]`, property.New([]property.Value{property.New(true)})),
			}),
			want: map[string]cty.Value{
				"input":            strSet,
				"output":           strSet,
				"triggers_replace": cty.SetVal([]cty.Value{cty.True}),
			},
		},
		{
			name:    "wrapped scalar unboxes (its re-expanded shape is a cty map)",
			resType: packages.TerraformDataType,
			outputs: map[string]cty.Value{
				"output": cty.MapVal(map[string]cty.Value{
					"type":  cty.StringVal(`"string"`),
					"value": cty.StringVal("hello"),
				}),
			},
			props: property.NewMap(map[string]property.Value{
				"output": wrap(`"string"`, property.New("hello")),
			}),
			want: map[string]cty.Value{
				"output": cty.StringVal("hello"),
			},
		},
		{
			name:    "unknown wrapped value takes the recorded type",
			resType: packages.TerraformDataType,
			outputs: map[string]cty.Value{"output": cty.UnknownVal(cty.DynamicPseudoType)},
			props: property.NewMap(map[string]property.Value{
				"output": wrap(`["set","string"]`, property.New(property.Computed)),
			}),
			want: map[string]cty.Value{
				"output": cty.UnknownVal(cty.Set(cty.String)),
			},
		},
		{
			name:    "null and unknown placeholders re-expand without help",
			resType: packages.TerraformDataType,
			outputs: map[string]cty.Value{
				"input":            cty.NullVal(cty.DynamicPseudoType),
				"output":           cty.NullVal(cty.DynamicPseudoType),
				"triggers_replace": cty.UnknownVal(cty.DynamicPseudoType),
			},
			props: property.NewMap(map[string]property.Value{
				"input":            property.New(property.Null),
				"output":           property.New(property.Null),
				"triggers_replace": property.New(property.Computed),
			}),
			want: map[string]cty.Value{
				"input":            cty.NullVal(cty.DynamicPseudoType),
				"output":           cty.NullVal(cty.DynamicPseudoType),
				"triggers_replace": cty.UnknownVal(cty.DynamicPseudoType),
			},
		},
		{
			name:    "unconvertible value is kept at its own type",
			resType: packages.TerraformDataType,
			outputs: map[string]cty.Value{"output": reexpanded},
			props: property.NewMap(map[string]property.Value{
				"output": wrap(`["set","string"]`, property.New(true)),
			}),
			want: map[string]cty.Value{
				"output": cty.True,
			},
		},
		{
			name:    "wrapper secrecy and element secrets mark the result",
			resType: packages.TerraformDataType,
			outputs: map[string]cty.Value{
				"input":  reexpanded,
				"output": reexpanded,
			},
			props: property.NewMap(map[string]property.Value{
				"input": wrap(`["set","string"]`, arr).WithSecret(true),
				"output": wrap(`["set","string"]`, property.New([]property.Value{
					property.New("a").WithSecret(true), property.New("b"),
				})),
			}),
			want: map[string]cty.Value{
				"input":  strSet.Mark(eval.SensitiveMark),
				"output": strSet.Mark(eval.SensitiveMark),
			},
		},
		{
			name:    "unparsable recorded type is rejected",
			resType: packages.TerraformDataType,
			outputs: map[string]cty.Value{"output": reexpanded},
			props: property.NewMap(map[string]property.Value{
				"output": wrap(`not a type`, arr),
			}),
			wantErr: `output: invalid recorded cty type "not a type": invalid character 'o' in literal null (expecting 'u')`,
		},
		{
			name:    "malformed wrapper is rejected",
			resType: packages.TerraformDataType,
			outputs: map[string]cty.Value{"input": reexpanded},
			props: property.NewMap(map[string]property.Value{
				"input": property.New(map[string]property.Value{
					"type": property.New(`"string"`),
				}),
			}),
			wantErr: "input: malformed {type, value} wrapper",
		},
		{
			name:    "other types are untouched",
			resType: "random_pet",
			outputs: map[string]cty.Value{"output": reexpanded},
			props: property.NewMap(map[string]property.Value{
				"output": wrap(`["set","string"]`, arr),
			}),
			want: map[string]cty.Value{"output": reexpanded},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := unwrapTerraformDataOutputs(tt.resType, tt.outputs, tt.props)
			if tt.wantErr != "" {
				assert.EqualError(t, err, tt.wantErr)
				return
			}
			assert.NoError(t, err)
			assert.Equal(t, tt.want, tt.outputs)
		})
	}
}
