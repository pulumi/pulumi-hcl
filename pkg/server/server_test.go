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
	"encoding/base64"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/blang/semver"
	"github.com/pulumi/pulumi-hcl/pkg/hcl/parser"
	"github.com/pulumi/pulumi-hcl/pkg/hcl/run"
	"github.com/pulumi/pulumi/sdk/v3/go/common/apitype"
	"github.com/pulumi/pulumi/sdk/v3/go/common/workspace"
	pulumirpc "github.com/pulumi/pulumi/sdk/v3/proto/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// bridgedSDK is the shape `pulumi install` writes for a non-Pulumi provider:
// a terraform-provider descriptor parameterized with the resolved package.
func bridgedSDK(pkg string) workspace.PackageDescriptor {
	return workspace.PackageDescriptor{
		PluginDescriptor: workspace.PluginDescriptor{Name: "terraform-provider"},
		Parameterization: &workspace.Parameterization{Name: pkg},
	}
}

// Test that we correctly inline additional keys.
func TestGenerateProjectInlinesAdditionalKeys(t *testing.T) {
	t.Parallel()

	test := func(t *testing.T, projectJSON, expectedYAML string) {
		sourceDir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(sourceDir, "main.pp"),
			[]byte("output hello {\n    value = \"world\"\n}\n"), 0o600))

		targetDir := t.TempDir()

		host := &LanguageHost{}
		_, err := host.GenerateProject(t.Context(), &pulumirpc.GenerateProjectRequest{
			SourceDirectory: sourceDir,
			TargetDirectory: targetDir,
			Project:         projectJSON,
			LoaderTarget:    "127.0.0.1:1",
		})
		require.NoError(t, err)

		data, err := os.ReadFile(filepath.Join(targetDir, "Pulumi.yaml"))
		require.NoError(t, err)

		require.Equal(t, expectedYAML, string(data))
	}

	t.Run("no additional keys", func(t *testing.T) {
		t.Parallel()

		json := `{
        "name": "test",
        "description": "test project"
    }`

		yaml := `name: test
runtime: hcl
description: test project
`

		test(t, json, yaml)
	})

	t.Run("with additional keys", func(t *testing.T) {
		t.Parallel()

		json := `{
        "name": "test",
        "description": "test project",
	"AdditionalKeys": { "fizz": "buzz" }
    }`

		yaml := `name: test
runtime: hcl
description: test project
fizz: buzz
`

		test(t, json, yaml)
	})
}

// TestGeneratePackageAndRunUseSameSdksDir locks in the contract that the
// language writes parameterization info to <projectDir>/sdks/<name>/hcl.sdk.json
// (where `pulumi package add` puts it) and that GetRequiredPackages reads it
// from the same place. Conformance tests previously masked a mismatch where
// GeneratePackage wrote to sdks/ but the runtime read from .hcl/sdks/.
func TestGeneratePackageAndRunUseSameSdksDir(t *testing.T) {
	t.Parallel()

	projectDir := t.TempDir()

	// Mirror `pulumi package add`: caller creates sdks/<name>/ then calls
	// GeneratePackage with that directory.
	const alias = "myparam"
	sdkDir := filepath.Join(projectDir, "sdks", alias)
	require.NoError(t, os.MkdirAll(sdkDir, 0o755))

	schema := `{
		"name": "myparam",
		"version": "1.2.3",
		"parameterization": {
			"baseProvider": {
				"name": "baseplugin",
				"version": "1.0.0"
			},
			"parameter": "aGVsbG8="
		}
	}`

	host := &LanguageHost{}
	_, err := host.GeneratePackage(t.Context(), &pulumirpc.GeneratePackageRequest{
		Directory: sdkDir,
		Schema:    schema,
	})
	require.NoError(t, err)

	// Lock in the path: GeneratePackage must write here, not under .hcl/sdks.
	_, err = os.Stat(filepath.Join(projectDir, "sdks", alias, "hcl.sdk.json"))
	require.NoError(t, err, "GeneratePackage must write hcl.sdk.json to sdks/<name>/")
	_, err = os.Stat(filepath.Join(projectDir, ".hcl", "sdks", alias, "hcl.sdk.json"))
	require.True(t, os.IsNotExist(err), "hcl.sdk.json must not be written under .hcl/sdks/")

	// Write an HCL program that references the alias.
	program := `terraform {
  required_providers {
    myparam = {
      source  = "myparam"
    }
  }
}
`
	require.NoError(t, os.WriteFile(filepath.Join(projectDir, "main.tf"), []byte(program), 0o600))

	resp, err := host.GetRequiredPackages(t.Context(), &pulumirpc.GetRequiredPackagesRequest{
		Info: &pulumirpc.ProgramInfo{
			ProgramDirectory: projectDir,
			RootDirectory:    projectDir,
			EntryPoint:       ".",
		},
	})
	require.NoError(t, err)

	require.Len(t, resp.Packages, 1)
	assert.Equal(t, &pulumirpc.PackageDependency{
		Name:    "baseplugin",
		Version: "1.0.0",
		Kind:    "resource",
		Parameterization: &pulumirpc.PackageParameterization{
			Name:    "myparam",
			Version: "1.2.3",
			Value:   []byte("hello"),
		},
	}, resp.Packages[0])
}

