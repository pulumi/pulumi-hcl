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
	"archive/tar"
	"bytes"
	"cmp"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/blang/semver"
	regaddr "github.com/opentofu/registry-address/v2"
	p "github.com/pulumi/pulumi-go-provider"
	pulumiSchema "github.com/pulumi/pulumi/pkg/v3/codegen/schema"
	"github.com/pulumi/pulumi/sdk/v3/go/common/tokens"
	"github.com/pulumi/pulumi/sdk/v3/go/common/util/contract"
	"github.com/pulumi/pulumi/sdk/v3/go/common/workspace"
	"github.com/spf13/cobra"
	"google.golang.org/grpc/codes"

	"github.com/pulumi/pulumi-hcl/pkg/grpcerr"
	"github.com/pulumi/pulumi-hcl/pkg/hcl/ast"
	"github.com/pulumi/pulumi-hcl/pkg/hcl/modules"
	"github.com/pulumi/pulumi-hcl/pkg/hcl/packages"
	"github.com/pulumi/pulumi-hcl/pkg/hcl/resolve"
	"github.com/pulumi/pulumi-hcl/pkg/hcl/schema"
	"github.com/pulumi/pulumi-hcl/pkg/potel"
	"github.com/pulumi/pulumi-hcl/vendored/getmodules"
)

// parameterizedModule is the state of a moduleProvider that has been
// parameterized by a specific module source. While set, the provider serves that
// module package's typed component schemas rather than the generic
// hcl:index:Module.
type parameterizedModule struct {
	// spec is the combined package schema of every component.
	spec pulumiSchema.PackageSpec
	// components maps each component token to the schema and source Construct
	// dispatches on.
	components map[tokens.Type]component
	// loader resolves the components and their children. On the usage path it
	// resolves entirely from the unpacked bundle, with no network access.
	loader *modules.Loader
	// versionConstraint is the constraint component sources are loaded under
	// through loader, as passed to `pulumi package add`.
	versionConstraint string
	// value is the parameterization Value (the encoded bundle), surfaced in the
	// schema so a generated SDK can re-parameterize without re-downloading.
	value []byte
	name  string
	// tempDir is the directory the bundle was unpacked into, removed on Cancel.
	// Empty on the args (CLI) path, which reads the freshly-downloaded module.
	tempDir string
}

// bundle is the parameterization Value: a self-contained, offline snapshot of a
// module tree.
type bundle struct {
	// Manifest records how every module reference resolves within Archive, and
	// how to reach the root module.
	Manifest bundleManifest `json:"manifest"`
	// Packages are each component's resolved provider descriptors, keyed by the
	// component's root-relative directory ("" for the root) and then by local
	// provider name, computed once at `package add` time.
	Packages map[string]map[string]workspace.PackageDescriptor `json:"packages"`
	// Archive is the deterministic tar.gz of the module package directories.
	Archive []byte `json:"archive"`
}

// encodeBundle serialises a bundle to the parameterization Value: gzipped JSON,
// so the base64-encoded copy the engine bakes into schema.json stays small.
func (b bundle) encode() []byte {
	data, err := json.Marshal(b)
	contract.AssertNoErrorf(err, "bundle is always serializable")
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	_, err = gz.Write(data)
	contract.AssertNoErrorf(err, "gzipping bundle is always possible")
	contract.AssertNoErrorf(gz.Close(), "closing gzip writer is always possible")
	return buf.Bytes()
}

// decodeBundle parses a parameterization Value produced by encode.
func decodeBundle(value []byte) (bundle, error) {
	gz, err := gzip.NewReader(bytes.NewReader(value))
	if err != nil {
		return bundle{}, grpcerr.Errorf(codes.InvalidArgument, "decompressing bundle: %w", err)
	}
	defer func() { _ = gz.Close() }()
	data, err := io.ReadAll(gz)
	if err != nil {
		return bundle{}, grpcerr.Errorf(codes.InvalidArgument, "decompressing bundle: %w", err)
	}
	var b bundle
	if err := json.Unmarshal(data, &b); err != nil {
		return bundle{}, grpcerr.Errorf(codes.InvalidArgument, "decoding bundle: %w", err)
	}
	return b, nil
}

