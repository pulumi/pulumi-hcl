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

	"github.com/pulumi/pulumi-hcl/pkg/hcl/ast"
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
		Name:      "pinned",
		Meta:      &schema.MetadataSpec{ModuleFormat: `(.*)(?:/[^/]*)`},
		Resources: map[string]schema.ResourceSpec{"pinned:index/uuid:Uuid": {}},
		Functions: map[string]schema.FunctionSpec{"pinned:index/getUuid:getUuid": {}},
	})
	info := &tfbridge.ProviderInfo{
		Name: "pinned",
		Resources: map[string]*tfbridge.ResourceInfo{
			"pinned_uuid4": {Tok: "pinned:index/pinnedUuid4:PinnedUuid4"},
		},
		DataSources: map[string]*tfbridge.DataSourceInfo{
			"pinned_uuid4": {Tok: "pinned:index/getPinnedUuid4:getPinnedUuid4"},
		},
	}
	r := NewResolver(loader, staticInfoSource{info}, nil, nil)
	res := &ast.Resource{Type: "pinned_uuid4"}
	ds := &ast.DataSource{Type: "pinned_uuid4"}

	boundRes, err := r.BindResource(t.Context(), nil, res)
	assert.Nil(t, boundRes)
	assert.ErrorIs(t, err, ErrNotFound)

	boundDS, err := r.BindDataSource(t.Context(), nil, ds)
	assert.Nil(t, boundDS)
	assert.ErrorIs(t, err, ErrNotFound)

	// A mapping whose package the loader cannot serve at all is not-found too.
	info.Name = "missing"
	boundRes, err = r.BindResource(t.Context(), nil, res)
	assert.Nil(t, boundRes)
	assert.ErrorIs(t, err, ErrNotFound)

	boundDS, err = r.BindDataSource(t.Context(), nil, ds)
	assert.Nil(t, boundDS)
	assert.ErrorIs(t, err, ErrNotFound)
}

// A block binds against the required_providers of its own module.
func TestResolver_BindResourceUsesItsModule(t *testing.T) {
	t.Parallel()

	thing := schema.ResourceSpec{ObjectTypeSpec: schema.ObjectTypeSpec{Type: "object"}}
	loader := schemaloader.New(t,
		schema.PackageSpec{
			Name:      "snake",
			Resources: map[string]schema.ResourceSpec{"snake:index:namesThing": thing},
		},
		schema.PackageSpec{
			Name:      "snake_names",
			Resources: map[string]schema.ResourceSpec{"snake_names:index:thing": thing},
		},
	)
	r := NewResolver(loader, nil, nil, nil)
	module := &ast.Terraform{RequiredProviders: map[string]*ast.RequiredProvider{
		"snake_names": {Name: "snake_names"},
	}}

	resourceSchema := func(pkgName, token string) *schema.Resource {
		pkg, err := loader.LoadPackageReferenceV2(t.Context(), &schema.PackageDescriptor{Name: pkgName})
		require.NoError(t, err)
		res, ok, err := pkg.Resources().Get(token)
		require.NoError(t, err)
		require.True(t, ok)
		return res
	}

	bound, err := r.BindResource(t.Context(), module, &ast.Resource{Type: "snake_names_thing"})
	require.NoError(t, err)
	assert.Equal(t, &BoundResource{Schema: resourceSchema("snake_names", "snake_names:index:thing")}, bound)

	bound, err = r.BindResource(t.Context(), nil, &ast.Resource{Type: "snake_names_thing"})
	require.NoError(t, err)
	assert.Equal(t, &BoundResource{Schema: resourceSchema("snake", "snake:index:namesThing")}, bound)
}
