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

package server

import (
	"path/filepath"
	"testing"

	"github.com/pulumi/pulumi-hcl/pkg/hcl/modules"
	"github.com/pulumi/pulumi-hcl/pkg/hcl/parser"
	"github.com/pulumi/pulumi-hcl/tests/testutil/schemaloader"
	pulumirpc "github.com/pulumi/pulumi/sdk/v3/proto/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRun_BrokenModule verifies that Run on a program that calls a broken
// module logs each error of the module, fails, and registers nothing. The
// loader returns the blocks of the module that parsed, and Run must not run
// them or use the requirements that they declare.
func TestRun_BrokenModule(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		files    map[string]string
		wantLogs func(dir string) []string
	}{
		{
			name: "file that does not parse",
			files: map[string]string{
				"child/good.tf": requiredRandom("4.18.5") + `
resource "random_uuid" "child" {}
`,
				"child/bad.tf": `not_a_block "x" {}`,
			},
			wantLogs: func(dir string) []string {
				return []string{
					filepath.Join(dir, "child", "bad.tf") + `:1,1-12: Unsupported block type; ` +
						`Blocks of type "not_a_block" are not expected here.`,
				}
			},
		},
		{
			// The parser keeps the version that it rejects, and a pin of it
			// panics.
			name: "invalid Pulumi provider version",
			files: map[string]string{
				"child/main.tf": `
terraform {
  required_providers {
    random = {
      source  = "pulumi/random"
      version = "not-semver"
    }
  }
}
`,
			},
			wantLogs: func(dir string) []string {
				return []string{
					filepath.Join(dir, "child", "main.tf") + `:4,14-7,6: Invalid provider version; ` +
						`"not-semver" is not a valid semver version for Pulumi provider "random": ` +
						`Short version cannot contain PreRelease/Build meta data`,
				}
			},
		},
		{
			// The provider has no SDK, so its requirement makes Run fail with
			// a missing SDK error instead of the error of the module.
			name: "provider without an SDK",
			files: map[string]string{
				"child/good.tf": `
terraform {
  required_providers {
    rollbar = {
      source = "rollbar/rollbar"
    }
  }
}
`,
				"child/bad.tf": `not_a_block "x" {}`,
			},
			wantLogs: func(dir string) []string {
				return []string{
					filepath.Join(dir, "child", "bad.tf") + `:1,1-12: Unsupported block type; ` +
						`Blocks of type "not_a_block" are not expected here.`,
				}
			},
		},
		{
			name: "warning and error",
			files: map[string]string{
				"child/a.tf": `
terraform {
  required_version = ">= 1.0"
}
`,
				"child/b.tf": `not_a_block "x" {}`,
			},
			wantLogs: func(dir string) []string {
				return []string{
					filepath.Join(dir, "child", "b.tf") + `:1,1-12: Unsupported block type; ` +
						`Blocks of type "not_a_block" are not expected here.`,
				}
			},
		},
		{
			name: "two broken modules",
			files: map[string]string{
				"child/main.tf": `
module "other" {
  source = "../other"
}
`,
				"child/bad.tf":  `not_a_block "child" {}`,
				"other/main.tf": `not_a_block "other" {}`,
			},
			wantLogs: func(dir string) []string {
				return []string{
					filepath.Join(dir, "child", "bad.tf") + `:1,1-12: Unsupported block type; ` +
						`Blocks of type "not_a_block" are not expected here.`,
					filepath.Join(dir, "other", "main.tf") + `:1,1-12: Unsupported block type; ` +
						`Blocks of type "not_a_block" are not expected here.`,
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			w := newPinnedWorkspace(t, schemaloader.New(t, randomSpec("4.18.5", "uuid")))
			engine := &logRecorder{}
			w.host.engine = engine
			dir := t.TempDir()
			tt.files["main.tf"] = requiredRandom("4.18.5") + `
resource "random_uuid" "root" {}

module "child" {
  source = "./child"
}
`
			errMsg, err := w.run(t.Context(), dir, tt.files)
			require.NoError(t, err)
			assert.Equal(t, errInvalidProgram.Error(), errMsg)

			var logs []string
			for _, l := range engine.logs {
				require.Equal(t, pulumirpc.LogSeverity_ERROR, l.Severity)
				logs = append(logs, l.Message)
			}
			assert.Equal(t, tt.wantLogs(dir), logs)
			assert.Empty(t, w.monitor.registered)
		})
	}
}

// TestCollectRequirements_BrokenModuleErrors verifies that the error of a
// broken module holds only its errors. As an error, hcl.Diagnostics shows only
// its first diagnostic, so a warning before the error would hide it.
func TestCollectRequirements_BrokenModuleErrors(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, writeFiles(dir, map[string]string{
		"main.tf": `
module "child" {
  source = "./child"
}
`,
		"child/a.tf": `
terraform {
  required_version = ">= 1.0"
}
`,
		"child/b.tf": `not_a_block "x" {}`,
	}))
	config, diags := parser.NewParser().ParseDirectory(dir)
	require.False(t, diags.HasErrors(), diags.Error())

	_, _, _, err := collectRequirements(t.Context(), modules.NewLoader(modules.LiveResolver(t.Context())), config, dir)
	require.Error(t, err)
	assert.Equal(t, filepath.Join(dir, "child", "b.tf")+`:1,1-12: Unsupported block type; `+
		`Blocks of type "not_a_block" are not expected here.`, err.Error())
}
