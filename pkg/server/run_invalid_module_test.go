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

	"github.com/pulumi/pulumi-hcl/tests/testutil/schemaloader"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRun_BrokenModuleRegistersNothing verifies that Run fails on a module
// with a file that does not parse, and registers no resource of the program. The loader
// returns the blocks of the module's other files, and Run must not run them.
func TestRun_BrokenModuleRegistersNothing(t *testing.T) {
	t.Parallel()

	w := newPinnedWorkspace(t, schemaloader.New(t, randomSpec("4.18.5", "uuid")))
	dir := t.TempDir()
	errMsg, err := w.run(t.Context(), dir, map[string]string{
		"main.tf": requiredRandom("4.18.5") + `
resource "random_uuid" "root" {}

module "child" {
  source = "./child"
}
`,
		"child/good.tf": requiredRandom("4.18.5") + `
resource "random_uuid" "child" {}
`,
		"child/bad.tf": `not_a_block "x" {}`,
	})
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(dir, "child", "bad.tf")+`:1,1-12: Unsupported block type; `+
		`Blocks of type "not_a_block" are not expected here.`, errMsg)
	assert.Empty(t, w.monitor.registered)
}

// TestRun_BrokenModuleRequirements verifies that Run reports the errors of a
// broken module, and does not use the requirements that the module declares.
func TestRun_BrokenModuleRequirements(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		files   map[string]string
		wantErr func(dir string) string
	}{
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
			wantErr: func(dir string) string {
				return filepath.Join(dir, "child", "main.tf") + `:4,14-7,6: Invalid provider version; ` +
					`"not-semver" is not a valid semver version for Pulumi provider "random": ` +
					`Short version cannot contain PreRelease/Build meta data`
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
			wantErr: func(dir string) string {
				return filepath.Join(dir, "child", "bad.tf") + `:1,1-12: Unsupported block type; ` +
					`Blocks of type "not_a_block" are not expected here.`
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			w := newPinnedWorkspace(t, schemaloader.New(t, randomSpec("4.18.5", "uuid")))
			dir := t.TempDir()
			tt.files["main.tf"] = `
module "child" {
  source = "./child"
}
`
			errMsg, err := w.run(t.Context(), dir, tt.files)
			require.NoError(t, err)
			assert.Equal(t, tt.wantErr(dir), errMsg)
		})
	}
}
