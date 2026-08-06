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
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blang/semver"
	p "github.com/pulumi/pulumi-go-provider"
	pulumiSchema "github.com/pulumi/pulumi/pkg/v3/codegen/schema"
	"github.com/pulumi/pulumi/sdk/v3/go/common/apitype"
	"github.com/pulumi/pulumi/sdk/v3/go/common/workspace"
	pulumirpc "github.com/pulumi/pulumi/sdk/v3/proto/go"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/pulumi/pulumi-hcl/pkg/hcl/ast"
	"github.com/pulumi/pulumi-hcl/pkg/hcl/modules"
)

// TestModuleIdentityVersion verifies version precedence: an explicit terraform
// `package` block wins, then the concrete version the module's source resolved
// to, then the "0.0.0-dev" fallback.
func TestModuleIdentityVersion(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		config   *ast.Config
		resolved string
		want     string
	}{
		{"fallback", &ast.Config{}, "", "0.0.0-dev"},
		{"resolved registry version", &ast.Config{}, "4.0.1", "4.0.1"},
		{
			"package block wins",
			&ast.Config{Terraform: &ast.Terraform{Package: &ast.PackageBlock{Version: "1.0.0"}}},
			"4.0.1",
			"1.0.0",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			loaded := &modules.LoadedModule{Config: tc.config, Version: tc.resolved}
			_, version, err := moduleIdentity(loaded, "pkg", false)
			require.NoError(t, err)
			require.Equal(t, semver.MustParse(tc.want), version)
		})
	}
}

func TestDefaultPackageName(t *testing.T) {
	t.Parallel()
	cases := []struct {
		source string
		want   string
	}{
		{"terraform-aws-modules/vpc/aws", "vpc-aws"},
		{"terraform-aws-modules/vpc/aws//modules/subnets", "vpc-aws"},
		{"github.com/org/my-repo", "my-repo"},
		{"git::https://github.com/org/repo.git//subdir", "repo"},
		{"git::https://example.com/foo/Bar_Baz.git", "bar-baz"},
		{"./local/path", "path"},
	}
	for _, tc := range cases {
		t.Run(tc.source, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tc.want, defaultPackageName(tc.source))
		})
	}
}

func TestPackageNamespace(t *testing.T) {
	t.Parallel()
	cases := []struct {
		source string
		want   string
	}{
		{"terraform-aws-modules/vpc/aws", "terraform-aws-modules"},
		{"terraform-aws-modules/vpc/aws//modules/subnets", "terraform-aws-modules"},
		{"app.terraform.io/acme/widget/aws", "acme"},
		{"registry.opentofu.org/acme/widget/aws", "acme"},
		{"registry.terraform.io/acme/widget/aws", "acme"},
		{"github.com/org/my-repo", ""},
		{"git::https://github.com/org/repo.git//subdir", ""},
		{"./local/path", ""},
	}
	for _, tc := range cases {
		t.Run(tc.source, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tc.want, packageNamespace(tc.source))
		})
	}
}

func TestParameterizeArgsRejected(t *testing.T) {
	t.Parallel()

	t.Run("before handshake", func(t *testing.T) {
		t.Parallel()
		_, err := (&moduleProvider{}).parameterizeArgs(t.Context(), []string{"module", "acme/widget/aws"})
		require.EqualError(t, err, "parameterize called before a successful handshake")
	})

	t.Run("missing module keyword", func(t *testing.T) {
		t.Parallel()
		_, err := (&moduleProvider{resolver: stubResolver{}}).parameterizeArgs(t.Context(), []string{"acme/widget/aws"})
		require.EqualError(t, err,
			`the hcl provider is parameterized by a module: expected "module" as the first argument`)
	})

	t.Run("no source", func(t *testing.T) {
		t.Parallel()
		_, err := (&moduleProvider{resolver: stubResolver{}}).parameterizeArgs(t.Context(), []string{"module"})
		require.EqualError(t, err, `the hcl provider is parameterized as "module <source> [version] [--name <name>]": `+
			`accepts between 1 and 2 arg(s), received 0`)
	})

	t.Run("too many args", func(t *testing.T) {
		t.Parallel()
		_, err := (&moduleProvider{resolver: stubResolver{}}).parameterizeArgs(t.Context(), []string{"module", "a", "b", "c"})
		require.EqualError(t, err, `the hcl provider is parameterized as "module <source> [version] [--name <name>]": `+
			`accepts between 1 and 2 arg(s), received 3`)
	})
}

