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

// Package tfcompat is the Terraform-compatibility test harness. RunCase loads
// a `.tf` program from testdata/cases/<name>/ and runs it through two paths,
// asserting they produce identical outputs and provoke identical provider
// operations:
//
//	Path A: real `tofu apply` against the in-memory TF providers (via reattach).
//	Path B: real `pulumi up` against the same providers, reached through the
//	        real terraform-provider plugin (via PULUMI_BRIDGE_REATTACH_PROVIDERS),
//	        running the pulumi-language-hcl runtime.
//
// Recordings are captured by wrapping each *schema.Provider with tfexec.Wrap
// before either path sees it, so the comparisons are apples-to-apples.
//
// Program files always live on disk under the case directory. A flat
// directory holds one file set; a directory containing only numbered stage
// subdirs (0/, 1/, ...) holds one file set per stage, for tests whose program
// changes between operations. Case.Stages carries optional per-stage behavior
// (mode, expected error, output assertion) and is matched positionally
// against the disk stages; a flat directory paired with multiple Stages
// entries runs the same program once per entry (e.g. preview then apply, or
// apply then destroy).
//
// Values that are only known at runtime (ports, temp paths, credentials) are
// never spliced into program text; declare a variable in the program and feed
// it through Case.Config, which reaches both paths identically.
package tfcompat

import (
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	pfprovider "github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	"github.com/pulumi/pulumi-hcl/tests/testutil"
	"github.com/pulumi/pulumi-hcl/tests/testutil/pulexec"
	"github.com/pulumi/pulumi-hcl/tests/testutil/tfexec"
	"github.com/pulumi/pulumi/sdk/v3/go/common/apitype"
	"github.com/stretchr/testify/require"
)

// Provider describes an in-memory TF provider that's available to both paths.
type Provider struct {
	Name string
	// Factory builds an SDKv2 (helper/schema) provider. Exactly one of
	// Factory and PFFactory must be set.
	Factory func() *schema.Provider
	// PFFactory builds a terraform-plugin-framework provider.
	PFFactory func() pfprovider.Provider
}

// buildTerraformProviders wires the tofu-side path: each provider records at
// the helper/schema CRUD boundary (tfexec.Wrap) or the tfprotov6 boundary
// (tfexec.WrapServer) into the path's shared recorder. Both paths build a
// fresh provider per configured instance (a provider block with for_each
// yields several).
func buildTerraformProviders(
	t *testing.T, provs []Provider, rec *tfexec.Recorder,
) []tfexec.Provider {
	t.Helper()
	tfProvs := make([]tfexec.Provider, len(provs))
	for i, p := range provs {
		switch {
		case p.Factory != nil && p.PFFactory == nil:
			factory := p.Factory
			tfProvs[i] = tfexec.SDKv2Provider(t, p.Name, func() *schema.Provider { return tfexec.Wrap(factory(), rec) })
		case p.PFFactory != nil && p.Factory == nil:
			tfProvs[i] = tfexec.PFProvider(p.Name, p.PFFactory, rec)
		default:
			t.Fatalf("provider %q: exactly one of Factory or PFFactory must be set", p.Name)
		}
	}
	return tfProvs
}

// buildDynamicPulumiProviders wires the Pulumi-side path: each provider is
// served in-process over go-plugin and the engine reaches it through the real
// terraform-provider plugin (pulexec.SDKv2ProviderDynamic /
// PFProviderDynamic), so the pulumi path runs the full parameterization flow
// the production runtime does.
func buildDynamicPulumiProviders(
	t *testing.T, provs []Provider, rec *tfexec.Recorder,
) []pulexec.Provider {
	t.Helper()
	pulProvs := make([]pulexec.Provider, len(provs))
	for i, p := range provs {
		switch {
		case p.Factory != nil && p.PFFactory == nil:
			factory := p.Factory
			pulProvs[i] = pulexec.SDKv2ProviderDynamic(t, p.Name, func() *schema.Provider { return tfexec.Wrap(factory(), rec) })
		case p.PFFactory != nil && p.Factory == nil:
			pulProvs[i] = pulexec.PFProviderDynamic(t, p.Name, p.PFFactory, rec)
		default:
			t.Fatalf("provider %q: exactly one of Factory or PFFactory must be set", p.Name)
		}
	}
	return pulProvs
}