// A parameterized package whose required_providers source is `pulumi/<name>`
// must still be reported via its local SDK descriptor (base provider + para-
// meterization), not as a plain pulumi dependency. The descriptor is the
// authoritative source and must win over the pulumi-source classification.
func TestGetRequiredPackages_ParameterizedPulumiSource(t *testing.T) {
	t.Parallel()

	projectDir := t.TempDir()

	const alias = "subpackage"
	sdkDir := filepath.Join(projectDir, "sdks", alias)
	require.NoError(t, os.MkdirAll(sdkDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(sdkDir, "hcl.sdk.json"), []byte(`{
		"Name": "parameterized",
		"Kind": "resource",
		"Version": "1.2.3",
		"Parameterization": {
			"Name": "subpackage",
			"Version": "2.0.0",
			"Value": "SGVsbG9Xb3JsZA=="
		}
	}`), 0o600))

	program := `terraform {
  required_providers {
    subpackage = {
      source  = "pulumi/subpackage"
    }
  }
}

resource "subpackage_hello_world" "example" {}
`
	require.NoError(t, os.WriteFile(filepath.Join(projectDir, "main.tf"), []byte(program), 0o600))

	host := &LanguageHost{}
	resp, err := host.GetRequiredPackages(t.Context(), &pulumirpc.GetRequiredPackagesRequest{
		Info: &pulumirpc.ProgramInfo{
			ProgramDirectory: projectDir,
			RootDirectory:    projectDir,
			EntryPoint:       ".",
		},
	})
	require.NoError(t, err)

	require.Len(t, resp.Packages, 1)
	assert.Equal(t, &pulumirpc.PackageDependency{
		Name:    "parameterized",
		Version: "1.2.3",
		Kind:    "resource",
		Parameterization: &pulumirpc.PackageParameterization{
			Name:    "subpackage",
			Version: "2.0.0",
			Value:   []byte("HelloWorld"),
		},
	}, resp.Packages[0])
}

// TestGetRequiredPackages_GitComponent reproduces
// https://github.com/pulumi/pulumi-hcl/issues/566: a git-sourced component
// lives in sdks/<namespace>-<name> and is found only by its download URL.
func TestGetRequiredPackages_GitComponent(t *testing.T) {
	t.Parallel()

	projectDir := t.TempDir()
	sdkDir := filepath.Join(projectDir, "sdks", "pulumi-tls-self-signed-cert")
	require.NoError(t, os.MkdirAll(sdkDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(sdkDir, "hcl.sdk.json"), []byte(`{
		"Name": "tls-self-signed-cert",
		"Kind": "resource",
		"Version": "0.0.0-x52a8a71555d964542b308da197755c64dbe63352",
		"PluginDownloadURL": "git://github.com/pulumi/component-test-providers/test-provider"
	}`), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(projectDir, "main.tf"), []byte(`terraform {
  required_providers {
    tls-self-signed-cert = {
      source = "pulumi/tls-self-signed-cert"
    }
  }
}

resource "tls-self-signed-cert_self_signed_certificate" "cert" {
  dns_name = "example.com"
}
`), 0o600))

	resp := getRequiredPackages(t, projectDir)
	assert.Empty(t, resp.Specs)
	assert.Equal(t, []*pulumirpc.PackageDependency{{
		Name:    "tls-self-signed-cert",
		Version: "0.0.0-x52a8a71555d964542b308da197755c64dbe63352",
		Kind:    "resource",
		Server:  "git://github.com/pulumi/component-test-providers/test-provider",
	}}, resp.Packages)
}

func TestSDKDescriptors_KeyedByPackageName(t *testing.T) {
	t.Parallel()

	plain := workspace.PackageDescriptor{
		PluginDescriptor: workspace.PluginDescriptor{Name: "tls-self-signed-cert"},
	}
	parameterized := workspace.PackageDescriptor{
		PluginDescriptor: workspace.PluginDescriptor{Name: "terraform-provider"},
		Parameterization: &workspace.Parameterization{Name: "random"},
	}
	extension := workspace.PackageDescriptor{
		PluginDescriptor:          workspace.PluginDescriptor{Name: "extbase"},
		ExtensionParameterization: &workspace.Parameterization{Name: "myext"},
	}
	assert.Equal(t, map[string]workspace.PackageDescriptor{
		"tls-self-signed-cert": plain,
		"random":               parameterized,
		"myext":                extension,
	}, sdkDescriptors(map[string]sdkInfo{
		"pulumi-tls-self-signed-cert": {desc: plain},
		"random":                      {desc: parameterized},
		"myext":                       {desc: extension},
	}))
}

// TestGetRequiredPackages_TransitiveModuleSource reproduces
// https://github.com/pulumi/pulumi-hcl/issues/184: a provider declared in
// a child module's required_providers with a non-hashicorp source must be
// resolved from that source, not defaulted to "hashicorp/<name>".
//
// GetRequiredPackages only consults the root config's RequiredProviders map, so
// a provider declared solely in a child module has a nil entry there and
// tfProviderSource falls back to "hashicorp/rollbar" — dropping both the
// declared source ("rollbar/rollbar") and the version constraint.
func TestGetRequiredPackages_TransitiveModuleSource(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "main.tf"), []byte(`
module "child" {
  source = "./child"
}
`), 0o600))

	childDir := filepath.Join(dir, "child")
	require.NoError(t, os.MkdirAll(childDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(childDir, "main.tf"), []byte(`
terraform {
  required_providers {
    rollbar = {
      source  = "rollbar/rollbar"
      version = ">= 1.0"
    }
  }
}
`), 0o600))

	host := &LanguageHost{}
	resp, err := host.GetRequiredPackages(t.Context(), &pulumirpc.GetRequiredPackagesRequest{
		Info: &pulumirpc.ProgramInfo{
			ProgramDirectory: dir,
			RootDirectory:    dir,
			EntryPoint:       ".",
		},
	})
	require.NoError(t, err)

	assert.Empty(t, resp.Packages)
	assert.Equal(t, []*pulumirpc.PackageSpec{{
		Source:     "terraform-provider",
		Version:    bridgePackageVersion,
		Parameters: []string{"rollbar/rollbar", ">= 1.0"},
	}}, resp.Specs)
}

