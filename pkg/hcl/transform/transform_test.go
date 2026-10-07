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

package transform

import (
	"testing"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/pulumi/pulumi-hcl/pkg/hcl/bridge"
	"github.com/pulumi/pulumi-hcl/pkg/hcl/eval"
	"github.com/pulumi/pulumi/pkg/v3/codegen/schema"
	"github.com/pulumi/pulumi/pkg/v3/codegen/testing/utils/rapidresource"
	"github.com/pulumi/pulumi/pkg/v3/codegen/testing/utils/rapidschema"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource"
	"github.com/pulumi/pulumi/sdk/v3/go/property"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zclconf/go-cty/cty"
	"pgregory.net/rapid"
)

func TestSnakeCaseFromCamelCase(t *testing.T) {
	t.Parallel()

	tests := []struct {
		input, expected string
	}{
		{"foo", "foo"},
		{"fooBar", "foo_bar"},
		{"FOO", "foo"},
		{"ec2", "ec2"},
		{"EC2", "ec2"},
		{"fooBARBuzz", "foo_bar_buzz"},
		{"e2e", "e2e"},
	}

	for _, tt := range tests {
		t.Run("", func(t *testing.T) {
			t.Parallel()
			assert.Equalf(t, tt.expected, snakeCaseFromCamelCase(tt.input),
				"snakeCaseFromCamelCase(%q)", tt.input)
		})
	}
}

func TestCtyToResourceInputs(t *testing.T) {
	t.Setenv("TEST_ENV_PORT", "9000")
	t.Setenv("TEST_ENV_ENABLED", "true")

	tests := []struct {
		name       string
		properties []*schema.Property
		input      cty.Value
		expected   property.Map
	}{
		{
			name: "simple string property",
			properties: []*schema.Property{
				{
					Name: "name",
					Type: schema.StringType,
				},
			},
			input: cty.ObjectVal(map[string]cty.Value{
				"name": cty.StringVal("test-resource"),
			}),
			expected: property.NewMap(map[string]property.Value{
				"name": property.New("test-resource"),
			}),
		},
		{
			name: "boolean and number primitives",
			properties: []*schema.Property{
				{Name: "enabled", Type: schema.BoolType},
				{Name: "count", Type: schema.NumberType},
			},
			input: cty.ObjectVal(map[string]cty.Value{
				"enabled": cty.BoolVal(true),
				"count":   cty.NumberIntVal(42),
			}),
			expected: property.NewMap(map[string]property.Value{
				"enabled": property.New(true),
				"count":   property.New(float64(42)),
			}),
		},
		{
			name: "object with name translation from snake_case",
			properties: []*schema.Property{
				{
					Name: "containerPort",
					Type: &schema.ObjectType{
						Properties: []*schema.Property{
							{Name: "portNumber", Type: schema.NumberType},
							{Name: "protocol", Type: schema.StringType},
						},
					},
				},
			},
			input: cty.ObjectVal(map[string]cty.Value{
				"container_port": cty.ObjectVal(map[string]cty.Value{
					"port_number": cty.NumberIntVal(8080),
					"protocol":    cty.StringVal("TCP"),
				}),
			}),
			expected: property.NewMap(map[string]property.Value{
				"containerPort": property.New(property.NewMap(map[string]property.Value{
					"portNumber": property.New(float64(8080)),
					"protocol":   property.New("TCP"),
				})),
			}),
		},
		{
			name: "map without name translation",
			properties: []*schema.Property{
				{
					Name: "tags",
					Type: &schema.MapType{ElementType: schema.StringType},
				},
			},
			input: cty.ObjectVal(map[string]cty.Value{
				"tags": cty.MapVal(map[string]cty.Value{
					"snake_case_key": cty.StringVal("value1"),
					"another_key":    cty.StringVal("value2"),
				}),
			}),
			expected: property.NewMap(map[string]property.Value{
				"tags": property.New(property.NewMap(map[string]property.Value{
					"snake_case_key": property.New("value1"),
					"another_key":    property.New("value2"),
				})),
			}),
		},
		{
			name: "array of primitives",
			properties: []*schema.Property{
				{
					Name: "ports",
					Type: &schema.ArrayType{ElementType: schema.NumberType},
				},
			},
			input: cty.ObjectVal(map[string]cty.Value{
				"ports": cty.ListVal([]cty.Value{
					cty.NumberIntVal(80),
					cty.NumberIntVal(443),
					cty.NumberIntVal(8080),
				}),
			}),
			expected: property.NewMap(map[string]property.Value{
				"ports": property.New(property.NewArray([]property.Value{
					property.New(float64(80)),
					property.New(float64(443)),
					property.New(float64(8080)),
				})),
			}),
		},
		{
			name: "array of objects with name translation",
			properties: []*schema.Property{
				{
					Name: "endpoints",
					Type: &schema.ArrayType{
						ElementType: &schema.ObjectType{
							Properties: []*schema.Property{
								{Name: "hostName", Type: schema.StringType},
								{Name: "portNumber", Type: schema.NumberType},
							},
						},
					},
				},
			},
			input: cty.ObjectVal(map[string]cty.Value{
				"endpoints": cty.ListVal([]cty.Value{
					cty.ObjectVal(map[string]cty.Value{
						"host_name":   cty.StringVal("api.example.com"),
						"port_number": cty.NumberIntVal(443),
					}),
					cty.ObjectVal(map[string]cty.Value{
						"host_name":   cty.StringVal("db.example.com"),
						"port_number": cty.NumberIntVal(5432),
					}),
				}),
			}),
			expected: property.NewMap(map[string]property.Value{
				"endpoints": property.New(property.NewArray([]property.Value{
					property.New(property.NewMap(map[string]property.Value{
						"hostName":   property.New("api.example.com"),
						"portNumber": property.New(float64(443)),
					})),
					property.New(property.NewMap(map[string]property.Value{
						"hostName":   property.New("db.example.com"),
						"portNumber": property.New(float64(5432)),
					})),
				})),
			}),
		},
		{
			name: "static default value float64",
			properties: []*schema.Property{
				{Name: "name", Type: schema.StringType},
				{
					Name: "port",
					Type: schema.NumberType,
					DefaultValue: &schema.DefaultValue{
						Value: 8080.0,
					},
				},
			},
			input: cty.ObjectVal(map[string]cty.Value{
				"name": cty.StringVal("my-service"),
			}),
			expected: property.NewMap(map[string]property.Value{
				"name": property.New("my-service"),
				"port": property.New(float64(8080)),
			}),
		},
		{
			name: "static default value int",
			properties: []*schema.Property{
				{Name: "name", Type: schema.StringType},
				{
					Name: "maxConnections",
					Type: schema.NumberType,
					DefaultValue: &schema.DefaultValue{
						Value: 100,
					},
				},
			},
			input: cty.ObjectVal(map[string]cty.Value{
				"name": cty.StringVal("my-service"),
			}),
			expected: property.NewMap(map[string]property.Value{
				"name":           property.New("my-service"),
				"maxConnections": property.New(float64(100)),
			}),
		},
		{
			name: "const values fill omitted properties",
			properties: []*schema.Property{
				{Name: "kind", Type: schema.StringType, ConstValue: "Constant"},
				{Name: "flag", Type: schema.BoolType, ConstValue: true},
				{Name: "count", Type: schema.IntType, ConstValue: int32(3)},
				{Name: "ratio", Type: schema.NumberType, ConstValue: 1.5},
			},
			input: cty.ObjectVal(map[string]cty.Value{}),
			expected: property.NewMap(map[string]property.Value{
				"kind":  property.New("Constant"),
				"flag":  property.New(true),
				"count": property.New(float64(3)),
				"ratio": property.New(1.5),
			}),
		},
		{
			name: "static default value string",
			properties: []*schema.Property{
				{
					Name: "region",
					Type: schema.StringType,
					DefaultValue: &schema.DefaultValue{
						Value: "us-west-2",
					},
				},
			},
			input: cty.ObjectVal(map[string]cty.Value{}),
			expected: property.NewMap(map[string]property.Value{
				"region": property.New("us-west-2"),
			}),
		},
		{
			name: "static default value boolean",
			properties: []*schema.Property{
				{
					Name: "autoScale",
					Type: schema.BoolType,
					DefaultValue: &schema.DefaultValue{
						Value: true,
					},
				},
			},
			input: cty.ObjectVal(map[string]cty.Value{}),
			expected: property.NewMap(map[string]property.Value{
				"autoScale": property.New(true),
			}),
		},
		{
			name: "environment variable default overrides static default",
			properties: []*schema.Property{
				{Name: "name", Type: schema.StringType},
				{
					Name: "port",
					Type: schema.NumberType,
					DefaultValue: &schema.DefaultValue{
						Environment: []string{"TEST_ENV_PORT"},
						Value:       8080,
					},
				},
			},
			input: cty.ObjectVal(map[string]cty.Value{
				"name": cty.StringVal("my-service"),
			}),
			expected: property.NewMap(map[string]property.Value{
				"name": property.New("my-service"),
				"port": property.New(float64(9000)),
			}),
		},
		{
			name: "environment variable default for boolean",
			properties: []*schema.Property{
				{
					Name: "enabled",
					Type: schema.BoolType,
					DefaultValue: &schema.DefaultValue{
						Environment: []string{"TEST_ENV_ENABLED"},
					},
				},
			},
			input: cty.ObjectVal(map[string]cty.Value{}),
			expected: property.NewMap(map[string]property.Value{
				"enabled": property.New(true),
			}),
		},
		{
			name: "secret property",
			properties: []*schema.Property{
				{Name: "password", Type: schema.StringType, Secret: true},
			},
			input: cty.ObjectVal(map[string]cty.Value{
				"password": cty.StringVal("super-secret"),
			}),
			expected: property.NewMap(map[string]property.Value{
				"password": property.New("super-secret").WithSecret(true),
			}),
		},
		{
			name: "missing property without default not in output",
			properties: []*schema.Property{
				{Name: "name", Type: schema.StringType},
				{Name: "optionalValue", Type: schema.StringType},
			},
			input: cty.ObjectVal(map[string]cty.Value{
				"name": cty.StringVal("test"),
			}),
			expected: property.NewMap(map[string]property.Value{
				"name": property.New("test"),
			}),
		},
		{
			name: "deeply nested objects with name translation",
			properties: []*schema.Property{
				{
					Name: "metadata",
					Type: &schema.ObjectType{
						Properties: []*schema.Property{
							{Name: "resourceName", Type: schema.StringType},
							{
								Name: "nestedConfig",
								Type: &schema.ObjectType{
									Properties: []*schema.Property{
										{Name: "maxRetries", Type: schema.NumberType},
										{Name: "timeoutSeconds", Type: schema.NumberType},
									},
								},
							},
						},
					},
				},
			},
			input: cty.ObjectVal(map[string]cty.Value{
				"metadata": cty.ObjectVal(map[string]cty.Value{
					"resource_name": cty.StringVal("my-resource"),
					"nested_config": cty.ObjectVal(map[string]cty.Value{
						"max_retries":     cty.NumberIntVal(3),
						"timeout_seconds": cty.NumberIntVal(30),
					}),
				}),
			}),
			expected: property.NewMap(map[string]property.Value{
				"metadata": property.New(property.NewMap(map[string]property.Value{
					"resourceName": property.New("my-resource"),
					"nestedConfig": property.New(property.NewMap(map[string]property.Value{
						"maxRetries":     property.New(float64(3)),
						"timeoutSeconds": property.New(float64(30)),
					})),
				})),
			}),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			result, err := ctyToResourceInputs(tt.input, &schema.Resource{
				Token:           "pkg:mod:Name",
				InputProperties: tt.properties,
			}, nil, nil)
			require.NoError(t, err)
			assert.Equal(t, tt.expected, result)
		})
	}
}

