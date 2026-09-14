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

// Package modules loads and parses Terraform-compatible HCL module
// configurations. Remote source resolution (git, http, registry, etc.) is
// delegated to the vendored opentofu getmodules package which wraps
// hashicorp/go-getter.
package modules

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	version "github.com/hashicorp/go-version"
	regaddr "github.com/opentofu/registry-address/v2"
	"github.com/opentofu/svchost"
	"github.com/opentofu/svchost/disco"
	"github.com/opentofu/svchost/svcauth"
	"github.com/pulumi/pulumi-hcl/pkg/hcl/ast"
	"github.com/pulumi/pulumi-hcl/pkg/hcl/parser"
	"github.com/pulumi/pulumi-hcl/pkg/potel"
	"github.com/pulumi/pulumi-hcl/vendored/getmodules"
	"github.com/pulumi/pulumi/sdk/v3/go/common/util/contract"
	"github.com/pulumi/pulumi/sdk/v3/go/common/util/fsutil"
	"github.com/pulumi/pulumi/sdk/v3/go/common/workspace"
)

// fetchMu serializes every PackageFetcher.FetchPackage call in the process.
// The vendored getmodules package builds each PackageFetcher from a shallow
// clone of a package-global getter table, so distinct fetchers — and thus
// distinct Loaders — still share the same go-getter getter instances.
// go-getter mutates those getters on every fetch, which upstream OpenTofu
// only gets away with because it installs modules sequentially. We hold this
// lock to enforce that same single-flight contract.
//
// fetchMu guards process memory only. The per-cacheDir file lock in
// fetchRemote guards the cache directory across processes, and is always
// taken before fetchMu.
var fetchMu sync.Mutex

// Loader loads and parses module configurations.
type Loader struct {
	mu sync.Mutex

	parser *parser.Parser
	cache  map[string]*LoadedModule

	resolve ResolverFunc
	// resolved caches successful package resolutions, so one package resolves
	// (and, for registry sources, selects a version) once per Loader no matter
	// how many of its subdirectories are loaded.
	resolved map[resolveKey]resolvedPackage
}

type resolveKey struct {
	packageSource     string
	versionConstraint string
	callerDir         string
}

type resolvedPackage struct {
	dir     string
	version string
}

// LoadedModule represents a loaded and parsed module.
type LoadedModule struct {
	Config     *ast.Config
	SourcePath string
	Version    string
}

type ResolverFunc = func(packageSource, versionConstraint, callerDir string) (dir, version string, err error)

// NewLoader creates a module loader that resolves sources from the filesystem,
// the registry, and remote getters, downloading and caching as needed.
func NewLoader(resolver ResolverFunc) *Loader {
	return &Loader{
		parser:   parser.NewParser(),
		cache:    make(map[string]*LoadedModule),
		resolve:  resolver,
		resolved: make(map[resolveKey]resolvedPackage),
	}
}

// LiveResolver returns the default resolution strategy: filesystem sources
// resolve directly, registry sources via the modules.v1 protocol, and everything
// else through the remote getter, downloading and caching under PULUMI_HOME.
func LiveResolver(ctx context.Context) ResolverFunc {
	cloud := newCloudRegistryCredentials(os.Getenv("PULUMI_API"), os.Getenv("PULUMI_ACCESS_TOKEN"))
	cfg := loadTerraformCLIConfig()

	// Precedence matches OpenTofu: an explicit TF_TOKEN_<host> or a `credentials`
	// block wins over the auto-injected Pulumi Cloud token, which only ever
	// matches the discovered cloud host anyway.
	d := disco.New(disco.WithCredentials(svcauth.Credentials{
		tfTokenCredentials(),
		cliConfigCredentials(cfg),
		cloud,
	}))
	applyHostServiceOverrides(d, cfg)

	n := &networkResolver{
		cacheDir: defaultCacheDir(),
		fetcher:  getmodules.NewPackageFetcher(ctx, nil),
		disco:    d,
	}
	return n.resolve
}