// Case is the test description passed to RunCase.
type Case struct {
	Providers []Provider
	// Config is set as stack config on the Pulumi path and passed as -var
	// flags on the tofu path. It is the only channel for values not known
	// until runtime: the program declares a variable and the test feeds it
	// here.
	Config map[string]string

	// AssertState, if set, runs after `pulumi up`. Use for assertions on
	// resource fields that aren't reachable via stack outputs (e.g. Protect).
	AssertState func(t *testing.T, resources []apitype.ResourceV3)

	// OrderDeterministic, if true, compares provider operations in arrival
	// order instead of sorted, so the op sequence itself asserts ordering
	// (e.g. that a dependency edge serializes creates or destroys).
	//
	// Use only when the program's dependency graph forces a total order over
	// every recorded op: both runtimes execute independent ops concurrently,
	// so any two ops not ordered by a dependency chain record in a racy order
	// and the comparison flakes. That includes data-source reads and
	// provider-function calls — each must sit on the same chain.
	//
	// An ordering focused test should also delay the op that must complete
	// *first* (see providers.OrderProvider): if the edge under test goes
	// missing, the ops run concurrently and the undelayed op reliably records
	// ahead of the delayed one, so the regression fails deterministically
	// instead of racing.
	OrderDeterministic bool

	// Stages attaches per-stage behavior to the stages loaded from disk,
	// matched positionally. For a case directory with numbered stage subdirs
	// its length must equal the subdir count. For a flat directory each entry
	// runs the whole directory's file set, so N entries drive N operations
	// over the same program. Omitted entries (or a nil Stages) default to a
	// plain apply.
	Stages []Stage

	// SkipImport, when non-empty, skips the case's state checks with the
	// given reason: some cases cannot round-trip through a fresh import
	// driver (e.g. providers whose simulated backend is per-instance state).
	SkipImport string
}

type StageMode int

const (
	StageApply   StageMode = iota // `tofu apply` / `pulumi up`; default
	StagePreview                  // `tofu plan` / `pulumi preview`
	StageDestroy                  // `tofu destroy` / `pulumi destroy`
)

// Stage describes how to run one operation of a Case. It carries behavior
// only; the program files come from the case directory on disk.
type Stage struct {
	Mode StageMode
	// ExpectErr, if non-empty, requires both runtimes to fail with an error
	// containing this substring.
	ExpectErr string
	// AssertOutput, if set, runs against each runtime's combined operation
	// output (so both are held to the same user-visible diagnostics, e.g. a
	// check-block warning's error_message). Called once per runtime. Only
	// invoked for a successful StageApply.
	AssertOutput func(t *testing.T, output string)
}

// stageRun pairs one stage's program files with its behavior.
type stageRun struct {
	files map[string]string
	Stage
}

// RunCase resolves testdata/cases/<caseName>/ relative to the calling test
// file, loads its stages, and runs the comparison.
func RunCase(t *testing.T, caseName string, c Case) {
	t.Helper()

	_, callerFile, _, _ := runtime.Caller(1)
	caseDir := filepath.Join(filepath.Dir(callerFile), "testdata", "cases", caseName)
	runCaseFromDir(t, caseDir, c)
}

// resolveStages matches Case.Stages positionally against the file sets loaded
// from disk. A flat case directory (one file set) fans out to max(1, len(meta))
// stages over the same files; numbered stage dirs require len(meta) to be
// zero or exactly the dir count.
func resolveStages(fileSets []map[string]string, numbered bool, meta []Stage) ([]stageRun, error) {
	if numbered {
		if len(meta) != 0 && len(meta) != len(fileSets) {
			return nil, fmt.Errorf(
				"case has %d numbered stage dirs but Case.Stages has %d entries",
				len(fileSets), len(meta))
		}
		runs := make([]stageRun, len(fileSets))
		for i, files := range fileSets {
			runs[i] = stageRun{files: files}
			if len(meta) != 0 {
				runs[i].Stage = meta[i]
			}
		}
		return runs, nil
	}

	runs := make([]stageRun, max(1, len(meta)))
	for i := range runs {
		runs[i] = stageRun{files: fileSets[0]}
		if i < len(meta) {
			runs[i].Stage = meta[i]
		}
	}
	return runs, nil
}