// A marked for_each container (e.g. carrying a DepMark from an unknown
// attribute) must not panic ElementIterator.
func TestEvalDynamicBlocks_MarkedForEach(t *testing.T) {
	t.Parallel()

	src := []byte(`
resource "fake" "f" {
  dynamic "settings" {
    for_each = local.vpcs
    content {
      name = settings.value.name
    }
  }
}`)
	file, diags := hclsyntax.ParseConfig(src, "test.hcl", hcl.Pos{Line: 1, Column: 1})
	require.False(t, diags.HasErrors(), diags.Error())

	body := file.Body.(*hclsyntax.Body)
	var resourceBody hcl.Body
	for _, b := range body.Blocks {
		if b.Type == "resource" {
			resourceBody = b.Body
			break
		}
	}
	require.NotNil(t, resourceBody)

	r := &schema.Resource{
		Token: "fake:index:F",
		InputProperties: []*schema.Property{{
			Name: "settings",
			Type: &schema.ArrayType{ElementType: &schema.ObjectType{
				Properties: []*schema.Property{{Name: "name", Type: schema.StringType}},
			}},
		}},
	}

	markedForEach := cty.MapVal(map[string]cty.Value{
		"primary": cty.ObjectVal(map[string]cty.Value{
			"name": cty.StringVal("p"),
		}),
	}).WithMarks(cty.NewValueMarks(eval.DepMark("urn:pulumi:dev::p::aws:ec2/vpc:Vpc::test")))

	evalFn := func(_ resource.PropertyKey, expr hcl.Expression, extraVars map[string]cty.Value) (cty.Value, hcl.Diagnostics) {
		if extraVars == nil {
			return markedForEach, nil
		}
		return expr.Value(&hcl.EvalContext{Variables: extraVars})
	}

	_, _, diags = EvalResourceWithSchema(resourceBody, r, nil, evalFn)
	require.False(t, diags.HasErrors(), "unexpected diags: %v", diags)
}

// A sensitive for_each container must propagate the mark to each.key /
// each.value so attributes derived from them become Pulumi secrets.
func TestEvalDynamicBlocks_SensitiveForEach(t *testing.T) {
	t.Parallel()

	src := []byte(`
resource "fake" "f" {
  dynamic "settings" {
    for_each = local.vpcs
    content {
      name = settings.value.name
    }
  }
}`)
	file, diags := hclsyntax.ParseConfig(src, "test.hcl", hcl.Pos{Line: 1, Column: 1})
	require.False(t, diags.HasErrors(), diags.Error())

	body := file.Body.(*hclsyntax.Body)
	var resourceBody hcl.Body
	for _, b := range body.Blocks {
		if b.Type == "resource" {
			resourceBody = b.Body
			break
		}
	}
	require.NotNil(t, resourceBody)

	r := &schema.Resource{
		Token: "fake:index:F",
		InputProperties: []*schema.Property{{
			Name: "settings",
			Type: &schema.ArrayType{ElementType: &schema.ObjectType{
				Properties: []*schema.Property{{Name: "name", Type: schema.StringType}},
			}},
		}},
	}

	sensitiveForEach := cty.MapVal(map[string]cty.Value{
		"primary": cty.ObjectVal(map[string]cty.Value{
			"name": cty.StringVal("p"),
		}),
	}).WithMarks(cty.NewValueMarks(eval.SensitiveMark))

	evalFn := func(_ resource.PropertyKey, expr hcl.Expression, extraVars map[string]cty.Value) (cty.Value, hcl.Diagnostics) {
		if extraVars == nil {
			return sensitiveForEach, nil
		}
		return expr.Value(&hcl.EvalContext{Variables: extraVars})
	}

	out, _, diags := EvalResourceWithSchema(resourceBody, r, nil, evalFn)
	require.False(t, diags.HasErrors(), "unexpected diags: %v", diags)

	expected := property.NewMap(map[string]property.Value{
		"settings": property.New(property.NewArray([]property.Value{
			property.New(property.NewMap(map[string]property.Value{
				"name": property.New("p").WithSecret(true),
			})),
		})),
	})
	assert.Equal(t, expected, out)
}

