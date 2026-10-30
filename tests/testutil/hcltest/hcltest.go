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

// Package hcltest is the Pulumi-only half of the tfcompat harness: it runs a
// `.tf` program from testdata/cases/<name>/ through `pulumi up` against
// in-process bridged providers (pulexec's attach path), asserting directly on
// stack outputs, exported Pulumi state, and recorded provider operations. Use
// it for setups a tf-compatible program cannot produce (Customize foremost)
// and to pin the linked-in bridge's correct behavior on cases skipped in
// tfcompat because the terraform-provider plugin path diverges. Everything
// else belongs in the tfcompat harness, where OpenTofu defines the expected
// behavior.
package hcltest

import (
	"path/filepath"
	"runtime"
	"testing"

	"github.com/pulumi/pulumi-hcl/tests/testutil/stacktest"
)

type (
	// Provider is a [stacktest.Provider].
	Provider = stacktest.Provider
	// Case is a [stacktest.Case].
	Case = stacktest.Case
)

// RunCase runs the HCL program in testdata/cases/<caseName>/, relative to the
// calling test file, and round trips it through PCL. See [stacktest.RunCase].
func RunCase(t *testing.T, caseName string, c Case) {
	t.Helper()

	_, callerFile, _, _ := runtime.Caller(1)
	stacktest.RunCase(t, filepath.Join(filepath.Dir(callerFile), "testdata"), caseName, "hcl", "pcl", c)
}
