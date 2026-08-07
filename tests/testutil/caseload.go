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
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
)

// LoadStages returns one file set per disk stage and whether the case
// directory used numbered stage subdirs: a directory containing only numbered
// subdirs (0/, 1/, ...) yields that many file sets, in order; any other shape
// yields a single file set built from the whole directory.
func LoadStages(caseDir string) ([]map[string]string, bool, error) {
	info, err := os.Stat(caseDir)
	if err != nil {
		return nil, false, fmt.Errorf("case directory: %w", err)
	}
	if !info.IsDir() {
		return nil, false, fmt.Errorf("case path %q is not a directory", caseDir)
	}

	entries, err := os.ReadDir(caseDir)
	if err != nil {
		return nil, false, err
	}

	stageDirs := make(map[int]string)
	for _, e := range entries {
		if !e.IsDir() {
			stageDirs = nil
			break
		}
		n, err := strconv.Atoi(e.Name())
		if err != nil || n < 0 {
			stageDirs = nil
			break
		}
		stageDirs[n] = filepath.Join(caseDir, e.Name())
	}

	if len(stageDirs) == 0 {
		files, err := LoadCaseDir(caseDir)
		if err != nil {
			return nil, false, err
		}
		return []map[string]string{files}, false, nil
	}

	keys := make([]int, 0, len(stageDirs))
	for k := range stageDirs {
		keys = append(keys, k)
	}
	sort.Ints(keys)
	fileSets := make([]map[string]string, 0, len(keys))
	for _, k := range keys {
		files, err := LoadCaseDir(stageDirs[k])
		if err != nil {
			return nil, false, fmt.Errorf("stage %d: %w", k, err)
		}
		fileSets = append(fileSets, files)
	}
	return fileSets, true, nil
}

// LoadCaseDir reads every regular file under caseDir and returns a map of
// relative-path → file contents. It errors if caseDir is missing, is not a
// directory, or contains no files.
func LoadCaseDir(caseDir string) (map[string]string, error) {
	info, err := os.Stat(caseDir)
	if err != nil {
		return nil, fmt.Errorf("case directory: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("case path %q is not a directory", caseDir)
	}

	files := make(map[string]string)
	err = filepath.WalkDir(caseDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, relErr := filepath.Rel(caseDir, path)
		if relErr != nil {
			return relErr
		}
		content, readErr := os.ReadFile(path) //nolint:gosec // caseDir is test-controlled
		if readErr != nil {
			return readErr
		}
		files[rel] = string(content)
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("no files found in case directory %q", caseDir)
	}
	return files, nil
}
