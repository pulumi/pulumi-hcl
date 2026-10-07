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
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/pulumi/pulumi-hcl/pkg/codegen"
	"github.com/pulumi/pulumi-hcl/tests/testutil/schemaloader"
	pclsyntax "github.com/pulumi/pulumi/pkg/v3/codegen/hcl2/syntax"
	"github.com/pulumi/pulumi/pkg/v3/codegen/pcl"
	"github.com/pulumi/pulumi/pkg/v3/codegen/schema"
	"github.com/pulumi/pulumi/pkg/v3/resource/plugin"
	"github.com/pulumi/pulumi/sdk/v3/go/common/util/contract"
	"github.com/pulumi/pulumi/sdk/v3/go/common/util/rpcutil"
	"github.com/pulumi/pulumi/sdk/v3/go/common/workspace"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zclconf/go-cty/cty"
	"google.golang.org/grpc"
)

// transformSingleFile is a test helper that runs the project transformer over
// a single HCL file. It mirrors the per-file pipeline used by ConvertProgram
// so tests can exercise emitFile without standing up a directory on disk.
func transformSingleFile(
	t *testing.T,
	src []byte, filename string, out *hclwrite.Body,
	loader schema.ReferenceLoader,
	paramInfos map[string]workspace.PackageDescriptor,
) hcl.Diagnostics {
	t.Helper()
	f, parseDiags := hclsyntax.ParseConfig(src, filename, hcl.Pos{Line: 1, Column: 1})
	require.False(t, parseDiags.HasErrors(), parseDiags.Error())
	body := f.Body.(*hclsyntax.Body)
	ft, scanDiags := newProjectTransformer(t.Context(), loader, []*hclsyntax.Body{body})
	require.False(t, scanDiags.HasErrors(), scanDiags.Error())
	emitDiags, err := ft.emitFile(t.Context(), src, filename, body, out, paramInfos)
	require.NoError(t, err)
	return emitDiags
}

func TestPropertiesOf(t *testing.T) {
	t.Parallel()

	objType := &schema.ObjectType{
		Properties: []*schema.Property{
			{Name: "foo", Type: schema.StringType},
		},
	}

	t.Run("nil", func(t *testing.T) {
		t.Parallel()
		assert.Nil(t, propertiesOf(nil))
	})
	t.Run("object", func(t *testing.T) {
		t.Parallel()
		assert.Equal(t, objType.Properties, propertiesOf(objType))
	})
	t.Run("non-object", func(t *testing.T) {
		t.Parallel()
		assert.Nil(t, propertiesOf(schema.StringType))
	})
	t.Run("optional-object", func(t *testing.T) {
		t.Parallel()
		assert.Equal(t, objType.Properties, propertiesOf(&schema.OptionalType{ElementType: objType}))
	})
	t.Run("array-not-unwrapped", func(t *testing.T) {
		t.Parallel()
		assert.Nil(t, propertiesOf(&schema.ArrayType{ElementType: objType}))
	})
}

func TestElementTypeOf(t *testing.T) {
	t.Parallel()

	t.Run("nil", func(t *testing.T) {
		t.Parallel()
		assert.Nil(t, elementTypeOf(nil))
	})
	t.Run("map", func(t *testing.T) {
		t.Parallel()
		assert.Equal(t, schema.StringType, elementTypeOf(&schema.MapType{ElementType: schema.StringType}))
	})
	t.Run("array", func(t *testing.T) {
		t.Parallel()
		assert.Equal(t, schema.IntType, elementTypeOf(&schema.ArrayType{ElementType: schema.IntType}))
	})
	t.Run("non-container", func(t *testing.T) {
		t.Parallel()
		assert.Nil(t, elementTypeOf(schema.StringType))
	})
}

func TestSchemaAwareTraversalAttrs(t *testing.T) {
	t.Parallel()

	nestedObj := &schema.ObjectType{
		Properties: []*schema.Property{
			{Name: "nested_output", Type: schema.StringType},
		},
	}
	topProps := []*schema.Property{
		{Name: "the_output", Type: &schema.MapType{
			ElementType: &schema.ArrayType{
				ElementType: nestedObj,
			},
		}},
	}

	trav := hcl.Traversal{
		hcl.TraverseRoot{Name: "first"},
		hcl.TraverseAttr{Name: "the_output"},
		hcl.TraverseIndex{Key: cty.StringVal("someKey")},
		hcl.TraverseIndex{Key: cty.NumberIntVal(0)},
		hcl.TraverseAttr{Name: "nested_output"},
	}

	result := schemaAwareTraversalAttrs(trav, topProps)
	require.Len(t, result, 5)
	assert.Equal(t, "the_output", result[1].(hcl.TraverseAttr).Name)
	assert.Equal(t, "nested_output", result[4].(hcl.TraverseAttr).Name)
}

func TestSchemaAwareTraversalAttrs_CamelCase(t *testing.T) {
	t.Parallel()

	nestedObj := &schema.ObjectType{
		Properties: []*schema.Property{
			{Name: "nestedOutput", Type: schema.StringType},
		},
	}
	topProps := []*schema.Property{
		{Name: "theOutput", Type: &schema.MapType{
			ElementType: &schema.ArrayType{
				ElementType: nestedObj,
			},
		}},
	}

	trav := hcl.Traversal{
		hcl.TraverseRoot{Name: "first"},
		hcl.TraverseAttr{Name: "the_output"},
		hcl.TraverseIndex{Key: cty.StringVal("someKey")},
		hcl.TraverseIndex{Key: cty.NumberIntVal(0)},
		hcl.TraverseAttr{Name: "nested_output"},
	}

	result := schemaAwareTraversalAttrs(trav, topProps)
	require.Len(t, result, 5)
	assert.Equal(t, "theOutput", result[1].(hcl.TraverseAttr).Name)
	assert.Equal(t, "nestedOutput", result[4].(hcl.TraverseAttr).Name)
}

