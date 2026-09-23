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
	"testing"

	"github.com/blang/semver"
	"github.com/pulumi/pulumi/pkg/v3/codegen/schema"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The Mock serves a descriptor as the engine's plugin loader serves an
// installed plugin set: an exact version only, the newest when none is
// named, and a parameterized descriptor by the parameterization's name.
func TestMock_ServesByVersion(t *testing.T) {
	t.Parallel()

	loader := New(t,
		schema.PackageSpec{Name: "random", Version: "1.0.0"},
		schema.PackageSpec{Name: "random", Version: "2.0.0"},
	)
	v1, v2, v3 := semver.MustParse("1.0.0"), semver.MustParse("2.0.0"), semver.MustParse("3.0.0")

	tests := []struct {
		name string
		desc schema.PackageDescriptor
		want string
	}{
		{"exact version", schema.PackageDescriptor{Name: "random", Version: &v1}, "1.0.0"},
		{"no version is the newest", schema.PackageDescriptor{Name: "random"}, "2.0.0"},
		{"version not installed", schema.PackageDescriptor{Name: "random", Version: &v3}, ""},
		{"package not installed", schema.PackageDescriptor{Name: "tls"}, ""},
		{
			"parameterized by the parameterization's name and version",
			schema.PackageDescriptor{
				Name:             "terraform-provider",
				Parameterization: &schema.ParameterizationDescriptor{Name: "random", Version: v2},
			},
			"2.0.0",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ref, err := loader.LoadPackageReferenceV2(t.Context(), &tt.desc)
			if tt.want == "" {
				assert.Nil(t, ref)
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, "random", ref.Name())
			assert.Equal(t, tt.want, ref.Version().String())
		})
	}
}