// More than one block (static, dynamically expanded, or a mix) for a
// MaxItems=1 field is an error, not last-block-wins.
func TestSingularBlockRepeated(t *testing.T) {
	t.Parallel()

	r := &schema.Resource{
		Token: "fake:index:F",
		InputProperties: []*schema.Property{{
			Name: "settings",
			Type: &schema.ObjectType{
				Properties: []*schema.Property{{Name: "mode", Type: schema.StringType}},
			},
		}},
	}
	mapping := &bridge.BodyMapping{Fields: map[string]*bridge.FieldMapping{
		"settings": {TFName: "settings", PulumiName: "settings", TFBlock: true, MaxItemsOne: true},
	}}

	tests := []struct {
		name string
		src  string
	}{
		{"two static blocks", `
resource "fake" "f" {
  settings { mode = "a" }
  settings { mode = "b" }
}`},
		{"dynamic expansion of two", `
resource "fake" "f" {
  dynamic "settings" {
    for_each = ["a", "b"]
    content { mode = settings.value }
  }
}`},
		{"static and dynamic", `
resource "fake" "f" {
  settings { mode = "a" }
  dynamic "settings" {
    for_each = ["b"]
    content { mode = settings.value }
  }
}`},
		{"two dynamic expansions", `
resource "fake" "f" {
  dynamic "settings" {
    for_each = ["a"]
    content { mode = settings.value }
  }
  dynamic "settings" {
    for_each = ["b"]
    content { mode = settings.value }
  }
}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			file, diags := hclsyntax.ParseConfig([]byte(tt.src), "test.hcl", hcl.Pos{Line: 1, Column: 1})
			require.False(t, diags.HasErrors(), diags.Error())
			resourceBody := file.Body.(*hclsyntax.Body).Blocks[0].Body

			evalFn := func(_ resource.PropertyKey, expr hcl.Expression, extraVars map[string]cty.Value) (cty.Value, hcl.Diagnostics) {
				return expr.Value(&hcl.EvalContext{Variables: extraVars})
			}

			_, _, diags = EvalResourceWithSchema(resourceBody, r, mapping, evalFn)
			require.Len(t, diags, 1)
			assert.Equal(t, "Too many settings blocks", diags[0].Summary)
			assert.Equal(t, `No more than 1 "settings" blocks are allowed`, diags[0].Detail)
		})
	}
}

// A TF name the mapping admits but the Pulumi schema has no property for (a
// provider whose schema and mapping disagree) is a diagnostic, not a panic.
func TestUnmappedProperty(t *testing.T) {
	t.Parallel()

	r := &schema.Resource{
		Token: "fake:index:F",
		InputProperties: []*schema.Property{{
			Name: "settings",
			Type: &schema.ObjectType{
				Properties: []*schema.Property{{
					Name: "innerBlocks",
					Type: &schema.ArrayType{ElementType: &schema.ObjectType{
						Properties: []*schema.Property{{Name: "attr", Type: schema.StringType}},
					}},
				}},
			},
		}},
	}
	mapping := &bridge.BodyMapping{Fields: map[string]*bridge.FieldMapping{
		"settings": {
			TFName: "settings", PulumiName: "settings", TFBlock: true, MaxItemsOne: true,
			Nested: &bridge.BodyMapping{Fields: map[string]*bridge.FieldMapping{
				"inner_block": {TFName: "inner_block", PulumiName: "innerBlock", TFBlock: true, MaxItemsOne: true},
			}},
		},
	}}

	src := `
resource "fake" "f" {
  settings {
    inner_block { attr = "a" }
  }
}`
	file, diags := hclsyntax.ParseConfig([]byte(src), "test.hcl", hcl.Pos{Line: 1, Column: 1})
	require.False(t, diags.HasErrors(), diags.Error())
	resourceBody := file.Body.(*hclsyntax.Body).Blocks[0].Body

	evalFn := func(_ resource.PropertyKey, expr hcl.Expression, extraVars map[string]cty.Value) (cty.Value, hcl.Diagnostics) {
		return expr.Value(&hcl.EvalContext{Variables: extraVars})
	}

	_, _, diags = EvalResourceWithSchema(resourceBody, r, mapping, evalFn)
	require.Len(t, diags, 1)
	assert.Equal(t, `Provider schema has no property for "inner_block"`, diags[0].Summary)
	assert.Equal(t, `The provider's mapping names "inner_block" as "innerBlock", but its schema defines only: innerBlocks. `+
		`The provider's schema and mapping disagree; this is a provider bug.`, diags[0].Detail)
	assert.Equal(t, 4, diags[0].Subject.Start.Line)
}

func TestCtyToPropertyValue_Primitives(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		input    cty.Value
		expected property.Value
	}{
		{
			name:     "null",
			input:    cty.NullVal(cty.String),
			expected: property.New(property.Null),
		},
		{
			name:     "bool true",
			input:    cty.BoolVal(true),
			expected: property.New(true),
		},
		{
			name:     "bool false",
			input:    cty.BoolVal(false),
			expected: property.New(false),
		},
		{
			name:     "string",
			input:    cty.StringVal("hello"),
			expected: property.New("hello"),
		},
		{
			name:     "number int",
			input:    cty.NumberIntVal(42),
			expected: property.New(float64(42)),
		},
		{
			name:     "number float",
			input:    cty.NumberFloatVal(3.14),
			expected: property.New(3.14),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			result, err := CtyToPropertyValue(tt.input)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !result.Equals(tt.expected) {
				t.Errorf("expected %v, got %v", tt.expected, result)
			}
		})
	}
}

func TestCtyToPropertyValue_Collections(t *testing.T) {
	t.Parallel()

	t.Run("list", func(t *testing.T) {
		t.Parallel()

		input := cty.ListVal([]cty.Value{
			cty.StringVal("a"),
			cty.StringVal("b"),
			cty.StringVal("c"),
		})

		result, err := CtyToPropertyValue(input)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if !result.IsArray() {
			t.Fatal("expected array")
		}

		arr := result.AsArray().AsSlice()
		if len(arr) != 3 {
			t.Errorf("expected 3 elements, got %d", len(arr))
		}
		if arr[0].AsString() != "a" || arr[1].AsString() != "b" || arr[2].AsString() != "c" {
			t.Error("unexpected array contents")
		}
	})

	t.Run("map", func(t *testing.T) {
		t.Parallel()

		// Maps in cty require homogeneous types, so use all strings
		input := cty.MapVal(map[string]cty.Value{
			"key1": cty.StringVal("value1"),
			"key2": cty.StringVal("value2"),
		})

		result, err := CtyToPropertyValue(input)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if !result.IsMap() {
			t.Fatal("expected object")
		}

		obj := result.AsMap().AsMap()
		if obj["key1"].AsString() != "value1" {
			t.Errorf("expected key1=value1, got %v", obj["key1"])
		}
		if obj["key2"].AsString() != "value2" {
			t.Errorf("expected key2=value2, got %v", obj["key2"])
		}
	})

	t.Run("object", func(t *testing.T) {
		t.Parallel()

		input := cty.ObjectVal(map[string]cty.Value{
			"name": cty.StringVal("test"),
			"port": cty.NumberIntVal(8080),
		})

		result, err := CtyToPropertyValue(input)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if !result.IsMap() {
			t.Fatal("expected object")
		}

		obj := result.AsMap().AsMap()
		if obj["name"].AsString() != "test" {
			t.Errorf("expected name=test, got %v", obj["name"])
		}
		if obj["port"].AsNumber() != 8080 {
			t.Errorf("expected port=8080, got %v", obj["port"])
		}
	})

	t.Run("tuple", func(t *testing.T) {
		t.Parallel()

		input := cty.TupleVal([]cty.Value{
			cty.StringVal("a"),
			cty.NumberIntVal(1),
			cty.BoolVal(true),
		})

		result, err := CtyToPropertyValue(input)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if !result.IsArray() {
			t.Fatal("expected array")
		}

		arr := result.AsArray().AsSlice()
		if len(arr) != 3 {
			t.Errorf("expected 3 elements, got %d", len(arr))
		}
	})

	t.Run("set", func(t *testing.T) {
		t.Parallel()

		input := cty.SetVal([]cty.Value{
			cty.StringVal("a"),
			cty.StringVal("b"),
		})

		result, err := CtyToPropertyValue(input)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if !result.IsArray() {
			t.Fatal("expected array")
		}

		arr := result.AsArray().AsSlice()
		if len(arr) != 2 {
			t.Errorf("expected 2 elements, got %d", len(arr))
		}
	})

	t.Run("empty set", func(t *testing.T) {
		t.Parallel()

		result, err := CtyToPropertyValue(cty.SetValEmpty(cty.String))
		require.NoError(t, err)
		require.True(t, result.IsArray(), "an empty set must be an empty array, not null")
		require.Equal(t, 0, result.AsArray().Len())
	})
}

func TestCtyToPropertyValue_Nested(t *testing.T) {
	t.Parallel()

	input := cty.ObjectVal(map[string]cty.Value{
		"tags": cty.MapVal(map[string]cty.Value{
			"env": cty.StringVal("prod"),
		}),
		"ports": cty.ListVal([]cty.Value{
			cty.NumberIntVal(80),
			cty.NumberIntVal(443),
		}),
	})

	result, err := CtyToPropertyValue(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !result.IsMap() {
		t.Fatal("expected object")
	}

	obj := result.AsMap().AsMap()
	tags := obj["tags"]
	if !tags.IsMap() {
		t.Fatal("expected tags to be object")
	}
	if tags.AsMap().AsMap()["env"].AsString() != "prod" {
		t.Error("expected tags.env=prod")
	}

	ports := obj["ports"]
	if !ports.IsArray() {
		t.Fatal("expected ports to be array")
	}
	if len(ports.AsArray().AsSlice()) != 2 {
		t.Error("expected 2 ports")
	}
}

func TestPropertyValueToCty_Primitives(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		input    property.Value
		expected cty.Value
	}{
		{
			name:     "null",
			input:    property.New(property.Null),
			expected: cty.NullVal(cty.DynamicPseudoType),
		},
		{
			name:     "bool",
			input:    property.New(true),
			expected: cty.BoolVal(true),
		},
		{
			name:     "string",
			input:    property.New("hello"),
			expected: cty.StringVal("hello"),
		},
		{
			name:     "number",
			input:    property.New(float64(42)),
			expected: cty.NumberFloatVal(42),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			result := PropertyValueToCty(tt.input)
			if !result.RawEquals(tt.expected) {
				t.Errorf("expected %v, got %v", tt.expected, result)
			}
		})
	}
}

func TestPropertyValueToCty_Collections(t *testing.T) {
	t.Parallel()

	t.Run("array", func(t *testing.T) {
		t.Parallel()

		input := property.New(property.NewArray([]property.Value{
			property.New("a"),
			property.New("b"),
		}))

		result := PropertyValueToCty(input)

		// Should be a tuple
		if !result.Type().IsTupleType() {
			t.Fatal("expected tuple type")
		}

		vals := result.AsValueSlice()
		if len(vals) != 2 {
			t.Errorf("expected 2 elements, got %d", len(vals))
		}
		if vals[0].AsString() != "a" || vals[1].AsString() != "b" {
			t.Error("unexpected values")
		}
	})

	t.Run("object", func(t *testing.T) {
		t.Parallel()

		input := property.New(property.NewMap(map[string]property.Value{
			"key": property.New("value"),
		}))

		result := PropertyValueToCty(input)

		if !result.Type().IsObjectType() {
			t.Fatal("expected object type")
		}

		val := result.GetAttr("key")
		if val.AsString() != "value" {
			t.Errorf("expected value, got %v", val.AsString())
		}
	})
}

