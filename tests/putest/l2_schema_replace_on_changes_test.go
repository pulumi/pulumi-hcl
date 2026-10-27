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
	"context"
	"encoding/json"
	"fmt"
	"sync/atomic"
	"testing"

	gp "github.com/pulumi/pulumi-go-provider"
	"github.com/pulumi/pulumi-hcl/tests/testutil/putest"
	"github.com/pulumi/pulumi/pkg/v3/codegen/schema"
	"github.com/pulumi/pulumi/sdk/v3/go/common/apitype"
)

// rocProvider is a native provider whose schema marks properties as
// replaceOnChanges: one whose name is not an HCL identifier, one in
// camelCase, and one nested in an object. Each create that is not a preview
// gets a new ID, so a replacement shows in the state.
func rocProvider() gp.Provider {
	var creates atomic.Int32
	return gp.Provider{
		GetSchema: func(context.Context, gp.GetSchemaRequest) (gp.GetSchemaResponse, error) {
			str := schema.TypeSpec{Type: "string"}
			props := map[string]schema.PropertySpec{
				"value":   {TypeSpec: str},
				"fooBar":  {TypeSpec: str, ReplaceOnChanges: true},
				"dot.ted": {TypeSpec: str, ReplaceOnChanges: true},
				"nest":    {TypeSpec: schema.TypeSpec{Ref: "#/types/roc:index:Nest"}},
			}
			spec := schema.PackageSpec{
				Name:    "roc",
				Version: "0.0.1",
				Types: map[string]schema.ComplexTypeSpec{
					"roc:index:Nest": {ObjectTypeSpec: schema.ObjectTypeSpec{
						Type: "object",
						Properties: map[string]schema.PropertySpec{
							"inNer": {TypeSpec: str, ReplaceOnChanges: true},
						},
					}},
				},
				Resources: map[string]schema.ResourceSpec{
					"roc:index:Thing": {
						InputProperties: props,
						ObjectTypeSpec:  schema.ObjectTypeSpec{Properties: props},
					},
				},
			}
			raw, err := json.Marshal(spec)
			return gp.GetSchemaResponse{Schema: string(raw)}, err
		},
		Diff: func(_ context.Context, req gp.DiffRequest) (gp.DiffResponse, error) {
			detailed := map[string]gp.PropertyDiff{}
			for k, v := range req.Inputs.All {
				if old, ok := req.State.GetOk(k); !ok || !old.Equals(v) {
					detailed[k] = gp.PropertyDiff{Kind: gp.Update, InputDiff: true}
				}
			}
			return gp.DiffResponse{HasChanges: len(detailed) > 0, DetailedDiff: detailed}, nil
		},
		Create: func(_ context.Context, req gp.CreateRequest) (gp.CreateResponse, error) {
			if req.DryRun {
				return gp.CreateResponse{Properties: req.Properties}, nil
			}
			id := fmt.Sprintf("id-%d", creates.Add(1)-1)
			return gp.CreateResponse{ID: id, Properties: req.Properties}, nil
		},
		Delete: func(context.Context, gp.DeleteRequest) error { return nil },
		Update: func(_ context.Context, req gp.UpdateRequest) (gp.UpdateResponse, error) {
			return gp.UpdateResponse{Properties: req.Inputs}, nil
		},
	}
}

// TestL2SchemaReplaceOnChanges checks that the schema's replaceOnChanges
// paths survive the round trip, so a change to `fooBar` replaces the
// resource and a change to `value` does not.
func TestL2SchemaReplaceOnChanges(t *testing.T) {
	t.Parallel()
	putest.RunCase(t, "l2_schema_replace_on_changes", putest.Case{
		Providers:       []putest.Provider{{Name: "roc", Native: rocProvider}},
		ExpectedOutputs: map[string]string{"id": "id-1"},
		AssertState: func(t *testing.T, resources []apitype.ResourceV3) {
			assertResources(t, resources, map[string]wantResource{
				"t": {"roc:index:Thing", map[string]any{"value": "b", "fooBar": "b"}},
			})
		},
	})
}
