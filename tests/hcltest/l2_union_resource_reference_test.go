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
	"github.com/pulumi/pulumi/sdk/v3/go/common/apitype"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource/urn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// unionRefProvider is a native provider whose `unionref:index:Pipeline`
// resource takes inputs typed as a union of two resource types: at the top
// level, inside an object, and as the elements of a list.
func unionRefProvider() gp.Provider {
	return gp.Provider{
		GetSchema: func(context.Context, gp.GetSchemaRequest) (gp.GetSchemaResponse, error) {
			source := schema.TypeSpec{OneOf: []schema.TypeSpec{
				{Ref: "#/resources/unionref:index:Github"},
				{Ref: "#/resources/unionref:index:Gitlab"},
			}}
			properties := map[string]schema.PropertySpec{
				"source":  {TypeSpec: source},
				"program": {TypeSpec: schema.TypeSpec{Ref: "#/types/unionref:index:Program"}},
				"sources": {TypeSpec: schema.TypeSpec{Type: "array", Items: &source}},
			}
			spec := schema.PackageSpec{
				Name:    "unionref",
				Version: "0.0.1",
				Resources: map[string]schema.ResourceSpec{
					"unionref:index:Github": {},
					"unionref:index:Gitlab": {},
					"unionref:index:Pipeline": {
						InputProperties: properties,
						ObjectTypeSpec:  schema.ObjectTypeSpec{Properties: properties},
					},
				},
				Types: map[string]schema.ComplexTypeSpec{
					"unionref:index:Program": {ObjectTypeSpec: schema.ObjectTypeSpec{
						Type:       "object",
						Properties: map[string]schema.PropertySpec{"source": {TypeSpec: source}},
					}},
				},
			}
			raw, err := json.Marshal(spec)
			return gp.GetSchemaResponse{Schema: string(raw)}, err
		},
		Create: func(_ context.Context, req gp.CreateRequest) (gp.CreateResponse, error) {
			return gp.CreateResponse{ID: "id-" + req.Urn.Name(), Properties: req.Properties}, nil
		},
		Delete: func(context.Context, gp.DeleteRequest) error { return nil },
	}
}

// TestL2UnionResourceReference checks that a whole resource passed to a
// property typed as a union of resource types stays a resource reference. It
// must not reach the provider as the outputs object of the resource.
func TestL2UnionResourceReference(t *testing.T) {
	t.Parallel()

	// assertReference checks that a value in the exported state is a resource
	// reference to the resource of the given type and name.
	assertReference := func(t *testing.T, value any, typ, name string) {
		t.Helper()
		reference, ok := value.(map[string]any)
		require.Truef(t, ok, "expected a resource reference to %s, found %#v", name, value)
		require.Equal(t, resource.ResourceReferenceSig, reference[resource.SigKey])
		referenced := urn.URN(reference["urn"].(string))
		assert.Equal(t, typ, string(referenced.Type()))
		assert.Equal(t, name, referenced.Name())
	}

	hcltest.RunCase(t, "l2_union_resource_reference", hcltest.Case{
		Providers: []hcltest.Provider{{Name: "unionref", Native: unionRefProvider}},
		AssertState: func(t *testing.T, resources []apitype.ResourceV3) {
			for _, r := range resources {
				if r.URN.Name() != "p" {
					continue
				}
				const github, gitlab = "unionref:index:Github", "unionref:index:Gitlab"
				assertReference(t, r.Inputs["source"], github, "gh")
				assertReference(t, r.Inputs["program"].(map[string]any)["source"], gitlab, "gl")
				sources := r.Inputs["sources"].([]any)
				require.Len(t, sources, 2)
				assertReference(t, sources[0], github, "gh")
				assertReference(t, sources[1], gitlab, "gl")
				return
			}
			require.Fail(t, `resource "p" is missing`)
		},
	})
}