func (n *networkResolver) resolve(packageSource, versionConstraint, callerDir string) (string, string, error) {
	switch {
	case strings.HasPrefix(packageSource, "./") || strings.HasPrefix(packageSource, "../"):
		resolved := filepath.Join(callerDir, packageSource)
		absPath, err := filepath.Abs(resolved)
		if err != nil {
			return "", "", fmt.Errorf("resolving path: %w", err)
		}
		dir, err := statDir(absPath)
		return dir, "", err
	case filepath.IsAbs(packageSource):
		dir, err := statDir(packageSource)
		return dir, "", err
	case isRegistrySource(packageSource):
		return n.resolveRegistrySource(packageSource, versionConstraint)
	default:
		// Anything else (git::, github.com/..., bitbucket.org/..., http(s)://,
		// s3::, gcs::, hg::, etc.) goes through the package fetcher, which
		// runs the upstream opentofu detectors to normalize the address.
		dir, err := n.fetchRemote(packageSource, "remote")
		return dir, "", err
	}
}

func defaultCacheDir() string {
	// Resolve "modules" under the Pulumi home the same way plugins resolve under
	// it (workspace.GetPluginDir uses GetPulumiPath), so PULUMI_HOME relocates the
	// module cache too.
	dir, err := workspace.GetPulumiPath("modules")
	if err != nil {
		return filepath.Join(os.TempDir(), "modules")
	}
	return dir
}

// LoadModule loads a module from the given source. versionConstraint is a
// Terraform-style constraint (`~> 4.0`, `>= 1.0.0`, `4.2.1`); ignored for
// non-registry sources.
func (l *Loader) LoadModule(ctx context.Context, source, versionConstraint, callerDir string) (*LoadedModule, error) {
	_, loadSpan := potel.Start(ctx, "modules.LoadModule")
	defer loadSpan.End()

	l.mu.Lock()
	defer l.mu.Unlock()

	resolvedPath, resolvedVersion, err := l.resolveSource(source, versionConstraint, callerDir)
	if err != nil {
		return nil, fmt.Errorf("resolving module source %q: %w", source, err)
	}

	if cached, ok := l.cache[resolvedPath]; ok {
		return cached, nil
	}

	config, diags := l.parser.ParseDirectory(resolvedPath)
	if diags.HasErrors() {
		return nil, classified(ErrInvalid, fmt.Errorf("parsing module: %s", diags.Error()))
	}

	module := &LoadedModule{Config: config, SourcePath: resolvedPath, Version: resolvedVersion}
	l.cache[resolvedPath] = module
	return module, nil
}

// ResolveDir resolves a module source to its directory on disk — downloading it
// if necessary — without parsing it, plus the concrete version a registry source
// resolved to.
func (l *Loader) ResolveDir(source, versionConstraint, callerDir string) (string, string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.resolveSource(source, versionConstraint, callerDir)
}

// resolveSource resolves a module source to an absolute path on disk, plus the
// concrete version a registry source resolved to. versionConstraint applies
// only to registry sources.
func (l *Loader) resolveSource(source, versionConstraint, callerDir string) (string, string, error) {
	packageSource, subdir := getmodules.SplitPackageSubdir(source)
	key := resolveKey{packageSource, versionConstraint, callerDir}
	pkg, ok := l.resolved[key]
	if !ok {
		packageDir, version, err := l.resolve(packageSource, versionConstraint, callerDir)
		if err != nil {
			return "", "", err
		}
		pkg = resolvedPackage{dir: packageDir, version: version}
		l.resolved[key] = pkg
	}
	packageDir, version := pkg.dir, pkg.version

	if subdir != "" {
		resolved := filepath.Join(packageDir, subdir)
		if info, statErr := os.Stat(resolved); statErr != nil || !info.IsDir() {
			return "", "", classified(ErrNotFound, fmt.Errorf("subdir %q does not exist in module", subdir))
		}
		return resolved, version, nil
	}
	return packageDir, version, nil
}