// bundleManifest records how to resolve every module reference in a bundled tree
// with no network access, and how to reach the root module.
type bundleManifest struct {
	// Root addresses the root module, as passed to `pulumi package add`.
	Root moduleRef `json:"root"`
	// Edges record, for every resolved module reference, the archive directory it
	// resolves to.
	Edges []resolvedEdge `json:"edges"`
}

// moduleRef is a module source and version constraint as written in a
// configuration (or on the CLI).
type moduleRef struct {
	Source  string `json:"source"`
	Version string `json:"version,omitempty"`
}

// resolvedEdge records that a module reference — the (Source, VersionConstraint)
// requested from the module at archive-relative directory Caller ("" for the
// root) — resolves to the archive-relative package directory Target.
type resolvedEdge struct {
	Caller            string `json:"caller"`
	Source            string `json:"source"`
	VersionConstraint string `json:"version,omitempty"`
	Target            string `json:"target"`
}

// parameterize configures the provider for a specific module source. Args is the
// CLI path (`pulumi package add hcl module <source> [version] [--name <name>]`):
// it downloads the module tree and bundles it. Value is the usage path: it
// unpacks a previously bundled tree and resolves everything from it, reusing the
// package name the CLI path chose.
func (m *moduleProvider) parameterize(ctx context.Context, req p.ParameterizeRequest) (p.ParameterizeResponse, error) {
	switch {
	case req.Args != nil:
		return m.parameterizeArgs(ctx, req.Args.Args)
	case req.Value != nil:
		return m.parameterizeValue(ctx, req.Value.Name, req.Value.Version, req.Value.Value)
	default:
		return p.ParameterizeResponse{}, grpcerr.Errorf(codes.InvalidArgument,
			"parameterize requires either arguments or a value")
	}
}

// parameterizeArgs handles `pulumi package add hcl module <source> [version]
// [--name <name>]`. It downloads the module, generates its schema, and —
// recording every resolution along the way — bundles the whole module tree into
// the parameterization Value.
func (m *moduleProvider) parameterizeArgs(ctx context.Context, args []string) (p.ParameterizeResponse, error) {
	if m.resolver == nil {
		return p.ParameterizeResponse{}, grpcerr.Errorf(codes.FailedPrecondition,
			"parameterize called before a successful handshake")
	}

	// The fixed "module" keyword leaves room for the provider to grow other
	// parameterization kinds; today a module source is the only one.
	if len(args) == 0 || args[0] != "module" {
		return p.ParameterizeResponse{}, grpcerr.Errorf(codes.InvalidArgument,
			`the hcl provider is parameterized by a module: expected "module" as the first argument`)
	}

	cmd := &cobra.Command{Version: m.version, SilenceUsage: true, SilenceErrors: true}
	cmd.SetArgs(args)
	cmd.SetContext(ctx)

	module := &cobra.Command{Use: "module <source> [version]", SilenceUsage: true, SilenceErrors: true}
	var source, version, nameOverride string
	module.Flags().StringVar(&nameOverride, "name", "", "override the package name")
	module.Run = func(cmd *cobra.Command, args []string) {
		source = args[0]
		if len(args) >= 2 {
			version = args[1]
		}
	}
	module.Args = cobra.RangeArgs(1, 2)
	cmd.AddCommand(module)
	if err := cmd.Execute(); err != nil {
		return p.ParameterizeResponse{}, grpcerr.Errorf(codes.InvalidArgument,
			`the hcl provider is parameterized as "module <source> [version] [--name <name>]": %v`, err)
	}

	// A recording loader resolves sources live while capturing where each one
	// landed, so the schema-generation and provider walks below populate a
	// complete manifest as a side effect.
	rec := newResolveRecorder(modules.LiveResolver(ctx))
	loader := modules.NewLoader(rec.resolve)

	comps, resolvedVersion, err := loadComponents(ctx, loader, source, version)
	if err != nil {
		return p.ParameterizeResponse{}, fmt.Errorf("loading module %q: %w", source, err)
	}

	// Resolve each component's providers now — independently, as the separate
	// configurations they are — while the recording loader is active, so the
	// descriptors can be baked into the bundle and reused offline at usage.
	for i := range comps {
		comps[i].packages, err = m.resolvePackages(ctx, loader, comps[i].loaded.Config, comps[i].loaded.SourcePath)
		if err != nil {
			return p.ParameterizeResponse{}, fmt.Errorf("%s: %w", comps[i].dir.describe(), err)
		}
	}

	// finishParameterize's schema walk drives the recorder too; bundle the
	// recorded tree once it has run.
	return m.finishParameterize(ctx, loader, comps, resolvedVersion, source, version, nameOverride, "", nil,
		func() ([]byte, error) {
			value, err := rec.bundle(moduleRef{Source: source, Version: version}, componentPackages(comps))
			if err != nil {
				return nil, fmt.Errorf("bundling module %q: %w", source, err)
			}
			return value, nil
		})
}