func TestPropertyValueToCty_Secret(t *testing.T) {
	t.Parallel()

	input := property.New("secret-value").WithSecret(true)

	result := PropertyValueToCty(input)

	if !result.IsMarked() {
		t.Fatal("expected value to be marked")
	}
	unmarked, marks := result.Unmark()
	if unmarked.AsString() != "secret-value" {
		t.Errorf("expected secret-value, got %v", unmarked.AsString())
	}
	if _, ok := marks[eval.SensitiveMark]; !ok {
		t.Error("expected sensitive mark")
	}
}

// TestPropertyValueToCty_ResourceReference covers the schemaless path a
// resource reference takes into a dynamic module input: without a schema to
// type the referenced resource, the value is its outputs object (here just id)
// marked as a reference, rather than a dropped null.
func TestPropertyValueToCty_ResourceReference(t *testing.T) {
	t.Parallel()

	ref := property.ResourceReference{
		URN: resource.URN("urn:pulumi:dev::p::test:index:Resource::fn"),
		ID:  property.New("fn-id"),
	}

	result := PropertyValueToCty(property.New(ref))

	gotURN, ok := eval.ResourceReferenceURN(result)
	require.True(t, ok, "the value should be identifiable as a resource reference")
	require.Equal(t, ref.URN, gotURN)

	unmarked, _ := result.Unmark()
	require.Equal(t, ref, resourceReferenceFromOutputs(gotURN, unmarked.AsValueMap()))
}