func TestBlocksToObjectAttrs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		hcl      string
		props    []*schema.Property
		expected string
	}{
		{
			name: "snake_case attrs preserved",
			hcl: `outer {
  outer_attr = "hello"
}`,
			props: []*schema.Property{
				{Name: "outer", Type: &schema.ArrayType{
					ElementType: &schema.ObjectType{
						Properties: []*schema.Property{
							{Name: "outer_attr", Type: schema.StringType},
						},
					},
				}},
			},
			expected: "{\n  outer = [{\n    outer_attr = \"hello\"\n  }]\n}",
		},
		{
			name: "camelCase attrs from snake_case input",
			hcl: `outer {
  outer_attr = "hello"
}`,
			props: []*schema.Property{
				{Name: "outer", Type: &schema.ArrayType{
					ElementType: &schema.ObjectType{
						Properties: []*schema.Property{
							{Name: "outerAttr", Type: schema.StringType},
						},
					},
				}},
			},
			expected: "{\n  outer = [{\n    outerAttr = \"hello\"\n  }]\n}",
		},
		{
			name: "nested blocks with attrs",
			hcl: `outer {
  outer_attr = "hello"
  inner {
    inner_value = "world"
  }
}`,
			props: []*schema.Property{
				{Name: "outer", Type: &schema.ArrayType{
					ElementType: &schema.ObjectType{
						Properties: []*schema.Property{
							{Name: "outer_attr", Type: schema.StringType},
							{Name: "inner", Type: &schema.ArrayType{
								ElementType: &schema.ObjectType{
									Properties: []*schema.Property{
										{Name: "inner_value", Type: schema.StringType},
									},
								},
							}},
						},
					},
				}},
			},
			expected: "{\n  outer = [{\n    outer_attr = \"hello\"\n    inner = [{\n      inner_value = \"world\"\n    }]\n  }]\n}",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			src := []byte(tt.hcl)
			file, diags := hclsyntax.ParseConfig(src, "test.tf", hcl.Pos{})
			require.False(t, diags.HasErrors(), diags.Error())

			ft := &fileTransformer{sources: map[string][]byte{"test.tf": src}}
			result := ft.blocksToObjectAttrs(file.Body.(*hclsyntax.Body).Blocks, tt.props)
			assert.Equal(t, tt.expected, string(hclwrite.TokensForObject(result).Bytes()))
		})
	}
}

func TestTransformFunctionCall(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		hcl      string
		expected string
	}{
		{
			name:     "pcl supported function passes through",
			hcl:      `value = join(", ", split(",", "a,b,c"))`,
			expected: `join(", ", split(",", "a,b,c"))`,
		},
		{
			name:     "hcl function mapped to pcl equivalent",
			hcl:      `value = base64encode("hello")`,
			expected: `toBase64("hello")`,
		},
		{
			name:     "hcl-only function wrapped in notImplemented",
			hcl:      `value = upper("hello")`,
			expected: `notImplemented("upper(\"hello\")")`,
		},
		{
			name:     "hcl-only function with nested args wrapped in notImplemented",
			hcl:      `value = lower(format("Hello %s", "world"))`,
			expected: `notImplemented("lower(format(\"Hello %s\", \"world\"))")`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			src := []byte(tt.name + " {\n  " + tt.hcl + "\n}\n")
			file, diags := hclsyntax.ParseConfig(src, "test.tf", hcl.Pos{})
			require.False(t, diags.HasErrors(), diags.Error())

			ft := &fileTransformer{sources: map[string][]byte{"test.tf": src}}
			body := file.Body.(*hclsyntax.Body)
			require.Len(t, body.Blocks, 1)
			require.Contains(t, body.Blocks[0].Body.Attributes, "value")

			tokens := ft.transformExpr(body.Blocks[0].Body.Attributes["value"].Expr)
			assert.Equal(t, tt.expected, string(tokens.Bytes()))
		})
	}
}

// TestEjectDataBlockWithForEach verifies that an HCL program containing a data
// block with `for_each` (the shape produced by the PCL → HCL codegen for invokes
// that close over range.*) round-trips back to valid PCL. The inlined invoke
// picks up the resource's range iterator via `range.value`, and the per-instance
// indexing (`[each.key]`) collapses into the single inlined invoke expression.
func TestEjectDataBlockWithForEach(t *testing.T) {
	t.Parallel()

	testSchema := schema.PackageSpec{
		Name:    "test",
		Version: "1.0.0",
		Resources: map[string]schema.ResourceSpec{
			"test:index:Item": {
				InputProperties: map[string]schema.PropertySpec{
					"value": {TypeSpec: schema.TypeSpec{Type: "string"}},
				},
				ObjectTypeSpec: schema.ObjectTypeSpec{
					Properties: map[string]schema.PropertySpec{
						"value": {TypeSpec: schema.TypeSpec{Type: "string"}},
					},
				},
			},
		},
		Functions: map[string]schema.FunctionSpec{
			"test:index:echo": {
				Inputs: &schema.ObjectTypeSpec{
					Properties: map[string]schema.PropertySpec{
						"input": {TypeSpec: schema.TypeSpec{Type: "string"}},
					},
				},
				Outputs: &schema.ObjectTypeSpec{
					Properties: map[string]schema.PropertySpec{
						"result": {TypeSpec: schema.TypeSpec{Type: "string"}},
					},
				},
			},
		},
	}
	loader := schemaloader.New(t, testSchema)

	src := []byte(`terraform {
  required_providers {
    test = {
      source  = "pulumi/test"
      version = "1.0.0"
    }
  }
}

data "test_echo" "invoke_0" {
  for_each = {
    "a" = "alpha"
    "b" = "bravo"
  }
  input = each.value
}

resource "test_item" "inbound" {
  for_each = {
    "a" = "alpha"
    "b" = "bravo"
  }
  value = data.test_echo.invoke_0[each.key].result
}
`)

	out := hclwrite.NewEmptyFile()
	diags := transformSingleFile(t, src, "main.tf", out.Body(), loader, nil)
	require.False(t, diags.HasErrors(), diags.Error())

	expected := `resource "inbound" "test:index:Item" {
  value = invoke("test:index:echo", {
    input = range.value
  }).result
  options {
    range = {
      "a" = "alpha"
      "b" = "bravo"
    }
    deleteBeforeReplace = true
  }
}

`
	assert.Equal(t, expected, string(out.Bytes()))
}

// TestEjectInvokeInListComprehension feeds in the HCL shape produced by the
// codegen when a PCL invoke inside a list comprehension is hoisted: a data
// block with for_each over the comprehension's collection, plus a
// comprehension that indexes the data source by its key variable. The eject
// should inline the invoke back inside the comprehension, with references to
// each.value / each.key rewritten to the comprehension's iteration
// variables — NOT to range.* (which would be unbound inside a comprehension).
func TestEjectInvokeInListComprehension(t *testing.T) {
	t.Parallel()

	testSchema := schema.PackageSpec{
		Name:    "test",
		Version: "1.0.0",
		Functions: map[string]schema.FunctionSpec{
			"test:index:echo": {
				Inputs: &schema.ObjectTypeSpec{
					Properties: map[string]schema.PropertySpec{
						"input": {TypeSpec: schema.TypeSpec{Type: "string"}},
					},
				},
				Outputs: &schema.ObjectTypeSpec{
					Properties: map[string]schema.PropertySpec{
						"result": {TypeSpec: schema.TypeSpec{Type: "string"}},
					},
				},
			},
		},
	}
	loader := schemaloader.New(t, testSchema)
	src := []byte(`terraform {
  required_providers {
    test = {
      source  = "pulumi/test"
      version = "1.0.0"
    }
  }
}

data "test_echo" "invoke_0" {
  for_each = {
    "a" = "alpha"
    "b" = "bravo"
  }
  input = each.value
}

output "results" {
  value = [for k, v in {
    "a" = "alpha"
    "b" = "bravo"
  } : data.test_echo.invoke_0[k].result]
}
`)

	out := hclwrite.NewEmptyFile()
	diags := transformSingleFile(t, src, "main.tf", out.Body(), loader, nil)
	require.False(t, diags.HasErrors(), diags.Error())

	expected := `output "results" {
  value = [for k, v in {
    "a" = "alpha"
    "b" = "bravo"
    } : invoke("test:index:echo", {
      input = v
  }).result]
}

`
	assert.Equal(t, expected, string(out.Bytes()))
}