// SourceName is the name a module source declares: the subdirectory it selects,
// the registry module name, or the last element of the path. It names a module
// independently of where that module happens to be resolved on disk, which for a
// bundled module is a numbered directory carrying no name at all.
func SourceName(source string) string {
	packageSource, subdir := getmodules.SplitPackageSubdir(source)
	if subdir != "" {
		return path.Base(subdir)
	}
	if mod, err := regaddr.ParseModuleSource(packageSource); err == nil {
		return mod.Package.Name
	}
	base, _, _ := strings.Cut(packageSource, "?")
	return strings.TrimSuffix(path.Base(strings.TrimRight(base, "/")), ".git")
}

func statDir(p string) (string, error) {
	info, err := os.Stat(p)
	if err != nil {
		if os.IsNotExist(err) {
			return "", classified(ErrNotFound, fmt.Errorf("module directory does not exist: %s", p))
		}
		return "", fmt.Errorf("accessing module directory: %w", err)
	}
	if !info.IsDir() {
		return "", classified(ErrInvalid, fmt.Errorf("module source is not a directory: %s", p))
	}
	return p, nil
}

// fetchRemote normalizes source and downloads it (if not cached) into
// cacheDir/kind/<hash>. Returns the populated cache directory.
func (l *networkResolver) fetchRemote(source, kind string) (string, error) {
	pkgAddr, detectedSubdir, err := getmodules.NormalizePackageAddress(source)
	if err != nil {
		return "", classified(ErrInvalid, fmt.Errorf("normalizing module source %q: %w", source, err))
	}
	if detectedSubdir != "" {
		return "", fmt.Errorf("module source %q resolved to package %q with unexpected subdir %q",
			source, pkgAddr, detectedSubdir)
	}

	cacheDir := filepath.Join(l.cacheDir, kind, hashSource(pkgAddr))
	if err := os.MkdirAll(filepath.Dir(cacheDir), 0o755); err != nil {
		return "", fmt.Errorf("creating cache parent directory: %w", err)
	}

	// `pulumi install` runs one provider process per package, so sibling
	// subdirectories of one repository fetch into this cacheDir from separate
	// processes. The file lock serializes them, and the second one takes the
	// cache-hit path below.
	mu := fsutil.NewFileMutex(cacheDir + ".lock")
	if err := mu.Lock(); err != nil {
		return "", fmt.Errorf("locking module cache %s: %w", cacheDir, err)
	}
	defer func() { contract.IgnoreError(mu.Unlock()) }()

	if dirHasFiles(cacheDir) {
		return cacheDir, nil
	}
	// An empty cacheDir is a leftover from a failed fetch, and go-getter
	// refuses to clone into an existing directory.
	if err := os.RemoveAll(cacheDir); err != nil {
		return "", fmt.Errorf("clearing stale cache dir %s: %w", cacheDir, err)
	}

	// Fetch into a sibling and rename it into place, so a fetch that dies
	// part-way never leaves a cacheDir that a later run mistakes for a hit.
	partial := cacheDir + ".partial"
	if err := os.RemoveAll(partial); err != nil {
		return "", fmt.Errorf("clearing partial fetch %s: %w", partial, err)
	}
	fetchMu.Lock()
	err = l.fetcher.FetchPackage(context.Background(), partial, pkgAddr)
	fetchMu.Unlock()
	if err != nil {
		contract.IgnoreError(os.RemoveAll(partial))
		return "", fmt.Errorf("fetching module from %q: %w", pkgAddr, err)
	}
	if err := os.Rename(partial, cacheDir); err != nil {
		return "", fmt.Errorf("publishing module cache %s: %w", cacheDir, err)
	}
	return cacheDir, nil
}

// dirHasFiles reports whether dir contains any entries.
func dirHasFiles(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	return len(entries) > 0
}

type networkResolver struct {
	cacheDir string
	fetcher  *getmodules.PackageFetcher
	disco    *disco.Disco
}

