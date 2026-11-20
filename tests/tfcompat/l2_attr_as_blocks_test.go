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

package tfcompat_test

import (
	"testing"

	"github.com/pulumi/pulumi-hcl/tests/testutil/tfcompat"
	"github.com/pulumi/pulumi-hcl/tests/testutil/tfcompat/providers"
)

// TestL2AttrAsBlocks writes a list of objects attribute with `rule { ... }`
// blocks and a `dynamic "rule"` block. OpenTofu reads the blocks as the
// elements of the attribute, which is how the `ingress` of
// `aws_security_group` is written.
//
// https://github.com/pulumi/pulumi-hcl/issues/838
func TestL2AttrAsBlocks(t *testing.T) {
	t.Parallel()
	tfcompat.RunCase(t, "l2_attr_as_blocks", tfcompat.Case{
		Providers: []tfcompat.Provider{
			{Name: "pfx", PFFactory: providers.PFXProvider},
		},
	})
}