func TestGetRequiredPackages_TransitiveModuleSourceResolvedSDK(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "main.tf"), []byte(`
module "child" {
  source = "./child"
}
`), 0o600))

	childDir := filepath.Join(dir, "child")
	require.NoError(t, os.MkdirAll(childDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(childDir, "main.tf"), []byte(`
terraform {
  required_providers {
    rollbar = {
      source  = "rollbar/rollbar"
      version = ">= 1.0"
    }
  }
}
`), 0o600))

	// Mirror what `pulumi install` leaves behind: a local SDK resolving the
	// rollbar spec to a parameterized terraform-provider package.
	host := &LanguageHost{}
	sdkDir := filepath.Join(dir, "sdks", "rollbar")
	require.NoError(t, os.MkdirAll(sdkDir, 0o755))
	_, err := host.GeneratePackage(t.Context(), &pulumirpc.GeneratePackageRequest{
		Directory: sdkDir,
		Schema: `{
			"name": "rollbar",
			"version": "1.17.0",
			"parameterization": {
				"baseProvider": {"name": "terraform-provider", "version": "0.0.1"},
				"parameter": "aGVsbG8="
			}
		}`,
	})
	require.NoError(t, err)

	resp, err := host.GetRequiredPackages(t.Context(), &pulumirpc.GetRequiredPackagesRequest{
		Info: &pulumirpc.ProgramInfo{
			ProgramDirectory: dir,
			RootDirectory:    dir,
			EntryPoint:       ".",
		},
	})
	require.NoError(t, err)

	// The local SDK resolves the provider, so it must be reported as a package
	// and the now-redundant spec must be dropped — exactly as it is when the
	// declaration lives in the root module.
	assert.Equal(t, []*pulumirpc.PackageDependency{{
		Name:    "terraform-provider",
		Version: "0.0.1",
		Kind:    "resource",
		Parameterization: &pulumirpc.PackageParameterization{
			Name:    "rollbar",
			Version: "1.17.0",
			Value:   []byte("hello"),
		},
	}}, resp.Packages)
	assert.Empty(t, resp.Specs)
}

// A provider whose required_providers local name differs from its package
// name (the source basename) still resolves against the SDK directory
// `pulumi install` wrote under the package name, and the alias plus the
// resource-type-prefix alias share that one descriptor.
func TestGetRequiredPackages_RenamedLocalName(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "main.tf"), []byte(`
terraform {
  required_providers {
    myp = {
      source = "hashicorp/simple"
    }
  }
}

provider "myp" {}

resource "simple_resource" "r" {
  provider = myp
}
`), 0o600))

	host := &LanguageHost{}
	sdkDir := filepath.Join(dir, "sdks", "simple")
	require.NoError(t, os.MkdirAll(sdkDir, 0o755))
	_, err := host.GeneratePackage(t.Context(), &pulumirpc.GeneratePackageRequest{
		Directory: sdkDir,
		Schema: `{
			"name": "simple",
			"version": "1.0.0",
			"parameterization": {
				"baseProvider": {"name": "terraform-provider", "version": "0.0.1"},
				"parameter": "aGVsbG8="
			}
		}`,
	})
	require.NoError(t, err)

	resp, err := host.GetRequiredPackages(t.Context(), &pulumirpc.GetRequiredPackagesRequest{
		Info: &pulumirpc.ProgramInfo{
			ProgramDirectory: dir,
			RootDirectory:    dir,
			EntryPoint:       ".",
		},
	})
	require.NoError(t, err)

	assert.Equal(t, []*pulumirpc.PackageDependency{{
		Name:    "terraform-provider",
		Version: "0.0.1",
		Kind:    "resource",
		Parameterization: &pulumirpc.PackageParameterization{
			Name:    "simple",
			Version: "1.0.0",
			Value:   []byte("hello"),
		},
	}}, resp.Packages)
	assert.Empty(t, resp.Specs)
}

// missingNonPulumiSDKs must find the SDK directory under the resolved package
// name when the required_providers local name renames the provider.
func TestMissingNonPulumiSDKs_RenamedLocalName(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "main.tf"), []byte(`
terraform {
  required_providers {
    myp = {
      source = "hashicorp/simple"
    }
  }
}

provider "myp" {}

resource "simple_resource" "r" {
  provider = myp
}
`), 0o600))

	config, diags := parser.NewParser().ParseDirectory(dir)
	require.False(t, diags.HasErrors(), diags.Error())

	sdks := map[string]sdkInfo{
		"simple": {desc: bridgedSDK("simple")},
	}
	assert.Empty(t, missingNonPulumiSDKs(t.Context(), config, sdks, dir))
	assert.Equal(t, []string{"hashicorp/simple"},
		missingNonPulumiSDKs(t.Context(), config, nil, dir))
}