func TestCtyToPropertyMap(t *testing.T) {
	t.Parallel()

	input := cty.ObjectVal(map[string]cty.Value{
		"name":    cty.StringVal("test"),
		"enabled": cty.BoolVal(true),
	})

	result, err := CtyToPropertyMap(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result.Get("name").AsString() != "test" {
		t.Errorf("expected name=test")
	}
	if result.Get("enabled").AsBool() != true {
		t.Errorf("expected enabled=true")
	}
}

func TestPropertyMapToCty(t *testing.T) {
	t.Parallel()

	input := property.NewMap(map[string]property.Value{
		"name":    property.New("test"),
		"enabled": property.New(true),
	})

	result := PropertyMapToCty(input)

	if !result.Type().IsObjectType() {
		t.Fatal("expected object type")
	}

	if result.GetAttr("name").AsString() != "test" {
		t.Error("expected name=test")
	}
	if result.GetAttr("enabled").True() != true {
		t.Error("expected enabled=true")
	}
}

func TestRoundTrip(t *testing.T) {
	t.Parallel()

	// Test that converting cty -> property -> cty preserves values
	original := cty.ObjectVal(map[string]cty.Value{
		"name":  cty.StringVal("test"),
		"count": cty.NumberIntVal(5),
		"tags": cty.MapVal(map[string]cty.Value{
			"env": cty.StringVal("prod"),
		}),
	})

	pv, err := CtyToPropertyValue(original)
	if err != nil {
		t.Fatalf("CtyToPropertyValue error: %v", err)
	}

	result := PropertyValueToCty(pv)

	// Check individual fields since types may differ slightly
	if result.GetAttr("name").AsString() != "test" {
		t.Error("name mismatch")
	}
	if result.GetAttr("count").AsBigFloat().String() != "5" {
		t.Error("count mismatch")
	}
	if result.GetAttr("tags").GetAttr("env").AsString() != "prod" {
		t.Error("tags.env mismatch")
	}
}

// TestResourceOutputToCtyDoesNotErrorOnValidValues exercises ResourceOutputToCty across
// rapid-drawn schemas and output values.
//
// Since all property bags conform to the schema, they should all be able to be translated
// to [cty.Value] bags.
func TestResourceOutputToCtyDoesNotErrorOnValidValues(t *testing.T) {
	t.Parallel()

	rapid.Check(t, func(rt *rapid.T) {
		pkg := rapidschema.Package().Draw(rt, "pkg")
		for _, res := range pkg.Resources {
			if res.IsProvider {
				continue
			}
			outputs := rapidresource.ResourceProperties(res).Draw(rt, "outputs:"+res.Token)
			_, err := ResourceOutputToCty(outputs, res, nil, false)
			require.NoError(t, err)
		}
	})
}

// TestResourceOutputToCtySingularBlockPlaceholder pins the shape of the
// placeholder emitted when a MaxItemsOne block property is missing or null:
// TF models the block as a list, so the unknown/null value must be list-typed
// for a traversal like `r.block[0].field` to resolve.
func TestResourceOutputToCtySingularBlockPlaceholder(t *testing.T) {
	t.Parallel()

	res := &schema.Resource{
		Token: "test:index:R",
		Properties: []*schema.Property{
			{Name: "block", Type: &schema.ObjectType{
				Properties: []*schema.Property{
					{Name: "field", Type: schema.StringType},
				},
			}},
		},
	}
	mapping := &bridge.BodyMapping{Fields: map[string]*bridge.FieldMapping{
		"block": {
			TFName:      "block",
			PulumiName:  "block",
			TFBlock:     true,
			MaxItemsOne: true,
			Nested: &bridge.BodyMapping{Fields: map[string]*bridge.FieldMapping{
				"field": {TFName: "field", PulumiName: "field"},
			}},
		},
	}}
	blockList := cty.List(cty.Object(map[string]cty.Type{"field": cty.String}))

	t.Run("missing during preview", func(t *testing.T) {
		t.Parallel()
		r, err := ResourceOutputToCty(property.Map{}, res, mapping, true)
		require.NoError(t, err)
		assert.Equal(t, map[string]cty.Value{"block": cty.UnknownVal(blockList)}, r)
	})

	t.Run("null during preview", func(t *testing.T) {
		t.Parallel()
		outputs := property.NewMap(map[string]property.Value{
			"block": property.New(property.Null),
		})
		r, err := ResourceOutputToCty(outputs, res, mapping, true)
		require.NoError(t, err)
		assert.Equal(t, map[string]cty.Value{"block": cty.UnknownVal(blockList)}, r)
	})

	t.Run("missing during apply", func(t *testing.T) {
		t.Parallel()
		r, err := ResourceOutputToCty(property.Map{}, res, mapping, false)
		require.NoError(t, err)
		assert.Equal(t, map[string]cty.Value{"block": cty.NullVal(blockList)}, r)
	})
}

// TestResourceOutputToCtySetField pins that a TF TypeSet field — a set block
// or a set-typed attribute — re-expands as a cty set (content-based,
// order-independent equality), not an ordered list/tuple.
func TestResourceOutputToCtySetField(t *testing.T) {
	t.Parallel()

	elemObj := &schema.ObjectType{
		Properties: []*schema.Property{
			{Name: "name", Type: schema.StringType},
		},
	}
	res := &schema.Resource{
		Token: "test:index:R",
		Properties: []*schema.Property{
			{Name: "filters", Type: &schema.ArrayType{ElementType: elemObj}},
			{Name: "ports", Type: &schema.ArrayType{ElementType: schema.NumberType}},
		},
	}
	mapping := &bridge.BodyMapping{Fields: map[string]*bridge.FieldMapping{
		"filter": {
			TFName:     "filter",
			PulumiName: "filters",
			TFBlock:    true,
			TFSet:      true,
			Nested: &bridge.BodyMapping{Fields: map[string]*bridge.FieldMapping{
				"name": {TFName: "name", PulumiName: "name"},
			}},
		},
		"ports": {TFName: "ports", PulumiName: "ports", TFSet: true},
	}}
	filterSet := cty.Set(cty.Object(map[string]cty.Type{"name": cty.String}))

	t.Run("elements", func(t *testing.T) {
		t.Parallel()
		outputs := property.NewMap(map[string]property.Value{
			"filters": property.New([]property.Value{
				property.New(property.NewMap(map[string]property.Value{"name": property.New("zebra")})),
				property.New(property.NewMap(map[string]property.Value{"name": property.New("apple")})),
			}),
			"ports": property.New([]property.Value{property.New(443.0), property.New(80.0)}),
		})
		r, err := ResourceOutputToCty(outputs, res, mapping, false)
		require.NoError(t, err)
		assert.Equal(t, map[string]cty.Value{
			"filter": cty.SetVal([]cty.Value{
				cty.ObjectVal(map[string]cty.Value{"name": cty.StringVal("apple")}),
				cty.ObjectVal(map[string]cty.Value{"name": cty.StringVal("zebra")}),
			}),
			"ports": cty.SetVal([]cty.Value{cty.NumberFloatVal(80), cty.NumberFloatVal(443)}),
		}, r)
	})

	t.Run("missing during preview", func(t *testing.T) {
		t.Parallel()
		r, err := ResourceOutputToCty(property.Map{}, res, mapping, true)
		require.NoError(t, err)
		assert.Equal(t, map[string]cty.Value{
			"filter": cty.UnknownVal(filterSet),
			"ports":  cty.UnknownVal(cty.Set(cty.Number)),
		}, r)
	})

	t.Run("missing during apply", func(t *testing.T) {
		t.Parallel()
		r, err := ResourceOutputToCty(property.Map{}, res, mapping, false)
		require.NoError(t, err)
		assert.Equal(t, map[string]cty.Value{
			"filter": cty.NullVal(filterSet),
			"ports":  cty.NullVal(cty.Set(cty.Number)),
		}, r)
	})

	t.Run("secret element lifts to the set", func(t *testing.T) {
		t.Parallel()
		outputs := property.NewMap(map[string]property.Value{
			"ports": property.New([]property.Value{property.New(443.0).WithSecret(true)}),
		})
		r, err := ResourceOutputToCty(outputs, res, mapping, false)
		require.NoError(t, err)
		assert.Equal(t,
			cty.SetVal([]cty.Value{cty.NumberFloatVal(443)}).Mark(eval.SensitiveMark),
			r["ports"])
	})
}

// TestResourceReferenceTypeSetField pins that the static type of a reference
// to a resource types TF TypeSet fields as cty sets, matching the runtime
// values ResourceOutputToCty produces.
func TestResourceReferenceTypeSetField(t *testing.T) {
	t.Parallel()

	res := &schema.Resource{
		Token: "test:index:R",
		Properties: []*schema.Property{
			{Name: "ports", Type: &schema.ArrayType{ElementType: schema.NumberType}},
		},
	}
	mapping := &bridge.BodyMapping{Fields: map[string]*bridge.FieldMapping{
		"ports": {TFName: "ports", PulumiName: "ports", TFSet: true},
	}}

	typ := ResourceReferenceType(res, mapping)
	assert.Equal(t, cty.Set(cty.Number), typ.AttributeType("ports"))
}

// TestResourceReferenceTypeTimeouts pins that a resource declaring operation
// timeouts types `timeouts` as an object of exactly those operations, matching
// the runtime value the engine exposes.
func TestResourceReferenceTypeTimeouts(t *testing.T) {
	t.Parallel()

	res := &schema.Resource{Token: "test:index:R"}
	typ := ResourceReferenceType(res, &bridge.BodyMapping{Timeouts: []string{"create", "default"}})
	assert.Equal(t, cty.Object(map[string]cty.Type{"create": cty.String, "default": cty.String}), typ.AttributeType("timeouts"))

	assert.False(t, ResourceReferenceType(res, &bridge.BodyMapping{}).HasAttribute("timeouts"))
}

// TestDataSourceReferenceTypeTimeouts pins that a data source declaring
// operation timeouts types `timeouts` alongside its return properties, and
// that a non-object return type stays dynamic.
func TestDataSourceReferenceTypeTimeouts(t *testing.T) {
	t.Parallel()

	fn := &schema.Function{
		Token: "test:index:getR",
		ReturnType: &schema.ObjectType{Properties: []*schema.Property{
			{Name: "result", Type: schema.StringType},
		}},
	}
	typ := DataSourceReferenceType(fn, &bridge.BodyMapping{Timeouts: []string{"read"}})
	assert.Equal(t, cty.Object(map[string]cty.Type{"read": cty.String}), typ.AttributeType("timeouts"))
	assert.True(t, typ.HasAttribute("result"))

	scalar := &schema.Function{Token: "test:index:getS", ReturnType: schema.StringType}
	assert.Equal(t, cty.DynamicPseudoType, DataSourceReferenceType(scalar, nil))
}

// TestResourceOutputToCtySchemaSecretElided pins that a schema-secret output
// keeps its sensitive mark even when the provider elides the (unknown) value
// during preview, so a downstream stack output stays secret.
func TestResourceOutputToCtySchemaSecretElided(t *testing.T) {
	t.Parallel()

	res := &schema.Resource{
		Token: "test:index:R",
		Properties: []*schema.Property{
			{Name: "secretOutput", Type: schema.StringType, Secret: true},
		},
	}

	r, err := ResourceOutputToCty(property.Map{}, res, nil, true)
	require.NoError(t, err)
	assert.Equal(t, map[string]cty.Value{
		"secret_output": cty.UnknownVal(cty.String).Mark(eval.SensitiveMark),
	}, r)
}

func TestResourceOutputToCtyUnionTypeCollapse(t *testing.T) {
	t.Parallel()

	obj1 := &schema.ObjectType{
		Properties: []*schema.Property{
			{Name: "fooBar", Type: schema.StringType},
		},
	}

	obj2 := &schema.MapType{ElementType: schema.StringType}

	union := &schema.UnionType{
		ElementTypes: []schema.Type{obj1, obj2},
	}

	nested := &schema.ObjectType{
		Properties: []*schema.Property{
			{Name: "p", Type: union},
		},
	}

	res := &schema.Resource{
		Token: "test:index:R",
		Properties: []*schema.Property{
			{Name: "p", Type: nested},
		},
	}

	test := func(t *testing.T, key string, expected cty.Value) {
		outputs := property.NewMap(map[string]property.Value{
			"p": property.New(map[string]property.Value{
				"p": property.New(map[string]property.Value{
					key: property.New("hello"),
				}),
			}),
		})

		r, err := ResourceOutputToCty(outputs, res, nil, false)
		require.NoError(t, err)
		assert.Equal(t, map[string]cty.Value{
			"p": cty.ObjectVal(map[string]cty.Value{
				"p": expected,
			}),
		}, r)
	}

	t.Run("object", func(t *testing.T) {
		t.Parallel()

		test(t, "fooBar", cty.ObjectVal(map[string]cty.Value{
			"foo_bar": cty.StringVal("hello"),
		}))
	})

	t.Run("map", func(t *testing.T) {
		t.Parallel()

		test(t, "someKey", cty.MapVal(map[string]cty.Value{
			"someKey": cty.StringVal("hello"),
		}))
	})
}

// TestResourceOutputToCtyUnionTypeCollapseNested exercises union resolution
// where the union sits underneath arrays/maps and contains itself nested
// object/map members. The selector has to descend through the carrier
// collections AND through the candidate types to find a match.
func TestResourceOutputToCtyUnionTypeCollapseNested(t *testing.T) {
	t.Parallel()

	obj := &schema.ObjectType{
		Properties: []*schema.Property{
			{Name: "fooBar", Type: schema.StringType},
		},
	}
	mp := &schema.MapType{ElementType: schema.StringType}
	union := &schema.UnionType{ElementTypes: []schema.Type{obj, mp}}

	t.Run("array_of_union", func(t *testing.T) {
		t.Parallel()

		res := &schema.Resource{
			Token: "test:index:R",
			Properties: []*schema.Property{
				{Name: "items", Type: &schema.ArrayType{ElementType: union}},
			},
		}
		outputs := property.NewMap(map[string]property.Value{
			"items": property.New([]property.Value{
				property.New(map[string]property.Value{"fooBar": property.New("hi")}),
				property.New(map[string]property.Value{"loose": property.New("yo")}),
			}),
		})

		r, err := ResourceOutputToCty(outputs, res, nil, false)
		require.NoError(t, err)
		// Object{foo_bar:String} and Map<String> unify to Map<String>, so the
		// outer array can stay as a clean cty.ListVal of cty.MapVal.
		assert.Equal(t, map[string]cty.Value{
			"items": cty.ListVal([]cty.Value{
				cty.MapVal(map[string]cty.Value{"foo_bar": cty.StringVal("hi")}),
				cty.MapVal(map[string]cty.Value{"loose": cty.StringVal("yo")}),
			}),
		}, r)
	})

	t.Run("map_of_union", func(t *testing.T) {
		t.Parallel()

		res := &schema.Resource{
			Token: "test:index:R",
			Properties: []*schema.Property{
				{Name: "lookup", Type: &schema.MapType{ElementType: union}},
			},
		}
		outputs := property.NewMap(map[string]property.Value{
			"lookup": property.New(map[string]property.Value{
				"a": property.New(map[string]property.Value{"fooBar": property.New("hi")}),
				"b": property.New(map[string]property.Value{"x": property.New("y")}),
			}),
		})

		r, err := ResourceOutputToCty(outputs, res, nil, false)
		require.NoError(t, err)
		assert.Equal(t, map[string]cty.Value{
			"lookup": cty.MapVal(map[string]cty.Value{
				"a": cty.MapVal(map[string]cty.Value{"foo_bar": cty.StringVal("hi")}),
				"b": cty.MapVal(map[string]cty.Value{"x": cty.StringVal("y")}),
			}),
		}, r)
	})

	t.Run("array_of_map_of_union", func(t *testing.T) {
		t.Parallel()

		res := &schema.Resource{
			Token: "test:index:R",
			Properties: []*schema.Property{
				{Name: "deep", Type: &schema.ArrayType{
					ElementType: &schema.MapType{ElementType: union},
				}},
			},
		}
		outputs := property.NewMap(map[string]property.Value{
			"deep": property.New([]property.Value{
				property.New(map[string]property.Value{
					"k1": property.New(map[string]property.Value{"fooBar": property.New("hi")}),
					"k2": property.New(map[string]property.Value{"fooBar": property.New("ho")}),
				}),
				property.New(map[string]property.Value{
					"k3": property.New(map[string]property.Value{"free": property.New("form")}),
				}),
			}),
		})

		r, err := ResourceOutputToCty(outputs, res, nil, false)
		require.NoError(t, err)
		// After union resolution every leaf becomes Map<String>, so both the
		// inner maps and the outer array can stay homogeneous.
		assert.Equal(t, map[string]cty.Value{
			"deep": cty.ListVal([]cty.Value{
				cty.MapVal(map[string]cty.Value{
					"k1": cty.MapVal(map[string]cty.Value{"foo_bar": cty.StringVal("hi")}),
					"k2": cty.MapVal(map[string]cty.Value{"foo_bar": cty.StringVal("ho")}),
				}),
				cty.MapVal(map[string]cty.Value{
					"k3": cty.MapVal(map[string]cty.Value{"free": cty.StringVal("form")}),
				}),
			}),
		}, r)
	})

	t.Run("map_of_union_heterogeneous", func(t *testing.T) {
		t.Parallel()

		res := &schema.Resource{
			Token: "test:index:R",
			Properties: []*schema.Property{
				{Name: "mixed", Type: &schema.MapType{ElementType: union}},
			},
		}
		outputs := property.NewMap(map[string]property.Value{
			"mixed": property.New(map[string]property.Value{
				"a": property.New(map[string]property.Value{"fooBar": property.New("x")}),
				"b": property.New(map[string]property.Value{"free": property.New("y")}),
			}),
		})

		r, err := ResourceOutputToCty(outputs, res, nil, false)
		require.NoError(t, err)
		// Object{foo_bar:String} and Map<String> unify to Map<String>, so the
		// outer collection can stay as a clean cty.MapVal of cty.MapVal.
		assert.Equal(t, map[string]cty.Value{
			"mixed": cty.MapVal(map[string]cty.Value{
				"a": cty.MapVal(map[string]cty.Value{"foo_bar": cty.StringVal("x")}),
				"b": cty.MapVal(map[string]cty.Value{"free": cty.StringVal("y")}),
			}),
		}, r)
	})

	t.Run("nested_union_disambiguated_by_inner_value", func(t *testing.T) {
		t.Parallel()

		// Both members are objects with a property "p", but the inner type
		// differs. The selector must look at the inner value to choose.
		innerString := &schema.ObjectType{
			Properties: []*schema.Property{
				{Name: "p", Type: schema.StringType},
			},
		}
		innerBool := &schema.ObjectType{
			Properties: []*schema.Property{
				{Name: "p", Type: schema.BoolType},
			},
		}
		uu := &schema.UnionType{ElementTypes: []schema.Type{innerString, innerBool}}

		res := &schema.Resource{
			Token: "test:index:R",
			Properties: []*schema.Property{
				{Name: "v", Type: uu},
			},
		}

		t.Run("string_branch", func(t *testing.T) {
			t.Parallel()

			outputs := property.NewMap(map[string]property.Value{
				"v": property.New(map[string]property.Value{"p": property.New("hi")}),
			})
			r, err := ResourceOutputToCty(outputs, res, nil, false)
			require.NoError(t, err)
			assert.Equal(t, map[string]cty.Value{
				"v": cty.ObjectVal(map[string]cty.Value{"p": cty.StringVal("hi")}),
			}, r)
		})

		t.Run("bool_branch", func(t *testing.T) {
			t.Parallel()

			outputs := property.NewMap(map[string]property.Value{
				"v": property.New(map[string]property.Value{"p": property.New(true)}),
			})
			r, err := ResourceOutputToCty(outputs, res, nil, false)
			require.NoError(t, err)
			assert.Equal(t, map[string]cty.Value{
				"v": cty.ObjectVal(map[string]cty.Value{"p": cty.BoolVal(true)}),
			}, r)
		})
	})

	t.Run("required_property_missing_falls_back_to_map", func(t *testing.T) {
		t.Parallel()

		// objBothRequired requires both x and y. The value omits y, so it
		// cannot be a valid objBothRequired. The only fitting member is
		// Map<string>. A matcher that only checks that value keys belong
		// to the object's properties — without verifying that *required*
		// properties of the object are present in the value — would
		// incorrectly pick objBothRequired.
		objBothRequired := &schema.ObjectType{
			Properties: []*schema.Property{
				{Name: "x", Type: schema.StringType},
				{Name: "y", Type: schema.StringType},
			},
		}
		mp := &schema.MapType{ElementType: schema.StringType}
		uu := &schema.UnionType{ElementTypes: []schema.Type{objBothRequired, mp}}

		res := &schema.Resource{
			Token: "test:index:R",
			Properties: []*schema.Property{
				{Name: "v", Type: uu},
			},
		}

		outputs := property.NewMap(map[string]property.Value{
			"v": property.New(map[string]property.Value{
				"x": property.New("hi"),
			}),
		})

		r, err := ResourceOutputToCty(outputs, res, nil, false)
		require.NoError(t, err)
		assert.Equal(t, map[string]cty.Value{
			"v": cty.MapVal(map[string]cty.Value{
				"x": cty.StringVal("hi"),
			}),
		}, r)
	})

	t.Run("optional_property_value_must_type_check", func(t *testing.T) {
		t.Parallel()

		// objA has a required `x` and an optional `extras` typed as int.
		// The value provides `extras` as a string — that must disqualify
		// objA. Map<string> is the only remaining valid member.
		objA := &schema.ObjectType{
			Properties: []*schema.Property{
				{Name: "x", Type: schema.StringType},
				{Name: "extras", Type: &schema.OptionalType{ElementType: schema.IntType}},
			},
		}
		mp := &schema.MapType{ElementType: schema.StringType}
		uu := &schema.UnionType{ElementTypes: []schema.Type{objA, mp}}

		res := &schema.Resource{
			Token: "test:index:R",
			Properties: []*schema.Property{
				{Name: "v", Type: uu},
			},
		}

		outputs := property.NewMap(map[string]property.Value{
			"v": property.New(map[string]property.Value{
				"x":      property.New("hi"),
				"extras": property.New("not-an-int"),
			}),
		})

		r, err := ResourceOutputToCty(outputs, res, nil, false)
		require.NoError(t, err)
		assert.Equal(t, map[string]cty.Value{
			"v": cty.MapVal(map[string]cty.Value{
				"x":      cty.StringVal("hi"),
				"extras": cty.StringVal("not-an-int"),
			}),
		}, r)
	})

	t.Run("optional_present_picks_object_over_object_without_it", func(t *testing.T) {
		t.Parallel()

		// objA only has x. objB has x required and extras optional.
		// When the value carries `extras`, only objB can describe it.
		// objA is listed first, but the matcher must reject it because
		// `extras` is not a property of objA.
		objA := &schema.ObjectType{
			Properties: []*schema.Property{
				{Name: "x", Type: schema.StringType},
			},
		}
		objB := &schema.ObjectType{
			Properties: []*schema.Property{
				{Name: "x", Type: schema.StringType},
				{Name: "extras", Type: &schema.OptionalType{ElementType: schema.StringType}},
			},
		}
		uu := &schema.UnionType{ElementTypes: []schema.Type{objA, objB}}

		res := &schema.Resource{
			Token: "test:index:R",
			Properties: []*schema.Property{
				{Name: "v", Type: uu},
			},
		}

		outputs := property.NewMap(map[string]property.Value{
			"v": property.New(map[string]property.Value{
				"x":      property.New("hi"),
				"extras": property.New("ho"),
			}),
		})

		r, err := ResourceOutputToCty(outputs, res, nil, false)
		require.NoError(t, err)
		assert.Equal(t, map[string]cty.Value{
			"v": cty.ObjectVal(map[string]cty.Value{
				"x":      cty.StringVal("hi"),
				"extras": cty.StringVal("ho"),
			}),
		}, r)
	})
}

func TestResourceOutputToCtyJSONType(t *testing.T) {
	t.Parallel()

	inner := &schema.ObjectType{
		Properties: []*schema.Property{
			{Name: "blob", Type: schema.JSONType},
		},
	}
	res := &schema.Resource{
		Token: "test:index:R",
		Properties: []*schema.Property{
			{Name: "wrapper", Type: inner},
		},
	}

	outputs := property.NewMap(map[string]property.Value{
		"wrapper": property.New(map[string]property.Value{
			"blob": property.New(map[string]property.Value{
				"k": property.New("v"),
			}),
		}),
	})

	r, err := ResourceOutputToCty(outputs, res, nil, false)
	require.NoError(t, err)
	assert.Equal(t, map[string]cty.Value{
		"wrapper": cty.ObjectVal(map[string]cty.Value{
			"blob": cty.MapVal(map[string]cty.Value{
				"k": cty.StringVal("v"),
			}),
		}),
	}, r)
}

// TestCtyEqualsConst exercises every Go type that bindConstValue is
// documented to produce (string, bool, int32, float64). Earlier versions
// of SelectUnionMemberByConst only compared strings, so other-typed
// discriminators silently failed to match.
func TestCtyEqualsConst(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		val   cty.Value
		c     any
		match bool
	}{
		{"string match", cty.StringVal("a"), "a", true},
		{"string mismatch", cty.StringVal("a"), "b", false},
		{"string vs bool", cty.StringVal("true"), true, false},

		{"bool match", cty.True, true, true},
		{"bool mismatch", cty.False, true, false},
		{"bool vs string", cty.True, "true", false},

		{"int32 match", cty.NumberIntVal(7), int32(7), true},
		{"int32 mismatch", cty.NumberIntVal(7), int32(8), false},
		{"int32 vs fractional", cty.NumberFloatVal(7.5), int32(7), false},
		{"int32 vs string-typed", cty.StringVal("7"), int32(7), false},

		{"float64 match", cty.NumberFloatVal(1.5), 1.5, true},
		{"float64 mismatch", cty.NumberFloatVal(1.5), 2.5, false},
		{"float64 vs string-typed", cty.StringVal("1.5"), 1.5, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.match, CtyEqualsConst(tt.val, tt.c))
		})
	}
}