// componentPackages collects each component's resolved descriptors, keyed by
// the component's root-relative directory ("" for the root).
func componentPackages(comps []loadedComponent) map[string]map[string]workspace.PackageDescriptor {
	out := make(map[string]map[string]workspace.PackageDescriptor, len(comps))
	for _, c := range comps {
		out[c.dir.Subdir] = c.packages
	}
	return out
}

// parameterizeValue handles re-parameterization from a generated SDK. It unpacks
// the bundled module files, builds a loader that resolves every source from the
// bundle's manifest with no network access, and reuses the bundle's baked
// provider descriptors rather than re-resolving them through the engine. name
// and version are the identity the args path originally returned, echoed back
// by the engine; both are reused as-is.
func (m *moduleProvider) parameterizeValue(
	ctx context.Context, name string, version semver.Version, value []byte,
) (p.ParameterizeResponse, error) {
	if m.resolver == nil {
		return p.ParameterizeResponse{}, grpcerr.Errorf(codes.FailedPrecondition,
			"parameterize called before a successful handshake")
	}

	b, err := decodeBundle(value)
	if err != nil {
		return p.ParameterizeResponse{}, err
	}

	dir, err := os.MkdirTemp("", "pulumi-hcl-module-")
	if err != nil {
		return p.ParameterizeResponse{}, fmt.Errorf("creating unpack directory: %w", err)
	}
	// On success the directory is owned by m.param and removed on Cancel; until
	// then, clean it up if any step below fails.
	keep := false
	defer func() {
		if !keep {
			_ = os.RemoveAll(dir)
		}
	}()

	err = unpackArchive(ctx, b.Archive, dir)
	if err != nil {
		return p.ParameterizeResponse{}, fmt.Errorf("unpacking module bundle: %w", err)
	}

	loader := modules.NewLoader(bundleResolver(dir, b.Manifest))
	comps, err := loadRecordedComponents(ctx, loader, b.Manifest.Root, b.Packages)
	if err != nil {
		return p.ParameterizeResponse{}, fmt.Errorf("loading bundled module %q: %w", b.Manifest.Root.Source, err)
	}

	resp, err := m.finishParameterize(ctx, loader, comps, "", b.Manifest.Root.Source,
		b.Manifest.Root.Version, name, dir, &version, func() ([]byte, error) { return value, nil })
	if err != nil {
		return p.ParameterizeResponse{}, err
	}
	keep = true
	return resp, nil
}

// finishParameterize derives the package identity, generates every component's
// schema, and installs the parameterized state. A non-nil pkgVersion (the usage
// path, where the engine echoes the version the args path returned) overrides
// the version derived from the package. resolvedVersion is the concrete version
// the package's source resolved to, naming the package version when there is no
// root module to carry a terraform `package` block.
func (m *moduleProvider) finishParameterize(
	ctx context.Context, loader *modules.Loader, comps []loadedComponent, resolvedVersion,
	rootSource, versionConstraint, pkgName, tempDir string, pkgVersion *semver.Version,
	makeValue func() ([]byte, error),
) (p.ParameterizeResponse, error) {
	forceDefault := pkgName != ""
	if pkgName == "" {
		pkgName = defaultPackageName(rootSource)
	}
	pkgName, rootToken, pkgVer, err := packageIdentity(comps, pkgName, forceDefault, resolvedVersion)
	if err != nil {
		return p.ParameterizeResponse{}, err
	}
	if pkgVersion != nil {
		pkgVer = *pkgVersion
	}

	components, spec, err := buildComponents(ctx, comps, pkgName, rootToken, pkgVer,
		m.componentBinderFactory(loader))
	if err != nil {
		// A module that loaded but cannot be typed uses something the
		// conversion does not support, unless the chain pins a more specific
		// cause (a registry or network failure resolving a child module).
		return p.ParameterizeResponse{}, grpcerr.Classify(err, codes.Unimplemented)
	}

	value, err := makeValue()
	if err != nil {
		return p.ParameterizeResponse{}, err
	}

	m.param = &parameterizedModule{
		spec:              spec,
		components:        components,
		loader:            loader,
		versionConstraint: versionConstraint,
		value:             value,
		name:              pkgName,
		tempDir:           tempDir,
	}
	return p.ParameterizeResponse{Name: pkgName, Version: pkgVer}, nil
}

