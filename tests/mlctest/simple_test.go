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
	"github.com/pulumi/pulumi-hcl/tests/testutil/tfexec"
	"github.com/pulumi/pulumi/sdk/v3/go/common/apitype"
	"github.com/stretchr/testify/assert"
)

// A YAML program passes a variable to a local HCL component and reads its
// output; the component's resource is a child of the component.
func TestSimple(t *testing.T) {
	t.Parallel()
	mlctest.RunCase(t, "simple", mlctest.Case{
		Providers: []mlctest.Provider{
			{Name: "simple", Factory: providers.SimpleProvider},
		},
		ExpectedOutputs: map[string]string{
			"result": "hello-true",
		},
		AssertState: func(t *testing.T, resources []apitype.ResourceV3) {
			type res struct{ Type, URN, Parent string }
			got := make([]res, len(resources))
			for i, r := range resources {
				got[i] = res{string(r.Type), string(r.URN), string(r.Parent)}
			}
			const stack = "urn:pulumi:test::tfcompat::pulumi:pulumi:Stack::tfcompat-test"
			const module = "urn:pulumi:test::tfcompat::wrapper:index:Module::w"
			assert.Equal(t, []res{
				{"pulumi:pulumi:Stack", stack, ""},
				{"pulumi:providers:wrapper", "urn:pulumi:test::tfcompat::pulumi:providers:wrapper::default_0_0_0_dev", ""},
				{"wrapper:index:Module", module, stack},
				{"pulumi:providers:simple", "urn:pulumi:test::tfcompat::pulumi:providers:simple::default_0_0_0", ""},
				{
					"simple:index/resource:Resource",
					"urn:pulumi:test::tfcompat::wrapper:index:Module$simple:index/resource:Resource::w-res",
					module,
				},
			}, got)
		},
		AssertOps: func(t *testing.T, ops []tfexec.Op) {
			assert.Equal(t, []tfexec.Op{{
				Kind: tfexec.OpCreate,
				Type: "simple_resource",
				Inputs: map[string]any{
					"for_each": "", "input_one": "hello", "input_two": true,
					"prefix_result": "", "result": "", "version": "",
				},
				Outputs: map[string]any{
					"for_each": "", "input_one": "hello", "input_two": true,
					"prefix_result": "-hello", "result": "hello-true", "version": "",
				},
			}}, ops)
		},
	})
}
