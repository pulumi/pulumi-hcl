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

package packages

import (
	"context"
	"maps"
	"slices"
	"testing"

	"github.com/pulumi/pulumi-hcl/tests/testutil/schemaloader"
	"github.com/pulumi/pulumi-terraform-bridge/v3/pkg/tfbridge"
	"github.com/pulumi/pulumi/pkg/v3/codegen/schema"
	"github.com/pulumi/pulumi/sdk/v3/go/common/workspace"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// arnInputSpec is the input bag shared by the multi-argument function specs
// below: a single required string property "arn", matching the property
// MultiArgumentInputs promotes to a positional parameter.
func arnInputSpec() *schema.ObjectTypeSpec {
	return &schema.ObjectTypeSpec{
		Type:     "object",
		Required: []string{"arn"},
		Properties: map[string]schema.PropertySpec{
			"arn": {TypeSpec: schema.TypeSpec{Type: "string"}},
		},
	}
}

func stringReturnSpec() *schema.ReturnTypeSpec {
	return &schema.ReturnTypeSpec{TypeSpec: &schema.TypeSpec{Type: "string"}}
}

// TestResolverProviderFunctions_ConventionPath covers the no-bridge path
// (nil ProviderInfoSource): a multi-argument function is exposed under its
// TF snake_case name, while a data-source-style function (no
// MultiArgumentInputs) is not.
func TestResolverProviderFunctions_ConventionPath(t *testing.T) {
	t.Parallel()

	loader := schemaloader.New(t, schema.PackageSpec{
		Name: "simple",
		Functions: map[string]schema.FunctionSpec{
			"simple:index:parseArn": {
				MultiArgumentInputs: []string{"arn"},
				Inputs:              arnInputSpec(),
				ReturnType:          stringReturnSpec(),
			},
			"simple:index:getThing": {},
		},
	})

	r := NewResolver(loader, nil, nil, nil)
	fns, err := r.ProviderFunctions(t.Context(), "simple")
	require.NoError(t, err)

	assert.Equal(t, []string{"parse_arn"}, slices.Sorted(maps.Keys(fns)))
	require.NotNil(t, fns["parse_arn"])
	assert.Equal(t, "simple:index:parseArn", fns["parse_arn"].Token)
}

// TestResolverProviderFunctions_DedupAcrossModulesIsDeterministic covers two
// multi-argument functions in different modules that derive the same TF
// name: exactly one survives, and the index-module token wins. The non-index
// module name ("aaa") sorts before "index", so this passes only when the
// index preference actually fires, not via the lexicographic tie-break.
func TestResolverProviderFunctions_DedupAcrossModulesIsDeterministic(t *testing.T) {
	t.Parallel()

	loader := schemaloader.New(t, schema.PackageSpec{
		Name: "simple",
		Functions: map[string]schema.FunctionSpec{
			"simple:index:dup": {
				MultiArgumentInputs: []string{"arn"},
				Inputs:              arnInputSpec(),
				ReturnType:          stringReturnSpec(),
			},
			"simple:aaa:dup": {
				MultiArgumentInputs: []string{"arn"},
				Inputs:              arnInputSpec(),
				ReturnType:          stringReturnSpec(),
			},
		},
	})

	r := NewResolver(loader, nil, nil, nil)
	fns, err := r.ProviderFunctions(t.Context(), "simple")
	require.NoError(t, err)

	assert.Equal(t, []string{"dup"}, slices.Sorted(maps.Keys(fns)))
	assert.Equal(t, "simple:index:dup", fns["dup"].Token)
}

// TestResolverProviderFunctions_UnknownProvider covers a provider local name
// with no matching package: the loader's not-found error propagates rather
// than resolving to an empty map.
func TestResolverProviderFunctions_UnknownProvider(t *testing.T) {
	t.Parallel()

	loader := schemaloader.New(t, schema.PackageSpec{Name: "simple"})
	r := NewResolver(loader, nil, nil, nil)

	_, err := r.ProviderFunctions(t.Context(), "unknown")
	require.Error(t, err)
}

type staticInfoSource struct{ info *tfbridge.ProviderInfo }

func (s staticInfoSource) GetProviderInfo(
	context.Context, string, *workspace.PackageDescriptor,
) (*tfbridge.ProviderInfo, error) {
	return s.info, nil
}

// The bridge mapping comes from whichever plugin the engine's mapper picks,
// while the schema is pinned by required_providers. A TF type the mapping
// knows but the pinned schema lacks must resolve to not-found, never to a nil
// schema without an error.
func TestResolver_MappedTokenMissingFromSchema(t *testing.T) {
	t.Parallel()

	loader := schemaloader.New(t, schema.PackageSpec{
		Name:      "random",
		Meta:      &schema.MetadataSpec{ModuleFormat: `(.*)(?:/[^/]*)`},
		Resources: map[string]schema.ResourceSpec{"random:index/uuid:Uuid": {}},
		Functions: map[string]schema.FunctionSpec{"random:index/getUuid:getUuid": {}},
	})
	info := &tfbridge.ProviderInfo{
		Name: "random",
		Resources: map[string]*tfbridge.ResourceInfo{
			"random_uuid4": {Tok: "random:index/randomUuid4:RandomUuid4"},
		},
		DataSources: map[string]*tfbridge.DataSourceInfo{
			"random_uuid4": {Tok: "random:index/getRandomUuid4:getRandomUuid4"},
		},
	}
	r := NewResolver(loader, staticInfoSource{info}, nil, []string{"random"})

	res, err := r.ResolveResource(t.Context(), "random_uuid4")
	assert.Nil(t, res)
	assert.ErrorIs(t, err, ErrNotFound)

	fn, err := r.ResolveFunction(t.Context(), "random_uuid4")
	assert.Nil(t, fn)
	assert.ErrorIs(t, err, ErrNotFound)

	// A mapping whose package the loader cannot serve at all is not-found too.
	info.Name = "missing"
	res, err = r.ResolveResource(t.Context(), "random_uuid4")
	assert.Nil(t, res)
	assert.ErrorIs(t, err, ErrNotFound)

	fn, err = r.ResolveFunction(t.Context(), "random_uuid4")
	assert.Nil(t, fn)
	assert.ErrorIs(t, err, ErrNotFound)
}
