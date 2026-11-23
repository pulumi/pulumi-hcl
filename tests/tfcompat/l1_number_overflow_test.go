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

	"github.com/pulumi-labs/pulumi-hcl/tests/testutil/tfcompat"
)

// TestL1NumberOverflow checks that a numeric value exceeding the float64 range
// keeps its finite arbitrary-precision value. OpenTofu evaluates 1e400 (and
// 1e308*100) to a finite cty big.Float; pulumi-hcl must not collapse it to
// +Infinity when serializing the stack output.
func TestL1NumberOverflow(t *testing.T) {
	t.Parallel()
	tfcompat.RunCase(t, "l1_number_overflow", tfcompat.Case{})
}
