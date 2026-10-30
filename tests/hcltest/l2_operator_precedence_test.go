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

package hcltest_test

import (
	"testing"

	"github.com/pulumi/pulumi-hcl/tests/testutil/hcltest"
)

// TestL2OperatorPrecedence covers parentheses that change the value of an
// expression. PCL keeps no parentheses, so the round trip must write them
// back where HCL precedence needs them.
func TestL2OperatorPrecedence(t *testing.T) {
	t.Parallel()
	hcltest.RunCase(t, "l2_operator_precedence", hcltest.Case{
		ExpectedOutputs: map[string]string{
			"sum_times":         "9",
			"minus_minus":       "9",
			"not_and":           "true",
			"negate_sum":        "-3",
			"eq_eq":             "true",
			"conditional_index": "1",
			"conditional_cond":  "no",
		},
	})
}
