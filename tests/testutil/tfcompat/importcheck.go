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
	"fmt"
	"path/filepath"
	"testing"

	"github.com/pulumi/pulumi-hcl/tests/testutil/pulexec"
	"github.com/pulumi/pulumi-hcl/tests/testutil/tfexec"
	"github.com/pulumi/pulumi/sdk/v3/go/common/apitype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// runImportCheck re-runs the case through the TF-state import flow: the
// terraform state produced by the tofu-side apply is fed to
// `pulumi import --from hcl` against a fresh Pulumi stack, and the next
// preview and up of the case's program must touch no resource — mirroring the
// promise that if `tofu plan` is clean after `tofu apply`, then Pulumi
// operations are no-ops after `pulumi import --from hcl`.
//
// The CLI drives the whole flow: it spawns the converter (an in-process server
// behind a PATH shim) and resolves mappings through its own engine-side
// mapper, which reaches the case's providers through the terraform-provider
// plugin exactly like the main comparison does. The same flow against real
// released plugins is covered by tests/smoke.
func runImportCheck(
	t *testing.T, c Case, stage int, files map[string]string, tfStateDir string,
) {
	t.Helper()

	// Later stages get an explicit suffix: Go's own duplicate-name suffix
	// ("#01") breaks the file-backend URL, which parses "#" as a fragment.
	name := "state-import"
	if stage > 0 {
		name = fmt.Sprintf("state-import-%d", stage)
	}
	t.Run(name, func(t *testing.T) {
		if c.SkipImport != "" {
			t.Skipf("state check skipped: %s", c.SkipImport)
		}
		for _, p := range c.Providers {
			if p.PFFactory != nil {
				t.Skip("TODO[github.com/pulumi/pulumi-hcl#167]: state-import check does not support plugin-framework providers yet")
			}
		}
		statePath := filepath.Join(tfStateDir, "terraform.tfstate")
		require.FileExists(t, statePath)

		// Import into a fresh stack.
		rec := &tfexec.Recorder{}
		pulProvs := buildDynamicPulumiProviders(t, c.Providers, rec)
		d := pulexec.NewDriver(t, pulProvs, c.Config)
		out, err := d.Import(t, files, statePath)
		require.NoErrorf(t, err, "pulumi import --from hcl failed:\n%s", out)

		// A clean import means the next operations plan and perform no
		// resource changes, observed at the TF CRUD boundary: a no-op plan
		// invokes no CRUD, so any Create/Update/Delete the recorder sees is a
		// resource change. A planned change surfaces when the up executes it.
		// The stack shell, default providers, and module component shells are
		// still created, but none of those reach the TF provider.
		summary, err := d.Preview(t, files)
		require.NoError(t, err)
		assertNoMutatingOps(t, "preview", rec)
		assertNoDestructiveOps(t, "preview", summary)

		res, err := d.TryApply(t, files)
		require.NoErrorf(t, err, "up after import failed:\n%s", res.Output)
		assertNoMutatingOps(t, "up", rec)
		assertNoDestructiveOps(t, "up", res.Changes)

		// TF state records no module inputs, so an imported component carries
		// none and the up above records them. Nothing may remain after that.
		settled, err := d.Preview(t, files)
		require.NoError(t, err)
		assertNoMutatingOps(t, "settled preview", rec)
		for op, n := range settled {
			assert.Equalf(t, apitype.OpSame, op, "%d %q operations remain after the import converged", n, op)
		}
	})
}

// assertNoMutatingOps fails if the recorder saw any Create, Update, or
// Delete since the import.
func assertNoMutatingOps(t *testing.T, phase string, rec *tfexec.Recorder) {
	t.Helper()
	for _, op := range rec.Ops() {
		switch op.Kind {
		case tfexec.OpCreate, tfexec.OpUpdate, tfexec.OpDelete:
			t.Errorf("%s after import performed a mutating %q operation (kind %d)", phase, op.Type, op.Kind)
		}
	}
}

// assertNoDestructiveOps fails on any operation that would disturb an existing
// resource. Creates cover the stack shell, default providers and component
// shells; updates cover the component inputs the import cannot carry. The
// recorder cannot see either: resources the engine's builtin provider serves
// in-process (e.g. terraform_data's Stash) reach no provider RPC.
func assertNoDestructiveOps[K ~string](t *testing.T, phase string, ops map[K]int) {
	t.Helper()
	for op, n := range ops {
		switch string(op) {
		case "same", "create", "read", "update":
		default:
			t.Errorf("%s after import performed %d %q operations", phase, n, op)
		}
	}
}
