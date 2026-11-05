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

package codegen

import (
	"testing"

	"github.com/pulumi/pulumi/pkg/v3/codegen/hcl2/model"
	"github.com/pulumi/pulumi/pkg/v3/codegen/pcl"
	"github.com/pulumi/pulumi/pkg/v3/codegen/schema"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zclconf/go-cty/cty"
)

func TestOrderedSourceFiles(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		source map[string]string
		want   []string
	}{
		{
			name:   "empty",
			source: map[string]string{},
			want:   []string{},
		},
		{
			name:   "main only",
			source: map[string]string{"main.pp": ""},
			want:   []string{"main.pp"},
		},
		{
			name: "main first, then alphabetical",
			source: map[string]string{
				"outputs.pp":   "",
				"main.pp":      "",
				"variables.pp": "",
			},
			want: []string{"main.pp", "outputs.pp", "variables.pp"},
		},
		{
			name: "no main, alphabetical",
			source: map[string]string{
				"outputs.pp":   "",
				"variables.pp": "",
			},
			want: []string{"outputs.pp", "variables.pp"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, orderedSourceFiles(tc.source))
		})
	}
}

func TestPackageDeclarationsByFile(t *testing.T) {
	t.Parallel()

	source := map[string]string{
		"main.pp": `resource "res" "aws:s3:Bucket" {
}
`,
		"output.pp": `package "output" {
  baseProviderName    = "output"
  baseProviderVersion = "23.0.0"
}
`,
		"providers.pp": `package "aws" {
  baseProviderName    = "aws"
  baseProviderVersion = "1.0.0"
}

package "random" {
  baseProviderName    = "random"
  baseProviderVersion = "2.0.0"
}
`,
	}

	got := packageDeclarationsByFile(source)
	want := map[string]map[string]bool{
		"output.pp":    {"output": true},
		"providers.pp": {"aws": true, "random": true},
	}
	assert.Equal(t, want, got)
}

func TestOutputFileName(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "main.tf", outputFileName("main.pp"))
	assert.Equal(t, "output.tf", outputFileName("output.pp"))
	assert.Equal(t, "no-ext.tf", outputFileName("no-ext"))
}

// TestPickUnionVariantFromObjectExpr_NonStringDiscriminator pins the
// codegen helper for the int32 case — earlier versions only matched
// string consts, so a union pinned by a numeric discriminator silently
// fell through to a quoted string-map key.
func TestPickUnionVariantFromObjectExpr_NonStringDiscriminator(t *testing.T) {
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

	expr := &model.ObjectConsExpression{
		Items: []model.ObjectConsItem{
			{
				Key:   &model.LiteralValueExpression{Value: cty.StringVal("version")},
				Value: &model.LiteralValueExpression{Value: cty.NumberIntVal(2)},
			},
			{
				Key:   &model.LiteralValueExpression{Value: cty.StringVal("b")},
				Value: &model.LiteralValueExpression{Value: cty.StringVal("hello")},
			},
		},
	}

	picked := pickUnionVariantFromObjectExpr(u, expr)
	require.NotNil(t, picked)
	assert.Same(t, v2, picked)
}

func TestComponentOutputDirs(t *testing.T) {
	t.Parallel()

	got := componentOutputDirs(map[string]*pcl.Component{
		"modules/mid":   nil,
		"modules/child": nil,
		"a/child":       nil,
		"b/child_2":     nil,
	})
	assert.Equal(t, map[string]string{
		"a/child":       "child",
		"b/child_2":     "child_2",
		"modules/child": "child_3",
		"modules/mid":   "mid",
	}, got)
}