func TestTransformSplatExpr(t *testing.T) {
	t.Parallel()

	// Schema: myRes has a property "detailItems" (camelCase) of type
	// list(object({ nestedValue: string })).
	// In HCL this would be written as "detail_items" and "nested_value".
	innerObj := &schema.ObjectType{
		Properties: []*schema.Property{
			{Name: "nestedValue", Type: schema.StringType},
		},
	}

	tests := []struct {
		name     string
		hcl      string
		schemas  map[string]*schema.Resource
		expected string
	}{
		{
			name: "splat with camelCase schema property",
			hcl:  `value = my_res.source.detail_items[*].nested_value`,
			schemas: map[string]*schema.Resource{
				"my_res": {
					Properties: []*schema.Property{
						{Name: "detailItems", Type: &schema.ArrayType{
							ElementType: innerObj,
						}},
					},
				},
			},
			expected: `source.detailItems[*].nestedValue`,
		},
		{
			name: "splat with snake_case schema property",
			hcl:  `value = my_res.source.detail_items[*].nested_value`,
			schemas: map[string]*schema.Resource{
				"my_res": {
					Properties: []*schema.Property{
						{Name: "detail_items", Type: &schema.ArrayType{
							ElementType: &schema.ObjectType{
								Properties: []*schema.Property{
									{Name: "nested_value", Type: schema.StringType},
								},
							},
						}},
					},
				},
			},
			expected: `source.detail_items[*].nested_value`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			src := []byte(tt.name + " {\n  " + tt.hcl + "\n}\n")
			file, diags := hclsyntax.ParseConfig(src, "test.tf", hcl.Pos{})
			require.False(t, diags.HasErrors(), diags.Error())

			ft := &fileTransformer{
				sources:         map[string][]byte{"test.tf": src},
				knownHCLTypes:   map[string]bool{"my_res": true},
				resourceSchemas: tt.schemas,
			}

			body := file.Body.(*hclsyntax.Body)
			require.Len(t, body.Blocks, 1)
			require.Contains(t, body.Blocks[0].Body.Attributes, "value")

			tokens := ft.transformExpr(body.Blocks[0].Body.Attributes["value"].Expr)
			assert.Equal(t, tt.expected, string(tokens.Bytes()))
		})
	}
}

// multiFileTestSchema is a minimal schema with one resource type used by the
// multi-file conversion tests.
var multiFileTestSchema = schema.PackageSpec{
	Name:    "test",
	Version: "1.0.0",
	Resources: map[string]schema.ResourceSpec{
		"test:index:Item": {
			InputProperties: map[string]schema.PropertySpec{
				"value": {TypeSpec: schema.TypeSpec{Type: "string"}},
			},
			ObjectTypeSpec: schema.ObjectTypeSpec{
				Properties: map[string]schema.PropertySpec{
					"value": {TypeSpec: schema.TypeSpec{Type: "string"}},
				},
			},
		},
	},
}

func parseAndBindMultiFile(
	t *testing.T,
	loader schema.ReferenceLoader,
	files map[string]string,
) *pcl.Program {
	t.Helper()
	parser := pclsyntax.NewParser()
	for name, content := range files {
		require.NoError(t, parser.ParseFile(strings.NewReader(content), name))
	}
	require.False(t, parser.Diagnostics.HasErrors(), parser.Diagnostics.Error())

	program, diags, err := pcl.BindProgram(parser.Files, loader)
	require.NoError(t, err)
	require.False(t, diags.HasErrors(), diags.Error())
	require.NotNil(t, program)
	return program
}

// serveLoader hosts loader on a local gRPC port and returns the host:port
// address. The server is torn down via t.Cleanup.
func serveLoader(t *testing.T, loader schema.ReferenceLoader) string {
	t.Helper()
	cancel := make(chan bool)
	handle, err := rpcutil.ServeWithOptions(rpcutil.ServeOptions{
		Init: func(srv *grpc.Server) error {
			schema.LoaderRegistration(schema.NewLoaderServer(loader))(srv)
			return nil
		},
		Cancel: cancel,
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		close(cancel)
		contract.IgnoreError(<-handle.Done)
	})
	return fmt.Sprintf("127.0.0.1:%d", handle.Port)
}

// TestGenerateProgramMultiFile verifies that converting a multi-file PCL
// program to HCL preserves the per-file structure: each <name>.pp produces a
// <name>.tf, with the `terraform { required_providers { ... } }` block placed
// in the file that declared the `package` block and resources/outputs placed
// in the file that declared each node.
func TestGenerateProgramMultiFile(t *testing.T) {
	t.Parallel()

	loader := schemaloader.New(t, multiFileTestSchema)

	program := parseAndBindMultiFile(t, loader, map[string]string{
		"main.pp": `resource "thing" "test:index:Item" {
    value = "hello"
}
`,
		"outputs.pp": `output "value" {
    value = thing.value
}
`,
		"providers.pp": `package "test" {
    baseProviderName = "test"
    baseProviderVersion = "1.0.0"
}
`,
	})

	filesB, diags, err := codegen.GenerateProgram(program)
	require.NoError(t, err)
	require.False(t, diags.HasErrors(), diags.Error())

	files := make(map[string]string, len(filesB))
	for k, v := range filesB {
		files[k] = string(v)
	}

	assert.Equal(t, map[string]string{
		"main.tf": `resource "test_item" "thing" {
  lifecycle {
    create_before_destroy = true
  }
  value = "hello"
}
`,
		"outputs.tf": `output "value" {
  value = test_item.thing.value
}
`,
		"providers.tf": `terraform {
  required_providers {
    test = {
      source  = "pulumi/test"
      version = "1.0.0"
    }
  }
}

`,
	}, files)
}