// TestParameterizeArgsServesTypedSchema drives the args path end-to-end for a
// provider-free local module: it must parameterize without touching the resolver
// and then serve the module's typed component schema (not the generic
// hcl:index:Module) with its parameterization Value attached.
func TestParameterizeArgsServesTypedSchema(t *testing.T) {
	t.Parallel()

	dir, err := filepath.Abs(filepath.Join("testdata", "module-one-var"))
	require.NoError(t, err)

	m := &moduleProvider{version: "1.2.3", resolver: stubResolver{}}
	resp, err := m.parameterizeArgs(t.Context(), []string{"module", dir})
	require.NoError(t, err)
	require.Equal(t, "module-one-var", resp.Name)
	require.NotNil(t, m.param)

	out, err := m.getSchema(t.Context(), p.GetSchemaRequest{})
	require.NoError(t, err)

	var spec pulumiSchema.PackageSpec
	require.NoError(t, json.Unmarshal([]byte(out.Schema), &spec))

	require.NotNil(t, spec.Parameterization)
	require.Equal(t, "hcl", spec.Parameterization.BaseProvider.Name)

	res, ok := spec.Resources["module-one-var:index:Module"]
	require.True(t, ok, "schema should declare the typed component")
	require.True(t, res.IsComponent)
	require.Equal(t, "string", res.InputProperties["name"].Type)
	require.Equal(t, "string", res.Properties["greeting"].Type)
	// "name" declares no `nullable = false`, so it is optional, matching the
	// MLC's handling of a nullable variable.
	require.Empty(t, res.RequiredInputs)
}

// TestParameterizeNameOverrideLifecycle drives `--name` through the full
// lifecycle: the args path must name the package and its component token after
// the override, and the usage path — re-parameterizing a fresh provider from the
// returned name and Value, as the engine does for a generated SDK — must
// preserve it.
func TestParameterizeNameOverrideLifecycle(t *testing.T) {
	t.Parallel()

	dir, err := filepath.Abs(filepath.Join("testdata", "module-one-var"))
	require.NoError(t, err)

	m := &moduleProvider{version: "1.2.3", resolver: stubResolver{}}
	resp, err := m.parameterizeArgs(t.Context(), []string{"module", dir, "--name", "greeter"})
	require.NoError(t, err)
	require.Equal(t, "greeter", resp.Name)

	assertGreeterSchema := func(m *moduleProvider) pulumiSchema.PackageSpec {
		out, err := m.getSchema(t.Context(), p.GetSchemaRequest{})
		require.NoError(t, err)
		var spec pulumiSchema.PackageSpec
		require.NoError(t, json.Unmarshal([]byte(out.Schema), &spec))
		require.Equal(t, "greeter", spec.Name)
		res, ok := spec.Resources["greeter:index:Module"]
		require.True(t, ok, "schema should declare the typed component under the overridden name")
		require.True(t, res.IsComponent)
		return spec
	}
	spec := assertGreeterSchema(m)

	usage := &moduleProvider{version: "1.2.3", resolver: stubResolver{}}
	resp2, err := usage.parameterize(t.Context(), p.ParameterizeRequest{
		Value: &p.ParameterizeRequestValue{
			Name:    resp.Name,
			Version: resp.Version,
			Value:   spec.Parameterization.Parameter,
		},
	})
	require.NoError(t, err)
	require.Equal(t, resp, resp2)
	assertGreeterSchema(usage)
}

// TestParameterizeValueEchoesEngineVersion verifies the usage path reports the
// version the engine echoes from the original ParameterizeResponse, not one
// re-derived from the bundle — offline, nothing else knows the concrete version
// a registry constraint resolved to at `package add` time.
func TestParameterizeValueEchoesEngineVersion(t *testing.T) {
	t.Parallel()

	dir, err := filepath.Abs(filepath.Join("testdata", "module-one-var"))
	require.NoError(t, err)

	m := &moduleProvider{version: "1.2.3", resolver: stubResolver{}}
	resp, err := m.parameterizeArgs(t.Context(), []string{"module", dir})
	require.NoError(t, err)
	require.Equal(t, semver.MustParse("0.0.0-dev"), resp.Version)

	usage := &moduleProvider{version: "1.2.3", resolver: stubResolver{}}
	resp2, err := usage.parameterize(t.Context(), p.ParameterizeRequest{
		Value: &p.ParameterizeRequestValue{
			Name:    resp.Name,
			Version: semver.MustParse("4.0.1"),
			Value:   m.param.value,
		},
	})
	require.NoError(t, err)
	require.Equal(t, p.ParameterizeResponse{Name: resp.Name, Version: semver.MustParse("4.0.1")}, resp2)
}