// TestGetRequiredPackages_SameSourceVersionIntersection mirrors tofu: two
// modules requiring the same provider source are installed once, with their
// version constraints unioned into one ", "-joined constraint. The
// terraform-provider plugin then intersects them at resolve time (verified
// end-to-end: ">= 3.0.0, < 3.2.0" resolves random v3.1.x), so an empty
// intersection fails exactly as `tofu init` does.
func TestGetRequiredPackages_SameSourceVersionIntersection(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "main.tf"), []byte(`
module "a" { source = "./a" }
module "b" { source = "./b" }
`), 0o600))

	for name, constraint := range map[string]string{"a": ">= 3.0", "b": "< 3.2"} {
		sub := filepath.Join(dir, name)
		require.NoError(t, os.MkdirAll(sub, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(sub, "main.tf"), []byte(`
terraform {
  required_providers {
    dns = {
      source  = "hashicorp/dns"
      version = "`+constraint+`"
    }
  }
}
`), 0o600))
	}

	host := &LanguageHost{}
	resp, err := host.GetRequiredPackages(t.Context(), &pulumirpc.GetRequiredPackagesRequest{
		Info: &pulumirpc.ProgramInfo{
			ProgramDirectory: dir,
			RootDirectory:    dir,
			EntryPoint:       ".",
		},
	})
	require.NoError(t, err)

	assert.Empty(t, resp.Packages)
	// Constraints are joined in sorted order, so the spec is deterministic
	// regardless of module-walk order ("< 3.2" sorts before ">= 3.0").
	assert.Equal(t, []*pulumirpc.PackageSpec{{
		Source:     "terraform-provider",
		Version:    bridgePackageVersion,
		Parameters: []string{"hashicorp/dns", "< 3.2, >= 3.0"},
	}}, resp.Specs)
}

// A root on pulumi/aws consuming a child module that implicitly requires
// hashicorp/aws holds two distinct requirements under one local name: after
// `pulumi install` writes the bridged SDK, both packages must be reported
// with no spec left over — not a spec re-emitted forever.
func TestGetRequiredPackages_PulumiRootTFChildSameName(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "main.tf"), []byte(`
terraform {
  required_providers {
    aws = {
      source = "pulumi/aws"
    }
  }
}

module "child" {
  source = "./child"
}
`), 0o600))
	childDir := filepath.Join(dir, "child")
	require.NoError(t, os.MkdirAll(childDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(childDir, "main.tf"), []byte(`
resource "aws_s3_bucket" "b" {}
`), 0o600))

	host := &LanguageHost{}
	info := &pulumirpc.ProgramInfo{ProgramDirectory: dir, RootDirectory: dir, EntryPoint: "."}

	resp, err := host.GetRequiredPackages(t.Context(), &pulumirpc.GetRequiredPackagesRequest{Info: info})
	require.NoError(t, err)
	assert.Equal(t, []*pulumirpc.PackageDependency{{
		Name: "aws",
		Kind: "resource",
	}}, resp.Packages)
	assert.Equal(t, []*pulumirpc.PackageSpec{{
		Source:     "terraform-provider",
		Version:    bridgePackageVersion,
		Parameters: []string{"hashicorp/aws"},
	}}, resp.Specs)

	// Mirror what `pulumi install` leaves behind for the spec.
	sdkDir := filepath.Join(dir, "sdks", "aws")
	require.NoError(t, os.MkdirAll(sdkDir, 0o755))
	_, err = host.GeneratePackage(t.Context(), &pulumirpc.GeneratePackageRequest{
		Directory: sdkDir,
		Schema: `{
			"name": "aws",
			"version": "6.55.0",
			"parameterization": {
				"baseProvider": {"name": "terraform-provider", "version": "0.0.1"},
				"parameter": "aGVsbG8="
			}
		}`,
	})
	require.NoError(t, err)

	resp, err = host.GetRequiredPackages(t.Context(), &pulumirpc.GetRequiredPackagesRequest{Info: info})
	require.NoError(t, err)
	assert.Equal(t, []*pulumirpc.PackageDependency{
		{
			Name:    "terraform-provider",
			Version: "0.0.1",
			Kind:    "resource",
			Parameterization: &pulumirpc.PackageParameterization{
				Name:    "aws",
				Version: "6.55.0",
				Value:   []byte("hello"),
			},
		},
		{
			Name: "aws",
			Kind: "resource",
		},
	}, resp.Packages)
	assert.Empty(t, resp.Specs)
}

// Two distinct sources resolving to one package name cannot both live at
// sdks/<name>, so `pulumi install` could never satisfy both.
func TestGetRequiredPackages_SameNameDistinctSourcesErrors(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "main.tf"), []byte(`
module "a" { source = "./a" }
module "b" { source = "./b" }
`), 0o600))
	for name, source := range map[string]string{"a": "hashicorp/dns", "b": "mycorp/dns"} {
		sub := filepath.Join(dir, name)
		require.NoError(t, os.MkdirAll(sub, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(sub, "main.tf"), []byte(`
terraform {
  required_providers {
    dns = {
      source = "`+source+`"
    }
  }
}
`), 0o600))
	}

	host := &LanguageHost{}
	_, err := host.GetRequiredPackages(t.Context(), &pulumirpc.GetRequiredPackagesRequest{
		Info: &pulumirpc.ProgramInfo{ProgramDirectory: dir, RootDirectory: dir, EntryPoint: "."},
	})
	assert.ErrorContains(t, err, `provider sources "hashicorp/dns" and "mycorp/dns" both resolve to package name "dns"`)
}

