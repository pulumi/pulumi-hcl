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

package eval

import (
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource/urn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zclconf/go-cty/cty"
	"github.com/zclconf/go-cty/cty/convert"
)

func parseExpr(t *testing.T, src string) hcl.Expression {
	expr, diags := hclsyntax.ParseExpression([]byte(src), "test.hcl", hcl.Pos{Line: 1, Column: 1})
	if diags.HasErrors() {
		t.Fatalf("Failed to parse expression %q: %s", src, diags.Error())
	}
	return expr
}

// evalString evaluates expr and returns its string value, unmarking deps
// the way a downstream string read would.
func evalString(t *testing.T, e *Evaluator, expr hcl.Expression) string {
	t.Helper()
	val, diags := e.Evaluate(expr)
	require.Empty(t, diags)
	val, _ = val.UnmarkDeep()
	val, err := convert.Convert(val, cty.String)
	require.NoError(t, err)
	require.False(t, val.IsNull())
	return val.AsString()
}

// evalInt evaluates expr and returns its integer value.
func evalInt(t *testing.T, e *Evaluator, expr hcl.Expression) int {
	t.Helper()
	val, diags := e.Evaluate(expr)
	require.Empty(t, diags)
	val, _ = val.UnmarkDeep()
	val, err := convert.Convert(val, cty.Number)
	require.NoError(t, err)
	require.False(t, val.IsNull())
	i64, _ := val.AsBigFloat().Int64()
	return int(i64)
}

func TestEvaluateCount(t *testing.T) {
	t.Parallel()
	ctx, err := NewContext("/tmp", "/tmp", "/tmp", "", "", "")
	require.NoError(t, err)
	ctx.SetVariable("instance_count", cty.NumberIntVal(3))
	ctx.SetVariable("unknown_count", cty.UnknownVal(cty.Number))
	ctx.SetVariable("null_count", cty.NullVal(cty.Number))
	ctx.SetVariable("null_bool_count", cty.NullVal(cty.Bool))

	eval := NewEvaluator(ctx)

	tests := []struct {
		name      string
		expr      string
		expected  int
		isBool    bool
		unknown   bool
		expectErr bool
	}{
		{"literal", `3`, 3, false, false, false},
		{"variable", `var.instance_count`, 3, false, false, false},
		{"zero", `0`, 0, false, false, false},
		{"negative", `-1`, 0, false, false, true},
		{"bool_true", `true`, 1, true, false, false},
		{"bool_false", `false`, 0, true, false, false},
		{"unknown", `var.unknown_count`, 0, false, true, false},
		{"null_number", `var.null_count`, 0, false, false, true},
		{"null_bool", `var.null_bool_count`, 0, false, false, true},
		{"null_literal", `null`, 0, false, false, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			expr := parseExpr(t, tt.expr)
			result, isBool, unknown, _, diags := eval.EvaluateCount(expr)
			if tt.expectErr {
				if !diags.HasErrors() {
					t.Error("Expected error, got none")
				}
			} else {
				if diags.HasErrors() {
					t.Errorf("Unexpected error: %s", diags.Error())
				}
				if result != tt.expected {
					t.Errorf("Expected %d, got %d", tt.expected, result)
				}
				if isBool != tt.isBool {
					t.Errorf("Expected isBool=%v, got %v", tt.isBool, isBool)
				}
				if unknown != tt.unknown {
					t.Errorf("Expected unknown=%v, got %v", tt.unknown, unknown)
				}
			}
		})
	}
}

// AsBigFloat panics on marked values; a non-sensitive mark (e.g. DepMark)
// must be stripped first.
func TestEvaluateCount_MarkedKnownValue(t *testing.T) {
	t.Parallel()
	ctx, err := NewContext("/tmp", "/tmp", "/tmp", "", "", "")
	require.NoError(t, err)
	ctx.SetVariable("n", cty.NumberIntVal(3).WithMarks(
		cty.NewValueMarks(DepMark("urn:pulumi:dev::p::aws:ec2/vpc:Vpc::test")),
	))
	eval := NewEvaluator(ctx)

	result, isBool, unknown, _, diags := eval.EvaluateCount(parseExpr(t, `var.n`))
	require.False(t, diags.HasErrors(), "unexpected diags: %v", diags)
	assert.Equal(t, 3, result)
	assert.False(t, isBool)
	assert.False(t, unknown)
}