// runCaseFromDir runs a case whose program files live in caseDir. Kept
// separate from RunCase so self-tests can drive the harness with a
// t.TempDir() rather than an on-disk testdata directory.
//
// Ops/outputs/AssertState are compared against the last successful apply
// stage; preview stages produce no state.
func runCaseFromDir(t *testing.T, caseDir string, c Case) {
	t.Helper()

	fileSets, numbered := loadStages(t, caseDir)
	stages, err := resolveStages(fileSets, numbered, c.Stages)
	require.NoError(t, err)

	recA, recB := &tfexec.Recorder{}, &tfexec.Recorder{}
	tfProvs := buildTerraformProviders(t, c.Providers, recA)
	pulProvs := buildDynamicPulumiProviders(t, c.Providers, recB)

	tfDriver := tfexec.NewDriver(t, tfProvs)
	pulDriver := pulexec.NewDriver(t, pulProvs, c.Config)

	var lastOK pulexec.Result
	var lastOKTfOutputs map[string]string
	for i, stage := range stages {
		var wg sync.WaitGroup
		wg.Add(2)

		var tfOut map[string]string
		var tfText string
		var tfErr error
		var pulRes pulexec.Result
		var pulErr error
		go func() {
			defer wg.Done()
			switch stage.Mode {
			case StagePreview:
				tfErr = tfDriver.Plan(t, stage.files, c.Config)
			case StageDestroy:
				tfErr = tfDriver.Destroy(t, stage.files, c.Config)
			default:
				tfOut, tfText, tfErr = tfDriver.TryApply(t, stage.files, c.Config)
			}
		}()
		go func() {
			defer wg.Done()
			switch stage.Mode {
			case StagePreview:
				_, pulErr = pulDriver.Preview(t, stage.files)
			case StageDestroy:
				pulErr = pulDriver.Destroy(t, stage.files)
			default:
				pulRes, pulErr = pulDriver.TryApply(t, stage.files)
			}
		}()
		wg.Wait()

		tfLabel, pulLabel := "tofu apply", "pulumi up"
		switch stage.Mode {
		case StagePreview:
			tfLabel, pulLabel = "tofu plan", "pulumi preview"
		case StageDestroy:
			tfLabel, pulLabel = "tofu destroy", "pulumi destroy"
		}

		if stage.ExpectErr != "" {
			require.Errorf(t, tfErr,
				"stage %d: %s was expected to fail with %q", i, tfLabel, stage.ExpectErr)
			require.Errorf(t, pulErr,
				"stage %d: %s was expected to fail with %q", i, pulLabel, stage.ExpectErr)
			require.Containsf(t, tfErr.Error(), stage.ExpectErr,
				"stage %d: %s error did not contain expected substring", i, tfLabel)
			require.Containsf(t, pulErr.Error(), stage.ExpectErr,
				"stage %d: %s error did not contain expected substring", i, pulLabel)
			continue
		}

		require.NoErrorf(t, tfErr, "stage %d: %s failed unexpectedly", i, tfLabel)
		require.NoErrorf(t, pulErr, "stage %d: %s failed unexpectedly", i, pulLabel)

		if stage.Mode == StageApply {
			if stage.AssertOutput != nil {
				stage.AssertOutput(t, tfText)
				stage.AssertOutput(t, pulRes.Output)
			}
			lastOK = pulRes
			lastOKTfOutputs = tfOut
			if !t.Failed() {
				runImportCheck(t, c, i, stage.files, tfDriver.Dir())
			}
		}
	}

	if lastOKTfOutputs != nil {
		require.Equal(t,
			scrubTmpDir(lastOKTfOutputs, tfDriver.Dir()),
			scrubTmpDir(lastOK.Outputs, pulDriver.Dir()),
			"stack outputs differ between tofu apply and pulumi up")
		if c.OrderDeterministic {
			require.NoError(t, tfexec.Subsumes(recA.OrderedOps(), recB.OrderedOps()),
				"provider operation order differs between tofu apply and pulumi up")
		} else {
			require.NoError(t, tfexec.Subsumes(recA.Ops(), recB.Ops()),
				"provider operations differ between tofu apply and pulumi up")
		}
		if c.AssertState != nil {
			c.AssertState(t, lastOK.Resources)
		}
	}
}

// scrubTmpDir replaces driver-specific temp paths in output values with the
// sentinel "<TMPDIR>" so cross-driver comparison ignores the fact that tofu
// and pulumi run from different temp directories. Symlink-resolved forms (on
// macOS, /var/folders/... is a symlink to /private/var/folders/...) are also
// scrubbed.
func scrubTmpDir(outputs map[string]string, dir string) map[string]string {
	if dir == "" {
		return outputs
	}
	forms := []string{dir}
	if resolved, err := filepath.EvalSymlinks(dir); err == nil && resolved != dir {
		forms = append(forms, resolved)
	}
	out := make(map[string]string, len(outputs))
	for k, v := range outputs {
		for _, form := range forms {
			v = strings.ReplaceAll(v, form, "<TMPDIR>")
		}
		out[k] = v
	}
	return out
}

// loadStages wraps testutil.LoadStages, failing the test on error.
func loadStages(t *testing.T, caseDir string) ([]map[string]string, bool) {
	t.Helper()
	fileSets, numbered, err := testutil.LoadStages(caseDir)
	require.NoError(t, err)
	return fileSets, numbered
}
