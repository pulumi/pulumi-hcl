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

package pcltest_test

import (
	"os"
	"testing"

	"github.com/pulumi/pulumi-hcl/tests/testutil/pcltest"
	"github.com/pulumi/pulumi-hcl/tests/testutil/pulexec"
	"github.com/pulumi/pulumi-hcl/tests/testutil/tfcompat/providers"
	"github.com/stretchr/testify/require"
)

// TestL2SetMaxItemsOneRef covers a PCL reference into a TypeSet MaxItems=1
// block, which HCL reads with one().
func TestL2SetMaxItemsOneRef(t *testing.T) {
	t.Parallel()
	pcltest.RunCase(t, "l2_set_max_items_one_ref", pcltest.Case{
		Providers: []pcltest.Provider{
			{Name: "blocky", Factory: providers.BlockyProvider},
		},
		ExpectedOutputs: map[string]string{
			"label": "b",
		},
	})
}

// A splat must unwrap each element's set, rather than indexing the set or
// applying one() to the entire splat result.
func TestL2SetMaxItemsOneSplatRef(t *testing.T) {
	t.Parallel()
	source, err := os.ReadFile("testdata/cases/l2_set_max_items_one_splat_ref/main.pp")
	require.NoError(t, err)
	provs := []pulexec.Provider{
		pulexec.SDKv2Provider(t, "blocky", providers.BlockyProvider, nil),
	}
	pclDriver := pulexec.NewPCLDriver(t, provs, nil)
	files := map[string]string{"main.pp": string(source)}
	original, err := pclDriver.TryApply(t, files)
	require.NoError(t, err)
	require.Equal(t, map[string]string{"labels": `["b","b"]`}, original.Outputs)

	converted, err := pclDriver.Convert(t, files, "pcl", "hcl")
	require.NoError(t, err)
	hclDriver := pulexec.NewDriver(t, provs, nil)
	result, err := hclDriver.TryApply(t, converted)
	require.NoError(t, err)
	require.Equal(t, original.Outputs, result.Outputs)
}
