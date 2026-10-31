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

package mlctest_test

import (
	"testing"

	"github.com/pulumi/pulumi-hcl/tests/testutil/mlctest"
	"github.com/pulumi/pulumi-hcl/tests/testutil/tfcompat/providers"
)

// A local HCL component sets a `MaxItems: 1` block. The bridge flattens the
// block to a single object, so the component needs the bridge mapping of its
// Terraform provider to accept the block syntax. TestL2BlockSingular in
// tests/tfcompat runs the same HCL as a root program.
func TestBlockSingular(t *testing.T) {
	t.Parallel()
	mlctest.RunCase(t, "block_singular", mlctest.Case{
		Providers: []mlctest.Provider{
			{Name: "blocky", Factory: providers.BlockyProvider},
		},
		ExpectedOutputs: map[string]string{
			"summary": "name=alpha|settings.mode=fast|settings.verbose=true|tags=[]",
		},
	})
}

// A local HCL component reads a `MaxItems: 1` block by index. Terraform
// exposes the block as a list, so `settings[0]` needs the bridge mapping both
// to infer the output type for the schema and to read the value at runtime.
func TestBlockSingularIndex(t *testing.T) {
	t.Parallel()
	mlctest.RunCase(t, "block_singular_index", mlctest.Case{
		Providers: []mlctest.Provider{
			{Name: "blocky", Factory: providers.BlockyProvider},
		},
		ExpectedOutputs: map[string]string{
			"mode": "fast",
		},
	})
}
