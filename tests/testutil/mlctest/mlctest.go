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

// Package mlctest is the component half of putest: it runs a YAML program
// that consumes one local HCL component directory, pins the component's
// package schema against a golden file, and asserts on stack outputs,
// exported Pulumi state, and recorded provider operations.
package mlctest

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	pfprovider "github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	gp "github.com/pulumi/pulumi-go-provider"
	"github.com/pulumi/pulumi-hcl/tests/testutil"
	"github.com/pulumi/pulumi-hcl/tests/testutil/pulexec"
	"github.com/pulumi/pulumi-hcl/tests/testutil/tfexec"
	"github.com/pulumi/pulumi/sdk/v3/go/common/apitype"
	"github.com/pulumi/pulumi/sdk/v3/go/common/util/cmdutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Provider describes an in-memory provider the component's resources use.
// Exactly one of Factory, PFFactory, and Native must be set.
type Provider struct {
	Name string
	// Factory builds an SDKv2 (helper/schema) Terraform provider.
	Factory func() *schema.Provider
	// PFFactory builds a terraform-plugin-framework provider.
	PFFactory func() pfprovider.Provider
	// Native builds a native pulumi-go-provider.
	Native func() gp.Provider
}

// Case is the test description passed to RunCase.
type Case struct {
	Providers []Provider
	// Config is set as stack config.
	Config map[string]string
	// ExpectedOutputs, if non-nil, must equal the stack outputs exactly.
	// Non-string outputs appear in their compact-JSON form (see
	// pulexec.Result).
	ExpectedOutputs map[string]string
	// AssertState, if set, runs against the exported resources after the
	// apply.
	AssertState func(t *testing.T, resources []apitype.ResourceV3)
	// AssertOps, if set, runs against the recorded provider operations after
	// the apply.
	AssertOps func(t *testing.T, ops []tfexec.Op)
}

// RunCase resolves testdata/cases/<caseName>/ relative to the calling test
// file and runs it. The case directory holds Main.yaml, the golden
// schema.json, and exactly one subdirectory: the HCL component, whose name is
// the package name the YAML program refers to.
func RunCase(t *testing.T, caseName string, c Case) {
	t.Helper()

	_, callerFile, _, _ := runtime.Caller(1)
	caseDir := filepath.Join(filepath.Dir(callerFile), "testdata", "cases", caseName)
	files, err := testutil.LoadCaseDir(caseDir)
	require.NoError(t, err)
	delete(files, "schema.json")

	pkgs := map[string]struct{}{}
	for path := range files {
		if dir, _, nested := strings.Cut(path, string(filepath.Separator)); nested {
			pkgs[dir] = struct{}{}
		}
	}
	require.Lenf(t, pkgs, 1, "case %s must hold exactly one component directory", caseName)
	var pkg string
	for dir := range pkgs {
		pkg = dir
	}
	_, hasMain := files["Main.yaml"]
	require.Truef(t, hasMain, "case %s has no Main.yaml", caseName)

	rec := &tfexec.Recorder{}
	driver := pulexec.NewYAMLDriver(t, pkg, providers(t, rec, c.Providers), c.Config)

	schema, err := driver.PackageSchema(t, files, pkg)
	require.NoError(t, err, "pulumi package get-schema failed")
	assertGolden(t, filepath.Join(caseDir, "schema.json"), schema)

	_, err = driver.Preview(t, files)
	require.NoError(t, err, "pulumi preview failed")
	res, err := driver.TryApply(t, files)
	require.NoError(t, err, "pulumi up failed")

	changes, err := driver.Preview(t, files)
	require.NoError(t, err, "second pulumi preview failed")
	for op := range changes {
		require.Equalf(t, apitype.OpSame, op, "second preview is not a no-op: %v", changes)
	}

	if c.ExpectedOutputs != nil {
		require.Equal(t, c.ExpectedOutputs, res.Outputs)
	}
	if c.AssertState != nil {
		c.AssertState(t, res.Resources)
	}
	if c.AssertOps != nil {
		c.AssertOps(t, rec.Ops())
	}
}

// providers builds the in-process pulexec form of each Provider, with every
// provider operation recorded into rec. SDKv2 providers record at the
// helper/schema CRUD boundary, plugin-framework providers at the tfprotov6
// boundary; native providers are not recorded.
func providers(t *testing.T, rec *tfexec.Recorder, provs []Provider) []pulexec.Provider {
	t.Helper()
	out := make([]pulexec.Provider, len(provs))
	for i, prov := range provs {
		switch {
		case prov.Factory != nil && prov.PFFactory == nil && prov.Native == nil:
			factory := prov.Factory
			out[i] = pulexec.SDKv2ProviderDynamic(t, prov.Name,
				func() *schema.Provider { return tfexec.Wrap(factory(), rec) })
		case prov.PFFactory != nil && prov.Factory == nil && prov.Native == nil:
			out[i] = pulexec.PFProviderDynamic(t, prov.Name, prov.PFFactory, rec)
		case prov.Native != nil && prov.Factory == nil && prov.PFFactory == nil:
			out[i] = pulexec.NativeProvider(prov.Name, "0.0.1", prov.Native())
		default:
			t.Fatalf("provider %q: exactly one of Factory, PFFactory, or Native must be set", prov.Name)
		}
	}
	return out
}

// assertGolden compares got against the JSON golden file at path, with both
// sides re-indented so formatting differences do not fail the test. If
// PULUMI_ACCEPT is truthy, it writes the golden file instead.
func assertGolden(t *testing.T, path string, got []byte) {
	t.Helper()

	indent := func(raw []byte) string {
		var buf bytes.Buffer
		require.NoError(t, json.Indent(&buf, bytes.TrimSpace(raw), "", "  "))
		return buf.String() + "\n"
	}

	if cmdutil.IsTruthy(os.Getenv("PULUMI_ACCEPT")) {
		require.NoError(t, os.WriteFile(path, []byte(indent(got)), 0o644)) //nolint:gosec // golden file
		return
	}
	want, err := os.ReadFile(path) //nolint:gosec // test-controlled path
	require.NoError(t, err, "golden file %s not found; run with PULUMI_ACCEPT=1 to generate", path)
	assert.Equal(t, indent(want), indent(got))
}