// resolvePackages resolves every provider the module tree rooted at config
// references to a concrete descriptor, walking child modules through loader.
func (m *moduleProvider) resolvePackages(
	ctx context.Context, loader *modules.Loader, config *ast.Config, workDir string,
) (map[string]workspace.PackageDescriptor, error) {
	ctx, span := potel.Start(ctx, "resolvePackages")
	defer span.End()
	specs, err := RequirementSpecs(ctx, loader, config, workDir)
	if err != nil {
		return nil, err
	}
	resolved, err := resolve.Packages(ctx, m.resolver, specs)
	if err != nil {
		return nil, fmt.Errorf("resolving module providers: %w", err)
	}
	return resolved, nil
}

// componentBinderFactory builds the per-component binder schema generation
// resolves resource, data source, and child-module references through.
func (m *moduleProvider) componentBinderFactory(
	loader *modules.Loader,
) func(loadedComponent) *schema.Binder {
	return func(c loadedComponent) *schema.Binder {
		cachedLoader := pulumiSchema.NewCachedLoader(
			packages.NewParameterizationAwareLoader(m.schemaLoader, c.packages))
		return &schema.Binder{
			Resources: packages.NewResolver(cachedLoader, m.providerInfoSource, c.packages, knownProviderNames(c.loaded.Config)),
			Modules:   moduleLoaderAdapter{loader},
			ModuleDir: c.loaded.SourcePath,
		}
	}
}

// knownProviderNames lists the local names the module's terraform block declares,
// so resource/data type tokens split on the declared provider rather than the
// first underscore.
func knownProviderNames(config *ast.Config) []string {
	if config.Terraform == nil {
		return nil
	}
	names := make([]string, 0, len(config.Terraform.RequiredProviders))
	for name := range config.Terraform.RequiredProviders {
		names = append(names, name)
	}
	return names
}

// resolveRecorder wraps a resolver, recording where every module source resolves
// so the tree can be archived and replayed offline. Each distinct package
// directory is assigned a numeric archive-relative path; a source resolving
// inside an already-seen package (a local reference) reuses that package's path.
type resolveRecorder struct {
	inner  modules.ResolverFunc
	pkgRel map[string]string // absolute package directory -> archive-relative path
	edges  []resolvedEdge
}

func newResolveRecorder(inner modules.ResolverFunc) *resolveRecorder {
	return &resolveRecorder{inner: inner, pkgRel: map[string]string{}}
}

// resolve resolves a source through the wrapped resolver and records the edge.
// The loader serializes calls under its lock, so the recorder needs none.
func (r *resolveRecorder) resolve(packageSource, versionConstraint, callerDir string) (string, string, error) {
	target, resolved, err := r.inner(packageSource, versionConstraint, callerDir)
	if err != nil {
		return "", "", err
	}
	r.edges = append(r.edges, resolvedEdge{
		Caller:            r.rel(callerDir),
		Source:            packageSource,
		VersionConstraint: versionConstraint,
		Target:            r.rel(target),
	})
	return target, resolved, nil
}

// rel returns the archive-relative path for an absolute module directory. The
// root caller (".") maps to "". A directory inside an already-seen package
// reuses that package's path with the remaining sub-path appended; any other
// directory starts a new package.
func (r *resolveRecorder) rel(dir string) string {
	if dir == "." || dir == "" {
		return ""
	}
	for pkg, rel := range r.pkgRel {
		if dir == pkg {
			return rel
		}
		if strings.HasPrefix(dir, pkg+string(os.PathSeparator)) {
			sub, _ := filepath.Rel(pkg, dir)
			return path.Join(rel, filepath.ToSlash(sub))
		}
	}
	rel := strconv.Itoa(len(r.pkgRel))
	r.pkgRel[dir] = rel
	return rel
}