// Every spelling of one provider source is one requirement: the short form
// and the fully-qualified registry form must union into a single spec, shown
// with the shortest spelling.
func TestGetRequiredPackages_CanonicalSourceDedup(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "main.tf"), []byte(`
module "a" { source = "./a" }
module "b" { source = "./b" }
`), 0o600))
	for name, source := range map[string]string{"a": "hashicorp/dns", "b": "registry.opentofu.org/hashicorp/dns"} {
		sub := filepath.Join(dir, name)
		require.NoError(t, os.MkdirAll(sub, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(sub, "main.tf"), []byte(`
terraform {
  required_providers {
    dns = {
      source  = "`+source+`"
      version = ">= 3.0"
    }
  }
}
`), 0o600))
	}

	host := &LanguageHost{}
	resp, err := host.GetRequiredPackages(t.Context(), &pulumirpc.GetRequiredPackagesRequest{
		Info: &pulumirpc.ProgramInfo{ProgramDirectory: dir, RootDirectory: dir, EntryPoint: "."},
	})
	require.NoError(t, err)
	assert.Empty(t, resp.Packages)
	assert.Equal(t, []*pulumirpc.PackageSpec{{
		Source:     "terraform-provider",
		Version:    bridgePackageVersion,
		Parameters: []string{"hashicorp/dns", ">= 3.0"},
	}}, resp.Specs)
}

// A stamped SDK satisfies exactly its stamped source: after the program's
// required_providers source changes, the stale SDK must not satisfy the new
// source by directory-name inference, so `pulumi install` regenerates it.
func TestDescriptorForSource_StampedSource(t *testing.T) {
	t.Parallel()

	stamped := map[string]sdkInfo{"dns": {desc: bridgedSDK("dns"), source: "hashicorp/dns"}}

	dir, _, ok := descriptorForSource(canonicalSource("hashicorp/dns"), stamped)
	assert.True(t, ok)
	assert.Equal(t, "dns", dir)

	_, _, ok = descriptorForSource(canonicalSource("mycorp/dns"), stamped)
	assert.False(t, ok)

	// Unstamped descriptors keep satisfying by directory name.
	unstamped := map[string]sdkInfo{"dns": {desc: bridgedSDK("dns")}}
	_, _, ok = descriptorForSource(canonicalSource("mycorp/dns"), unstamped)
	assert.True(t, ok)
}

// GeneratePackage records the provider source a bridged SDK satisfies,
// decoded from the terraform-provider parameterization it was built from;
// non-bridge parameters leave the source empty.
func TestGeneratePackageRecordsSource(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	host := &LanguageHost{}
	generate := func(sdkName, schema string) {
		sdkDir := filepath.Join(dir, "sdks", sdkName)
		require.NoError(t, os.MkdirAll(sdkDir, 0o755))
		_, err := host.GeneratePackage(t.Context(), &pulumirpc.GeneratePackageRequest{
			Directory: sdkDir,
			Schema:    schema,
		})
		require.NoError(t, err)
	}

	param := base64.StdEncoding.EncodeToString(
		[]byte(`{"remote":{"url":"registry.opentofu.org/hashicorp/dns","version":"3.1.0"}}`))
	generate("dns", `{
		"name": "dns",
		"version": "3.1.0",
		"parameterization": {
			"baseProvider": {"name": "terraform-provider", "version": "0.0.1"},
			"parameter": "`+param+`"
		}
	}`)
	generate("myparam", `{
		"name": "myparam",
		"version": "1.2.3",
		"parameterization": {
			"baseProvider": {"name": "baseplugin", "version": "1.0.0"},
			"parameter": "aGVsbG8="
		}
	}`)

	infos, err := readSDKInfos(dir)
	require.NoError(t, err)
	require.Len(t, infos, 2)
	assert.Equal(t, "registry.opentofu.org/hashicorp/dns", infos["dns"].source)
	assert.Equal(t, "terraform-provider", infos["dns"].desc.Name)
	require.NotNil(t, infos["dns"].desc.Parameterization)
	assert.Equal(t, "dns", infos["dns"].desc.Parameterization.Name)
	assert.Equal(t, "", infos["myparam"].source)
}

// TestGetRequiredPackages_DistinctSourcesSameLocalName mirrors tofu: provider
// requirements are keyed by source, not local name, so two modules using the
// same local name ("dns") for different sources yield two distinct installs.
func TestGetRequiredPackages_DistinctSourcesSameLocalName(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "main.tf"), []byte(`
module "a" { source = "./a" }
module "b" { source = "./b" }
`), 0o600))

	for name, source := range map[string]string{"a": "hashicorp/dns", "b": "rollbar/rollbar"} {
		sub := filepath.Join(dir, name)
		require.NoError(t, os.MkdirAll(sub, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(sub, "main.tf"), []byte(`
terraform {
  required_providers {
    dns = {
      source = "`+source+`"
    }
  }
}
`), 0o600))
	}

	host := &LanguageHost{}
	resp, err := host.GetRequiredPackages(t.Context(), &pulumirpc.GetRequiredPackagesRequest{
		Info: &pulumirpc.ProgramInfo{
			ProgramDirectory: dir,
			RootDirectory:    dir,
			EntryPoint:       ".",
		},
	})
	require.NoError(t, err)

	assert.Empty(t, resp.Packages)
	// Specs are sorted by source.
	assert.Equal(t, []*pulumirpc.PackageSpec{
		{Source: "terraform-provider", Version: bridgePackageVersion, Parameters: []string{"hashicorp/dns"}},
		{Source: "terraform-provider", Version: bridgePackageVersion, Parameters: []string{"rollbar/rollbar"}},
	}, resp.Specs)
}

