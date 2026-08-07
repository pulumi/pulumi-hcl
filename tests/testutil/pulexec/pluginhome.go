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

package pulexec

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blang/semver"
	"github.com/stretchr/testify/require"
)

// runtimePlugins are the resource plugins tfcompat programs resolve through
// the engine at runtime (terraform_remote_state → terraform, dynamic provider
// sources → terraform-provider, its local backend → local). Mirrors the set
// ci.yml pre-installs.
var runtimePlugins = []string{"terraform", "terraform-provider", "local"}

// isolatedPulumiHome returns a fresh PULUMI_HOME whose plugin cache contains
// only runtimePlugins, borrowed from the host cache. Resolving a dynamic TF
// package makes the engine interrogate every installed plugin for a Terraform
// mapping, so pointing the CLI at the real host cache costs one
// spawn-every-plugin sweep per `pulumi up` (~10s on a dev machine with many
// plugins installed) and makes test runtime depend on what happens to be
// installed there.
//
// Each seeded plugin is a real directory of symlinks rather than a symlinked
// directory: the engine's plugin scan reads directory entries without
// following links (workspace.tryPlugin requires DirEntry.IsDir), so a
// symlinked plugin directory is invisible to it, while a symlinked file
// inside is resolved normally when the binary is stat'ed and spawned.
//
// When the host cache is missing the home is left empty: a test that resolves
// one of these plugins fails the same way it would have without isolation,
// since the harness disables plugin acquisition.
func isolatedPulumiHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	pluginsDir := filepath.Join(home, "plugins")
	require.NoError(t, os.MkdirAll(pluginsDir, 0o755))

	hostDir := hostPluginsDir()
	entries, err := os.ReadDir(hostDir)
	if err != nil {
		return home
	}
	for _, name := range runtimePlugins {
		// Seed only the highest installed version (pre-releases included, so
		// a dev build newer than the last release wins): the engine's own
		// "latest" selection skips pre-releases, which would silently pick a
		// stale release over a dev build when both are installed.
		version, ok := highestPluginVersion(entries, name)
		if !ok {
			continue
		}
		// The version directory and its adjacent .lock file.
		dir := "resource-" + name + "-v" + version.String()
		seedEntry(t, filepath.Join(hostDir, dir), filepath.Join(pluginsDir, dir))
		if lock := dir + ".lock"; fileExists(filepath.Join(hostDir, lock)) {
			seedEntry(t, filepath.Join(hostDir, lock), filepath.Join(pluginsDir, lock))
		}
	}
	return home
}

func fileExists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// highestPluginVersion returns the highest version of plugin name present in
// the host cache entries, comparing semver with pre-releases included.
func highestPluginVersion(entries []os.DirEntry, name string) (semver.Version, bool) {
	var best semver.Version
	found := false
	prefix := "resource-" + name + "-v"
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), prefix) {
			continue
		}
		v, err := semver.Parse(strings.TrimPrefix(e.Name(), prefix))
		if err != nil {
			continue
		}
		if !found || v.GT(best) {
			best, found = v, true
		}
	}
	return best, found
}

// seedEntry materializes one host plugin-cache entry at dst: a directory
// becomes a real directory whose contents are symlinks into the host cache;
// anything else is symlinked directly.
func seedEntry(t *testing.T, src, dst string) {
	t.Helper()
	info, err := os.Lstat(src)
	require.NoError(t, err)
	if !info.IsDir() {
		require.NoError(t, os.Symlink(src, dst))
		return
	}
	require.NoError(t, os.MkdirAll(dst, 0o755))
	inner, err := os.ReadDir(src)
	require.NoError(t, err)
	for _, f := range inner {
		require.NoError(t, os.Symlink(filepath.Join(src, f.Name()), filepath.Join(dst, f.Name())))
	}
}

func hostPluginsDir() string {
	if h := os.Getenv("PULUMI_HOME"); h != "" {
		return filepath.Join(h, "plugins")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".pulumi", "plugins")
}
