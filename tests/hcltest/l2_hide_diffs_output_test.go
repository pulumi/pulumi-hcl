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
	"github.com/pulumi/pulumi/sdk/v3/go/property"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// hdProvider is a native provider with the input `value` and the output
// `outVal`, which is not an input.
func hdProvider() gp.Provider {
	return gp.Provider{
		GetSchema: func(context.Context, gp.GetSchemaRequest) (gp.GetSchemaResponse, error) {
			str := schema.TypeSpec{Type: "string"}
			spec := schema.PackageSpec{
				Name:    "hd",
				Version: "0.0.1",
				Resources: map[string]schema.ResourceSpec{
					"hd:index:Thing": {
						InputProperties: map[string]schema.PropertySpec{"value": {TypeSpec: str}},
						ObjectTypeSpec: schema.ObjectTypeSpec{Properties: map[string]schema.PropertySpec{
							"value":  {TypeSpec: str},
							"outVal": {TypeSpec: str},
						}},
					},
				},
			}
			raw, err := json.Marshal(spec)
			return gp.GetSchemaResponse{Schema: string(raw)}, err
		},
		Create: func(_ context.Context, req gp.CreateRequest) (gp.CreateResponse, error) {
			props := req.Properties.Set("outVal", property.New("out-"+req.Properties.Get("value").AsString()))
			return gp.CreateResponse{ID: "id", Properties: props}, nil
		},
		Delete: func(context.Context, gp.DeleteRequest) error { return nil },
	}
}

// TestL2HideDiffsOutput checks that hide_diffs can name an output that is not
// an input. The engine applies hideDiffs to the diff of the outputs.
func TestL2HideDiffsOutput(t *testing.T) {
	t.Parallel()
	hcltest.RunCase(t, "l2_hide_diffs_output", hcltest.Case{
		Providers:       []hcltest.Provider{{Name: "hd", Native: hdProvider}},
		ExpectedOutputs: map[string]string{"out_val": "out-a"},
		AssertState: func(t *testing.T, resources []apitype.ResourceV3) {
			for _, r := range resources {
				if r.URN.Name() == "t" {
					assert.Equal(t, []resource.PropertyPath{{"outVal"}}, r.HideDiff)
					return
				}
			}
			require.Fail(t, `resource "t" is missing`)
		},
	})
}