// ElementIterator panics on marked containers; a non-sensitive mark must
// be stripped first.
func TestEvaluateForEach_MarkedContainer(t *testing.T) {
	t.Parallel()
	ctx, err := NewContext("/tmp", "/tmp", "/tmp", "", "", "")
	require.NoError(t, err)
	ctx.SetVariable("m", cty.MapVal(map[string]cty.Value{
		"primary": cty.StringVal("p"),
	}).WithMarks(cty.NewValueMarks(DepMark("urn:pulumi:dev::p::aws:ec2/vpc:Vpc::test"))))
	eval := NewEvaluator(ctx)

	result, unknown, _, diags := eval.EvaluateForEach(parseExpr(t, `var.m`))
	require.False(t, diags.HasErrors(), "unexpected diags: %v", diags)
	assert.False(t, unknown)
	assert.Equal(t, map[string]cty.Value{
		"primary": cty.StringVal("p"),
	}, result)
}

func TestEvaluateCountNil(t *testing.T) {
	t.Parallel()
	ctx, err := NewContext("/tmp", "/tmp", "/tmp", "", "", "")
	require.NoError(t, err)
	eval := NewEvaluator(ctx)

	result, isBool, unknown, _, diags := eval.EvaluateCount(nil)
	if diags.HasErrors() {
		t.Errorf("Unexpected error: %s", diags.Error())
	}
	if result != 1 {
		t.Errorf("Expected 1 for nil count, got %d", result)
	}
	if isBool {
		t.Error("Expected isBool=false for nil count")
	}
	if unknown {
		t.Error("Expected unknown=false for nil count")
	}
}

func TestEvaluateForEach(t *testing.T) {
	t.Parallel()
	ctx, err := NewContext("/tmp", "/tmp", "/tmp", "", "", "")
	require.NoError(t, err)
	eval := NewEvaluator(ctx)

	t.Run("map", func(t *testing.T) {
		t.Parallel()
		expr := parseExpr(t, `{a = "x", b = "y"}`)
		result, unknown, _, diags := eval.EvaluateForEach(expr)
		if diags.HasErrors() {
			t.Errorf("Unexpected error: %s", diags.Error())
		}
		assert.False(t, unknown)
		if len(result) != 2 {
			t.Errorf("Expected 2 elements, got %d", len(result))
		}
		if result["a"].AsString() != "x" {
			t.Errorf("Expected result[a]='x', got %q", result["a"].AsString())
		}
	})

	t.Run("set of strings", func(t *testing.T) {
		t.Parallel()
		expr := parseExpr(t, `toset(["a", "b", "c"])`)
		result, unknown, _, diags := eval.EvaluateForEach(expr)
		if diags.HasErrors() {
			t.Errorf("Unexpected error: %s", diags.Error())
		}
		assert.False(t, unknown)
		if len(result) != 3 {
			t.Errorf("Expected 3 elements, got %d", len(result))
		}
	})

	t.Run("set with null element rejected", func(t *testing.T) {
		t.Parallel()
		expr := parseExpr(t, `toset(["a", null])`)
		_, _, _, diags := eval.EvaluateForEach(expr)
		require.Len(t, diags, 1)
		assert.Equal(t, "Invalid for_each value", diags[0].Summary)
		assert.Equal(t, "for_each set must not contain null values.", diags[0].Detail)
	})

	t.Run("nil returns nil", func(t *testing.T) {
		t.Parallel()
		result, unknown, _, diags := eval.EvaluateForEach(nil)
		if diags.HasErrors() {
			t.Errorf("Unexpected error: %s", diags.Error())
		}
		assert.False(t, unknown)
		if result != nil {
			t.Errorf("Expected nil, got %v", result)
		}
	})

	t.Run("list rejected", func(t *testing.T) {
		t.Parallel()
		expr := parseExpr(t, `["a", "b"]`)
		_, _, _, diags := eval.EvaluateForEach(expr)
		if !diags.HasErrors() {
			t.Error("Expected error for list for_each, got none")
		}
	})

	t.Run("unknown container", func(t *testing.T) {
		t.Parallel()
		unknownCtx, err := NewContext("/tmp", "/tmp", "/tmp", "", "", "")
		require.NoError(t, err)
		unknownCtx.SetVariable("m", cty.UnknownVal(cty.Map(cty.String)))

		result, unknown, _, diags := NewEvaluator(unknownCtx).EvaluateForEach(parseExpr(t, `var.m`))
		require.False(t, diags.HasErrors(), "unexpected diags: %v", diags)
		assert.True(t, unknown)
		assert.Nil(t, result)
	})

	t.Run("set with unknown element", func(t *testing.T) {
		t.Parallel()
		unknownCtx, err := NewContext("/tmp", "/tmp", "/tmp", "", "", "")
		require.NoError(t, err)
		unknownCtx.SetVariable("s", cty.UnknownVal(cty.String))

		result, unknown, _, diags := NewEvaluator(unknownCtx).EvaluateForEach(parseExpr(t, `toset([var.s, "known"])`))
		require.False(t, diags.HasErrors(), "unexpected diags: %v", diags)
		assert.True(t, unknown)
		assert.Nil(t, result)
	})
}

