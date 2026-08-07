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

// TestL2SingleSegmentToken pins that providers whose type token is a single
// word (no underscore) — e.g. `hashicorp/external` exposing `data "external"`,
// or `hashicorp/http` exposing `data "http"` — resolve through the dynamic
// bridge without being rejected as malformed tokens. The hand-renamed
// body-mapping variant lives in tests/putest, which supports Customize.
func TestL2SingleSegmentToken(t *testing.T) {
	t.Parallel()
	tfcompat.RunCase(t, "l2_single_segment_token", tfcompat.Case{
		Providers: []tfcompat.Provider{
			{Name: "single", Factory: providers.SingleSegmentProvider},
		},
	})
}
