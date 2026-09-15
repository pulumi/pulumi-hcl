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

package server

import (
	"os"
	"path/filepath"
	"testing"

	p "github.com/pulumi/pulumi-go-provider"
	"github.com/pulumi/pulumi/pkg/v3/codegen/schema"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource"
	"github.com/pulumi/pulumi/sdk/v3/go/property"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pulumi/pulumi-hcl/pkg/hcl/modules"
	"github.com/pulumi/pulumi-hcl/tests/testutil/schemaloader"
)

// A source MLC component's own required_providers pin reaches the resources it
// registers, as it does for a root program.
func TestLocalProviderHonorsRequiredProvidersPin(t *testing.T) {
	t.Parallel()

	dir := filepath.Join(t.TempDir(), "pinned")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "main.tf"), []byte(`
terraform {
  required_providers {
    random = {
      source  = "pulumi/random"
      version = "4.18.5"
    }
  }
}

resource "random_uuid" "id" {}
`), 0o600))

	ctx := t.Context()
	loader := modules.NewLoader(modules.LiveResolver(ctx))
	comps, resolvedVersion, err := localComponents(ctx, loader, dir)
	require.NoError(t, err)
	pkgName, rootToken, version, err := packageIdentity(comps, filepath.Base(dir), false, resolvedVersion)
	require.NoError(t, err)

	m := &moduleProvider{
		version:      version.String(),
		moduleLoader: loader,
		schemaLoader: schemaloader.New(t, schema.PackageSpec{
			Name: "random",
			Meta: &schema.MetadataSpec{ModuleFormat: `(.*)(?:/[^/]*)`},
			Resources: map[string]schema.ResourceSpec{
				"random:index/uuid:Uuid": {},
			},
		}),
	}
	components, spec, err := buildComponents(ctx, comps, pkgName, rootToken, version,
		m.componentBinderFactory(loader))
	require.NoError(t, err)
	m.param = &parameterizedModule{spec: spec, components: components, loader: loader, name: pkgName}

	mon := newHookInvokingMonitor()
	_, err = m.construct(ctx, p.ConstructRequest{
		Urn:             resource.URN("urn:pulumi:test::proj::" + string(rootToken) + "::mod"),
		MonitorEndpoint: serveResourceMonitor(t, mon),
		Inputs:          property.NewMap(nil),
	})
	require.NoError(t, err)

	reg := mon.registeredType("random:index/uuid:Uuid")
	require.NotNil(t, reg)
	assert.Equal(t, "4.18.5", reg.Version)
}