func TestContextVariables(t *testing.T) {
	t.Parallel()
	ctx, err := NewContext("/tmp", "/tmp", "/tmp", "", "", "")
	require.NoError(t, err)
	ctx.SetVariable("name", cty.StringVal("test"))
	ctx.SetVariable("count", cty.NumberIntVal(5))

	eval := NewEvaluator(ctx)

	// Test var.name
	expr := parseExpr(t, `var.name`)
	assert.Equal(t, "test", evalString(t, eval, expr))
}

func TestContextLocals(t *testing.T) {
	t.Parallel()
	ctx, err := NewContext("/tmp", "/tmp", "/tmp", "", "", "")
	require.NoError(t, err)
	ctx.SetLocal("common_tags", cty.ObjectVal(map[string]cty.Value{
		"Environment": cty.StringVal("dev"),
		"ManagedBy":   cty.StringVal("Pulumi"),
	}))

	eval := NewEvaluator(ctx)

	// Test local.common_tags.Environment
	expr := parseExpr(t, `local.common_tags.Environment`)
	assert.Equal(t, "dev", evalString(t, eval, expr))
}

func TestContextCountIndex(t *testing.T) {
	t.Parallel()
	ctx, err := NewContext("/tmp", "/tmp", "/tmp", "", "", "")
	require.NoError(t, err)
	ctx.SetCount(2)

	eval := NewEvaluator(ctx)
	expr := parseExpr(t, `count.index`)
	result := evalInt(t, eval, expr)
	if result != 2 {
		t.Errorf("Expected 2, got %d", result)
	}
}

func TestContextEach(t *testing.T) {
	t.Parallel()
	ctx, err := NewContext("/tmp", "/tmp", "/tmp", "", "", "")
	require.NoError(t, err)
	ctx.SetEach(cty.StringVal("mykey"), cty.StringVal("myvalue"))

	eval := NewEvaluator(ctx)

	// Test each.key
	assert.Equal(t, "mykey", evalString(t, eval, parseExpr(t, `each.key`)))

	// Test each.value
	assert.Equal(t, "myvalue", evalString(t, eval, parseExpr(t, `each.value`)))
}