// TestMissingNonPulumiSDKs_ImplicitProvider reproduces the tf_stack_test bug:
// a program references `data "archive_file" ...` without declaring `archive`
// in required_providers. The provider is *implicit* — its only mention is in
// the data source's type prefix. The previous implementation only looked at
// required_providers, so the missing SDK slipped through and Run sent the
// engine a raw "registry.terraform.io/hashicorp/archive" provider request.
// missingNonPulumiSDKs must catch the implicit provider too.
func TestMissingNonPulumiSDKs_ImplicitProvider(t *testing.T) {
	t.Parallel()

	const src = `terraform {
  required_providers {
    aws = {
      source  = "hashicorp/aws"
    }
  }
}

resource "aws_s3_bucket" "b" {}

data "archive_file" "lambda" {}
`
	cfg, diags := parser.NewParser().ParseSource("main.tf", []byte(src))
	require.False(t, diags.HasErrors(), "diags: %v", diags)

	// No SDKs on disk: both non-Pulumi providers (explicit aws, implicit
	// archive) must be reported missing.
	assert.Equal(t,
		[]string{"hashicorp/archive", "hashicorp/aws"},
		missingNonPulumiSDKs(t.Context(), cfg, nil, ""))

	// Once both have SDKs, nothing is missing.
	sdks := map[string]sdkInfo{
		"aws":     {desc: bridgedSDK("aws")},
		"archive": {desc: bridgedSDK("archive")},
	}
	assert.Empty(t, missingNonPulumiSDKs(t.Context(), cfg, sdks, ""))

	// A pulumi-source provider needs no SDK even when it's only referenced
	// by a resource type prefix.
	const pulumiSrc = `terraform {
  required_providers {
    aws = {
      source = "pulumi/aws"
    }
  }
}

resource "aws_s3_bucket" "b" {}
`
	cfgPulumi, diags := parser.NewParser().ParseSource("main.tf", []byte(pulumiSrc))
	require.False(t, diags.HasErrors(), "diags: %v", diags)
	assert.Empty(t, missingNonPulumiSDKs(t.Context(), cfgPulumi, nil, ""))
}

func TestMissingNonPulumiSDKs_BuiltinProvider(t *testing.T) {
	t.Parallel()

	const src = `resource "pulumi_stash" "myStash" {
  input = "test"
}
`
	cfg, diags := parser.NewParser().ParseSource("main.tf", []byte(src))
	require.False(t, diags.HasErrors(), "diags: %v", diags)

	assert.Empty(t, missingNonPulumiSDKs(t.Context(), cfg, nil, ""))
}

// A pulumi-sourced provider declared only in a child module must not be
// reported missing: it needs no local SDK regardless of which module declares
// it.
func TestMissingNonPulumiSDKs_PulumiSourceInChildModule(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "main.tf"), []byte(`
module "deploy" {
  source = "./deploy"
}
`), 0o600))

	childDir := filepath.Join(dir, "deploy")
	require.NoError(t, os.MkdirAll(childDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(childDir, "main.tf"), []byte(`
terraform {
  required_providers {
    example = {
      source = "pulumi/example"
    }
  }
}

provider "example" {}

resource "example_resource" "hello" {}
`), 0o600))

	cfg, diags := parser.NewParser().ParseDirectory(dir)
	require.False(t, diags.HasErrors(), "diags: %v", diags)

	assert.Empty(t, missingNonPulumiSDKs(t.Context(), cfg, nil, dir))
}

// A provider local name that contains underscores (e.g. "snake_names") must
// be resolved against the declared providers, not split at the first
// underscore. The naive split yielded a spurious "snake" provider that was
// then reported missing even though "snake_names" is pulumi-sourced.
func TestMissingNonPulumiSDKs_UnderscoreProviderName(t *testing.T) {
	t.Parallel()

	const src = `terraform {
  required_providers {
    snake_names = {
      source  = "pulumi/snake_names"
    }
  }
}

resource "snake_names_cool_module_some_resource" "first" {
  the_input = true
}
`
	cfg, diags := parser.NewParser().ParseSource("main.tf", []byte(src))
	require.False(t, diags.HasErrors(), "diags: %v", diags)

	assert.Empty(t, missingNonPulumiSDKs(t.Context(), cfg, nil, ""))
}

