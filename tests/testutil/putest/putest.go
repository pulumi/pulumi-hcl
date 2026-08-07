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

// Package putest is the Pulumi-only half of the tfcompat harness: it runs a
// `.tf` program from testdata/cases/<name>/ through `pulumi up` against
// in-process bridged providers (pulexec's attach path), asserting directly on
// stack outputs, exported Pulumi state, and recorded provider operations. Use
// it for setups a tf-compatible program cannot produce (Customize foremost)
// and to pin the linked-in bridge's correct behavior on cases skipped in
// tfcompat because the terraform-provider plugin path diverges. Everything
// else belongs in the tfcompat harness, where OpenTofu defines the expected
// behavior.
package putest

import (
	"path/filepath"
	"runtime"
	"testing"

	pfprovider "github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	"github.com/pulumi/pulumi-hcl/tests/testutil"
	"github.com/pulumi/pulumi-hcl/tests/testutil/pulexec"
	"github.com/pulumi/pulumi-hcl/tests/testutil/tfexec"
	"github.com/pulumi/pulumi-terraform-bridge/v3/pkg/tfbridge"
	"github.com/pulumi/pulumi/sdk/v3/go/common/apitype"
	"github.com/stretchr/testify/require"
)

// Provider describes an in-memory TF provider bridged in-process for the
// Pulumi path.
type Provider struct {
	Name string
	// Factory builds an SDKv2 (helper/schema) provider. Exactly one of
	// Factory and PFFactory must be set.
	Factory func() *schema.Provider
	// PFFactory builds a terraform-plugin-framework provider.
	PFFactory func() pfprovider.Provider
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
	// ExpectedOutputs, if non-nil, must equal the stack outputs exactly.
	// Non-string outputs appear in their compact-JSON form (see
	// pulexec.Result).
	ExpectedOutputs map[string]string
	// AssertState, if set, runs against the exported resources after the
	// apply.
	AssertState func(t *testing.T, resources []apitype.ResourceV3)
	// AssertOps, if set, runs against the recorded provider operations after
	// the apply. SDKv2 providers record at the helper/schema CRUD boundary,
	// plugin-framework providers at the tfprotov6 boundary.
	AssertOps func(t *testing.T, ops []tfexec.Op)
}

// RunCase resolves testdata/cases/<caseName>/ relative to the calling test
// file, runs `pulumi up` on it, and applies the Case's assertions. A case
// directory containing only numbered stage subdirs (0/, 1/, ...) applies one
// file set per subdir in order; assertions run after the last apply.
func RunCase(t *testing.T, caseName string, c Case) {
	t.Helper()

	_, callerFile, _, _ := runtime.Caller(1)
	caseDir := filepath.Join(filepath.Dir(callerFile), "testdata", "cases", caseName)
	stages, _, err := testutil.LoadStages(caseDir)
	require.NoError(t, err)

	rec := &tfexec.Recorder{}
	provs := make([]pulexec.Provider, len(c.Providers))
	for i, p := range c.Providers {
		switch {
		case p.Factory != nil && p.PFFactory == nil:
			factory := p.Factory
			provs[i] = pulexec.SDKv2Provider(t, p.Name,
				func() *schema.Provider { return tfexec.Wrap(factory(), rec) }, p.Customize)
		case p.PFFactory != nil && p.Factory == nil:
			provs[i] = pulexec.PFProvider(t, p.Name, p.PFFactory, rec, p.Customize)
		default:
			t.Fatalf("provider %q: exactly one of Factory or PFFactory must be set", p.Name)
		}
	}

	driver := pulexec.NewDriver(t, provs, c.Config)
	var res pulexec.Result
	for i, files := range stages {
		res, err = driver.TryApply(t, files)
		require.NoErrorf(t, err, "stage %d: pulumi up failed", i)
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