// TestSelectUnionMemberByConst_IntDiscriminator proves the non-string
// path end-to-end: a union of two object variants whose only difference
// is an int32-valued `version` const is correctly resolved.
func TestSelectUnionMemberByConst_IntDiscriminator(t *testing.T) {
	t.Parallel()

	v1 := &schema.ObjectType{
		Token: "test:index:V1",
		Properties: []*schema.Property{
			{Name: "version", Type: schema.IntType, ConstValue: int32(1)},
			{Name: "a", Type: schema.StringType},
		},
	}
	v2 := &schema.ObjectType{
		Token: "test:index:V2",
		Properties: []*schema.Property{
			{Name: "version", Type: schema.IntType, ConstValue: int32(2)},
			{Name: "b", Type: schema.StringType},
		},
	}
	u := &schema.UnionType{ElementTypes: []schema.Type{v1, v2}}

	picked, err := SelectUnionMemberByConst(cty.ObjectVal(map[string]cty.Value{
		"version": cty.NumberIntVal(2),
		"b":       cty.StringVal("hello"),
	}), u)
	require.NoError(t, err)
	assert.Same(t, v2, picked)

	picked, err = SelectUnionMemberByConst(cty.ObjectVal(map[string]cty.Value{
		"version": cty.NumberIntVal(1),
		"a":       cty.StringVal("hello"),
	}), u)
	require.NoError(t, err)
	assert.Same(t, v1, picked)

	_, err = SelectUnionMemberByConst(cty.ObjectVal(map[string]cty.Value{
		"version": cty.NumberIntVal(99),
	}), u)
	var unrecognized *unrecognizedDiscriminatorError
	require.ErrorAs(t, err, &unrecognized)
	assert.Equal(t, []string{"1", "2"}, unrecognized.Allowed)
	assert.Equal(t, "99", unrecognized.Actual)
}

