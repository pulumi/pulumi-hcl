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

package modules

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/hashicorp/hcl/v2"
	"github.com/pulumi/pulumi-hcl/pkg/hcl/parser"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCachedLoadsReturnDiagnostics(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name     string
		source   string
		severity hcl.DiagnosticSeverity
	}{
		{"warning", `terraform {
  backend "local" {}
}`, hcl.DiagWarning},
		{"error", `output "unfinished" {`, hcl.DiagError},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			filename := filepath.Join(dir, "main.tf")
			require.NoError(t, os.WriteFile(filename, []byte(tt.source), 0o600))
			_, expected := parser.NewParser().ParseDirectory(dir)
			require.NotEmpty(t, expected)
			require.Equal(t, tt.severity, expected[0].Severity)
			loader := NewLoader(func(_, _, _ string) (string, string, error) { return dir, "", nil })
			first, firstDiags, err := loader.LoadModule(t.Context(), "./child", "", ".")
			require.NoError(t, err)
			require.Equal(t, expected, firstDiags)
			require.NotNil(t, first)
			// Removing the source proves subsequent loads use the cached parse result.
			require.NoError(t, os.Remove(filename))
			for range 2 {
				cached, diags, err := loader.LoadModule(t.Context(), "./child", "", ".")
				require.NoError(t, err)
				assert.Equal(t, first, cached)
				assert.Equal(t, firstDiags, diags)
				assert.Same(t, firstDiags[0], diags[0])
			}
		})
	}
}
