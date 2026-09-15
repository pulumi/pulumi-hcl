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

package run_test

import (
	"context"
	"sync"
	"testing"

	"github.com/blang/semver"
	"github.com/pulumi/pulumi-hcl/pkg/hcl/packages"
	"github.com/pulumi/pulumi-hcl/pkg/hcl/parser"
	"github.com/pulumi/pulumi-hcl/pkg/hcl/run"
	"github.com/pulumi/pulumi-hcl/tests/testutil"
	"github.com/pulumi/pulumi-hcl/tests/testutil/schemaloader"
	"github.com/pulumi/pulumi/pkg/v3/codegen/schema"
	"github.com/pulumi/pulumi/sdk/v3/go/common/apitype"
	"github.com/pulumi/pulumi/sdk/v3/go/common/workspace"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// descriptorRecordingLoader records every descriptor the engine asks the
// wrapped loader for. The engine resolves blocks in parallel.
type descriptorRecordingLoader struct {
	schema.ReferenceLoader
	mu          sync.Mutex
	descriptors []schema.PackageDescriptor
}

func (l *descriptorRecordingLoader) LoadPackageReferenceV2(
	ctx context.Context, d *schema.PackageDescriptor,
) (schema.PackageReference, error) {
	l.mu.Lock()
	l.descriptors = append(l.descriptors, *d)
	l.mu.Unlock()
	return l.ReferenceLoader.LoadPackageReferenceV2(ctx, d)
}

// A pinned Pulumi package in EngineOptions.Packages must pin both the schema
// the engine type checks against and the package every block registers or
// invokes against, while a block-level version option still overrides the pin.
// Each distinct identity is registered with the engine exactly once.
func TestEngine_PinnedPackageDrivesSchemaAndRegistration(t *testing.T) {
	t.Parallel()

	src := []byte(`
terraform {
  required_providers {
    random = {
      source  = "pulumi/random"
      version = "4.18.5"
    }
  }
}

resource "random_uuid" "id" {}

resource "random_uuid" "newer" {
  pulumi {
    version = "4.19.0"
  }
}

data "random_uuid" "pinned" {}

data "random_uuid" "newer" {
  pulumi {
    version = "4.19.0"
  }
}
`)
	p := parser.NewParser()
	config, diags := p.ParseSource("test.hcl", src)
	require.False(t, diags.HasErrors(), diags.Error())

	pinned := semver.MustParse("4.18.5")
	desc := workspace.PackageDescriptor{PluginDescriptor: workspace.PluginDescriptor{
		Name: "random", Kind: apitype.ResourcePlugin, Version: &pinned,
	}}
	inner := &descriptorRecordingLoader{ReferenceLoader: schemaloader.New(t, schema.PackageSpec{
		Name: "random",
		Meta: &schema.MetadataSpec{ModuleFormat: `(.*)(?:/[^/]*)`},
		Resources: map[string]schema.ResourceSpec{
			"random:index/uuid:Uuid": {},
		},
		Functions: map[string]schema.FunctionSpec{
			"random:index/getUuid:getUuid": {},
		},
	})}
	pkgs := map[string]workspace.PackageDescriptor{"random": desc}

	mock := &testutil.MockResourceMonitor{}
	engine := newTestEngine(t, config, &run.EngineOptions{
		ModuleLoader:    testModuleLoader(t),
		ProjectName:     "test-project",
		StackName:       "dev",
		ResourceMonitor: mock,
		WorkDir:         t.TempDir(),
		RootDir:         t.TempDir(),
		SchemaLoader:    packages.NewParameterizationAwareLoader(inner, pkgs),
		Packages:        pkgs,
	})
	require.NoError(t, engine.Run(t.Context()))

	// Type checking follows the block's version option where one is set, and
	// the required_providers pin otherwise.
	loaded := map[string]bool{}
	for _, d := range inner.descriptors {
		assert.Equal(t, "random", d.Name)
		require.NotNil(t, d.Version)
		loaded[d.Version.String()] = true
	}
	assert.Equal(t, map[string]bool{"4.18.5": true, "4.19.0": true}, loaded)
	registered := map[string]int{}
	for _, req := range mock.RegisteredPackages {
		registered[req.Name+"@"+req.Version]++
	}
	assert.Equal(t, map[string]int{"random@4.18.5": 1, "random@4.19.0": 1}, registered)

	require.Len(t, mock.RegisteredResources, 3)
	byName := map[string]run.RegisterResourceRequest{}
	for _, r := range mock.RegisteredResources[1:] {
		byName[r.Name] = r
	}
	assert.Equal(t, "random:index/uuid:Uuid", byName["id"].Type)
	assert.Equal(t, "random@4.18.5", byName["id"].Package.String())
	assert.Equal(t, "random@4.19.0", byName["newer"].Package.String())

	invoked := map[string]int{}
	for _, inv := range mock.InvokedFunctions {
		assert.Equal(t, "random:index/getUuid:getUuid", inv.Token)
		invoked[inv.Package.String()]++
	}
	assert.Equal(t, map[string]int{"random@4.18.5": 1, "random@4.19.0": 1}, invoked)
}

// A method call runs against the provider and package the resource registered
// with, so a resource-level version option reaches its methods too.
func TestEngine_CallUsesTheResourcePackage(t *testing.T) {
	t.Parallel()

	src := []byte(`
terraform {
  required_providers {
    random = {
      source  = "pulumi/random"
      version = "4.18.5"
    }
  }
}

resource "random_thing" "newer" {
  pulumi {
    version = "4.19.0"
  }
}

call "newer" "ping" {}
`)
	p := parser.NewParser()
	config, diags := p.ParseSource("test.hcl", src)
	require.False(t, diags.HasErrors(), diags.Error())

	pinned := semver.MustParse("4.18.5")
	pkgs := map[string]workspace.PackageDescriptor{"random": {PluginDescriptor: workspace.PluginDescriptor{
		Name: "random", Kind: apitype.ResourcePlugin, Version: &pinned,
	}}}
	loader := schemaloader.New(t, schema.PackageSpec{
		Name: "random",
		Meta: &schema.MetadataSpec{ModuleFormat: `(.*)(?:/[^/]*)`},
		Resources: map[string]schema.ResourceSpec{
			"random:index/thing:Thing": {
				Methods: map[string]string{"ping": "random:index/thing:Thing/ping"},
			},
		},
		Functions: map[string]schema.FunctionSpec{
			"random:index/thing:Thing/ping": {
				Inputs: &schema.ObjectTypeSpec{
					Type: "object",
					Properties: map[string]schema.PropertySpec{
						"__self__": {TypeSpec: schema.TypeSpec{Ref: "#/resources/random:index/thing:Thing"}},
					},
					Required: []string{"__self__"},
				},
				ReturnType: &schema.ReturnTypeSpec{ObjectTypeSpec: &schema.ObjectTypeSpec{
					Type:       "object",
					Properties: map[string]schema.PropertySpec{"result": {TypeSpec: schema.TypeSpec{Type: "string"}}},
				}},
			},
		},
	})

	mock := &testutil.MockResourceMonitor{}
	engine := newTestEngine(t, config, &run.EngineOptions{
		ModuleLoader:    testModuleLoader(t),
		ProjectName:     "test-project",
		StackName:       "dev",
		ResourceMonitor: mock,
		WorkDir:         t.TempDir(),
		RootDir:         t.TempDir(),
		SchemaLoader:    packages.NewParameterizationAwareLoader(loader, pkgs),
		Packages:        pkgs,
	})
	require.NoError(t, engine.Run(t.Context()))

	require.Len(t, mock.Calls, 1)
	assert.Equal(t, "random:index/thing:Thing/ping", mock.Calls[0].Token)
	assert.Equal(t, "random@4.19.0", mock.Calls[0].Package.String())
}
