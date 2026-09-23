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

package schemaloader

import (
	"context"
	"maps"
	"slices"

	"github.com/blang/semver"
	"github.com/pulumi/pulumi/pkg/v3/codegen/schema"
	"github.com/pulumi/pulumi/sdk/v3/go/common/apitype"
	"github.com/pulumi/pulumi/sdk/v3/go/common/workspace"
	"github.com/stretchr/testify/require"
)

var _ schema.ReferenceLoader = Mock{}

// Mock is a schema.ReferenceLoader over a fixed set of packages, the plugins
// installed in a workspace. Keys are labels only; a descriptor is served by
// package name and version the way the engine's plugin loader serves an
// installed plugin set: a descriptor that names a version is served only by
// that exact version, and one that names none by the newest package of that
// name. A parameterized descriptor is also served by a package registered
// under the parameterization's name and version. Entries must not be nil.
type Mock map[string]*schema.Package

func (m Mock) LoadPackage(pkg string, version *semver.Version) (*schema.Package, error) {
	return m.LoadPackageV2(context.Background(), &schema.PackageDescriptor{
		Name:    pkg,
		Version: version,
	})
}

func (m Mock) LoadPackageV2(ctx context.Context, descriptor *schema.PackageDescriptor) (*schema.Package, error) {
	ref, err := m.LoadPackageReferenceV2(ctx, descriptor)
	if err != nil {
		return nil, err
	}
	return ref.Definition()
}

func (m Mock) LoadPackageReference(pkg string, version *semver.Version) (schema.PackageReference, error) {
	return m.LoadPackageReferenceV2(context.Background(), &schema.PackageDescriptor{
		Name:    pkg,
		Version: version,
	})
}

func (m Mock) LoadPackageReferenceV2(ctx context.Context, descriptor *schema.PackageDescriptor) (schema.PackageReference, error) {
	if p, ok := m.find(descriptor.Name, descriptor.Version); ok {
		return p.Reference(), nil
	}
	if param := descriptor.Parameterization; param != nil {
		if p, ok := m.find(param.Name, &param.Version); ok {
			return p.Reference(), nil
		}
	}
	return nil, workspace.NewMissingError(workspace.PluginDescriptor{
		Kind:              apitype.ResourcePlugin,
		Name:              descriptor.Name,
		Version:           descriptor.Version,
		PluginDownloadURL: descriptor.DownloadURL,
	}, false)
}

// find returns the package of name at version, or the newest package of name
// when version is nil. Keys break ties so the choice is deterministic.
func (m Mock) find(name string, version *semver.Version) (*schema.Package, bool) {
	var newest *schema.Package
	for _, key := range slices.Sorted(maps.Keys(m)) {
		p := m[key]
		if p.Name != name {
			continue
		}
		if version != nil {
			if p.Version != nil && p.Version.EQ(*version) {
				return p, true
			}
			continue
		}
		if newest == nil || newer(p, newest) {
			newest = p
		}
	}
	return newest, newest != nil
}

// newer reports whether a is a newer version than b. A package without a
// version is older than any versioned one.
func newer(a, b *schema.Package) bool {
	switch {
	case a.Version == nil:
		return false
	case b.Version == nil:
		return true
	default:
		return a.Version.GT(*b.Version)
	}
}

// New creates a Mock from the given PackageSpecs.
func New(t interface {
	require.TestingT
	Context() context.Context
}, schemas ...schema.PackageSpec,
) schema.ReferenceLoader {
	loader := Mock{}
	for _, spec := range schemas {
		pkg, diag, err := schema.BindSpec(spec, loader, schema.ValidationOptions{})
		require.NoError(t, err)
		require.Len(t, diag, 0)
		d, err := pkg.Descriptor(t.Context())
		require.NoError(t, err)

		params := func() *schema.ParameterizationDescriptor {
			if d.Parameterization == nil {
				return nil
			}
			return &schema.ParameterizationDescriptor{
				Name:    d.Parameterization.Name,
				Version: d.Parameterization.Version,
				Value:   d.Parameterization.Value,
			}
		}
		loader[(&schema.PackageDescriptor{
			Name:             d.Name,
			Version:          d.Version,
			DownloadURL:      d.PluginDownloadURL,
			Parameterization: params(),
		}).String()] = pkg
	}
	return loader
}