func (r *resolveRecorder) bundle(
	root moduleRef, packages map[string]map[string]workspace.PackageDescriptor,
) ([]byte, error) {
	dirs := make(map[string]string, len(r.pkgRel))
	for absDir, rel := range r.pkgRel {
		dirs[rel] = absDir
	}
	archive, err := packArchive(dirs)
	if err != nil {
		return nil, err
	}
	return bundle{
		Manifest: bundleManifest{Root: root, Edges: dedupeEdges(r.edges)},
		Packages: packages,
		Archive:  archive,
	}.encode(), nil
}

// dedupeEdges drops duplicate edges — each reference is recorded once per walk
// that loads it (provider resolution, schema generation) — and sorts the result,
// so the manifest is stable regardless of how often a module was loaded.
func dedupeEdges(edges []resolvedEdge) []resolvedEdge {
	seen := make(map[resolvedEdge]struct{}, len(edges))
	out := make([]resolvedEdge, 0, len(edges))
	for _, e := range edges {
		if _, ok := seen[e]; ok {
			continue
		}
		seen[e] = struct{}{}
		out = append(out, e)
	}
	slices.SortFunc(out, func(a, b resolvedEdge) int {
		return cmp.Or(
			cmp.Compare(a.Caller, b.Caller),
			cmp.Compare(a.Source, b.Source),
			cmp.Compare(a.VersionConstraint, b.VersionConstraint),
			cmp.Compare(a.Target, b.Target),
		)
	})
	return out
}

// bundleResolver resolves module sources from an unpacked bundle's manifest, with
// no network access. callerDir is mapped back to its archive-relative form so the
// recorded edge matches regardless of where the bundle was unpacked. It reports
// no resolved version: on the usage path the package version comes from the
// engine's ParameterizeRequest, not from re-resolution.
func bundleResolver(unpackDir string, manifest bundleManifest) modules.ResolverFunc {
	type edgeKey struct{ caller, source, version string }
	index := make(map[edgeKey]string, len(manifest.Edges))
	for _, e := range manifest.Edges {
		index[edgeKey{e.Caller, e.Source, e.VersionConstraint}] = e.Target
	}
	return func(packageSource, versionConstraint, callerDir string) (string, string, error) {
		caller := ""
		if callerDir != "." && callerDir != "" {
			rel, err := filepath.Rel(unpackDir, callerDir)
			if err != nil {
				return "", "", err
			}
			caller = filepath.ToSlash(rel)
		}
		target, ok := index[edgeKey{caller, packageSource, versionConstraint}]
		if !ok {
			return "", "", fmt.Errorf("module source %q is not present in the parameterization bundle", packageSource)
		}
		return filepath.Join(unpackDir, filepath.FromSlash(target)), "", nil
	}
}

// defaultPackageName derives a Pulumi package name from a module source, used
// when the module declares no terraform `package` block. Registry sources use
// their module name segment; other sources use the last path component.
func defaultPackageName(source string) string {
	pkgSource, _ := getmodules.SplitPackageSubdir(source)
	if i := strings.IndexByte(pkgSource, '?'); i >= 0 {
		pkgSource = pkgSource[:i]
	}
	if mod, err := regaddr.ParseModuleSource(pkgSource); err == nil {
		pkg := sanitizePackageName(mod.Package.Name, "module")
		if sys := sanitizePackageName(mod.Package.TargetSystem, ""); sys != "" {
			pkg += "-" + sys
		}
		return pkg
	}
	s := strings.TrimRight(pkgSource, "/")
	if i := strings.LastIndexByte(s, '/'); i >= 0 {
		s = s[i+1:]
	}
	s = strings.TrimSuffix(s, ".git")
	return sanitizePackageName(s, "module")
}