// TestCreateBeforeDestroyDefault locks in the PCL → HCL codegen rule for
// `lifecycle.create_before_destroy`. Pulumi's default replace mode is
// create-then-delete; Terraform's is the opposite. So:
//
//   - PCL with no `deleteBeforeReplace`: emit `create_before_destroy = true`
//     (Pulumi default, the inverse of TF's, must be made explicit).
//   - PCL `deleteBeforeReplace = true` (literal): emit no `create_before_destroy`
//     — TF's default already matches Pulumi's request.
//   - PCL `deleteBeforeReplace = false` (literal): equivalent to no option; emit
//     `create_before_destroy = true`.
func TestCreateBeforeDestroyDefault(t *testing.T) {
	t.Parallel()

	loader := schemaloader.New(t, multiFileTestSchema)

	cases := []struct {
		name string
		pcl  string
		want string
	}{
		{
			name: "no options",
			pcl: `resource "thing" "test:index:Item" {
    value = "hello"
}
`,
			want: `resource "test_item" "thing" {
  lifecycle {
    create_before_destroy = true
  }
  value = "hello"
}
`,
		},
		{
			name: "deleteBeforeReplace = true",
			pcl: `resource "thing" "test:index:Item" {
    value = "hello"
    options {
        deleteBeforeReplace = true
    }
}
`,
			want: `resource "test_item" "thing" {
  value = "hello"
}
`,
		},
		{
			name: "deleteBeforeReplace = false",
			pcl: `resource "thing" "test:index:Item" {
    value = "hello"
    options {
        deleteBeforeReplace = false
    }
}
`,
			want: `resource "test_item" "thing" {
  lifecycle {
    create_before_destroy = true
  }
  value = "hello"
}
`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			program := parseAndBindMultiFile(t, loader, map[string]string{
				"main.pp": tc.pcl,
				"providers.pp": `package "test" {
    baseProviderName = "test"
    baseProviderVersion = "1.0.0"
}
`,
			})
			filesB, diags, err := codegen.GenerateProgram(program)
			require.NoError(t, err)
			require.False(t, diags.HasErrors(), diags.Error())
			assert.Equal(t, tc.want, string(filesB["main.tf"]))
		})
	}
}

// TestConvertProgramCrossFileReferences locks in the multi-file fix by
// converting a project where every kind of cross-file reference appears, and
// asserting on the full output of every emitted PCL file:
//
//   - A `pulumi.required_providers` entry declared in providers.tf must be
//     visible to data-source resolution in sibling files (otherwise the
//     test_echo data block in data.tf would fail to resolve).
//   - A resource in main.tf is referenced from outputs.tf. The reference
//     `test_item.thing.value` must rewrite to the PCL form `thing.value`.
//   - A data block in data.tf is referenced from outputs.tf. The reference
//     `data.test_echo.lookup.result` must be inlined as an `invoke(...)`
//     expression, with the data block's argument literals slurped from
//     data.tf's source bytes — not the active file's bytes.
func TestConvertProgramCrossFileReferences(t *testing.T) {
	t.Parallel()

	testSchema := schema.PackageSpec{
		Name:    "test",
		Version: "1.0.0",
		Resources: map[string]schema.ResourceSpec{
			"test:index:Item": {
				InputProperties: map[string]schema.PropertySpec{
					"value": {TypeSpec: schema.TypeSpec{Type: "string"}},
				},
				ObjectTypeSpec: schema.ObjectTypeSpec{
					Properties: map[string]schema.PropertySpec{
						"value": {TypeSpec: schema.TypeSpec{Type: "string"}},
					},
				},
			},
		},
		Functions: map[string]schema.FunctionSpec{
			"test:index:echo": {
				Inputs: &schema.ObjectTypeSpec{
					Properties: map[string]schema.PropertySpec{
						"input": {TypeSpec: schema.TypeSpec{Type: "string"}},
					},
				},
				Outputs: &schema.ObjectTypeSpec{
					Properties: map[string]schema.PropertySpec{
						"result": {TypeSpec: schema.TypeSpec{Type: "string"}},
					},
				},
			},
		},
	}
	loader := schemaloader.New(t, testSchema)
	loaderTarget := serveLoader(t, loader)

	sourceDir := t.TempDir()
	targetDir := t.TempDir()

	require.NoError(t, os.WriteFile(filepath.Join(sourceDir, "Pulumi.yaml"),
		[]byte("name: cross-file\nruntime: hcl\n"), 0o644))

	hclFiles := map[string]string{
		"providers.tf": `terraform {
  required_providers {
    test = {
      source  = "pulumi/test"
      version = "1.0.0"
    }
  }
}
`,
		"main.tf": `resource "test_item" "thing" {
  value = "hello"
}
`,
		"data.tf": `data "test_echo" "lookup" {
  input = "the-input-from-data-hcl"
}
`,
		"outputs.tf": `output "value" {
  value = test_item.thing.value
}

output "echoed" {
  value = data.test_echo.lookup.result
}
`,
	}
	for name, content := range hclFiles {
		require.NoError(t, os.WriteFile(filepath.Join(sourceDir, name), []byte(content), 0o644))
	}

	resp, err := New().ConvertProgram(t.Context(), &plugin.ConvertProgramRequest{
		SourceDirectory: sourceDir,
		TargetDirectory: targetDir,
		LoaderTarget:    loaderTarget,
	})
	require.NoError(t, err)
	require.False(t, resp.Diagnostics.HasErrors(), resp.Diagnostics.Error())

	pclFiles := map[string]string{}
	entries, err := os.ReadDir(targetDir)
	require.NoError(t, err)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".pp") {
			continue
		}
		bytes, err := os.ReadFile(filepath.Join(targetDir, e.Name()))
		require.NoError(t, err)
		pclFiles[e.Name()] = string(bytes)
	}

	assert.Equal(t, map[string]string{
		"providers.pp": "",
		"main.pp": `resource "thing" "test:index:Item" {
  value = "hello"
  options {
    deleteBeforeReplace = true
  }
}

`,
		"data.pp": "",
		"outputs.pp": `output "value" {
  value = thing.value
}

output "echoed" {
  value = invoke("test:index:echo", {
    input = "the-input-from-data-hcl"
  }).result
}

`,
	}, pclFiles)
}