func TestContextPath(t *testing.T) {
	t.Parallel()
	t.Run("root module yields '.'", func(t *testing.T) {
		t.Parallel()
		ctx, err := NewContext("/project/module", "/project/module", "/project/module", "", "", "")
		require.NoError(t, err)
		eval := NewEvaluator(ctx)
		assert.Equal(t, ".", evalString(t, eval, parseExpr(t, `path.module`)))
		assert.Equal(t, ".", evalString(t, eval, parseExpr(t, `path.root`)))
	})

	t.Run("nested module yields relative path from root", func(t *testing.T) {
		t.Parallel()
		ctx, err := NewContext("/project/modules/sub", "/project", "/project", "", "", "")
		require.NoError(t, err)
		eval := NewEvaluator(ctx)
		assert.Equal(t, "modules/sub", evalString(t, eval, parseExpr(t, `path.module`)))
		assert.Equal(t, ".", evalString(t, eval, parseExpr(t, `path.root`)))
	})

	t.Run("absolute-path root module", func(t *testing.T) {
		t.Parallel()
		ctx, err := NewAbsolutePathContext("/project/module", "/project/module", "/project/module", "", "", "")
		require.NoError(t, err)
		eval := NewEvaluator(ctx)
		assert.Equal(t, "/project/module", evalString(t, eval, parseExpr(t, `path.module`)))
		assert.Equal(t, "/project/module", evalString(t, eval, parseExpr(t, `path.root`)))
	})

	t.Run("absolute-path nested module", func(t *testing.T) {
		t.Parallel()
		ctx, err := NewAbsolutePathContext("/project/modules/sub", "/project", "/project", "", "", "")
		require.NoError(t, err)
		eval := NewEvaluator(ctx)
		assert.Equal(t, "/project/modules/sub", evalString(t, eval, parseExpr(t, `path.module`)))
		assert.Equal(t, "/project", evalString(t, eval, parseExpr(t, `path.root`)))
	})
}

func TestContextFileResolvesAgainstRootModuleDir(t *testing.T) {
	t.Parallel()

	rootDir := t.TempDir()
	modDir := filepath.Join(rootDir, "mod")
	require.NoError(t, os.MkdirAll(modDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(modDir, "aux.txt"), []byte("hello\n"), 0o644))

	ctx, err := NewContext(modDir, rootDir, rootDir, "", "", "")
	require.NoError(t, err)
	eval := NewEvaluator(ctx)

	require.Equal(t, "mod", evalString(t, eval, parseExpr(t, `path.module`)))
	assert.Equal(t, "5891b5b522d5df086d0ff0b110fbd9d21bb4fc7163af34d08286a2e846f6be03",
		evalString(t, eval, parseExpr(t, `filesha256("${path.module}/aux.txt")`)))
}

func TestContextTerraform(t *testing.T) {
	t.Parallel()
	ctx, err := NewContext("/tmp", "/tmp", "/tmp", "production", "", "")
	require.NoError(t, err)

	eval := NewEvaluator(ctx)

	assert.Equal(t, "production", evalString(t, eval, parseExpr(t, `pulumi.stack`)))
	assert.Equal(t, "production", evalString(t, eval, parseExpr(t, `terraform.workspace`)))

	_, diags := eval.Evaluate(parseExpr(t, `terraform.applying`))
	assert.Equal(t,
		`test.hcl:1,10-19: Unsupported attribute; This object does not have an attribute named "applying".`,
		diags.Error())
}

