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
	"github.com/pulumi/pulumi/sdk/v3/go/common/apitype"
	"github.com/stretchr/testify/assert"
)

// A local HCL component declares a native Pulumi provider with
// `source = "pulumi/echo"`; `pulumi install` resolves the attached provider
// and the component's resource is created through it. `pulumi install`
// writes no SDK descriptor for a Pulumi package that is not parameterized,
// so the runtime resolves the provider by name and its default provider
// carries no version.
func TestNative(t *testing.T) {
	t.Parallel()
	mlctest.RunCase(t, "native", mlctest.Case{
		Providers: []mlctest.Provider{
			{Name: "echo", Native: providers.EchoProvider},
		},
		ExpectedOutputs: map[string]string{
			"greeting": "hello world",
		},
		AssertState: func(t *testing.T, resources []apitype.ResourceV3) {
			type res struct{ Type, URN, Parent string }
			got := make([]res, len(resources))
			for i, r := range resources {
				got[i] = res{string(r.Type), string(r.URN), string(r.Parent)}
			}
			const stack = "urn:pulumi:test::tfcompat::pulumi:pulumi:Stack::tfcompat-test"
			const module = "urn:pulumi:test::tfcompat::greeter:index:Module::g"
			assert.Equal(t, []res{
				{"pulumi:pulumi:Stack", stack, ""},
				{"pulumi:providers:greeter", "urn:pulumi:test::tfcompat::pulumi:providers:greeter::default_0_0_0_dev", ""},
				{"greeter:index:Module", module, stack},
				{"pulumi:providers:echo", "urn:pulumi:test::tfcompat::pulumi:providers:echo::default", ""},
				{
					"echo:index:Thing",
					"urn:pulumi:test::tfcompat::greeter:index:Module$echo:index:Thing::g-hello",
					module,
				},
			}, got)
		},
	})
}