// resolveRegistrySource downloads a module via modules.v1, returning the
// directory and the concrete version selected. source is
// `[host/]namespace/name/provider` with an optional ?version=…; versionConstraint
// takes precedence over the query string.
func (l *networkResolver) resolveRegistrySource(source, versionConstraint string) (string, string, error) {
	baseSource := source
	if before, query, ok := strings.Cut(source, "?"); ok {
		baseSource = before
		for param := range strings.SplitSeq(query, "&") {
			if after, ok := strings.CutPrefix(param, "version="); ok && versionConstraint == "" {
				versionConstraint = after
			}
		}
	}

	mod, err := regaddr.ParseModuleSource(baseSource)
	if err != nil {
		return "", "", classified(ErrInvalid, fmt.Errorf("invalid registry source format %q: %w", source, err))
	}
	pkg := mod.Package

	baseURL, err := l.registryBaseURLForHost(pkg.Host)
	if err != nil {
		return "", "", err
	}

	downloadURL, chosen, err := l.getRegistryDownloadURL(pkg.Host, baseURL, pkg.Namespace, pkg.Name, pkg.TargetSystem, versionConstraint)
	if err != nil {
		return "", "", err
	}

	// Cache by the resolved URL so different version constraints don't collide.
	dir, err := l.fetchRemote(downloadURL, "registry")
	return dir, chosen, err
}

// registryBaseURLForHost resolves a registry host to its modules.v1 base URL
// via service discovery.
func (l *networkResolver) registryBaseURLForHost(host svchost.Hostname) (string, error) {
	u, err := l.disco.DiscoverServiceURL(context.Background(), host, "modules.v1")
	if err != nil {
		err = fmt.Errorf("discovering registry %q: %w", host, err)
		var notProvided *disco.ErrServiceNotProvided
		switch {
		case errors.As(err, &notProvided):
			return "", classified(ErrNotFound, err)
		case errors.As(err, &disco.ErrServiceDiscoveryNetworkRequest{}):
			return "", classified(ErrTransient, err)
		}
		return "", err
	}
	return strings.TrimSuffix(u.String(), "/"), nil
}

// registryGet performs a GET against a modules.v1 endpoint on host. When host is the discovered
// Pulumi Cloud module registry, the request carries the engine-provided access token; for any other
// host it is anonymous, exactly like the service-discovery requests.
func (l *networkResolver) registryGet(host svchost.Hostname, rawURL string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	if creds, err := l.disco.CredentialsForHost(context.Background(), host); err == nil && creds != nil {
		creds.PrepareRequest(req)
	}
	return http.DefaultClient.Do(req)
}

type registryModuleVersion struct {
	Version string `json:"version"`
}

type registryModuleVersions struct {
	Modules []struct {
		Versions []registryModuleVersion `json:"versions"`
	} `json:"modules"`
}

// getRegistryDownloadURL resolves a concrete download URL via the modules.v1
// protocol, returning it with the version it selected. Empty versionConstraint
// means "highest published version". host identifies the registry for
// credential lookup; see registryGet.
func (l *networkResolver) getRegistryDownloadURL(host svchost.Hostname, baseURL, namespace, name, provider, versionConstraint string) (string, string, error) {
	chosen, err := l.selectRegistryVersion(host, baseURL, namespace, name, provider, versionConstraint)
	if err != nil {
		return "", "", err
	}

	downloadURL := fmt.Sprintf("%s/%s/%s/%s/%s/download", baseURL, namespace, name, provider, chosen)
	resp, err := l.registryGet(host, downloadURL)
	if err != nil {
		return "", "", classified(ErrTransient, fmt.Errorf("getting download URL: %w", err))
	}
	defer contract.IgnoreClose(resp.Body)

	// Terraform Registry: 204 + X-Terraform-Get header.
	// OpenTofu Registry:  200 + JSON body {"location": "..."}.
	// Some impls:         200 + raw URL body.
	if resp.StatusCode == http.StatusNoContent {
		actualURL := resp.Header.Get("X-Terraform-Get")
		if actualURL == "" {
			return "", "", fmt.Errorf("registry did not return download URL")
		}
		return actualURL, chosen, nil
	}

	if resp.StatusCode != http.StatusOK {
		return "", "", classifiedHTTP(resp.StatusCode, fmt.Errorf("getting download URL: HTTP %d", resp.StatusCode))
	}

	if hdr := resp.Header.Get("X-Terraform-Get"); hdr != "" {
		return hdr, chosen, nil
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", "", fmt.Errorf("reading download URL: %w", err)
	}
	trimmed := strings.TrimSpace(string(body))
	if strings.HasPrefix(trimmed, "{") {
		var parsed struct {
			Location string `json:"location"`
		}
		if err := json.Unmarshal([]byte(trimmed), &parsed); err != nil {
			return "", "", fmt.Errorf("parsing registry download response: %w", err)
		}
		if parsed.Location == "" {
			return "", "", fmt.Errorf("registry download response missing 'location'")
		}
		return parsed.Location, chosen, nil
	}
	return trimmed, chosen, nil
}