func TestContextRangedResources(t *testing.T) {
	t.Parallel()
	t.Run("count resources are accessible by index", func(t *testing.T) {
		t.Parallel()
		ctx, err := NewContext("/tmp", "/tmp", "/tmp", "", "", "")
		require.NoError(t, err)
		ctx.SetCountResource("aws_instance.web", 0, "", cty.ObjectVal(map[string]cty.Value{
			"id": cty.StringVal("i-000"),
		}))
		ctx.SetCountResource("aws_instance.web", 1, "", cty.ObjectVal(map[string]cty.Value{
			"id": cty.StringVal("i-001"),
		}))

		eval := NewEvaluator(ctx)
		assert.Equal(t, "i-000", evalString(t, eval, parseExpr(t, `aws_instance.web[0].id`)))
		assert.Equal(t, "i-001", evalString(t, eval, parseExpr(t, `aws_instance.web[1].id`)))
	})

	t.Run("for_each resources are accessible by key", func(t *testing.T) {
		t.Parallel()
		ctx, err := NewContext("/tmp", "/tmp", "/tmp", "", "", "")
		require.NoError(t, err)
		ctx.SetEachResource("aws_instance.web", "east", "", cty.ObjectVal(map[string]cty.Value{
			"id": cty.StringVal("i-east"),
		}))
		ctx.SetEachResource("aws_instance.web", "west", "", cty.ObjectVal(map[string]cty.Value{
			"id": cty.StringVal("i-west"),
		}))

		eval := NewEvaluator(ctx)
		assert.Equal(t, "i-east", evalString(t, eval, parseExpr(t, `aws_instance.web["east"].id`)))
	})

	t.Run("resource named with brackets is not confused with ranged", func(t *testing.T) {
		t.Parallel()
		ctx, err := NewContext("/tmp", "/tmp", "/tmp", "", "", "")
		require.NoError(t, err)
		ctx.SetResource("aws_instance.foo[0]", "", cty.ObjectVal(map[string]cty.Value{
			"id": cty.StringVal("i-literal"),
		}))

		hclCtx := ctx.HCLContext()
		awsInst := hclCtx.Variables["aws_instance"]
		attr := awsInst.GetAttr("foo[0]")
		attr, _ = attr.UnmarkDeep()
		assert.Equal(t, "i-literal", attr.GetAttr("id").AsString())
	})

	t.Run("single and ranged resources coexist under same type", func(t *testing.T) {
		t.Parallel()
		ctx, err := NewContext("/tmp", "/tmp", "/tmp", "", "", "")
		require.NoError(t, err)
		ctx.SetResource("aws_instance.single", "", cty.ObjectVal(map[string]cty.Value{
			"id": cty.StringVal("i-single"),
		}))
		ctx.SetCountResource("aws_instance.multi", 0, "", cty.ObjectVal(map[string]cty.Value{
			"id": cty.StringVal("i-multi-0"),
		}))

		eval := NewEvaluator(ctx)
		assert.Equal(t, "i-single", evalString(t, eval, parseExpr(t, `aws_instance.single.id`)))
		assert.Equal(t, "i-multi-0", evalString(t, eval, parseExpr(t, `aws_instance.multi[0].id`)))
	})
}

func TestContextClone(t *testing.T) {
	t.Parallel()
	ctx, err := NewContext("/tmp", "/tmp", "/tmp", "", "", "")
	require.NoError(t, err)
	ctx.SetVariable("name", cty.StringVal("original"))

	clone := ctx.Clone()
	clone.SetVariable("name", cty.StringVal("cloned"))

	// Original should be unchanged
	origEval := NewEvaluator(ctx)
	expr := parseExpr(t, `var.name`)
	assert.Equal(t, "original", evalString(t, origEval, expr))

	// Clone should have new value
	cloneEval := NewEvaluator(clone)
	assert.Equal(t, "cloned", evalString(t, cloneEval, expr))
}

func TestParseTraversal(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name              string
		expr              string
		expectedNamespace string
		expectedParts     []string
	}{
		{
			name:              "simple variable",
			expr:              `var.name`,
			expectedNamespace: "var",
			expectedParts:     []string{"name"},
		},
		{
			name:              "nested attribute",
			expr:              `local.tags.Environment`,
			expectedNamespace: "local",
			expectedParts:     []string{"tags", "Environment"},
		},
		{
			name:              "resource reference",
			expr:              `aws_instance.web.id`,
			expectedNamespace: "aws_instance",
			expectedParts:     []string{"web", "id"},
		},
		{
			name:              "data source",
			expr:              `data.aws_ami.ubuntu.id`,
			expectedNamespace: "data",
			expectedParts:     []string{"aws_ami", "ubuntu", "id"},
		},
		{
			name:              "module output",
			expr:              `module.vpc.vpc_id`,
			expectedNamespace: "module",
			expectedParts:     []string{"vpc", "vpc_id"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			expr := parseExpr(t, tt.expr)
			traversals := expr.Variables()
			if len(traversals) != 1 {
				t.Fatalf("Expected 1 traversal, got %d", len(traversals))
			}

			namespace, parts := ParseTraversal(traversals[0])
			if namespace != tt.expectedNamespace {
				t.Errorf("Expected namespace %q, got %q", tt.expectedNamespace, namespace)
			}
			if len(parts) != len(tt.expectedParts) {
				t.Errorf("Expected %d parts, got %d", len(tt.expectedParts), len(parts))
			} else {
				for i, part := range parts {
					if part != tt.expectedParts[i] {
						t.Errorf("Part %d: expected %q, got %q", i, tt.expectedParts[i], part)
					}
				}
			}
		})
	}
}