// TestEjectMultiFileHCL verifies the inverse direction: a multi-file HCL
// project converted via hclConverter.ConvertProgram produces the same
// per-file structure in PCL, with cross-file references rewritten correctly
// because the project transformer pre-scans every body before emitting any
// file.
func TestEjectMultiFileHCL(t *testing.T) {
	t.Parallel()

	loader := schemaloader.New(t, multiFileTestSchema)
	loaderTarget := serveLoader(t, loader)

	sourceDir := t.TempDir()
	targetDir := t.TempDir()

	require.NoError(t, os.WriteFile(filepath.Join(sourceDir, "Pulumi.yaml"),
		[]byte("name: multifile-eject\nruntime: hcl\n"), 0o644))

	hclFiles := map[string]string{
		"main.tf": `resource "test_item" "thing" {
  value = "hello"
}
`,
		"outputs.tf": `output "value" {
  value = test_item.thing.value
}
`,
		"providers.tf": `terraform {
  required_providers {
    test = {
      source  = "pulumi/test"
      version = "1.0.0"
    }
  }
}
`,
	}
	for name, content := range hclFiles {
		require.NoError(t, os.WriteFile(filepath.Join(sourceDir, name), []byte(content), 0o644))
	}

	resp, err := New().ConvertProgram(t.Context(), &plugin.ConvertProgramRequest{
		SourceDirectory: sourceDir,
		TargetDirectory: targetDir,
		LoaderTarget:    loaderTarget,
	})
	require.NoError(t, err)
	require.False(t, resp.Diagnostics.HasErrors(), resp.Diagnostics.Error())

	pclFiles := map[string]string{}
	entries, err := os.ReadDir(targetDir)
	require.NoError(t, err)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".pp") {
			continue
		}
		bytes, err := os.ReadFile(filepath.Join(targetDir, e.Name()))
		require.NoError(t, err)
		pclFiles[e.Name()] = string(bytes)
	}

	assert.Equal(t, map[string]string{
		"main.pp": `resource "thing" "test:index:Item" {
  value = "hello"
  options {
    deleteBeforeReplace = true
  }
}

`,
		"outputs.pp": `output "value" {
  value = thing.value
}

`,
		"providers.pp": "",
	}, pclFiles)
}

// TestEjectEscapedArguments feeds in the `_` escaping blocks the codegen emits
// for arguments whose names collide with meta-arguments: the escaped
// arguments convert to inputs, while the top-level meta-arguments of the same
// names still convert to options.
func TestEjectEscapedArguments(t *testing.T) {
	t.Parallel()

	testSchema := schema.PackageSpec{
		Name:    "test",
		Version: "1.0.0",
		Resources: map[string]schema.ResourceSpec{
			"test:index:Item": {
				InputProperties: map[string]schema.PropertySpec{
					"count":    {TypeSpec: schema.TypeSpec{Type: "integer"}},
					"provider": {TypeSpec: schema.TypeSpec{Type: "string"}},
					"value":    {TypeSpec: schema.TypeSpec{Type: "string"}},
				},
			},
		},
		Functions: map[string]schema.FunctionSpec{
			"test:index:echo": {
				Inputs: &schema.ObjectTypeSpec{
					Properties: map[string]schema.PropertySpec{
						"provider": {TypeSpec: schema.TypeSpec{Type: "string"}},
					},
				},
				Outputs: &schema.ObjectTypeSpec{
					Properties: map[string]schema.PropertySpec{
						"result": {TypeSpec: schema.TypeSpec{Type: "string"}},
					},
				},
			},
		},
	}
	loader := schemaloader.New(t, testSchema)

	src := []byte(`terraform {
  required_providers {
    test = {
      source  = "pulumi/test"
      version = "1.0.0"
    }
  }
}

data "test_echo" "lookup" {
  _ {
    provider = "escaped"
  }
}

resource "test_item" "escaped" {
  count = 2
  value = data.test_echo.lookup.result
  _ {
    count    = 3
    provider = "literal"
  }
}

module "child" {
  source = "./child"
  _ {
    source = "escaped"
  }
}
`)

	out := hclwrite.NewEmptyFile()
	diags := transformSingleFile(t, src, "main.tf", out.Body(), loader, nil)
	require.False(t, diags.HasErrors(), diags.Error())

	expected := `resource "escaped" "test:index:Item" {
  value = invoke("test:index:echo", {
    provider = "escaped"
  }).result
  count    = 3
  provider = "literal"
  options {
    range               = 2
    deleteBeforeReplace = true
  }
}

component "child" "./child" {
  source = "escaped"
}

`
	assert.Equal(t, expected, string(out.Bytes()))
}

// TestEjectTopLevelOptionNamesAreInputs checks that the converter reserves
// the same top-level names as the parser: an attribute named after an option
// that lives in the `pulumi` block (`version`, `protect`, ...) is an input
// property, while the `pulumi` block still supplies the option.
func TestEjectTopLevelOptionNamesAreInputs(t *testing.T) {
	t.Parallel()

	testSchema := schema.PackageSpec{
		Name:    "test",
		Version: "1.0.0",
		Resources: map[string]schema.ResourceSpec{
			"test:index:Item": {
				InputProperties: map[string]schema.PropertySpec{
					"protect": {TypeSpec: schema.TypeSpec{Type: "boolean"}},
					"version": {TypeSpec: schema.TypeSpec{Type: "string"}},
				},
			},
		},
		Functions: map[string]schema.FunctionSpec{
			"test:index:echo": {
				Inputs: &schema.ObjectTypeSpec{
					Properties: map[string]schema.PropertySpec{
						"version": {TypeSpec: schema.TypeSpec{Type: "string"}},
					},
				},
				Outputs: &schema.ObjectTypeSpec{
					Properties: map[string]schema.PropertySpec{
						"result": {TypeSpec: schema.TypeSpec{Type: "string"}},
					},
				},
			},
		},
	}
	loader := schemaloader.New(t, testSchema)

	src := []byte(`terraform {
  required_providers {
    test = {
      source  = "pulumi/test"
      version = "1.0.0"
    }
  }
}

data "test_echo" "lookup" {
  version = "v2"
  pulumi {
    version = "2.0.0"
  }
}

resource "test_item" "item" {
  version = "v1"
  protect = true
  pulumi {
    protect = false
  }
}

output "result" {
  value = data.test_echo.lookup.result
}
`)

	out := hclwrite.NewEmptyFile()
	diags := transformSingleFile(t, src, "main.tf", out.Body(), loader, nil)
	require.False(t, diags.HasErrors(), diags.Error())

	expected := `resource "item" "test:index:Item" {
  version = "v1"
  protect = true
  options {
    protect             = false
    deleteBeforeReplace = true
  }
}

output "result" {
  value = invoke("test:index:echo", {
    version = "v2"
    }, {
    version = "2.0.0"
  }).result
}

`
	assert.Equal(t, expected, string(out.Bytes()))
}

