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

package putest_test

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	gp "github.com/pulumi/pulumi-go-provider"
	"github.com/pulumi/pulumi-hcl/tests/testutil/putest"
	"github.com/pulumi/pulumi/pkg/v3/codegen/schema"
	"github.com/pulumi/pulumi/sdk/v3/go/common/apitype"
	"github.com/pulumi/pulumi/sdk/v3/go/property"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// oddPropertyNames are schema property names that are not HCL identifiers,
// keyed by the HCL name of each. Each one binds, and each one reduces to a
// different name.
var oddPropertyNames = map[string]string{
	"dot_ted":    "dot.ted",
	"sla_sh":     "sla/sh",
	"at":         "@at",
	"bang":       "bang!",
	"dollar":     "$dollar",
	"colon_name": "colon:name",
	"plus_name":  "plus+name",
	"digits":     "12digits",
	"emoji":      "😃emoji",
	"mid_tilde":  "mid~tilde",
}

// oddProvider is a native provider whose tokens and property names contain
// runes that an HCL identifier cannot contain. Its resources echo their
// inputs, and its function returns its arguments.
type oddProvider struct {
	mu sync.Mutex
	// invokes holds the arguments of each invoke since the provider was
	// last built.
	invokes []property.Map
}

func (o *oddProvider) build() gp.Provider {
	o.mu.Lock()
	o.invokes = nil
	o.mu.Unlock()
	return gp.Provider{
		GetSchema: func(context.Context, gp.GetSchemaRequest) (gp.GetSchemaResponse, error) {
			props := map[string]schema.PropertySpec{
				"value": {TypeSpec: schema.TypeSpec{Type: "string"}},
			}
			for _, name := range oddPropertyNames {
				props[name] = schema.PropertySpec{TypeSpec: schema.TypeSpec{Type: "string"}}
			}
			resource := schema.ResourceSpec{
				InputProperties: props,
				ObjectTypeSpec:  schema.ObjectTypeSpec{Properties: props},
			}
			spec := schema.PackageSpec{
				Name:    "odd",
				Version: "0.0.1",
				Resources: map[string]schema.ResourceSpec{
					"odd:dot.ted/v1:Thing": resource,
					"odd:mod:Dot.Member":   resource,
				},
				Functions: map[string]schema.FunctionSpec{
					"odd:dot.ted/v1:getThing": {
						Inputs:  &schema.ObjectTypeSpec{Properties: props},
						Outputs: &schema.ObjectTypeSpec{Properties: props},
					},
				},
			}
			raw, err := json.Marshal(spec)
			return gp.GetSchemaResponse{Schema: string(raw)}, err
		},
		Invoke: func(_ context.Context, req gp.InvokeRequest) (gp.InvokeResponse, error) {
			o.mu.Lock()
			defer o.mu.Unlock()
			o.invokes = append(o.invokes, req.Args)
			return gp.InvokeResponse{Return: req.Args}, nil
		},
	}
}

// assertInvokes checks that at least one invoke happened since the provider
// was last built, and that each one got args.
func (o *oddProvider) assertInvokes(t *testing.T, args map[string]any) {
	want := property.NewMap(map[string]property.Value{})
	for k, v := range args {
		want = want.Set(k, property.New(v.(string)))
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	require.NotEmpty(t, o.invokes)
	for _, got := range o.invokes {
		assert.Equal(t, want, got)
	}
}

type wantResource struct {
	typ    string
	inputs map[string]any
}

func assertResources(t *testing.T, resources []apitype.ResourceV3, want map[string]wantResource) {
	got := map[string]apitype.ResourceV3{}
	for _, r := range resources {
		got[r.URN.Name()] = r
	}
	for name, w := range want {
		r, ok := got[name]
		require.Truef(t, ok, "resource %q is missing", name)
		assert.Equal(t, w.typ, string(r.Type), name)
		assert.Equal(t, w.inputs, r.Inputs, name)
	}
}

// TestL2NonIdentifierTypeNames checks that a type whose token is not an HCL
// identifier can be written with its generated name or with the dots of its
// token, for both resources and data sources, and that references to it
// work. The round trip checks that codegen emits the generated names.
func TestL2NonIdentifierTypeNames(t *testing.T) {
	t.Parallel()

	odd := &oddProvider{}
	putest.RunCase(t, "l2_non_identifier_type_names", putest.Case{
		Providers:       []putest.Provider{{Name: "odd", Native: odd.build}},
		ExpectedOutputs: map[string]string{"value": "c"},
		AssertState: func(t *testing.T, resources []apitype.ResourceV3) {
			odd.assertInvokes(t, map[string]any{"value": "c"})
			assertResources(t, resources, map[string]wantResource{
				"a": {"odd:dot.ted/v1:Thing", map[string]any{"value": "a"}},
				"b": {"odd:dot.ted/v1:Thing", map[string]any{"value": "a"}},
				"c": {"odd:mod:Dot.Member", map[string]any{"value": "c"}},
				"d": {"odd:mod:Dot.Member", map[string]any{"value": "c"}},
			})
		},
	})
}

// TestL2NonIdentifierPropertyNames checks that each property whose name is
// not an HCL identifier can be set and referenced by its generated name, on
// resources and data sources.
func TestL2NonIdentifierPropertyNames(t *testing.T) {
	t.Parallel()

	inputs := map[string]any{}
	outputs := map[string]string{}
	for hclName, name := range oddPropertyNames {
		inputs[name] = "a-" + hclName
		outputs[hclName] = inputs[name].(string)
	}

	odd := &oddProvider{}
	putest.RunCase(t, "l2_non_identifier_property_names", putest.Case{
		Providers:       []putest.Provider{{Name: "odd", Native: odd.build}},
		ExpectedOutputs: outputs,
		AssertState: func(t *testing.T, resources []apitype.ResourceV3) {
			odd.assertInvokes(t, inputs)
			assertResources(t, resources, map[string]wantResource{
				"a": {"odd:dot.ted/v1:Thing", inputs},
				"b": {"odd:dot.ted/v1:Thing", inputs},
			})
		},
		SkipRoundTrip: "TODO[https://github.com/pulumi/pulumi/issues/23232]: " +
			"PCL cannot write a property whose name is not an identifier",
	})
}

// TestL2NonIdentifierTypeWarning checks that a type label that is not an HCL
// identifier warns once and names the valid spelling.
func TestL2NonIdentifierTypeWarning(t *testing.T) {
	t.Parallel()

	odd := &oddProvider{}
	putest.RunCase(t, "l2_non_identifier_type_warning", putest.Case{
		Providers:       []putest.Provider{{Name: "odd", Native: odd.build}},
		ExpectedOutputs: map[string]string{"value": "ok"},
		AssertOutput: func(t *testing.T, output string) {
			assert.Equal(t, 1, strings.Count(diagnosticsSection(t, output),
				`/main.tf:9,10-32: Type name is not a valid identifier; `+
					`The resource type "odd_dot.ted_v1_thing" is not a valid identifier, `+
					`so references to this resource cannot work. Use "odd_dot_ted_v1_thing" instead.`+"\n"))
		},
		SkipRoundTrip: "The round trip writes the valid spelling, so the program does not warn.",
	})
}
