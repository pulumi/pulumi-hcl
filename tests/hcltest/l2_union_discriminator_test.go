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

package hcltest_test

import (
	"context"
	"encoding/json"
	"testing"

	gp "github.com/pulumi/pulumi-go-provider"
	"github.com/pulumi/pulumi-hcl/tests/testutil/hcltest"
	"github.com/pulumi/pulumi/pkg/v3/codegen/schema"
	"github.com/pulumi/pulumi/sdk/v3/go/property"
)

// unionProvider is a native provider whose `union:index:Thing` resource takes a
// `cfg` input typed as a discriminated union: two object members distinguished
// by a const `type` property. The Terraform bridge never emits this shape, so
// it is served natively rather than bridged.
func unionProvider() gp.Provider {
	return gp.Provider{
		GetSchema: func(context.Context, gp.GetSchemaRequest) (gp.GetSchemaResponse, error) {
			member := func(disc string) schema.ComplexTypeSpec {
				return schema.ComplexTypeSpec{ObjectTypeSpec: schema.ObjectTypeSpec{
					Type: "object",
					Properties: map[string]schema.PropertySpec{
						"type":  {TypeSpec: schema.TypeSpec{Type: "string"}, Const: disc},
						"value": {TypeSpec: schema.TypeSpec{Type: "string"}},
					},
				}}
			}
			cfg := schema.PropertySpec{TypeSpec: schema.TypeSpec{OneOf: []schema.TypeSpec{
				{Ref: "#/types/union:index:MemberA"},
				{Ref: "#/types/union:index:MemberB"},
			}}}
			out := schema.PropertySpec{TypeSpec: schema.TypeSpec{Type: "string"}}
			spec := schema.PackageSpec{
				Name:    "union",
				Version: "0.0.1",
				Resources: map[string]schema.ResourceSpec{
					"union:index:Thing": {
						InputProperties: map[string]schema.PropertySpec{"cfg": cfg},
						ObjectTypeSpec: schema.ObjectTypeSpec{
							Properties: map[string]schema.PropertySpec{"cfg": cfg, "out": out},
							Required:   []string{"out"},
						},
					},
				},
				Types: map[string]schema.ComplexTypeSpec{
					"union:index:MemberA": member("a"),
					"union:index:MemberB": member("b"),
				},
			}
			raw, err := json.Marshal(spec)
			return gp.GetSchemaResponse{Schema: string(raw)}, err
		},
		// `out` is a known discriminator once applied, so the unknown-at-preview
		// case still resolves a variant at up.
		Create: func(_ context.Context, req gp.CreateRequest) (gp.CreateResponse, error) {
			return gp.CreateResponse{ID: "id-0", Properties: req.Properties.Set("out", property.New("a"))}, nil
		},
	}
}

// TestL2UnionDiscriminatorSensitive applies a union whose discriminator is a
// sensitive (marked) value; it must be unmarked before it is compared.
//
// TODO[https://github.com/pulumi/pulumi/pull/24337]: This test can be replaced by a conformance test.
func TestL2UnionDiscriminatorSensitive(t *testing.T) {
	t.Parallel()
	hcltest.RunCase(t, "l2_union_discriminator_sensitive", hcltest.Case{
		Providers: []hcltest.Provider{{Name: "union", Native: unionProvider}},
	})
}

// TestL2UnionDiscriminatorUnknown previews a union whose discriminator is a
// computed output, unknown during preview; the variant cannot be chosen so the
// conversion defers instead of comparing an unknown value.
//
// TODO[https://github.com/pulumi/pulumi/pull/24337]: This test can be replaced by a conformance test.
func TestL2UnionDiscriminatorUnknown(t *testing.T) {
	t.Parallel()
	hcltest.RunCase(t, "l2_union_discriminator_unknown", hcltest.Case{
		Providers: []hcltest.Provider{{Name: "union", Native: unionProvider}},
	})
}
