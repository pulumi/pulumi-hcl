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

// Package pcltest runs a `.pp` program from testdata/cases/<name>/ through
// `pulumi up` against in-process bridged providers, and round trips it
// through HCL. Use it for PCL to HCL conversion, where the source is the PCL
// that a user writes, not the output of HCL to PCL conversion.
package pcltest

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

// RunCase runs the PCL program in testdata/cases/<caseName>/, relative to the
// calling test file, and round trips it through HCL. See [stacktest.RunCase].
func RunCase(t *testing.T, caseName string, c Case) {
	t.Helper()

	_, callerFile, _, _ := runtime.Caller(1)
	stacktest.RunCase(t, filepath.Join(filepath.Dir(callerFile), "testdata"), caseName, "pcl", "hcl", c)
}