// TestParameterizeNameOverrideBeatsPackageBlock verifies precedence between the
// terraform `package` block and the `--name` flag: the block names the package
// by default, and `--name` overrides it.
func TestParameterizeNameOverrideBeatsPackageBlock(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "main.tf"), []byte(`
terraform {
  package {
    name    = "blockname"
    version = "1.0.0"
  }
}
output "x" { value = 1 }
`), 0o600))

	parameterize := func(args ...string) p.ParameterizeResponse {
		m := &moduleProvider{version: "1.2.3", resolver: stubResolver{}}
		resp, err := m.parameterizeArgs(t.Context(), append([]string{"module", dir}, args...))
		require.NoError(t, err)
		return resp
	}

	require.Equal(t, "blockname", parameterize().Name)
	require.Equal(t, "greeter", parameterize("--name", "greeter").Name)
}

// TestBundleRoundTripResolvesOffline records a module tree — including a
// non-local submodule and an absolute-path root — bundles it, then resolves
// every source from the unpacked bundle with a resolver that has no fallback, so
// any unrecorded reference would error.
func TestBundleRoundTripResolvesOffline(t *testing.T) {
	t.Parallel()

	child := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(child, "main.tf"), []byte(`output "id" { value = "child" }`), 0o600))

	root := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(root, "main.tf"), []byte(`module "c" { source = "acme/widget/aws" }`), 0o600))

	// A custom resolver stands in for the network: the absolute-path root
	// resolves to itself and the submodule to the child package.
	rec := newResolveRecorder(func(packageSource, _, _ string) (string, string, error) {
		switch packageSource {
		case root:
			return root, "", nil
		case "acme/widget/aws":
			return child, "", nil
		default:
			return "", "", fmt.Errorf("unexpected source %q", packageSource)
		}
	})
	loader := modules.NewLoader(rec.resolve)

	// Loading the tree records every resolution into the recorder.
	loaded, err := loader.LoadModule(t.Context(), root, "", ".")
	require.NoError(t, err)
	_, err = loader.LoadModule(t.Context(), "acme/widget/aws", "", loaded.SourcePath)
	require.NoError(t, err)

	value, err := rec.bundle(moduleRef{Source: root}, nil)
	require.NoError(t, err)

	b, err := decodeBundle(value)
	require.NoError(t, err)
	dest := t.TempDir()
	require.NoError(t, unpackArchive(t.Context(), b.Archive, dest))
	manifest := b.Manifest

	// The bundle resolver has no network and no filesystem fallback: every
	// source — the absolute-path root included — must come from the manifest.
	offline := modules.NewLoader(bundleResolver(dest, manifest))
	rootAgain, err := offline.LoadModule(t.Context(), manifest.Root.Source, manifest.Root.Version, ".")
	require.NoError(t, err)
	require.Contains(t, rootAgain.Config.Modules, "c")
	require.True(t, strings.HasPrefix(rootAgain.SourcePath, dest),
		"root must resolve inside the unpacked bundle, got %q", rootAgain.SourcePath)

	childAgain, err := offline.LoadModule(t.Context(), "acme/widget/aws", "", rootAgain.SourcePath)
	require.NoError(t, err)
	require.Contains(t, childAgain.Config.Outputs, "id")
	require.True(t, strings.HasPrefix(childAgain.SourcePath, dest),
		"submodule must resolve inside the unpacked bundle, got %q", childAgain.SourcePath)
}