// Implicit provider inside a child module must surface at the top — without
// recursion the SDK check silently misses it (the aws-ia/label gap).
func TestMissingNonPulumiSDKs_TransitiveModuleProvider(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "main.tf"), []byte(`
module "child" {
  source = "./child"
}
`), 0o600))

	childDir := filepath.Join(dir, "child")
	require.NoError(t, os.MkdirAll(childDir, 0o755))
	// Only mention of "aws" is inside the module.
	require.NoError(t, os.WriteFile(filepath.Join(childDir, "main.tf"), []byte(`
resource "aws_s3_bucket" "b" {}
`), 0o600))

	cfg, diags := parser.NewParser().ParseDirectory(dir)
	require.False(t, diags.HasErrors(), "diags: %v", diags)

	assert.Equal(t,
		[]string{"hashicorp/aws"},
		missingNonPulumiSDKs(t.Context(), cfg, nil, dir))
}

// Same recursion via the module's `required_providers` block (no resources).
func TestMissingNonPulumiSDKs_TransitiveModuleRequiredProviders(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "main.tf"), []byte(`
module "child" {
  source = "./child"
}
`), 0o600))

	childDir := filepath.Join(dir, "child")
	require.NoError(t, os.MkdirAll(childDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(childDir, "main.tf"), []byte(`
terraform {
  required_providers {
    awscc = {
      source  = "hashicorp/awscc"
      version = ">= 1.0"
    }
  }
}
`), 0o600))

	cfg, diags := parser.NewParser().ParseDirectory(dir)
	require.False(t, diags.HasErrors(), "diags: %v", diags)

	assert.Equal(t,
		[]string{"hashicorp/awscc"},
		missingNonPulumiSDKs(t.Context(), cfg, nil, dir))
}

// terraform_remote_state is served by the external pulumi-terraform package, so
// it must be emitted as an installable dependency even though the `terraform`
// alias is otherwise a builtin provider.
func TestGetRequiredPackages_TerraformRemoteState(t *testing.T) {
	t.Parallel()

	projectDir := t.TempDir()
	program := `data "terraform_remote_state" "rs" {
  backend = "local"
  config = {
    path = "remote.tfstate"
  }
}
`
	require.NoError(t, os.WriteFile(filepath.Join(projectDir, "main.tf"), []byte(program), 0o600))

	host := &LanguageHost{}
	resp, err := host.GetRequiredPackages(t.Context(), &pulumirpc.GetRequiredPackagesRequest{
		Info: &pulumirpc.ProgramInfo{
			ProgramDirectory: projectDir,
			RootDirectory:    projectDir,
			EntryPoint:       ".",
		},
	})
	require.NoError(t, err)

	assert.Empty(t, resp.Specs)
	require.Len(t, resp.Packages, 1)
	assert.Equal(t, &pulumirpc.PackageDependency{
		Name:    run.TerraformStatePackage,
		Version: run.TerraformStatePackageVersion,
		Kind:    "resource",
	}, resp.Packages[0])
}

// terraform_remote_state nested in a module is still detected, so the
// pulumi-terraform package is emitted.
func TestGetRequiredPackages_TerraformRemoteStateInModule(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "main.tf"), []byte(`
module "child" {
  source = "./child"
}
`), 0o600))

	childDir := filepath.Join(dir, "child")
	require.NoError(t, os.MkdirAll(childDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(childDir, "main.tf"), []byte(`
data "terraform_remote_state" "rs" {
  backend = "local"
  config = {
    path = "remote.tfstate"
  }
}
`), 0o600))

	host := &LanguageHost{}
	resp, err := host.GetRequiredPackages(t.Context(), &pulumirpc.GetRequiredPackagesRequest{
		Info: &pulumirpc.ProgramInfo{
			ProgramDirectory: dir,
			RootDirectory:    dir,
			EntryPoint:       ".",
		},
	})
	require.NoError(t, err)

	require.Len(t, resp.Packages, 1)
	assert.Equal(t, &pulumirpc.PackageDependency{
		Name:    run.TerraformStatePackage,
		Version: run.TerraformStatePackageVersion,
		Kind:    "resource",
	}, resp.Packages[0])
}

// writeSubmodulePackage writes a component package whose modules/greeter
// submodule requires a provider the root never references.
func writeSubmodulePackage(t *testing.T, withPlugin, withRoot bool) string {
	t.Helper()
	dir := t.TempDir()
	if withPlugin {
		require.NoError(t, os.WriteFile(filepath.Join(dir, "PulumiPlugin.yaml"), []byte("runtime: hcl\n"), 0o600))
	}
	if withRoot {
		require.NoError(t, os.WriteFile(filepath.Join(dir, "main.tf"), []byte(`
output "x" {
  value = 1
}
`), 0o600))
	}
	greeterDir := filepath.Join(dir, "modules", "greeter")
	require.NoError(t, os.MkdirAll(greeterDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(greeterDir, "main.tf"), []byte(`
terraform {
  required_providers {
    greeting = {
      source  = "acme/greeting"
      version = ">= 3.0"
    }
  }
}

resource "greeting_card" "p" {}
`), 0o600))
	return dir
}

func getRequiredPackages(t *testing.T, dir string) *pulumirpc.GetRequiredPackagesResponse {
	t.Helper()
	host := &LanguageHost{}
	resp, err := host.GetRequiredPackages(t.Context(), &pulumirpc.GetRequiredPackagesRequest{
		Info: &pulumirpc.ProgramInfo{
			ProgramDirectory: dir,
			RootDirectory:    dir,
			EntryPoint:       ".",
		},
	})
	require.NoError(t, err)
	return resp
}