// sanitizePackageName reduces a name to the lowercase alphanumeric-and-hyphen
// form Pulumi package names use, falling back to "module" when nothing remains.
func sanitizePackageName(name, fallback string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			b.WriteRune(r)
		case r == '_' || r == ' ' || r == '.':
			b.WriteByte('-')
		}
	}
	if out := strings.Trim(b.String(), "-"); out != "" {
		return out
	}
	return fallback
}

// bundleEpoch is the fixed modification time stamped on every archive entry, so
// the archive bytes — and thus the parameterization Value the engine hashes for
// provider identity — are deterministic.
var bundleEpoch = time.Unix(0, 0).UTC()

// excludedFromBundle reports whether an archive-relative path should be left out
// of the bundle (skip), and whether its whole subtree can be pruned (skipDeep).
func excludedFromBundle(rel string) (skip, skipDeep bool) {
	// It mirrors go-slug's default `.terraformignore` ruleset that Terraform applies
	// when it packages a configuration for transmission.

	parts := strings.Split(filepath.ToSlash(rel), "/")
	// A `.git` anywhere wins, even under `.terraform/modules` (go-slug applies
	// the `.git` rule last, so it overrides the modules exception).
	if slices.Contains(parts, ".git") {
		return true, true
	}
	for i, part := range parts {
		if part != ".terraform" {
			continue
		}
		switch {
		case i+1 >= len(parts):
			// The `.terraform` directory itself: exclude it, but descend so a
			// `.terraform/modules` child below can still be kept.
			return true, false
		case parts[i+1] == "modules":
			return false, false
		default:
			return true, true
		}
	}
	return false, false
}

// packArchive packs each (archive-path → directory) tree into a deterministic
// tar.gz: entries are sorted, modification times fixed, and only regular files
// and directories are included.
func packArchive(dirs map[string]string) ([]byte, error) {
	type entry struct {
		name    string
		absPath string
		info    os.FileInfo
	}
	var files []entry
	for relPath, absDir := range dirs {
		err := filepath.Walk(absDir, func(p string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(absDir, p)
			if err != nil {
				return err
			}
			if skip, skipDeep := excludedFromBundle(rel); skip {
				if info.IsDir() && skipDeep {
					return filepath.SkipDir
				}
				return nil
			}
			files = append(files, entry{name: path.Join(relPath, filepath.ToSlash(rel)), absPath: p, info: info})
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	slices.SortFunc(files, func(a, b entry) int { return cmp.Compare(a.name, b.name) })

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, f := range files {
		hdr := &tar.Header{Name: f.name, ModTime: bundleEpoch}
		var data []byte
		switch {
		case f.info.IsDir():
			hdr.Typeflag = tar.TypeDir
			hdr.Mode = 0o755
			hdr.Name += "/"
		case !f.info.Mode().IsRegular():
			continue // skip symlinks, devices, and other irregular entries
		default:
			hdr.Typeflag = tar.TypeReg
			hdr.Mode = 0o644
			b, err := os.ReadFile(f.absPath)
			if err != nil {
				return nil, err
			}
			data = b
			hdr.Size = int64(len(data))
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return nil, err
		}
		if hdr.Typeflag == tar.TypeReg {
			if _, err := tw.Write(data); err != nil {
				return nil, err
			}
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// unpackArchive extracts a tar.gz produced by packArchive into destDir, rejecting
// any entry that would escape destDir.
func unpackArchive(ctx context.Context, data []byte, destDir string) error {
	_, span := potel.Start(ctx, "unpackArchive")
	defer span.End()
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return err
	}
	defer func() { _ = gz.Close() }()

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		target, err := safeJoin(destDir, hdr.Name)
		if err != nil {
			return err
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
			if err != nil {
				return err
			}
			if _, err := io.Copy(out, tr); err != nil { //nolint:gosec // archive is self-produced
				_ = out.Close()
				return err
			}
			if err := out.Close(); err != nil {
				return err
			}
		}
	}
	return nil
}

// safeJoin joins name onto base, returning an error if the result escapes base.
func safeJoin(base, name string) (string, error) {
	target := filepath.Join(base, filepath.FromSlash(name))
	if target != base && !strings.HasPrefix(target, base+string(os.PathSeparator)) {
		return "", fmt.Errorf("archive entry %q escapes the unpack directory", name)
	}
	return target, nil
}
