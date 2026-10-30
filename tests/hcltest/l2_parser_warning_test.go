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

package hcltest_test

import (
	"strings"
	"testing"

	"github.com/pulumi/pulumi-hcl/tests/testutil/hcltest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A parser warning in the root module reaches the user once. The output
// embeds the temporary program directory, so the assertions match the text
// after it.
func TestL2ParserWarningRoot(t *testing.T) {
	t.Parallel()
	hcltest.RunCase(t, "l2_parser_warning_root", hcltest.Case{
		SkipRoundTrip:   "HCL -> PCL drops `required_version`.",
		ExpectedOutputs: map[string]string{"out": "ok"},
		AssertOutput: func(t *testing.T, output string) {
			assert.Equal(t, 1, strings.Count(diagnosticsSection(t, output), "/main.tf:2,3-30: Unsupported attribute: required_version; "+
				"Pulumi HCL does not enforce required_version. "+
				"Use required_version_range to declare a supported Pulumi version range.\n"))
		},
	})
}

// A parser warning in a child module reaches the user once.
func TestL2ParserWarningModule(t *testing.T) {
	t.Parallel()
	hcltest.RunCase(t, "l2_parser_warning_module", hcltest.Case{
		SkipRoundTrip:   "HCL -> PCL drops the `backend` block.",
		ExpectedOutputs: map[string]string{"out": "ok"},
		AssertOutput: func(t *testing.T, output string) {
			assert.Equal(t, 1, strings.Count(diagnosticsSection(t, output), "/modules/child/main.tf:2,3-18: Ignoring terraform backend block; "+
				"Pulumi manages state through its own backend; the terraform `backend` block is ignored. "+
				"Configure the backend via `pulumi login`.\n"))
		},
	})
}

// diagnosticsSection returns the "Diagnostics:" summary of the `pulumi up`
// output. The progress display also echoes each diagnostic, so a count over
// the whole output does not show how often the engine received it.
func diagnosticsSection(t *testing.T, output string) string {
	t.Helper()
	_, section, ok := strings.Cut(output, "\nDiagnostics:\n")
	require.True(t, ok, "no Diagnostics section in output:\n%s", output)
	section, _, ok = strings.Cut(section, "\nOutputs:\n")
	require.True(t, ok, "no Outputs section after Diagnostics in output:\n%s", output)
	return section
}