func TestGetRequiredPackages_SubmoduleComponentProviders(t *testing.T) {
	t.Parallel()

	resp := getRequiredPackages(t, writeSubmodulePackage(t, true, true))
	assert.Empty(t, resp.Packages)
	assert.Equal(t, []*pulumirpc.PackageSpec{{
		Source:     "terraform-provider",
		Version:    bridgePackageVersion,
		Parameters: []string{"acme/greeting", ">= 3.0"},
	}}, resp.Specs)
}

func TestGetRequiredPackages_RootlessComponentProviders(t *testing.T) {
	t.Parallel()

	resp := getRequiredPackages(t, writeSubmodulePackage(t, true, false))
	assert.Empty(t, resp.Packages)
	assert.Equal(t, []*pulumirpc.PackageSpec{{
		Source:     "terraform-provider",
		Version:    bridgePackageVersion,
		Parameters: []string{"acme/greeting", ">= 3.0"},
	}}, resp.Specs)
}

func TestGetRequiredPackages_ProgramSkipsUnreferencedModules(t *testing.T) {
	t.Parallel()

	resp := getRequiredPackages(t, writeSubmodulePackage(t, false, true))
	assert.Empty(t, resp.Packages)
	assert.Empty(t, resp.Specs)
}

// TestPackageDescriptorFromSchemaExtension verifies that an extension
// parameterization in a package schema is read into the descriptor's extension
// slot, naming the base provider (whose namespace the extension's tokens use).
func TestPackageDescriptorFromSchemaExtension(t *testing.T) {
	t.Parallel()

	desc, err := packageDescriptorFromSchema([]byte(`{
		"name": "myext",
		"version": "2.0.0",
		"extensionParameterization": {
			"baseProvider": {"name": "extbase", "version": "45.0.0"},
			"parameter": "SGVsbG8="
		}
	}`))
	require.NoError(t, err)

	baseVersion := semver.MustParse("45.0.0")
	assert.Equal(t, workspace.PackageDescriptor{
		PluginDescriptor: workspace.PluginDescriptor{
			Name:    "extbase",
			Kind:    apitype.ResourcePlugin,
			Version: &baseVersion,
		},
		ExtensionParameterization: &workspace.Parameterization{
			Name:    "myext",
			Version: semver.MustParse("2.0.0"),
			Value:   []byte("Hello"),
		},
	}, desc)
}

func TestLinkInstructions(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	host := &LanguageHost{}
	var deps []*pulumirpc.LinkRequest_LinkDependency
	for dir, schema := range map[string]string{
		"aws":       `{"name": "aws", "version": "7.0.0"}`,
		"stackmgmt": `{"name": "stackmgmt", "version": "2.5.0", "parameterization": {"baseProvider": {"name": "pulumi-component", "version": "1.0.0"}, "parameter": "aGVsbG8="}}`,
		"random": `{"name": "random", "version": "3.6.0", "parameterization": {"baseProvider": {"name": "terraform-provider", "version": "1.3.0"}, "parameter": "` +
			base64.StdEncoding.EncodeToString([]byte(`{"remote":{"url":"hashicorp/random","version":"3.6.0"}}`)) + `"}}`,
	} {
		sdkDir := filepath.Join(root, "sdks", dir)
		require.NoError(t, os.MkdirAll(sdkDir, 0o755))
		_, err := host.GeneratePackage(t.Context(), &pulumirpc.GeneratePackageRequest{Directory: sdkDir, Schema: schema})
		require.NoError(t, err)
		deps = append(deps, &pulumirpc.LinkRequest_LinkDependency{Path: filepath.Join("sdks", dir)})
	}
	slices.SortFunc(deps, func(a, b *pulumirpc.LinkRequest_LinkDependency) int { return strings.Compare(a.Path, b.Path) })
	deps = append(deps, &pulumirpc.LinkRequest_LinkDependency{
		Path:    "/nonexistent/core-sdk",
		Package: &pulumirpc.PackageDependency{Name: "pulumi"},
	})
	link := func() string {
		resp, err := host.Link(t.Context(), &pulumirpc.LinkRequest{
			Info:     &pulumirpc.ProgramInfo{RootDirectory: root, ProgramDirectory: root},
			Packages: deps,
		})
		require.NoError(t, err)
		return resp.ImportInstructions
	}

	assert.Equal(t, `You can use the packages in your HCL program with:

terraform {
  required_providers {
    aws = {
      source = "pulumi/aws"
    }
    random = {
      source = "hashicorp/random"
    }
    stackmgmt = {
      source = "pulumi/stackmgmt"
    }
  }
}
`, link(), "an empty program references nothing")

	require.NoError(t, os.WriteFile(filepath.Join(root, "main.tf"), []byte(`
terraform {
  required_providers {
    aws    = { source = "pulumi/aws" }
    random = { source = "hashicorp/random" }
  }
}
`), 0o644))
	assert.Equal(t, `You can use the package in your HCL program with:

terraform {
  required_providers {
    stackmgmt = {
      source = "pulumi/stackmgmt"
    }
  }
}
`, link(), "only the undeclared package is reported")

	require.NoError(t, os.WriteFile(filepath.Join(root, "main.tf"), []byte(`
terraform {
  required_providers {
    aws       = { source = "pulumi/aws" }
    random    = { source = "hashicorp/random" }
    stackmgmt = { source = "pulumi/stackmgmt" }
  }
}
`), 0o644))
	assert.Equal(t, "", link(), "a program declaring every package needs no instructions")
}
