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

package testutil

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestLoadCaseDir_MultiFile checks that the directory loader picks up every
// regular file, keyed by path relative to the case directory.
func TestLoadCaseDir_MultiFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.tf"), []byte("a-content"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "b.tf"), []byte("b-content"), 0o600))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "mod"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "mod", "c.tf"), []byte("c-content"), 0o600))

	files, err := LoadCaseDir(dir)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{
		"a.tf":                       "a-content",
		"b.tf":                       "b-content",
		filepath.Join("mod", "c.tf"): "c-content",
	}, files)
}

// TestLoadCaseDir_MissingDir asserts the loader returns an error when the
// directory does not exist, rather than silently returning an empty map.
func TestLoadCaseDir_MissingDir(t *testing.T) {
	t.Parallel()
	_, err := LoadCaseDir(filepath.Join(t.TempDir(), "does-not-exist"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "case directory")
}

// TestLoadCaseDir_NotADir asserts the loader rejects a file path masquerading
// as a case directory.
func TestLoadCaseDir_NotADir(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "a.tf")
	require.NoError(t, os.WriteFile(path, []byte("x"), 0o600))
	_, err := LoadCaseDir(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not a directory")
}

// TestLoadCaseDir_Empty asserts the loader rejects an empty directory.
func TestLoadCaseDir_Empty(t *testing.T) {
	t.Parallel()
	_, err := LoadCaseDir(t.TempDir())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no files found")
}