func TestExtractDependencies(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		expr     string
		expected []string
	}{
		{
			name:     "no dependencies",
			expr:     `"literal"`,
			expected: nil,
		},
		{
			name:     "variable reference",
			expr:     `var.name`,
			expected: nil, // var is not a dependency
		},
		{
			name:     "resource reference",
			expr:     `aws_instance.web.id`,
			expected: []string{"aws_instance.web"},
		},
		{
			name:     "data source reference",
			expr:     `data.aws_ami.ubuntu.id`,
			expected: []string{"data.aws_ami.ubuntu"},
		},
		{
			name:     "module reference",
			expr:     `module.vpc.vpc_id`,
			expected: []string{"module.vpc"},
		},
		{
			name:     "multiple references",
			expr:     `"${aws_instance.web.id}-${aws_s3_bucket.mybucket.arn}"`,
			expected: []string{"aws_instance.web", "aws_s3_bucket.mybucket"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			expr := parseExpr(t, tt.expr)
			deps := ExtractDependencies(expr)

			if len(deps) != len(tt.expected) {
				t.Errorf("Expected %d dependencies, got %d: %v", len(tt.expected), len(deps), deps)
			} else {
				for i, dep := range deps {
					if dep != tt.expected[i] {
						t.Errorf("Dependency %d: expected %q, got %q", i, tt.expected[i], dep)
					}
				}
			}
		})
	}
}

func TestIsKnown(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		value    cty.Value
		expected bool
	}{
		{"known string", cty.StringVal("hello"), true},
		{"known number", cty.NumberIntVal(42), true},
		{"unknown string", cty.UnknownVal(cty.String), false},
		{"known list", cty.ListVal([]cty.Value{cty.StringVal("a")}), true},
		{"list with unknown", cty.ListVal([]cty.Value{cty.UnknownVal(cty.String)}), false},
		{"known map", cty.MapVal(map[string]cty.Value{"a": cty.StringVal("b")}), true},
		{"null", cty.NullVal(cty.String), true}, // null is known
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			result := IsKnown(tt.value)
			if result != tt.expected {
				t.Errorf("Expected %v, got %v", tt.expected, result)
			}
		})
	}
}

func TestCollectDepURNs(t *testing.T) {
	t.Parallel()
	a := DepMark("urn:test::a")
	b := DepMark("urn:test::b")

	t.Run("no marks", func(t *testing.T) {
		t.Parallel()
		assert.Empty(t, CollectDepURNs(cty.StringVal("hi")))
	})

	t.Run("single leaf mark", func(t *testing.T) {
		t.Parallel()
		assert.Equal(t, []urn.URN{"urn:test::a"},
			CollectDepURNs(cty.StringVal("hi").Mark(a)))
	})

	t.Run("nested distinct marks deduplicated and ordered first-seen", func(t *testing.T) {
		t.Parallel()
		// {x: marked(a), y: [marked(b), marked(a)]}
		v := cty.ObjectVal(map[string]cty.Value{
			"x": cty.StringVal("v").Mark(a),
			"y": cty.ListVal([]cty.Value{
				cty.StringVal("p").Mark(b),
				cty.StringVal("q").Mark(a),
			}),
		})
		urns := CollectDepURNs(v)
		assert.ElementsMatch(t, []urn.URN{"urn:test::a", "urn:test::b"}, urns)
	})

	t.Run("ignores non-DepMark marks like SensitiveMark", func(t *testing.T) {
		t.Parallel()
		v := cty.StringVal("secret").Mark(SensitiveMark)
		assert.Empty(t, CollectDepURNs(v))
	})

	t.Run("propagates through marked container", func(t *testing.T) {
		t.Parallel()
		obj := cty.ObjectVal(map[string]cty.Value{
			"id": cty.StringVal("xyz"),
		}).Mark(a)
		// GetAttr propagates the container's mark to the attribute.
		idAttr := obj.GetAttr("id")
		assert.Equal(t, []urn.URN{"urn:test::a"}, CollectDepURNs(idAttr))
	})
}