// recursiveResourceSchema builds a resource whose type graph reaches itself: A.b
// is a B, and B.as is a list of A, so A and B form a cycle. The object types
// share pointers, the way a loaded schema binds them.
func recursiveResourceSchema() *schema.Resource {
	a := &schema.ObjectType{Token: "test:index:A"}
	b := &schema.ObjectType{
		Token: "test:index:B",
		Properties: []*schema.Property{
			{Name: "name", Type: schema.StringType},
			{Name: "as", Type: &schema.ArrayType{ElementType: a}},
		},
	}
	a.Properties = []*schema.Property{
		{Name: "name", Type: schema.StringType},
		{Name: "b", Type: b},
	}
	return &schema.Resource{
		Token: "test:index:Res",
		Properties: []*schema.Property{
			{Name: "a", Type: a},
		},
	}
}

// A concrete value of a recursive schema is finite, so converting it to cty must
// terminate and every level the value reaches must keep its precise type.
func TestResourceOutputToCtyRecursiveValue(t *testing.T) {
	t.Parallel()

	// Descends two levels; the deepest A has no b, so the data terminates there.
	outputs := property.NewMap(map[string]property.Value{
		"a": property.New(property.NewMap(map[string]property.Value{
			"name": property.New("outer"),
			"b": property.New(property.NewMap(map[string]property.Value{
				"name": property.New("middle"),
				"as": property.New(property.NewArray([]property.Value{
					property.New(property.NewMap(map[string]property.Value{
						"name": property.New("inner"),
					})),
				})),
			})),
		})),
	})

	result, err := ResourceOutputToCty(outputs, recursiveResourceSchema(), nil, false)
	require.NoError(t, err)

	a := result["a"]
	require.True(t, a.Type().IsObjectType())
	assert.Equal(t, cty.String, a.GetAttr("name").Type())
	assert.Equal(t, "outer", a.GetAttr("name").AsString())

	b := a.GetAttr("b")
	require.True(t, b.Type().IsObjectType())
	assert.Equal(t, cty.String, b.GetAttr("name").Type())
	assert.Equal(t, "middle", b.GetAttr("name").AsString())

	as := b.GetAttr("as")
	require.True(t, as.Type().IsListType() || as.Type().IsTupleType())
	require.Equal(t, 1, as.LengthInt())

	inner := as.Index(cty.NumberIntVal(0))
	require.True(t, inner.Type().IsObjectType())
	assert.Equal(t, cty.String, inner.GetAttr("name").Type())
	assert.Equal(t, "inner", inner.GetAttr("name").AsString())

	// b was absent in the data, so the type stops unfolding here.
	assert.True(t, inner.GetAttr("b").IsNull())
}

// During preview a recursive output comes back unknown, so there is no value to
// bound the recursion; conversion must still terminate.
func TestResourceOutputToCtyRecursiveUnknown(t *testing.T) {
	t.Parallel()

	outputs := property.NewMap(map[string]property.Value{
		"a": property.New(property.Computed),
	})

	result, err := ResourceOutputToCty(outputs, recursiveResourceSchema(), nil, true)
	require.NoError(t, err)
	assert.False(t, result["a"].IsKnown())
}

// Typing a reference to a recursive resource has no value to bound the recursion,
// so the type unfolds its precise structure until the cycle closes, and only the
// frontier degrades to a dynamic type.
func TestResourceReferenceTypeRecursiveSchema(t *testing.T) {
	t.Parallel()

	ty := ResourceReferenceType(recursiveResourceSchema(), nil)
	require.True(t, ty.IsObjectType())

	a := ty.AttributeType("a")
	require.True(t, a.IsObjectType())
	assert.Equal(t, cty.String, a.AttributeType("name"))

	b := a.AttributeType("b")
	require.True(t, b.IsObjectType())
	assert.Equal(t, cty.String, b.AttributeType("name"))

	// b.as re-enters A, closing the cycle, so this is the frontier.
	assert.Equal(t, cty.DynamicPseudoType, b.AttributeType("as"))
}

// A single array can hold cyclic objects nested to different depths. Because the
// type is derived from each value, every element keeps the precise typing for its
// own depth rather than being forced to one shared element type.
func TestResourceOutputToCtyRecursiveArrayMixedDepth(t *testing.T) {
	t.Parallel()

	a := &schema.ObjectType{Token: "test:index:A"}
	b := &schema.ObjectType{
		Token: "test:index:B",
		Properties: []*schema.Property{
			{Name: "name", Type: schema.StringType},
			{Name: "as", Type: &schema.ArrayType{ElementType: a}},
		},
	}
	a.Properties = []*schema.Property{
		{Name: "name", Type: schema.StringType},
		{Name: "b", Type: b},
	}
	res := &schema.Resource{
		Token: "test:index:Res",
		Properties: []*schema.Property{
			{Name: "items", Type: &schema.ArrayType{ElementType: a}},
		},
	}

	// Three A values in one array at increasing depth: d0 stops at the top, d1
	// goes one level into b, d2 goes a further level into b.as.
	d0 := property.NewMap(map[string]property.Value{
		"name": property.New("d0"),
	})
	d1 := property.NewMap(map[string]property.Value{
		"name": property.New("d1"),
		"b": property.New(property.NewMap(map[string]property.Value{
			"name": property.New("d1b"),
		})),
	})
	d2 := property.NewMap(map[string]property.Value{
		"name": property.New("d2"),
		"b": property.New(property.NewMap(map[string]property.Value{
			"name": property.New("d2b"),
			"as": property.New(property.NewArray([]property.Value{
				property.New(property.NewMap(map[string]property.Value{
					"name": property.New("d2c"),
				})),
			})),
		})),
	})

	outputs := property.NewMap(map[string]property.Value{
		"items": property.New(property.NewArray([]property.Value{
			property.New(d0), property.New(d1), property.New(d2),
		})),
	})

	result, err := ResourceOutputToCty(outputs, res, nil, false)
	require.NoError(t, err)

	items := result["items"]
	require.True(t, items.Type().IsTupleType() || items.Type().IsListType())
	require.Equal(t, 3, items.LengthInt())

	e0 := items.Index(cty.NumberIntVal(0))
	assert.Equal(t, cty.String, e0.GetAttr("name").Type())
	assert.Equal(t, "d0", e0.GetAttr("name").AsString())

	e1 := items.Index(cty.NumberIntVal(1))
	assert.Equal(t, "d1b", e1.GetAttr("b").GetAttr("name").AsString())

	e2 := items.Index(cty.NumberIntVal(2))
	deep := e2.GetAttr("b").GetAttr("as").Index(cty.NumberIntVal(0)).GetAttr("name")
	assert.Equal(t, cty.String, deep.Type())
	assert.Equal(t, "d2c", deep.AsString())
}

