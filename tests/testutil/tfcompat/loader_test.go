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

package tfcompat

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestResolveStages pins the positional matching between disk-loaded file
// sets and Case.Stages metadata: a flat directory fans out to one stage per
// metadata entry (all sharing the same files), numbered stage dirs pair 1:1
// with metadata, and a count mismatch on numbered dirs is an error.
func TestResolveStages(t *testing.T) {
	t.Parallel()
	a := map[string]string{"main.tf": "a"}
	b := map[string]string{"main.tf": "b"}

	t.Run("flat default", func(t *testing.T) {
		t.Parallel()
		runs, err := resolveStages([]map[string]string{a}, false, nil)
		require.NoError(t, err)
		assert.Equal(t, []stageRun{{files: a}}, runs)
	})

	t.Run("flat fans out over metadata", func(t *testing.T) {
		t.Parallel()
		runs, err := resolveStages([]map[string]string{a}, false, []Stage{
			{Mode: StagePreview},
			{},
		})
		require.NoError(t, err)
		assert.Equal(t, []stageRun{
			{files: a, Stage: Stage{Mode: StagePreview}},
			{files: a},
		}, runs)
	})

	t.Run("numbered pairs positionally", func(t *testing.T) {
		t.Parallel()
		runs, err := resolveStages([]map[string]string{a, b}, true, []Stage{
			{},
			{ExpectErr: "boom"},
		})
		require.NoError(t, err)
		assert.Equal(t, []stageRun{
			{files: a},
			{files: b, Stage: Stage{ExpectErr: "boom"}},
		}, runs)
	})

	t.Run("numbered without metadata", func(t *testing.T) {
		t.Parallel()
		runs, err := resolveStages([]map[string]string{a, b}, true, nil)
		require.NoError(t, err)
		assert.Equal(t, []stageRun{{files: a}, {files: b}}, runs)
	})

	t.Run("numbered count mismatch", func(t *testing.T) {
		t.Parallel()
		_, err := resolveStages([]map[string]string{a, b}, true, []Stage{{}})
		require.EqualError(t, err, "case has 2 numbered stage dirs but Case.Stages has 1 entries")
	})
}