// TestBundleRoundTripEscapingTopology covers a package topology where a local
// reference escapes the root package: root -> ./A -> ../../B, with B a sibling of
// the root. B is outside the root package, so it must be bundled as its own
// package; at usage the escaping "../../B" must resolve to that bundled package
// from the manifest, not by walking ".." out of the unpacked tree (where nothing
// exists). A non-escaping local sibling ./C stays inside the root package.
func TestBundleRoundTripEscapingTopology(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	write := func(rel, content string) {
		t.Helper()
		full := filepath.Join(base, rel)
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
		require.NoError(t, os.WriteFile(full, []byte(content), 0o600))
	}
	write("root/main.tf", `module "a" { source = "./A" }`+"\n"+`module "c" { source = "./C" }`)
	write("root/A/main.tf", `module "b" { source = "../../B" }`)
	write("root/C/main.tf", `output "from_c" { value = true }`)
	write("B/main.tf", `output "from_b" { value = true }`)

	rec := newResolveRecorder(modules.LiveResolver(t.Context()))
	loader := modules.NewLoader(rec.resolve)

	// Load the whole tree (root, its two children, and the escaping grandchild) so
	// every reference is recorded.
	rootDir := filepath.Join(base, "root")
	root, err := loader.LoadModule(t.Context(), rootDir, "", ".")
	require.NoError(t, err)
	a, err := loader.LoadModule(t.Context(), "./A", "", root.SourcePath)
	require.NoError(t, err)
	_, err = loader.LoadModule(t.Context(), "./C", "", root.SourcePath)
	require.NoError(t, err)
	_, err = loader.LoadModule(t.Context(), "../../B", "", a.SourcePath)
	require.NoError(t, err)

	value, err := rec.bundle(moduleRef{Source: rootDir}, nil)
	require.NoError(t, err)

	b, err := decodeBundle(value)
	require.NoError(t, err)
	dest := t.TempDir()
	require.NoError(t, unpackArchive(t.Context(), b.Archive, dest))
	manifest := b.Manifest

	// The escaping reference must be bundled as its own package, not as a path
	// inside the root package "0".
	var escaped *resolvedEdge
	for i := range manifest.Edges {
		if manifest.Edges[i].Source == "../../B" {
			escaped = &manifest.Edges[i]
		}
	}
	require.NotNil(t, escaped, "the ../../B reference should be recorded")
	require.False(t, strings.HasPrefix(escaped.Target, "0/"),
		"../../B escapes the root package, so it must be its own package, got target %q", escaped.Target)

	offline := modules.NewLoader(bundleResolver(dest, manifest))
	root2, err := offline.LoadModule(t.Context(), manifest.Root.Source, manifest.Root.Version, ".")
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(root2.SourcePath, dest))

	// The non-escaping local sibling resolves inside the root package.
	c2, err := offline.LoadModule(t.Context(), "./C", "", root2.SourcePath)
	require.NoError(t, err)
	require.Contains(t, c2.Config.Outputs, "from_c")
	require.True(t, strings.HasPrefix(c2.SourcePath, dest))

	// The escaping ../../B resolves to its bundled package, staying inside the
	// unpack dir rather than walking ".." out of it.
	a2, err := offline.LoadModule(t.Context(), "./A", "", root2.SourcePath)
	require.NoError(t, err)
	b2, err := offline.LoadModule(t.Context(), "../../B", "", a2.SourcePath)
	require.NoError(t, err)
	require.Contains(t, b2.Config.Outputs, "from_b")
	require.True(t, strings.HasPrefix(b2.SourcePath, dest),
		"escaping ../../B must resolve inside the bundle, got %q", b2.SourcePath)
}

func TestPackArchiveDeterministic(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.tf"), []byte("a"), 0o600))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "sub"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "sub", "b.tf"), []byte("b"), 0o600))

	first, err := packArchive(map[string]string{"root": dir})
	require.NoError(t, err)
	second, err := packArchive(map[string]string{"root": dir})
	require.NoError(t, err)
	require.Equal(t, first, second)
}

func TestExcludedFromBundle(t *testing.T) {
	t.Parallel()

	type result struct{ skip, skipDeep bool }
	cases := []struct {
		path string
		want result
	}{
		{"main.tf", result{false, false}},
		{"sub/main.tf", result{false, false}},
		{".gitignore", result{false, false}},      // a file, not the .git dir
		{".gitattributes", result{false, false}},  // likewise
		{"modules/main.tf", result{false, false}}, // "modules" only matters under .terraform
		{".git", result{true, true}},
		{".git/config", result{true, true}},
		{"sub/.git/HEAD", result{true, true}},
		{".terraform", result{true, false}},            // exclude, but descend for modules
		{".terraform/environment", result{true, true}}, // any non-modules .terraform subtree
		{".terraform/providers/x/y", result{true, true}},
		{".terraform/modules", result{false, false}},
		{".terraform/modules/child/main.tf", result{false, false}},
		{".terraform/modules/.git/config", result{true, true}}, // .git wins over the modules exception
	}
	for _, tt := range cases {
		t.Run(tt.path, func(t *testing.T) {
			t.Parallel()
			skip, skipDeep := excludedFromBundle(tt.path)
			require.Equal(t, tt.want, result{skip, skipDeep})
		})
	}
}

