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

package run_test

import (
	"context"
	"testing"

	"github.com/pulumi/pulumi-hcl/pkg/hcl/parser"
	"github.com/pulumi/pulumi-hcl/pkg/hcl/run"
	"github.com/pulumi/pulumi-hcl/tests/testutil"
	"github.com/pulumi/pulumi-hcl/tests/testutil/schemaloader"
	"github.com/pulumi/pulumi/pkg/v3/codegen/schema"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource/urn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestEngine_InvalidResourceOptionURN: when the URN of a deleted_with or
// replace_with target is not a valid URN, the run fails with an error that
// names the URN.
func TestEngine_InvalidResourceOptionURN(t *testing.T) {
	t.Parallel()

	tests := []struct {
		option   string
		value    string
		expected string
	}{
		{"deleted_with", "pfx_res.target", `registering pfx_res.res: invalid delete_with URN: "not-a-urn"`},
		{"replace_with", "[pfx_res.target]", `registering pfx_res.res: invalid replace_with URN: "not-a-urn"`},
	}

	for _, tt := range tests {
		t.Run(tt.option, func(t *testing.T) {
			t.Parallel()

			src := []byte(`
resource "pfx_res" "target" {
}

resource "pfx_res" "res" {
  pulumi {
    ` + tt.option + ` = ` + tt.value + `
  }
}
`)

			p := parser.NewParser()
			config, diags := p.ParseSource("test.hcl", src)
			require.False(t, diags.HasErrors(), "parse error: %s", diags.Error())

			mock := &testutil.MockResourceMonitor{
				RegisterResourceHandler: func(
					_ context.Context, req run.RegisterResourceRequest,
				) (*run.RegisterResourceResponse, error) {
					u := urn.URN("urn:pulumi:test::project::" + req.Type + "::" + req.Name)
					if req.Name == "target" {
						u = "not-a-urn"
					}
					return &run.RegisterResourceResponse{URN: u, ID: req.Name + "-id", Outputs: req.Inputs}, nil
				},
			}
			engine := newTestEngine(t, config, &run.EngineOptions{
				ModuleLoader:    testModuleLoader(t),
				ProjectName:     "test-project",
				StackName:       "dev",
				ResourceMonitor: mock,
				WorkDir:         t.TempDir(),
				RootDir:         t.TempDir(),
				SchemaLoader: schemaloader.New(t, schema.PackageSpec{
					Name: "pfx",
					Resources: map[string]schema.ResourceSpec{
						"pfx:index:Res": {},
					},
				}),
			})

			assert.EqualError(t, engine.Run(t.Context()), tt.expected)
		})
	}
}