// parseDynamicBlockResource parses src and returns the body of its single
// `resource` block together with a schema whose `settings` property is a
// list-shaped block with a string `name` field.
func parseDynamicBlockResource(t *testing.T, src string) (hcl.Body, *schema.Resource) {
	t.Helper()

	file, diags := hclsyntax.ParseConfig([]byte(src), "test.hcl", hcl.Pos{Line: 1, Column: 1})
	require.False(t, diags.HasErrors(), diags.Error())

	body := file.Body.(*hclsyntax.Body)
	var resourceBody hcl.Body
	for _, b := range body.Blocks {
		if b.Type == "resource" {
			resourceBody = b.Body
			break
		}
	}
	require.NotNil(t, resourceBody)

	return resourceBody, &schema.Resource{
		Token: "fake:index:F",
		InputProperties: []*schema.Property{{
			Name: "settings",
			Type: &schema.ArrayType{ElementType: &schema.ObjectType{
				Properties: []*schema.Property{{Name: "name", Type: schema.StringType}},
			}},
		}},
	}
}

func TestEvalDynamicBlocks_UnknownForEach(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		forEach  cty.Value
		expected property.Value
	}{
		{
			name:     "unknown collection",
			forEach:  cty.UnknownVal(cty.List(cty.Object(map[string]cty.Type{"name": cty.String}))),
			expected: property.New(property.Computed),
		},
		{
			name:     "unknown of dynamic type",
			forEach:  cty.DynamicVal,
			expected: property.New(property.Computed),
		},
		{
			name: "sensitive unknown collection",
			forEach: cty.UnknownVal(cty.List(cty.String)).
				WithMarks(cty.NewValueMarks(eval.SensitiveMark)),
			expected: property.New(property.Computed).WithSecret(true),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			resourceBody, r := parseDynamicBlockResource(t, `
resource "fake" "f" {
  dynamic "settings" {
    for_each = local.vpcs
    content {
      name = settings.value.name
    }
  }
}`)

			evalFn := func(_ resource.PropertyKey, expr hcl.Expression, extraVars map[string]cty.Value) (cty.Value, hcl.Diagnostics) {
				if extraVars == nil {
					return tt.forEach, nil
				}
				return expr.Value(&hcl.EvalContext{Variables: extraVars})
			}

			out, _, diags := EvalResourceWithSchema(resourceBody, r, nil, evalFn)
			require.False(t, diags.HasErrors(), "unexpected diags: %v", diags)
			assert.Equal(t, property.NewMap(map[string]property.Value{
				"settings": tt.expected,
			}), out)
		})
	}
}

// A known dynamic block cannot restore an enumerable value once a sibling
// dynamic block with an unknown for_each made the whole property unknown.
func TestEvalDynamicBlocks_UnknownForEachThenKnown(t *testing.T) {
	t.Parallel()

	resourceBody, r := parseDynamicBlockResource(t, `
resource "fake" "f" {
  dynamic "settings" {
    for_each = local.unknown
    content {
      name = settings.value
    }
  }
  dynamic "settings" {
    for_each = local.known
    content {
      name = settings.value
    }
  }
}`)

	evalFn := func(_ resource.PropertyKey, expr hcl.Expression, extraVars map[string]cty.Value) (cty.Value, hcl.Diagnostics) {
		if extraVars == nil {
			traversal := expr.Variables()[0]
			if traversal[len(traversal)-1].(hcl.TraverseAttr).Name == "unknown" {
				return cty.UnknownVal(cty.List(cty.String)), nil
			}
			return cty.ListVal([]cty.Value{cty.StringVal("p")}), nil
		}
		return expr.Value(&hcl.EvalContext{Variables: extraVars})
	}

	out, _, diags := EvalResourceWithSchema(resourceBody, r, nil, evalFn)
	require.False(t, diags.HasErrors(), "unexpected diags: %v", diags)
	assert.Equal(t, property.NewMap(map[string]property.Value{
		"settings": property.New(property.Computed),
	}), out)
}

func TestEvalDynamicBlocks_InvalidForEach(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		forEach cty.Value
		detail  string
	}{
		{
			name:    "null",
			forEach: cty.NullVal(cty.List(cty.String)),
			detail:  "Cannot use a null value in for_each.",
		},
		{
			name:    "not iterable",
			forEach: cty.StringVal("nope"),
			detail:  "Cannot use a string value in for_each. An iterable collection is required.",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			resourceBody, r := parseDynamicBlockResource(t, `
resource "fake" "f" {
  dynamic "settings" {
    for_each = local.vpcs
    content {
      name = settings.value
    }
  }
}`)

			evalFn := func(_ resource.PropertyKey, expr hcl.Expression, extraVars map[string]cty.Value) (cty.Value, hcl.Diagnostics) {
				if extraVars == nil {
					return tt.forEach, nil
				}
				return expr.Value(&hcl.EvalContext{Variables: extraVars})
			}

			_, _, diags := EvalResourceWithSchema(resourceBody, r, nil, evalFn)
			require.True(t, diags.HasErrors())
			assert.Equal(t, "Invalid dynamic for_each value", diags[0].Summary)
			assert.Equal(t, tt.detail, diags[0].Detail)
		})
	}
}

func TestConformCtyToType_UnknownAndNull(t *testing.T) {
	t.Parallel()

	objType := cty.Object(map[string]cty.Type{"a": cty.String})
	for _, val := range []cty.Value{cty.UnknownVal(objType), cty.NullVal(objType)} {
		assert.Equal(t, val, conformCtyToType(val, cty.Map(cty.String)))
	}
}

// TestConformCtyToType_CollectionElements pins that map values are converted
// to the target element type before the map is assembled: two lists of
// different lengths are differently typed tuples, and cty.MapVal would panic
// on them.
func TestConformCtyToType_CollectionElements(t *testing.T) {
	t.Parallel()

	obj := cty.Object(map[string]cty.Type{"name": cty.String})
	name := func(s string) cty.Value { return cty.ObjectVal(map[string]cty.Value{"name": cty.StringVal(s)}) }

	val := cty.ObjectVal(map[string]cty.Value{
		"left":  cty.TupleVal([]cty.Value{name("a"), name("b")}),
		"right": cty.TupleVal([]cty.Value{name("c")}),
	})

	assert.Equal(t, cty.MapVal(map[string]cty.Value{
		"left":  cty.ListVal([]cty.Value{name("a"), name("b")}),
		"right": cty.ListVal([]cty.Value{name("c")}),
	}), conformCtyToType(val, cty.Map(cty.List(obj))))
}

// TestConformCtyToType_UnconvertibleElement pins that an element that cannot be
// converted to the map's element type leaves the value as an object rather than
// panicking in cty.MapVal.
func TestConformCtyToType_UnconvertibleElement(t *testing.T) {
	t.Parallel()

	val := cty.ObjectVal(map[string]cty.Value{
		"list":   cty.ListVal([]cty.Value{cty.StringVal("a")}),
		"number": cty.NumberIntVal(1),
	})

	assert.Equal(t, val, conformCtyToType(val, cty.Map(cty.List(cty.String))))
}

// TestUnifyPreservesNestedElementTypes pins that a heterogeneous collection
// whose unification would coerce primitive leaves — at any depth — keeps its
// per-element types instead of collapsing to a common type. cty unifies
// list(number) and list(string) to list(string), which would stringify the
// numbers (observable via jsonencode); OpenTofu keeps each element's own type.
func TestUnifyPreservesNestedElementTypes(t *testing.T) {
	t.Parallel()

	nums := cty.ListVal([]cty.Value{cty.NumberIntVal(1), cty.NumberIntVal(2)})
	strs := cty.ListVal([]cty.Value{cty.StringVal("a"), cty.StringVal("b")})

	assert.Equal(t,
		cty.ObjectVal(map[string]cty.Value{"nums": nums, "strs": strs}),
		unifyOrObject(map[string]cty.Value{"nums": nums, "strs": strs}))
	assert.Equal(t,
		cty.TupleVal([]cty.Value{nums, strs}),
		unifyOrTuple([]cty.Value{nums, strs}))

	// A structural unification whose primitive leaves are untouched still
	// collapses: object{a:string} and map(string) unify to map(string).
	obj := cty.ObjectVal(map[string]cty.Value{"a": cty.StringVal("x")})
	mp := cty.MapVal(map[string]cty.Value{"b": cty.StringVal("y")})
	assert.Equal(t,
		cty.MapVal(map[string]cty.Value{
			"o": cty.MapVal(map[string]cty.Value{"a": cty.StringVal("x")}),
			"m": mp,
		}),
		unifyOrObject(map[string]cty.Value{"o": obj, "m": mp}))
}
