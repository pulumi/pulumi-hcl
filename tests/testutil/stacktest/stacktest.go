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

// Package stacktest runs a program from testdata/cases/<name>/ through
// `pulumi up` against in-process bridged providers (pulexec's attach path),
// and asserts directly on stack outputs, exported Pulumi state, and recorded
// provider operations. It then round trips the program through the other
// language and runs the results. The hcltest and pcltest packages fix the
// language of the source.
package stacktest

import (
	"maps"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	pfprovider "github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	gp "github.com/pulumi/pulumi-go-provider"
	"github.com/pulumi/pulumi-hcl/tests/testutil"
	"github.com/pulumi/pulumi-hcl/tests/testutil/pulexec"
	"github.com/pulumi/pulumi-hcl/tests/testutil/tfexec"
	"github.com/pulumi/pulumi-terraform-bridge/v3/pkg/tfbridge"
	"github.com/pulumi/pulumi/sdk/v3/go/common/apitype"
	"github.com/pulumi/pulumi/sdk/v3/go/common/util/cmdutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Provider describes an in-memory TF provider bridged in-process for the
// Pulumi path.
type Provider struct {
	Name string
	// Factory builds an SDKv2 (helper/schema) provider. Exactly one of
	// Factory, PFFactory, and Native must be set.
	Factory func() *schema.Provider
	// PFFactory builds a terraform-plugin-framework provider.
	PFFactory func() pfprovider.Provider
	// Native builds a native pulumi-go-provider.
	//
	// It is an error to set Customize & Native.
	Native func() gp.Provider
	// Customize, if non-nil, runs against the bridged ProviderInfo so tests
	// can apply non-default Pulumi-side renames (or any other ProviderInfo
	// tweak) to exercise the bridge mapping behaviour.
	Customize func(*testing.T, *tfbridge.ProviderInfo)
}

// Case is the test description passed to RunCase.
type Case struct {
	Providers []Provider
	// Config is set as stack config.
	Config map[string]string
	// NoPreview skips the `pulumi preview` that otherwise precedes each stage's
	// `pulumi up`. Assertions run against the apply.
	NoPreview bool
	// ExpectedOutputs, if non-nil, must equal the stack outputs exactly.
	// Non-string outputs appear in their compact-JSON form (see
	// pulexec.Result).
	ExpectedOutputs map[string]string
	// AssertOutput, if set, checks the CLI output of the last apply.
	AssertOutput func(t *testing.T, output string)
	// AssertState, if set, runs against the exported resources after the
	// apply.
	AssertState func(t *testing.T, resources []apitype.ResourceV3)
	// AssertOps, if set, runs against the recorded provider operations after
	// the apply. SDKv2 providers record at the helper/schema CRUD boundary,
	// plugin-framework providers at the tfprotov6 boundary.
	AssertOps func(t *testing.T, ops []tfexec.Op)
	// SkipRoundTrip, if set, skips the round trip through the other
	// language. It is the reason the case cannot round trip.
	SkipRoundTrip string
	// SkipRunConverted, if set, skips the run of the program in the other
	// language that the round trip generates. It is the reason the program
	// cannot run.
	SkipRunConverted string
}

// newDrivers maps each language to the driver that runs a program in it.
var newDrivers = map[string]func(*testing.T, []pulexec.Provider, map[string]string) *pulexec.Driver{
	"hcl": pulexec.NewDriver,
	"pcl": pulexec.NewPCLDriver,
}

// RunCase loads testdata/cases/<caseName>/ under testdataDir, a program in
// the language from, runs `pulumi up` on it, and applies the Case's
// assertions. A case directory containing only numbered stage subdirs (0/,
// 1/, ...) applies one file set per subdir in order; assertions run after the
// last apply.
//
// RunCase then converts each stage to the language to and back with `pulumi
// convert`, and compares both results against the golden files in
// testdata/roundtrip/<caseName>/[<stage>/]{pcl,hcl}/. If PULUMI_ACCEPT is
// truthy, it writes the golden files instead. RunCase runs the converted
// stages on a new stack and applies the Case's assertions again. If the
// round-tripped stages differ from the source, RunCase also runs them on a new
// stack and applies the Case's assertions again.
func RunCase(t *testing.T, testdataDir, caseName, from, to string, c Case) {
	t.Helper()

	stages, staged, err := testutil.LoadStages(filepath.Join(testdataDir, "cases", caseName))
	require.NoError(t, err)

	driver := run(t, c, stages, newDrivers[from])

	t.Run("round-trip", func(t *testing.T) {
		if c.SkipRoundTrip != "" {
			t.Skip(c.SkipRoundTrip)
		}
		goldenDir := filepath.Join(testdataDir, "roundtrip", caseName)
		converted := make([]map[string]string, len(stages))
		roundTripped := make([]map[string]string, len(stages))
		identical := true
		for i, files := range stages {
			stageDir := goldenDir
			if staged {
				stageDir = filepath.Join(goldenDir, strconv.Itoa(i))
			}
			out, err := driver.Convert(t, files, from, to)
			require.NoErrorf(t, err, "stage %d: %s -> %s", i, from, to)
			assertGoldenDir(t, filepath.Join(stageDir, to), out)
			back, err := driver.Convert(t, out, to, from)
			require.NoErrorf(t, err, "stage %d: %s -> %s", i, to, from)
			assertGoldenDir(t, filepath.Join(stageDir, from), back)
			converted[i] = out
			roundTripped[i] = back
			identical = identical && maps.Equal(files, back)
		}
		t.Run(to, func(t *testing.T) {
			if c.SkipRunConverted != "" {
				t.Skip(c.SkipRunConverted)
			}
			run(t, c, converted, newDrivers[to])
		})
		t.Run(from, func(t *testing.T) {
			if identical {
				t.Skip("The round-tripped program is identical to the source.")
			}
			run(t, c, roundTripped, newDrivers[from])
		})
	})
}

// run applies stages in order on a new stack from newDriver, applies the Case's
// assertions, and returns the driver.
func run(
	t *testing.T, c Case, stages []map[string]string,
	newDriver func(*testing.T, []pulexec.Provider, map[string]string) *pulexec.Driver,
) *pulexec.Driver {
	t.Helper()

	rec := &tfexec.Recorder{}
	provs := make([]pulexec.Provider, len(c.Providers))
	for i, prov := range c.Providers {
		switch {
		case prov.Factory != nil && prov.PFFactory == nil && prov.Native == nil:
			factory := prov.Factory
			provs[i] = pulexec.SDKv2Provider(t, prov.Name,
				func() *schema.Provider { return tfexec.Wrap(factory(), rec) }, prov.Customize)
		case prov.PFFactory != nil && prov.Factory == nil && prov.Native == nil:
			provs[i] = pulexec.PFProvider(t, prov.Name, prov.PFFactory, rec, prov.Customize)
		case prov.Native != nil && prov.Factory == nil && prov.PFFactory == nil:
			if prov.Customize != nil {
				t.Fatalf("provider %q: Customize does not apply to a Native provider", prov.Name)
			}
			provs[i] = pulexec.NativeProvider(prov.Name, "0.0.1", prov.Native())
		default:
			t.Fatalf("provider %q: exactly one of Factory, PFFactory, or Native must be set", prov.Name)
		}
	}

	driver := newDriver(t, provs, c.Config)
	var res pulexec.Result
	var err error
	for i, files := range stages {
		if !c.NoPreview {
			_, err = driver.Preview(t, files)
			require.NoErrorf(t, err, "stage %d: pulumi preview failed", i)
		}
		res, err = driver.TryApply(t, files)
		require.NoErrorf(t, err, "stage %d: pulumi up failed", i)
	}

	if c.ExpectedOutputs != nil {
		require.Equal(t, c.ExpectedOutputs, res.Outputs)
	}
	if c.AssertOutput != nil {
		c.AssertOutput(t, res.Output)
	}
	if c.AssertState != nil {
		c.AssertState(t, res.Resources)
	}
	if c.AssertOps != nil {
		c.AssertOps(t, rec.Ops())
	}
	return driver
}

// assertGoldenDir compares got against the files under dir. If PULUMI_ACCEPT
// is truthy, it replaces the contents of dir with got instead.
func assertGoldenDir(t *testing.T, dir string, got map[string]string) {
	t.Helper()

	if cmdutil.IsTruthy(os.Getenv("PULUMI_ACCEPT")) {
		require.NoError(t, os.RemoveAll(dir))
		for path, content := range got {
			fullPath := filepath.Join(dir, path)
			require.NoError(t, os.MkdirAll(filepath.Dir(fullPath), 0o755))
			require.NoError(t, os.WriteFile(fullPath, []byte(content), 0o644)) //nolint:gosec // golden file
		}
		return
	}
	want, err := testutil.LoadCaseDir(dir)
	require.NoError(t, err, "golden files in %s not found; run with PULUMI_ACCEPT=1 to generate", dir)
	assert.Equal(t, want, got)
}