func TestStripSyntheticAttributes(t *testing.T) {
	t.Parallel()

	// resourceObj mirrors how the engine builds a resource output object: the
	// container carries DepMark(urn), and the synthetic `urn` attribute carries
	// SyntheticMark.
	resourceObj := func(u urn.URN) cty.Value {
		return cty.ObjectVal(map[string]cty.Value{
			"id":        cty.StringVal("simple-id"),
			"urn":       cty.StringVal(string(u)).WithMarks(cty.NewValueMarks(SyntheticMark)),
			"input_one": cty.StringVal("hello"),
		}).Mark(DepMark(u))
	}

	// keysOf returns the sorted attribute names of an object.
	keysOf := func(v cty.Value) []string {
		v, _ = v.Unmark()
		var keys []string
		for it := v.ElementIterator(); it.Next(); {
			k, _ := it.Element()
			keys = append(keys, k.AsString())
		}
		sort.Strings(keys)
		return keys
	}

	t.Run("single resource object drops synthetic urn", func(t *testing.T) {
		t.Parallel()
		u := urn.URN("urn:pulumi:test::p::simple:index/resource:Resource::r")
		got := stripSyntheticAttributes(resourceObj(u))
		assert.Equal(t, []string{"id", "input_one"}, keysOf(got))
		// DepMark survives so pulumiResourceName/Type can recover the URN.
		assert.Equal(t, []urn.URN{u}, CollectDepURNs(got))
	})

	t.Run("tuple of resource objects (count)", func(t *testing.T) {
		t.Parallel()
		got := stripSyntheticAttributes(cty.TupleVal([]cty.Value{
			resourceObj("urn:pulumi:test::p::simple:index/resource:Resource::r-0"),
			resourceObj("urn:pulumi:test::p::simple:index/resource:Resource::r-1"),
		}))
		for it := got.ElementIterator(); it.Next(); {
			_, v := it.Element()
			assert.Equal(t, []string{"id", "input_one"}, keysOf(v))
		}
	})

	t.Run("object of resource objects (for_each)", func(t *testing.T) {
		t.Parallel()
		got := stripSyntheticAttributes(cty.ObjectVal(map[string]cty.Value{
			"x": resourceObj("urn:pulumi:test::p::simple:index/resource:Resource::r-x"),
			"y": resourceObj("urn:pulumi:test::p::simple:index/resource:Resource::r-y"),
		}))
		assert.Equal(t, []string{"x", "y"}, keysOf(got))
		assert.Equal(t, []string{"id", "input_one"}, keysOf(got.GetAttr("x")))
	})

	t.Run("user object with literal urn key is preserved", func(t *testing.T) {
		t.Parallel()
		// The urn value carries no SyntheticMark, so this is a user-authored
		// object and must be returned untouched.
		userObj := cty.ObjectVal(map[string]cty.Value{
			"urn":  cty.StringVal("hello"),
			"name": cty.StringVal("world"),
		})
		assert.True(t, stripSyntheticAttributes(userObj).RawEquals(userObj))
	})

	t.Run("sensitive mark on container is preserved", func(t *testing.T) {
		t.Parallel()
		marked := resourceObj("urn:pulumi:test::p::simple:index/resource:Resource::r").
			Mark(SensitiveMark)
		got := stripSyntheticAttributes(marked)
		unmarked, marks := got.Unmark()
		assert.Equal(t, []string{"id", "input_one"}, keysOf(unmarked))
		_, isSensitive := marks[SensitiveMark]
		assert.True(t, isSensitive)
	})
}