// selectRegistryVersion returns the highest published version satisfying
// versionConstraint, or the highest overall when it's empty.
func (l *networkResolver) selectRegistryVersion(host svchost.Hostname, baseURL, namespace, name, provider, versionConstraint string) (string, error) {
	versionsURL := fmt.Sprintf("%s/%s/%s/%s/versions", baseURL, namespace, name, provider)
	resp, err := l.registryGet(host, versionsURL)
	if err != nil {
		return "", classified(ErrTransient, fmt.Errorf("querying registry versions: %w", err))
	}
	defer contract.IgnoreClose(resp.Body)

	if resp.StatusCode == http.StatusNotFound {
		return "", classified(ErrNotFound,
			fmt.Errorf("module %s/%s/%s not found in registry", namespace, name, provider))
	}
	if resp.StatusCode != http.StatusOK {
		return "", classifiedHTTP(resp.StatusCode, fmt.Errorf("querying registry versions: HTTP %d", resp.StatusCode))
	}

	var versions registryModuleVersions
	if err := json.NewDecoder(resp.Body).Decode(&versions); err != nil {
		return "", fmt.Errorf("parsing registry response: %w", err)
	}
	if len(versions.Modules) == 0 || len(versions.Modules[0].Versions) == 0 {
		return "", classified(ErrNotFound,
			fmt.Errorf("no versions available for module %s/%s/%s", namespace, name, provider))
	}

	var constraints version.Constraints
	if versionConstraint != "" {
		constraints, err = version.NewConstraint(versionConstraint)
		if err != nil {
			return "", classified(ErrInvalid,
				fmt.Errorf("parsing version constraint %q: %w", versionConstraint, err))
		}
	}

	candidates := make([]*version.Version, 0, len(versions.Modules[0].Versions))
	for _, v := range versions.Modules[0].Versions {
		parsed, parseErr := version.NewVersion(v.Version)
		if parseErr != nil {
			continue
		}
		if constraints != nil && !constraints.Check(parsed) {
			continue
		}
		candidates = append(candidates, parsed)
	}
	if len(candidates) == 0 {
		if versionConstraint != "" {
			return "", classified(ErrNotFound, fmt.Errorf(
				"no published version of module %s/%s/%s satisfies constraint %q",
				namespace, name, provider, versionConstraint))
		}
		return "", classified(ErrNotFound,
			fmt.Errorf("no valid versions for module %s/%s/%s", namespace, name, provider))
	}
	slices.SortFunc(candidates, func(a, b *version.Version) int { return b.Compare(a) })
	return candidates[0].Original(), nil
}

// isRegistrySource reports whether source is a Terraform Registry address
// (`[host/]namespace/name/provider`). Detection is delegated to
// opentofu/registry-address, which also rejects version-control hosts like
// github.com so they fall through to the remote getter. The query string
// (`?version=…`) is stripped first because registry addresses don't permit it.
func isRegistrySource(source string) bool {
	if idx := strings.Index(source, "?"); idx != -1 {
		source = source[:idx]
	}
	_, err := regaddr.ParseModuleSource(source)
	return err == nil
}

func hashSource(source string) string {
	h := sha256.Sum256([]byte(source))
	return hex.EncodeToString(h[:8])
}

// ComponentTypeName derives a component type name from a module's name,
// replicating PCL's DeclarationName logic.
func ComponentTypeName(name string) string {
	for _, ch := range []string{"-", ".", " "} {
		name = strings.ReplaceAll(name, ch, "_")
	}
	parts := strings.Split(name, "_")
	var b strings.Builder
	for _, p := range parts {
		if p != "" {
			b.WriteString(strings.ToUpper(p[:1]) + p[1:])
		}
	}
	return b.String()
}