// TestEjectRangedProvider converts a provider block with `for_each` to a PCL
// provider resource with a `range` option, and a keyed provider reference to
// an index into that resource.
func TestEjectRangedProvider(t *testing.T) {
	t.Parallel()

	testSchema := schema.PackageSpec{
		Name:    "test",
		Version: "1.0.0",
		Provider: &schema.ResourceSpec{
			InputProperties: map[string]schema.PropertySpec{
				"prefix": {TypeSpec: schema.TypeSpec{Type: "string"}},
			},
		},
		Resources: map[string]schema.ResourceSpec{
			"test:index:Item": {
				InputProperties: map[string]schema.PropertySpec{
					"value": {TypeSpec: schema.TypeSpec{Type: "string"}},
				},
			},
		},
	}
	loader := schemaloader.New(t, testSchema)

	src := []byte(`terraform {
  required_providers {
    test = {
      source  = "pulumi/test"
      version = "1.0.0"
    }
  }
}

provider "test" {
  alias = "by_key"
  for_each = {
    a = "alpha"
    b = "beta"
  }
  prefix = each.value
}

resource "test_item" "fixed" {
  provider = test.by_key["a"]
  value    = "fixed"
}
`)

	out := hclwrite.NewEmptyFile()
	diags := transformSingleFile(t, src, "main.tf", out.Body(), loader, nil)
	require.False(t, diags.HasErrors(), diags.Error())

	expected := `resource "by_key" "pulumi:providers:test" {
  prefix = range.value
  options {
    range = {
      a = "alpha"
      b = "beta"
    }
  }
}

resource "fixed" "test:index:Item" {
  value = "fixed"
  options {
    provider            = by_key["a"]
    deleteBeforeReplace = true
  }
}

`
	assert.Equal(t, expected, string(out.Bytes()))
}

// TestEjectObjectLiteralKeys checks that the keys of an object literal get
// their names from the schema type of the input at each position that the
// conformance tests do not reach. The schema uses snake_case names, so a
// conversion to camelCase without the schema gives a different result.
func TestEjectObjectLiteralKeys(t *testing.T) {
	t.Parallel()

	stringType := schema.TypeSpec{Type: "string"}
	leafRef := schema.TypeSpec{Ref: "#/types/test:index:Leaf"}
	branchRef := schema.TypeSpec{Ref: "#/types/test:index:Branch"}
	leafObject := map[string]schema.PropertySpec{"leaf_object": {TypeSpec: leafRef}}
	testSchema := schema.PackageSpec{
		Name:    "test",
		Version: "1.0.0",
		Types: map[string]schema.ComplexTypeSpec{
			"test:index:Leaf": {ObjectTypeSpec: schema.ObjectTypeSpec{
				Type:       "object",
				Properties: map[string]schema.PropertySpec{"leaf_value": {TypeSpec: stringType}},
			}},
			"test:index:Branch": {ObjectTypeSpec: schema.ObjectTypeSpec{
				Type:       "object",
				Properties: leafObject,
			}},
		},
		Provider: &schema.ResourceSpec{InputProperties: leafObject},
		Resources: map[string]schema.ResourceSpec{
			"test:index:Item": {
				InputProperties: map[string]schema.PropertySpec{
					"leaf_list":      {TypeSpec: schema.TypeSpec{Type: "array", Items: &leafRef}},
					"leaf_map":       {TypeSpec: schema.TypeSpec{Type: "object", AdditionalProperties: &leafRef}},
					"tags":           {TypeSpec: schema.TypeSpec{Type: "object", AdditionalProperties: &stringType}},
					"branch":         {TypeSpec: schema.TypeSpec{Type: "array", Items: &branchRef}},
					"dynamic_branch": {TypeSpec: schema.TypeSpec{Type: "array", Items: &branchRef}},
				},
			},
		},
		Functions: map[string]schema.FunctionSpec{
			"test:index:echo": {
				Inputs: &schema.ObjectTypeSpec{Properties: leafObject},
				Outputs: &schema.ObjectTypeSpec{
					Properties: map[string]schema.PropertySpec{"result": {TypeSpec: stringType}},
				},
			},
		},
	}
	loader := schemaloader.New(t, testSchema)

	src := []byte(`terraform {
  required_providers {
    test = {
      source  = "pulumi/test"
      version = "1.0.0"
    }
  }
}

provider "test" {
  leaf_object = {
    leaf_value = "provider"
  }
}

data "test_echo" "lookup" {
  leaf_object = {
    leaf_value = "invoke"
  }
}

resource "test_item" "item" {
  leaf_list = [{
    leaf_value = "tuple"
  }]
  leaf_map = {
    map_key = {
      leaf_value = "map element"
    }
  }
  tags = {
    tag_key = data.test_echo.lookup.result
  }
  branch {
    leaf_object = {
      leaf_value = "block"
    }
  }
  dynamic "dynamic_branch" {
    for_each = ["a"]
    content {
      leaf_object = {
        leaf_value = "dynamic block"
      }
    }
  }
}
`)

	out := hclwrite.NewEmptyFile()
	diags := transformSingleFile(t, src, "main.tf", out.Body(), loader, nil)
	require.False(t, diags.HasErrors(), diags.Error())

	expected := `resource "test" "pulumi:providers:test" {
  leaf_object = {
    leaf_value = "provider"
  }
}

resource "item" "test:index:Item" {
  leaf_list = [{
    leaf_value = "tuple"
  }]
  leaf_map = {
    map_key = {
      leaf_value = "map element"
    }
  }
  tags = {
    tag_key = invoke("test:index:echo", {
      leaf_object = {
        leaf_value = "invoke"
      }
    }).result
  }
  dynamic_branch = [for __key, __value in ["a"] : {
    leaf_object = {
      leaf_value = "dynamic block"
    }
  }]
  branch = [{
    leaf_object = {
      leaf_value = "block"
    }
  }]
  options {
    deleteBeforeReplace = true
  }
}

`
	assert.Equal(t, expected, string(out.Bytes()))
}

// TestEjectForEachMapKeys checks that the keys of a map literal in a
// `for_each` argument do not change. A key is the value of `each.key`, so it
// is data and not the name of a property.
func TestEjectForEachMapKeys(t *testing.T) {
	t.Parallel()

	stringType := schema.TypeSpec{Type: "string"}
	branchRef := schema.TypeSpec{Ref: "#/types/test:index:Branch"}
	testSchema := schema.PackageSpec{
		Name:    "test",
		Version: "1.0.0",
		Types: map[string]schema.ComplexTypeSpec{
			"test:index:Branch": {ObjectTypeSpec: schema.ObjectTypeSpec{
				Type:       "object",
				Properties: map[string]schema.PropertySpec{"name": {TypeSpec: stringType}},
			}},
		},
		Provider: &schema.ResourceSpec{
			InputProperties: map[string]schema.PropertySpec{"prefix": {TypeSpec: stringType}},
		},
		Resources: map[string]schema.ResourceSpec{
			"test:index:Item": {
				InputProperties: map[string]schema.PropertySpec{
					"value":  {TypeSpec: stringType},
					"branch": {TypeSpec: schema.TypeSpec{Type: "array", Items: &branchRef}},
				},
			},
		},
	}
	loader := schemaloader.New(t, testSchema)

	src := []byte(`terraform {
  required_providers {
    test = {
      source  = "pulumi/test"
      version = "1.0.0"
    }
  }
}

provider "test" {
  alias = "by_key"
  for_each = {
    provider_key = "alpha"
  }
  prefix = each.value
}

resource "test_item" "item" {
  for_each = {
    resource_key = "beta"
  }
  value = each.value
  dynamic "branch" {
    for_each = {
      dynamic_key = "gamma"
    }
    content {
      name = branch.value
    }
  }
}

module "child" {
  source = "./child"
  for_each = {
    module_key = "delta"
  }
  input = each.value
}
`)

	out := hclwrite.NewEmptyFile()
	diags := transformSingleFile(t, src, "main.tf", out.Body(), loader, nil)
	require.False(t, diags.HasErrors(), diags.Error())

	expected := `resource "by_key" "pulumi:providers:test" {
  prefix = range.value
  options {
    range = {
      provider_key = "alpha"
    }
  }
}

resource "item" "test:index:Item" {
  value = range.value
  branch = [for __key, __value in {
    dynamic_key = "gamma"
    } : {
    name = __value
  }]
  options {
    range = {
      resource_key = "beta"
    }
    deleteBeforeReplace = true
  }
}

component "child" "./child" {
  input = range.value
  options {
    range = {
      module_key = "delta"
    }
  }
}

`
	assert.Equal(t, expected, string(out.Bytes()))
}

