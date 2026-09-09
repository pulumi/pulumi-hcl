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

package providers

import (
	"context"
	"encoding/json"

	gp "github.com/pulumi/pulumi-go-provider"
	"github.com/pulumi/pulumi/pkg/v3/codegen/schema"
)

// EchoProvider is a native Pulumi provider whose `echo:index:Thing` resource
// copies its `text` input into its `out` output.
func EchoProvider() gp.Provider {
	return gp.Provider{
		GetSchema: func(context.Context, gp.GetSchemaRequest) (gp.GetSchemaResponse, error) {
			str := schema.PropertySpec{TypeSpec: schema.TypeSpec{Type: "string"}}
			spec := schema.PackageSpec{
				Name:    "echo",
				Version: "0.0.1",
				Resources: map[string]schema.ResourceSpec{
					"echo:index:Thing": {
						InputProperties: map[string]schema.PropertySpec{"text": str},
						RequiredInputs:  []string{"text"},
						ObjectTypeSpec: schema.ObjectTypeSpec{
							Properties: map[string]schema.PropertySpec{"text": str, "out": str},
							Required:   []string{"text", "out"},
						},
					},
				},
			}
			raw, err := json.Marshal(spec)
			return gp.GetSchemaResponse{Schema: string(raw)}, err
		},
		Create: func(_ context.Context, req gp.CreateRequest) (gp.CreateResponse, error) {
			out := req.Properties.Get("text")
			return gp.CreateResponse{ID: "id-0", Properties: req.Properties.Set("out", out)}, nil
		},
	}
}
