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

package modules

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	regaddr "github.com/opentofu/registry-address/v2"
	"github.com/opentofu/svchost"
	"github.com/opentofu/svchost/disco"
	"github.com/pulumi/pulumi-hcl/vendored/getmodules"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeRegistry serves the modules.v1 endpoints needed by getRegistryDownloadURL.
// versions is returned for /versions; downloads maps version → URL returned
// via the X-Terraform-Get header for /<v>/download (204 response).
type fakeRegistry struct {
	versions  []string
	downloads map[string]string

	// mu guards the hit counters, written from concurrent handler goroutines.
	mu           sync.Mutex
	versionsHits int
	downloadHits map[string]int
}

func (r *fakeRegistry) versionsHitCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.versionsHits
}

func (r *fakeRegistry) downloadHitCount(v string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.downloadHits[v]
}

func newFakeRegistry(t *testing.T, versions []string, downloads map[string]string) (*httptest.Server, *fakeRegistry) {
	t.Helper()
	reg := &fakeRegistry{
		versions:     versions,
		downloads:    downloads,
		downloadHits: map[string]int{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// modules.v1: /<ns>/<name>/<provider>/versions
		//             /<ns>/<name>/<provider>/<version>/download
		trimmed := strings.TrimPrefix(r.URL.Path, "/")
		segs := strings.Split(trimmed, "/")
		switch {
		case len(segs) == 4 && segs[3] == "versions":
			reg.mu.Lock()
			reg.versionsHits++
			reg.mu.Unlock()
			payload := map[string]any{
				"modules": []map[string]any{
					{"versions": versionsPayload(reg.versions)},
				},
			}
			_ = json.NewEncoder(w).Encode(payload)
		case len(segs) == 5 && segs[4] == "download":
			v := segs[3]
			reg.mu.Lock()
			reg.downloadHits[v]++
			reg.mu.Unlock()
			dl, ok := reg.downloads[v]
			if !ok {
				http.Error(w, "no such version", http.StatusNotFound)
				return
			}
			w.Header().Set("X-Terraform-Get", dl)
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, reg
}

func versionsPayload(vs []string) []map[string]string {
	out := make([]map[string]string, len(vs))
	for i, v := range vs {
		out[i] = map[string]string{"version": v}
	}
	return out
}

// newTestNetworkResolver builds a networkResolver whose default registry host
// resolves to modulesV1BaseURL, standing in for the real modules.v1 endpoint.
func newTestNetworkResolver(t *testing.T, modulesV1BaseURL string) *networkResolver {
	t.Helper()
	d := disco.New()
	d.ForceHostServices(regaddr.DefaultModuleRegistryHost, map[string]any{
		"modules.v1": modulesV1BaseURL + "/",
	})
	return &networkResolver{
		cacheDir: t.TempDir(),
		fetcher:  getmodules.NewPackageFetcher(t.Context(), nil),
		disco:    d,
	}
}

func newTestLoader(t *testing.T, modulesV1BaseURL string) *Loader {
	t.Helper()
	return NewLoader(newTestNetworkResolver(t, modulesV1BaseURL).resolve)
}

func TestIsRegistrySource(t *testing.T) {
	t.Parallel()
	cases := []struct {
		source string
		want   bool
	}{
		{"cloudposse/label/null", true},
		{"registry.terraform.io/cloudposse/label/null", true},
		// Query strings are stripped before delegating to the parser.
		{"app.terraform.io/my-org/vpc/aws?version=1.2.3", true},
		// Reserved version-control hosts fall through to the remote getter.
		{"github.com/hashicorp/consul/aws", false},
		// A bare github address: "github.com" is not a valid namespace.
		{"github.com/org/repo", false},
		// A host without a dot is not a valid registry host.
		{"localhost/org/name/aws", false},
		{"too/many/slashes/here/now", false},
		{"only/two", false},
		{"bad ns/name/aws", false},
	}
	for _, tc := range cases {
		t.Run(tc.source, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tc.want, isRegistrySource(tc.source))
		})
	}
}

func TestGetRegistryDownloadURL_NoConstraintPicksLatest(t *testing.T) {
	t.Parallel()
	// Registry orders Versions oldest-first. We must not pick versions[0].
	srv, reg := newFakeRegistry(t,
		[]string{"1.0.0", "3.2.1", "4.0.0", "4.2.0", "2.9.9"},
		map[string]string{"4.2.0": "https://example.com/v420.tar.gz"})
	n := newTestNetworkResolver(t, srv.URL)

	got, chosen, err := n.getRegistryDownloadURL(regaddr.DefaultModuleRegistryHost, srv.URL, "acme", "thing", "aws", "")
	require.NoError(t, err)
	require.Equal(t, "https://example.com/v420.tar.gz", got)
	require.Equal(t, "4.2.0", chosen)
	require.Equal(t, 1, reg.versionsHitCount())
	require.Equal(t, 1, reg.downloadHitCount("4.2.0"))
}

func TestGetRegistryDownloadURL_ConstraintPicksHighestMatching(t *testing.T) {
	t.Parallel()
	srv, reg := newFakeRegistry(t,
		[]string{"3.0.0", "3.5.0", "4.0.0", "4.0.1", "4.1.0", "5.0.0"},
		map[string]string{"4.1.0": "https://example.com/v410.tar.gz"})
	n := newTestNetworkResolver(t, srv.URL)

	got, chosen, err := n.getRegistryDownloadURL(regaddr.DefaultModuleRegistryHost, srv.URL, "acme", "thing", "aws", "~> 4.0")
	require.NoError(t, err)
	require.Equal(t, "https://example.com/v410.tar.gz", got)
	require.Equal(t, "4.1.0", chosen)
	require.Equal(t, 1, reg.downloadHitCount("4.1.0"))
}

func TestGetRegistryDownloadURL_ExactVersionPin(t *testing.T) {
	t.Parallel()
	srv, _ := newFakeRegistry(t,
		[]string{"1.0.0", "2.0.0", "3.0.0"},
		map[string]string{"2.0.0": "https://example.com/v200.tar.gz"})
	n := newTestNetworkResolver(t, srv.URL)

	got, chosen, err := n.getRegistryDownloadURL(regaddr.DefaultModuleRegistryHost, srv.URL, "acme", "thing", "aws", "2.0.0")
	require.NoError(t, err)
	require.Equal(t, "https://example.com/v200.tar.gz", got)
	require.Equal(t, "2.0.0", chosen)
}

func TestGetRegistryDownloadURL_NoMatchingVersionErrors(t *testing.T) {
	t.Parallel()
	srv, _ := newFakeRegistry(t,
		[]string{"1.0.0", "2.0.0"},
		map[string]string{})
	n := newTestNetworkResolver(t, srv.URL)

	_, _, err := n.getRegistryDownloadURL(regaddr.DefaultModuleRegistryHost, srv.URL, "acme", "thing", "aws", "~> 99.0")
	require.Error(t, err)
	require.Contains(t, err.Error(), "no published version")
	require.Contains(t, err.Error(), "~> 99.0")
}

// TestGetRegistryDownloadURL_OpenTofuStyle_JSONBody verifies the OpenTofu
// registry response shape: 200 + JSON `{"location":"..."}` body, rather than
// the 204 + X-Terraform-Get header used by the Terraform registry.
func TestGetRegistryDownloadURL_OpenTofuStyle_JSONBody(t *testing.T) {
	t.Parallel()

	const wantURL = "git::https://example.com/repo?ref=abc123"
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		trimmed := strings.TrimPrefix(r.URL.Path, "/")
		segs := strings.Split(trimmed, "/")
		switch {
		case len(segs) == 4 && segs[3] == "versions":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"modules": []map[string]any{
					{"versions": versionsPayload([]string{"1.0.0"})},
				},
			})
		case len(segs) == 5 && segs[4] == "download":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"location": "` + wantURL + `"}`))
		default:
			http.NotFound(w, r)
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	n := newTestNetworkResolver(t, srv.URL)
	got, _, err := n.getRegistryDownloadURL(regaddr.DefaultModuleRegistryHost, srv.URL, "acme", "thing", "aws", "")
	require.NoError(t, err)
	require.Equal(t, wantURL, got)
}

func TestGetRegistryDownloadURL_InvalidConstraintErrors(t *testing.T) {
	t.Parallel()
	srv, _ := newFakeRegistry(t,
		[]string{"1.0.0"},
		map[string]string{"1.0.0": "https://example.com/x.tar.gz"})
	n := newTestNetworkResolver(t, srv.URL)

	_, _, err := n.getRegistryDownloadURL(regaddr.DefaultModuleRegistryHost, srv.URL, "acme", "thing", "aws", "not-a-constraint")
	require.Error(t, err)
	require.Contains(t, err.Error(), "parsing version constraint")
}

func TestLoadModule_VersionConstraintPlumbedThroughToRegistry(t *testing.T) {
	t.Parallel()

	// Serve a module tarball that the registry will redirect us to. We pin
	// 4.0.1 as the only version-with-a-download so a successful load *proves*
	// the `~> 4.0` constraint selected 4.0.1 rather than 5.1.0 or 3.x.
	modTar := buildModuleTarGz(t, map[string]string{
		"main.tf": `output "ok" { value = "v4-output" }`,
	})
	tarSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/gzip")
		_, _ = w.Write(modTar)
	}))
	t.Cleanup(tarSrv.Close)
	// go-getter sniffs the archive type from the URL; force tar.gz so the
	// HTTP getter knows which decompressor to use.
	tarURL := tarSrv.URL + "/m.tar.gz?archive=tar.gz"

	regSrv, reg := newFakeRegistry(t,
		[]string{"3.0.0", "3.9.9", "4.0.0", "4.0.1", "5.1.0"},
		map[string]string{"4.0.1": tarURL})
	l := newTestLoader(t, regSrv.URL)

	loaded, err := l.LoadModule(t.Context(), "acme/thing/aws", "~> 4.0", t.TempDir())
	require.NoError(t, err)
	require.NotNil(t, loaded.Config)
	require.Contains(t, loaded.Config.Outputs, "ok",
		"the module body fetched should be the 4.0.1 fixture")
	require.Equal(t, "4.0.1", loaded.Version)
	require.Equal(t, 1, reg.downloadHitCount("4.0.1"),
		"constraint ~> 4.0 should resolve to 4.0.1 (highest 4.x), not 5.1.0 or 3.x")
}

func TestLoadModule_VersionQueryStringStillSupported(t *testing.T) {
	t.Parallel()
	modTar := buildModuleTarGz(t, map[string]string{
		"main.tf": `output "ok" { value = "v3-output" }`,
	})
	tarSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(modTar)
	}))
	t.Cleanup(tarSrv.Close)
	tarURL := tarSrv.URL + "/m.tar.gz?archive=tar.gz"

	regSrv, reg := newFakeRegistry(t,
		[]string{"3.0.0", "4.0.0"},
		map[string]string{"3.0.0": tarURL})
	l := newTestLoader(t, regSrv.URL)

	loaded, err := l.LoadModule(t.Context(), "acme/thing/aws?version=3.0.0", "", t.TempDir())
	require.NoError(t, err)
	require.Equal(t, "3.0.0", loaded.Version)
	require.Equal(t, 1, reg.downloadHitCount("3.0.0"))
}

// TestLoadModule_HostQualifiedRegistrySource verifies that a source carrying an
// explicit registry host (e.g. registry.terraform.io/...) is resolved against
// that host's discovered modules.v1 endpoint rather than the hardcoded default
// registry. ForceHostServices stands in for network service discovery.
func TestLoadModule_HostQualifiedRegistrySource(t *testing.T) {
	t.Parallel()

	modTar := buildModuleTarGz(t, map[string]string{
		"main.tf": `output "ok" { value = "host-qualified" }`,
	})

	tarSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/gzip")
		_, _ = w.Write(modTar)
	}))
	t.Cleanup(tarSrv.Close)
	tarURL := tarSrv.URL + "/m.tar.gz?archive=tar.gz"

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/modules/myorg/widget/aws/versions", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"modules": []map[string]any{{"versions": versionsPayload([]string{"1.0.0"})}},
		})
	})
	mux.HandleFunc("/v1/modules/myorg/widget/aws/1.0.0/download", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Terraform-Get", tarURL)
		w.WriteHeader(http.StatusNoContent)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	const host = "registry.example.com"
	hostname, err := svchost.ForComparison(host)
	require.NoError(t, err)
	d := disco.New()
	d.ForceHostServices(hostname, map[string]any{"modules.v1": srv.URL + "/v1/modules/"})

	n := &networkResolver{
		cacheDir: t.TempDir(),
		fetcher:  getmodules.NewPackageFetcher(t.Context(), nil),
		disco:    d,
	}
	l := NewLoader(n.resolve)

	loaded, err := l.LoadModule(t.Context(), host+"/myorg/widget/aws", "", t.TempDir())
	require.NoError(t, err)
	require.NotNil(t, loaded.Config)
	require.Contains(t, loaded.Config.Outputs, "ok")
}

func TestLoadModule_GitSubdirWithRef(t *testing.T) {
	t.Parallel()

	repo := initGitRepo(t, "modules/iam-policy/main.tf", `output "ok" { value = "v1" }`)
	git(t, repo, "tag", "v1")
	require.NoError(t, os.WriteFile(
		filepath.Join(repo, "modules/iam-policy/main.tf"),
		[]byte(`output "ok2" { value = "head" }`), 0o600))
	git(t, repo, "commit", "-am", "rename output")

	l := newTestLoader(t, "http://invalid.example")
	source := "git::file://" + repo + "//modules/iam-policy?ref=v1"

	loaded, err := l.LoadModule(t.Context(), source, "", t.TempDir())
	require.NoError(t, err)
	require.NotNil(t, loaded.Config)
	require.Contains(t, loaded.Config.Outputs, "ok",
		"ref=v1 should pin the checkout to the tagged commit")
	require.NotContains(t, loaded.Config.Outputs, "ok2",
		"HEAD's output must not appear when ref=v1 is requested")
}

// initGitRepo creates a git repository in a temp dir containing a single file
// (relativePath → content), committed on the default branch.
func initGitRepo(t *testing.T, relativePath, content string) string {
	t.Helper()
	repo := t.TempDir()
	full := filepath.Join(repo, relativePath)
	require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
	require.NoError(t, os.WriteFile(full, []byte(content), 0o600))

	git(t, repo, "init")
	git(t, repo, "config", "user.email", "test@example.com")
	git(t, repo, "config", "user.name", "test")
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-m", "initial")
	return repo
}

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	require.NoErrorf(t, err, "git %s: %s", strings.Join(args, " "), out)
}

// TestLoaderResolvesViaCustomResolver verifies a loader built from a custom
// resolver (the mechanism a bundle loader uses) resolves a package and joins
// subdir references under it, with the resolver seeing only the package source.
func TestLoaderResolvesViaCustomResolver(t *testing.T) {
	t.Parallel()

	pkg := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(pkg, "main.tf"), []byte(`output "root" { value = 1 }`), 0o600))
	for _, sub := range []string{"a", "b"} {
		dir := filepath.Join(pkg, sub)
		require.NoError(t, os.MkdirAll(dir, 0o755))
		require.NoError(t, os.WriteFile(
			filepath.Join(dir, "main.tf"), []byte(`output "leaf" { value = 1 }`), 0o600))
	}

	l := NewLoader(func(packageSource, versionConstraint, _ string) (string, string, error) {
		require.Equal(t, "acme/widget/cloud", packageSource)
		require.Equal(t, "~> 4.0", versionConstraint)
		return pkg, "4.2.0", nil
	})

	root, err := l.LoadModule(t.Context(), "acme/widget/cloud", "~> 4.0", t.TempDir())
	require.NoError(t, err)
	require.Equal(t, pkg, root.SourcePath)

	// Two subdir references into the one resolved package resolve under it; the
	// resolver only ever sees the package source.
	a, err := l.LoadModule(t.Context(), "acme/widget/cloud//a", "~> 4.0", t.TempDir())
	require.NoError(t, err)
	require.Equal(t, filepath.Join(pkg, "a"), a.SourcePath)

	b, err := l.LoadModule(t.Context(), "acme/widget/cloud//b", "~> 4.0", t.TempDir())
	require.NoError(t, err)
	require.Equal(t, filepath.Join(pkg, "b"), b.SourcePath)
}

func TestLoadModule_ParseFailureIsErrInvalid(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		files map[string]string
	}{
		{name: "no tf files", files: nil},
		{name: "syntax error", files: map[string]string{"main.tf": `output "root" {`}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			pkg := t.TempDir()
			for name, content := range tt.files {
				require.NoError(t, os.WriteFile(filepath.Join(pkg, name), []byte(content), 0o600))
			}

			l := NewLoader(func(_, _, _ string) (string, string, error) {
				return pkg, "", nil
			})
			_, err := l.LoadModule(t.Context(), "./mod", "", t.TempDir())
			assert.ErrorIs(t, err, ErrInvalid)
		})
	}
}

// buildModuleTarGz packs `files` (name → content) into a gzipped tar archive
// using the system `tar`. The returned bytes are the raw .tar.gz body that
// PackageFetcher / go-getter can consume.
func buildModuleTarGz(t *testing.T, files map[string]string) []byte {
	t.Helper()
	src := t.TempDir()
	for name, content := range files {
		full := filepath.Join(src, name)
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
		require.NoError(t, os.WriteFile(full, []byte(content), 0o600))
	}
	out := filepath.Join(t.TempDir(), "out.tar.gz")
	cmd := exec.Command("tar", "-czf", out, "-C", src, ".")
	// macOS BSD tar otherwise writes AppleDouble `._foo` companions which
	// our parser rejects as invalid HCL when the tarball is later extracted.
	cmd.Env = append(os.Environ(), "COPYFILE_DISABLE=1")
	combined, err := cmd.CombinedOutput()
	require.NoErrorf(t, err, "tar: %s", combined)
	data, err := os.ReadFile(out)
	require.NoError(t, err)
	return data
}

func TestSourceName(t *testing.T) {
	t.Parallel()

	tests := map[string]string{
		"../vendor/acme/aws/modules/networking":                "networking",
		"./modules/vpc":                                        "vpc",
		"terraform-aws-modules/vpc/aws":                        "vpc",
		"terraform-aws-modules/vpc/aws//modules/vpc-endpoints": "vpc-endpoints",
		"github.com/acme/terraform-modules.git?ref=v1.2.0":     "terraform-modules",
		"git::https://example.com/network.git//subnets":        "subnets",
	}
	for source, want := range tests {
		assert.Equal(t, want, SourceName(source), source)
	}
}

// TestLoaderResolvesPackageOnce verifies one package resolves once per Loader
// regardless of how many of its subdirectories are loaded, while a different
// constraint or caller still resolves anew.
func TestLoaderResolvesPackageOnce(t *testing.T) {
	t.Parallel()

	pkg := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(pkg, "main.tf"), []byte(`output "r" { value = 1 }`), 0o600))
	require.NoError(t, os.MkdirAll(filepath.Join(pkg, "sub"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(pkg, "sub", "main.tf"), []byte(`output "s" { value = 2 }`), 0o600))

	count := 0
	l := NewLoader(func(string, string, string) (string, string, error) {
		count++
		return pkg, "1.0.0", nil
	})

	_, _, err := l.ResolveDir("acme/widget/cloud", "", ".")
	require.NoError(t, err)
	_, err = l.LoadModule(t.Context(), "acme/widget/cloud", "", ".")
	require.NoError(t, err)
	_, err = l.LoadModule(t.Context(), "acme/widget/cloud//sub", "", ".")
	require.NoError(t, err)
	require.Equal(t, 1, count)

	_, err = l.LoadModule(t.Context(), "acme/widget/cloud", "~> 2", ".")
	require.NoError(t, err)
	require.Equal(t, 2, count)
}

const fetchHelperEnv = "PULUMI_HCL_TEST_FETCH_HELPER"

// TestFetchRemote_ConcurrentProcessesCloneOnce models `pulumi install` with
// several packages that point at subdirectories of one repository. The CLI runs
// one provider process per package, so each package fetches from its own
// process into one shared cache directory.
func TestFetchRemote_ConcurrentProcessesCloneOnce(t *testing.T) {
	t.Parallel()

	repo := initGitRepo(t, "modules/a/main.tf", `output "ok" { value = "a" }`)
	git(t, repo, "tag", "v1")
	cacheDir := t.TempDir()
	readyDir := t.TempDir()

	const n = 8
	cmds := make([]*exec.Cmd, n)
	outs := make([]bytes.Buffer, n)
	starts := make([]io.WriteCloser, n)
	for i := range cmds {
		cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestFetchRemoteHelperProcess$")
		cmd.Stdout, cmd.Stderr = &outs[i], &outs[i]
		cmd.Env = append(os.Environ(),
			fetchHelperEnv+"=1",
			"HELPER_SOURCE=git::file://"+repo+"?ref=v1",
			"HELPER_CACHE="+cacheDir,
			"HELPER_READY="+filepath.Join(readyDir, strconv.Itoa(i)),
		)
		stdin, err := cmd.StdinPipe()
		require.NoError(t, err)
		starts[i] = stdin
		cmds[i] = cmd
		require.NoError(t, cmd.Start())
	}
	// Every child blocks on stdin once it is ready; closing the pipes back to
	// back releases them within microseconds of each other, so their clones
	// overlap.
	for i := range cmds {
		waitForFile(t, filepath.Join(readyDir, strconv.Itoa(i)))
	}
	for _, start := range starts {
		require.NoError(t, start.Close())
	}

	for i, cmd := range cmds {
		assert.NoErrorf(t, cmd.Wait(), "child %d: %s", i, outs[i].String())
	}

	entries, err := os.ReadDir(filepath.Join(cacheDir, "remote"))
	require.NoError(t, err)
	var clones []string
	for _, e := range entries {
		if e.IsDir() {
			clones = append(clones, e.Name())
		}
	}
	assert.Len(t, clones, 1, "one repository must be cloned exactly once")
}

// TestFetchRemoteHelperProcess is the body of one child process of
// TestFetchRemote_ConcurrentProcessesCloneOnce. It is a no-op under `go test`.
func TestFetchRemoteHelperProcess(t *testing.T) {
	t.Parallel()
	if os.Getenv(fetchHelperEnv) == "" {
		t.Skip()
	}
	require.NoError(t, os.WriteFile(os.Getenv("HELPER_READY"), nil, 0o600))
	_, err := io.ReadAll(os.Stdin)
	require.NoError(t, err)

	r := &networkResolver{
		cacheDir: os.Getenv("HELPER_CACHE"),
		fetcher:  getmodules.NewPackageFetcher(t.Context(), nil),
	}
	dir, err := r.fetchRemote(os.Getenv("HELPER_SOURCE"), "remote")
	require.NoError(t, err)
	_, err = os.Stat(filepath.Join(dir, "modules", "a", "main.tf"))
	require.NoError(t, err, "clone incomplete after successful fetch")
}

// TestFetchRemote_RecoversFromInterruptedFetch seeds the cache with what a
// fetch that died part-way leaves behind, and checks the next fetch succeeds.
func TestFetchRemote_RecoversFromInterruptedFetch(t *testing.T) {
	t.Parallel()

	repo := initGitRepo(t, "modules/a/main.tf", `output "ok" { value = "a" }`)
	git(t, repo, "tag", "v1")
	r := newTestNetworkResolver(t, "http://invalid.example")
	source := "git::file://" + repo + "?ref=v1"

	pkgAddr, _, err := getmodules.NormalizePackageAddress(source)
	require.NoError(t, err)
	cacheDir := filepath.Join(r.cacheDir, "remote", hashSource(pkgAddr))
	require.NoError(t, os.MkdirAll(cacheDir, 0o755))
	require.NoError(t, os.MkdirAll(cacheDir+".partial", 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(cacheDir+".partial", "stale"), nil, 0o600))

	dir, err := r.fetchRemote(source, "remote")
	require.NoError(t, err)
	assert.Equal(t, cacheDir, dir)
	assert.FileExists(t, filepath.Join(dir, "modules", "a", "main.tf"))
	assert.NoDirExists(t, cacheDir+".partial")
}

func waitForFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		require.Less(t, time.Now(), deadline, "timed out waiting for %s", path)
		time.Sleep(time.Millisecond)
	}
}