// TestEjectKeysWithoutProperty checks that the key of an object literal does
// not change if the schema names no property for it, as in the runtime. A
// reference to the key must name the same attribute as the literal.
func TestEjectKeysWithoutProperty(t *testing.T) {
	t.Parallel()

	stringType := schema.TypeSpec{Type: "string"}
	camelRef := schema.TypeSpec{Ref: "#/types/test:index:Camel"}
	testSchema := schema.PackageSpec{
		Name:    "test",
		Version: "1.0.0",
		Types: map[string]schema.ComplexTypeSpec{
			"test:index:Camel": {ObjectTypeSpec: schema.ObjectTypeSpec{
				Type:       "object",
				Properties: map[string]schema.PropertySpec{"nestedValue": {TypeSpec: stringType}},
			}},
		},
		Resources: map[string]schema.ResourceSpec{
			"test:index:Item": {
				InputProperties: map[string]schema.PropertySpec{
					"value":      {TypeSpec: stringType},
					"anyValue":   {TypeSpec: schema.TypeSpec{Ref: "pulumi.json#/Any"}},
					"plainUnion": {TypeSpec: schema.TypeSpec{OneOf: []schema.TypeSpec{stringType, camelRef}}},
					"camel":      {TypeSpec: camelRef},
				},
			},
		},
	}
	loader := schemaloader.New(t, testSchema)

	src := []byte(`terraform {
  required_providers {
    test = {
      source  = "pulumi/test"
      version = "1.0.0"
    }
  }
}

variable "cfg" {
  default = {
    default_key = "variable"
  }
}

locals {
  local_map = {
    local_key = {
      some_attr = "local"
    }
  }
}

resource "test_item" "item" {
  for_each = {
    resource_key = {
      some_attr = "for_each"
    }
  }
  any_value = {
    any_key = "any"
  }
  plain_union = {
    nested_value = "union with no discriminator"
  }
  camel = {
    "nested_value" = "quoted key of a property"
  }
  value = "${each.value.some_attr}-${local.local_map.local_key.some_attr}-${var.cfg.default_key}"
  pulumi {
    env_var_mappings = {
      MY_VAR = "OTHER_VAR"
    }
  }
}

module "child" {
  source = "./child"
  input = {
    input_key = "module"
  }
}

output "out" {
  value = {
    output_key = "output"
  }
}
`)

	out := hclwrite.NewEmptyFile()
	diags := transformSingleFile(t, src, "main.tf", out.Body(), loader, nil)
	require.False(t, diags.HasErrors(), diags.Error())

	expected := `config "cfg" {
  default = {
    default_key = "variable"
  }
}

local_map = {
  local_key = {
    some_attr = "local"
  }
}

resource "item" "test:index:Item" {
  anyValue = {
    any_key = "any"
  }
  plainUnion = {
    nested_value = "union with no discriminator"
  }
  camel = {
    nestedValue = "quoted key of a property"
  }
  value = "${range.value.some_attr}-${local_map.local_key.some_attr}-${cfg.default_key}"
  options {
    range = {
      resource_key = {
        some_attr = "for_each"
      }
    }
    envVarMappings = {
      MY_VAR = "OTHER_VAR"
    }
    deleteBeforeReplace = true
  }
}

component "child" "./child" {
  input = {
    input_key = "module"
  }
}

output "out" {
  value = {
    output_key = "output"
  }
}

`
	assert.Equal(t, expected, string(out.Bytes()))
}

// TestEjectReferenceBelowMap checks that a reference to a resource names the
// same attributes as the runtime: a property gets its schema name, and a key
// of a map or of a value of type any does not change.
func TestEjectReferenceBelowMap(t *testing.T) {
	t.Parallel()

	stringType := schema.TypeSpec{Type: "string"}
	camelRef := schema.TypeSpec{Ref: "#/types/test:index:Camel"}
	properties := map[string]schema.PropertySpec{
		"value":    {TypeSpec: stringType},
		"anyValue": {TypeSpec: schema.TypeSpec{Ref: "pulumi.json#/Any"}},
		"tags":     {TypeSpec: schema.TypeSpec{Type: "object", AdditionalProperties: &stringType}},
		"camelMap": {TypeSpec: schema.TypeSpec{Type: "object", AdditionalProperties: &camelRef}},
	}
	testSchema := schema.PackageSpec{
		Name:    "test",
		Version: "1.0.0",
		Types: map[string]schema.ComplexTypeSpec{
			"test:index:Camel": {ObjectTypeSpec: schema.ObjectTypeSpec{
				Type:       "object",
				Properties: map[string]schema.PropertySpec{"nestedValue": {TypeSpec: stringType}},
			}},
		},
		Resources: map[string]schema.ResourceSpec{
			"test:index:Item": {
				ObjectTypeSpec:  schema.ObjectTypeSpec{Properties: properties},
				InputProperties: properties,
			},
			"test:index:Reader": {
				InputProperties: map[string]schema.PropertySpec{
					"values": {TypeSpec: schema.TypeSpec{Type: "array", Items: &stringType}},
				},
			},
		},
	}
	loader := schemaloader.New(t, testSchema)

	// The reader is before the item, so the reference is to a resource that
	// the converter did not emit yet.
	src := []byte(`terraform {
  required_providers {
    test = {
      source  = "pulumi/test"
      version = "1.0.0"
    }
  }
}

resource "test_reader" "reader" {
  values = [
    test_item.item.tags.tag_key,
    test_item.item.camel_map.map_key.nested_value,
    test_item.item.any_value.any_key,
  ]
}

resource "test_item" "item" {
  value = "item"
  pulumi {
    additional_secret_outputs = [tags.tag_key, camel_map["map_key"].nested_value]
  }
  lifecycle {
    ignore_changes = [tags.tag_key, camel_map["map_key"].nested_value]
  }
}
`)

	out := hclwrite.NewEmptyFile()
	diags := transformSingleFile(t, src, "main.tf", out.Body(), loader, nil)
	require.False(t, diags.HasErrors(), diags.Error())

	expected := `resource "reader" "test:index:Reader" {
  values = [item.tags.tag_key, item.camelMap.map_key.nestedValue, item.anyValue.any_key]
  options {
    deleteBeforeReplace = true
  }
}

resource "item" "test:index:Item" {
  value = "item"
  options {
    additionalSecretOutputs = [tags.tag_key, camelMap["map_key"].nestedValue]
    ignoreChanges           = [tags.tag_key, camelMap["map_key"].nestedValue]
    deleteBeforeReplace     = true
  }
}

`
	assert.Equal(t, expected, string(out.Bytes()))
}

