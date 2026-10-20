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
	"testing"

	"github.com/pulumi/pulumi-hcl/pkg/hcl/parser"
	"github.com/pulumi/pulumi-hcl/pkg/hcl/run"
	"github.com/pulumi/pulumi-hcl/pkg/hcl/transform"
	"github.com/pulumi/pulumi-hcl/tests/testutil"
	"github.com/pulumi/pulumi-hcl/tests/testutil/schemaloader"
	"github.com/pulumi/pulumi/pkg/v3/codegen/schema"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource/urn"
	"github.com/pulumi/pulumi/sdk/v3/go/property"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestEngine_ProviderOptionsFromMarkedVariable: a component input reaches the
// module as a variable with its marks, the same way the server's moduleConfig
// passes it. A provider option that reads such a variable must not panic. An
// option value loses the marks, and an input value keeps them.
func TestEngine_ProviderOptionsFromMarkedVariable(t *testing.T) {
	t.Parallel()

	deps := []urn.URN{"urn:pulumi:test::project::simple:index:Resource::up"}
	tests := []struct {
		name     string
		varType  string
		option   string
		input    property.Value
		get      func(*run.RegisterResourceRequest) any
		expected any
	}{
		{
			name:     "version with dependencies",
			varType:  "string",
			option:   "version",
			input:    property.New("1.2.3").WithDependencies(deps),
			get:      func(r *run.RegisterResourceRequest) any { return r.Version },
			expected: "1.2.3",
		},
		{
			name:     "secret version",
			varType:  "string",
			option:   "version",
			input:    property.New("1.2.3").WithSecret(true),
			get:      func(r *run.RegisterResourceRequest) any { return r.Version },
			expected: "1.2.3",
		},
		{
			name:     "additional_secret_outputs with dependencies",
			varType:  "list(string)",
			option:   "additional_secret_outputs",
			input:    property.New([]property.Value{property.New("x").WithDependencies(deps)}).WithDependencies(deps),
			get:      func(r *run.RegisterResourceRequest) any { return r.AdditionalSecretOutputs },
			expected: []string{"x"},
		},
		{
			name:     "plugin_download_url with dependencies",
			varType:  "string",
			option:   "plugin_download_url",
			input:    property.New("https://example.com").WithDependencies(deps),
			get:      func(r *run.RegisterResourceRequest) any { return r.Inputs.Get("pluginDownloadURL") },
			expected: property.New("https://example.com").WithDependencies(deps),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			src := []byte(`
variable "v" {
  type = ` + tt.varType + `
}

provider "simple" {
  pulumi {
    ` + tt.option + ` = var.v
  }
}

resource "simple_resource" "consumer" {
}
`)

			p := parser.NewParser()
			config, diags := p.ParseSource("test.hcl", src)
			require.False(t, diags.HasErrors(), "parse error: %s", diags.Error())

			mock := &testutil.MockResourceMonitor{}
			engine := newTestEngine(t, config, &run.EngineOptions{
				ModuleLoader:    testModuleLoader(t),
				ProjectName:     "test-project",
				StackName:       "dev",
				ResourceMonitor: mock,
				WorkDir:         t.TempDir(),
				RootDir:         t.TempDir(),
				SchemaLoader: schemaloader.New(t, schema.PackageSpec{
					Name:     "simple",
					Provider: &schema.ResourceSpec{},
					Resources: map[string]schema.ResourceSpec{
						"simple:index:Resource": {},
					},
				}),
				Config: map[string]run.ConfigValue{
					"test-project:v": run.TypedConfigValue(transform.PropertyValueToCty(tt.input)),
				},
			})

			require.NoError(t, engine.Run(t.Context()))

			var provider *run.RegisterResourceRequest
			for i := range mock.RegisteredResources {
				if mock.RegisteredResources[i].Type == "pulumi:providers:simple" {
					provider = &mock.RegisteredResources[i]
					break
				}
			}
			require.NotNil(t, provider, "the provider should register")
			assert.Equal(t, tt.expected, tt.get(provider))
		})
	}
}