// TestPackArchiveExcludesVCSArtifacts proves the bundle drops `.git` trees and
// `.terraform` (except `.terraform/modules`) — the go-slug default ruleset — so a
// git-fetched module's VCS/cache artifacts do not bloat the parameterization
// Value, while real module content is preserved end to end through pack+unpack.
func TestPackArchiveExcludesVCSArtifacts(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	write := func(rel, content string) {
		t.Helper()
		full := filepath.Join(dir, filepath.FromSlash(rel))
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
		require.NoError(t, os.WriteFile(full, []byte(content), 0o600))
	}
	write("main.tf", "root")
	write(".gitignore", "*.tfstate")
	write(".git/config", "[core]")
	write("sub/.git/HEAD", "ref: refs/heads/main")
	write(".terraform/environment", "default")
	write(".terraform/modules/child/main.tf", "child")

	archive, err := packArchive(map[string]string{"": dir})
	require.NoError(t, err)

	dest := t.TempDir()
	require.NoError(t, unpackArchive(t.Context(), archive, dest))

	var got []string
	require.NoError(t, filepath.Walk(dest, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			rel, err := filepath.Rel(dest, p)
			require.NoError(t, err)
			got = append(got, filepath.ToSlash(rel))
		}
		return nil
	}))

	require.ElementsMatch(t, []string{
		"main.tf",
		".gitignore",
		".terraform/modules/child/main.tf",
	}, got)
}

// TestBundleBakesPackages verifies the resolved provider descriptors survive the
// bundle round-trip — including a bridged provider's parameterization — so the
// usage path can reuse them instead of re-resolving, and that the encoding is
// deterministic (the engine hashes the Value for provider identity).
func TestBundleBakesPackages(t *testing.T) {
	t.Parallel()

	awsVer := semver.MustParse("6.46.0")
	tfVer := semver.MustParse("0.9.0")
	packages := map[string]workspace.PackageDescriptor{
		"aws": {
			PluginDescriptor: workspace.PluginDescriptor{
				Name:    "terraform-provider",
				Kind:    apitype.ResourcePlugin,
				Version: &tfVer,
			},
			Parameterization: &workspace.Parameterization{
				Name:    "aws",
				Version: awsVer,
				Value:   []byte(`{"remote":{"url":"registry.opentofu.org/hashicorp/aws","version":"6.46.0"}}`),
			},
		},
	}

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "main.tf"), []byte(`output "x" { value = 1 }`), 0o600))
	rec := newResolveRecorder(func(string, string, string) (string, string, error) { return dir, "", nil })
	loader := modules.NewLoader(rec.resolve)
	_, err := loader.LoadModule(t.Context(), dir, "", ".")
	require.NoError(t, err)

	value, err := rec.bundle(moduleRef{Source: dir}, packages)
	require.NoError(t, err)

	again, err := rec.bundle(moduleRef{Source: dir}, packages)
	require.NoError(t, err)
	require.Equal(t, value, again, "the bundle encoding must be deterministic")

	b, err := decodeBundle(value)
	require.NoError(t, err)
	require.Equal(t, packages, b.Packages)
}

// TestDedupeEdges verifies edges are deduplicated (each reference is recorded by
// both the provider and schema walks) and sorted, so the manifest is stable.
func TestDedupeEdges(t *testing.T) {
	t.Parallel()

	got := dedupeEdges([]resolvedEdge{
		{Caller: "0", Source: "b/m/aws", Target: "2"},
		{Caller: "", Source: "root", Target: "0"},
		{Caller: "0", Source: "a/m/aws", Target: "1"},
		{Caller: "", Source: "root", Target: "0"},     // duplicate
		{Caller: "0", Source: "b/m/aws", Target: "2"}, // duplicate
	})

	require.Equal(t, []resolvedEdge{
		{Caller: "", Source: "root", Target: "0"},
		{Caller: "0", Source: "a/m/aws", Target: "1"},
		{Caller: "0", Source: "b/m/aws", Target: "2"},
	}, got)
}

// nopResolver is a non-nil PackageResolverClient so parameterize gets past its
// handshake check without dialing anything.
type nopResolver struct {
	pulumirpc.PackageResolverClient
}

func TestParameterizeArgsStatusCodes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		args []string
		want codes.Code
	}{
		{"missing module keyword", []string{"not-module"}, codes.InvalidArgument},
		{"no args", nil, codes.InvalidArgument},
		{"too many args", []string{"module", "a", "b", "c"}, codes.InvalidArgument},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m := &moduleProvider{resolver: nopResolver{}}
			_, err := m.parameterize(t.Context(), p.ParameterizeRequest{
				Args: &p.ParameterizeRequestArgs{Args: tc.args},
			})
			require.Equal(t, tc.want, status.Code(err))
		})
	}
}

func TestParameterizeBeforeHandshakeIsFailedPrecondition(t *testing.T) {
	t.Parallel()
	m := &moduleProvider{}
	_, err := m.parameterize(t.Context(), p.ParameterizeRequest{
		Args: &p.ParameterizeRequestArgs{Args: []string{"module", "acme/thing/aws"}},
	})
	require.Equal(t, codes.FailedPrecondition, status.Code(err))
}