// TestEjectCallUsesMethodSchema checks that the arguments and the result of a
// `call` block get their names from the schema of the method, for a method of
// a resource and for a method of a provider.
func TestEjectCallUsesMethodSchema(t *testing.T) {
	t.Parallel()

	stringType := schema.TypeSpec{Type: "string"}
	camelRef := schema.TypeSpec{Ref: "#/types/test:index:Camel"}
	method := func(self string) schema.FunctionSpec {
		return schema.FunctionSpec{
			Inputs: &schema.ObjectTypeSpec{Properties: map[string]schema.PropertySpec{
				"__self__":  {TypeSpec: schema.TypeSpec{Ref: self}},
				"camelArg":  {TypeSpec: camelRef},
				"tagsInput": {TypeSpec: schema.TypeSpec{Type: "object", AdditionalProperties: &stringType}},
			}},
			Outputs: &schema.ObjectTypeSpec{Properties: map[string]schema.PropertySpec{
				"camelResult": {TypeSpec: camelRef},
			}},
		}
	}
	testSchema := schema.PackageSpec{
		Name:    "test",
		Version: "1.0.0",
		Types: map[string]schema.ComplexTypeSpec{
			"test:index:Camel": {ObjectTypeSpec: schema.ObjectTypeSpec{
				Type:       "object",
				Properties: map[string]schema.PropertySpec{"nestedValue": {TypeSpec: stringType}},
			}},
		},
		Provider: &schema.ResourceSpec{
			Methods: map[string]string{"doThing": "pulumi:providers:test/doThing"},
		},
		Resources: map[string]schema.ResourceSpec{
			"test:index:Item": {
				InputProperties: map[string]schema.PropertySpec{
					"values": {TypeSpec: schema.TypeSpec{Type: "array", Items: &stringType}},
				},
				Methods: map[string]string{"doThing": "test:index:Item/doThing"},
			},
		},
		Functions: map[string]schema.FunctionSpec{
			"test:index:Item/doThing":       method("#/resources/test:index:Item"),
			"pulumi:providers:test/doThing": method("#/provider"),
		},
	}
	loader := schemaloader.New(t, testSchema)

	// The call blocks are before the blocks of their resource and provider.
	src := []byte(`terraform {
  required_providers {
    test = {
      source  = "pulumi/test"
      version = "1.0.0"
    }
  }
}

call "item" "do_thing" {
  camel_arg = {
    nested_value = "resource"
  }
  tags_input = {
    tag_key = "resource"
  }
}

call "prov" "do_thing" {
  camel_arg = {
    nested_value = "provider"
  }
}

provider "test" {
  alias = "prov"
}

resource "test_item" "item" {
  values = [
    call.item.do_thing.camel_result.nested_value,
    call.prov.do_thing.camel_result.nested_value,
  ]
}
`)

	out := hclwrite.NewEmptyFile()
	diags := transformSingleFile(t, src, "main.tf", out.Body(), loader, nil)
	require.False(t, diags.HasErrors(), diags.Error())

	expected := `resource "prov" "pulumi:providers:test" {
}

resource "item" "test:index:Item" {
  values = [call(item, "doThing", {
    camelArg = {
      nestedValue = "resource"
    }
    tagsInput = {
      tag_key = "resource"
    }
    }).camelResult.nestedValue, call(prov, "doThing", {
    camelArg = {
      nestedValue = "provider"
    }
  }).camelResult.nestedValue]
  options {
    deleteBeforeReplace = true
  }
}

`
	assert.Equal(t, expected, string(out.Bytes()))
}

// TestEjectTypeThroughExpressions checks that the schema type of an input
// applies to the object literals in a conditional expression and in a `for`
// expression, because the runtime converts the value of the expression with
// that type.
func TestEjectTypeThroughExpressions(t *testing.T) {
	t.Parallel()

	stringType := schema.TypeSpec{Type: "string"}
	camelRef := schema.TypeSpec{Ref: "#/types/test:index:Camel"}
	testSchema := schema.PackageSpec{
		Name:    "test",
		Version: "1.0.0",
		Types: map[string]schema.ComplexTypeSpec{
			"test:index:Camel": {ObjectTypeSpec: schema.ObjectTypeSpec{
				Type:       "object",
				Properties: map[string]schema.PropertySpec{"nestedValue": {TypeSpec: stringType}},
			}},
		},
		Resources: map[string]schema.ResourceSpec{
			"test:index:Item": {
				InputProperties: map[string]schema.PropertySpec{
					"camel":     {TypeSpec: camelRef},
					"camelList": {TypeSpec: schema.TypeSpec{Type: "array", Items: &camelRef}},
					"camelMap":  {TypeSpec: schema.TypeSpec{Type: "object", AdditionalProperties: &camelRef}},
				},
			},
		},
	}
	loader := schemaloader.New(t, testSchema)

	src := []byte(`terraform {
  required_providers {
    test = {
      source  = "pulumi/test"
      version = "1.0.0"
    }
  }
}

variable "enabled" {
  type = bool
}

variable "names" {
  type = list(string)
}

resource "test_item" "item" {
  camel = var.enabled ? {
    nested_value = "true result"
    } : {
    nested_value = "false result"
  }
  camel_list = [for name in var.names : {
    nested_value = name
  }]
  camel_map = { for name in var.names : name => {
    nested_value = name
  } }
}
`)

	out := hclwrite.NewEmptyFile()
	diags := transformSingleFile(t, src, "main.tf", out.Body(), loader, nil)
	require.False(t, diags.HasErrors(), diags.Error())

	expected := `config "enabled" "bool" {
}

config "names" "list(string)" {
}

resource "item" "test:index:Item" {
  camel = enabled ? {
    nestedValue = "true result"
    } : {
    nestedValue = "false result"
  }
  camelList = [for name in names : {
    nestedValue = name
  } ]
  camelMap = {for name in names : name => {
    nestedValue = name
  } }
  options {
    deleteBeforeReplace = true
  }
}

`
	assert.Equal(t, expected, string(out.Bytes()))
}
