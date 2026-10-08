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
	"cmp"
	"context"
	"fmt"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/blang/semver"
	pulumiSchema "github.com/pulumi/pulumi/pkg/v3/codegen/schema"
	"github.com/pulumi/pulumi/sdk/v3/go/common/tokens"
	"github.com/pulumi/pulumi/sdk/v3/go/common/workspace"
	"google.golang.org/grpc/codes"

	"github.com/pulumi/pulumi-hcl/pkg/grpcerr"
	"github.com/pulumi/pulumi-hcl/pkg/hcl/modules"
	"github.com/pulumi/pulumi-hcl/pkg/hcl/parser"
	"github.com/pulumi/pulumi-hcl/pkg/hcl/schema"
	"github.com/pulumi/pulumi-hcl/pkg/potel"
	"github.com/pulumi/pulumi-hcl/vendored/getmodules"
)

// component is one consumable directory of a module package, served as its own
// component resource. source addresses the directory through the package's
// loader; packages are the component's own resolved provider descriptors —
// sibling components are independent configurations and resolve independently.
type component struct {
	schema   *schema.ModuleSchema
	source   string
	packages map[string]workspace.PackageDescriptor
}

// loadedComponent pairs a discovered component directory with its parsed
// configuration, the source that addresses it through the loader, and its
// resolved provider descriptors.
type loadedComponent struct {
	dir      componentDir
	source   string
	loaded   *modules.LoadedModule
	packages map[string]workspace.PackageDescriptor
}

// loadComponents loads every consumable component of a module package and
// returns the concrete version its source resolved to.
func loadComponents(
	ctx context.Context, loader *modules.Loader, source, versionConstraint string,
) ([]loadedComponent, string, error) {
	rootDir, resolvedVersion, err := loader.ResolveDir(source, versionConstraint, ".")
	if err != nil {
		return nil, "", err
	}
	dirs, err := discoverComponentDirs(rootDir)
	if err != nil {
		return nil, "", err
	}
	if len(dirs) == 0 {
		return nil, "", fmt.Errorf(
			"no Terraform files found: neither the module root nor any modules/<name> directory contains .tf files")
	}
	comps := make([]loadedComponent, 0, len(dirs))
	for _, d := range dirs {
		src := componentSource(source, d)
		loaded, err := loader.LoadModule(ctx, src, versionConstraint, ".")
		if err != nil {
			return nil, "", fmt.Errorf("loading %s: %w", d.describe(), err)
		}
		comps = append(comps, loadedComponent{dir: d, source: src, loaded: loaded})
	}
	return comps, resolvedVersion, nil
}

// loadRecordedComponents loads the component set a bundle recorded — the keys
// of its per-component package maps — attaching each component's descriptors.
// The bundle is the source of truth: discovery ran once, at `package add` time.
func loadRecordedComponents(
	ctx context.Context, loader *modules.Loader, root moduleRef,
	packages map[string]map[string]workspace.PackageDescriptor,
) ([]loadedComponent, error) {
	comps := make([]loadedComponent, 0, len(packages))
	for _, subdir := range slices.Sorted(maps.Keys(packages)) {
		d := componentDir{Subdir: subdir}
		src := componentSource(root.Source, d)
		loaded, err := loader.LoadModule(ctx, src, root.Version, ".")
		if err != nil {
			return nil, fmt.Errorf("loading %s: %w", d.describe(), err)
		}
		comps = append(comps, loadedComponent{dir: d, source: src, loaded: loaded, packages: packages[subdir]})
	}
	return comps, nil
}

// indexComponent returns the root ("index") component, or nil when the package
// root holds no Terraform files.
func indexComponent(comps []loadedComponent) *loadedComponent {
	for i := range comps {
		if comps[i].dir.Subdir == "" {
			return &comps[i]
		}
	}
	return nil
}

// packageIdentity derives the package name, version, and root component token.
// The root module names the package — its terraform `package` and `component`
// blocks apply — while a package with no root falls back to defaultName and the
// version its source resolved to, with an empty token.
func packageIdentity(
	comps []loadedComponent, defaultName string, forceDefault bool, resolvedVersion string,
) (string, tokens.Type, semver.Version, error) {
	if root := indexComponent(comps); root != nil {
		rootToken, version, err := moduleIdentity(root.loaded, defaultName, forceDefault)
		if err != nil {
			return "", "", semver.Version{}, err
		}
		return rootToken.Package().Name().String(), rootToken, version, nil
	}
	version, err := semver.ParseTolerant(cmp.Or(resolvedVersion, "0.0.0-dev"))
	if err != nil {
		return "", "", semver.Version{}, fmt.Errorf("parsing module version %q: %w", resolvedVersion, err)
	}
	return defaultName, "", version, nil
}

// buildComponents generates each component's schema — the root under rootToken,
// submodules as pkg:<segment>:Module — and combines them into the package
// schema.
func buildComponents(
	ctx context.Context, comps []loadedComponent, pkgName string, rootToken tokens.Type, version semver.Version,
	newBinder func(loadedComponent) *schema.Binder,
) (map[tokens.Type]component, pulumiSchema.PackageSpec, error) {
	ctx, span := potel.Start(ctx, "buildComponents")
	defer span.End()

	components := make(map[tokens.Type]component, len(comps))
	schemas := make([]*schema.ModuleSchema, 0, len(comps))
	var root *schema.ModuleSchema
	var errs schema.Errors
	for _, c := range comps {
		token := rootToken
		if c.dir.Subdir != "" {
			token = tokens.Type(fmt.Sprintf("%s:%s:Module", pkgName, c.dir.module()))
		}
		sch, err := schema.GenerateModuleSchema(ctx, c.loaded.Config, newBinder(c), token, version)
		if err != nil {
			errs = append(errs, schema.WrapEach(err, "generating schema for %s", c.dir.describe())...)
			continue
		}
		if c.dir.Subdir == "" {
			root = sch
		}
		components[token] = component{schema: sch, source: c.source, packages: c.packages}
		schemas = append(schemas, sch)
	}
	if len(errs) > 0 {
		return nil, pulumiSchema.PackageSpec{}, errs
	}
	spec, err := schema.PackageSchema(root, schemas)
	if err != nil {
		return nil, pulumiSchema.PackageSpec{}, grpcerr.Classify(err, codes.InvalidArgument)
	}
	return components, spec, nil
}

// componentDir names one consumable directory of a module package by its
// root-relative path: "" for the package root itself, "modules/<name>" for a
// submodule directory.
type componentDir struct {
	Subdir string
}

// module is the token module segment the directory's component is served
// under: "index" for the root, the sanitized directory name otherwise.
func (c componentDir) module() string {
	if c.Subdir == "" {
		return "index"
	}
	return sanitizePackageName(path.Base(c.Subdir), "")
}

// describe names the component directory in error messages.
func (c componentDir) describe() string {
	if c.Subdir == "" {
		return "the module root"
	}
	return fmt.Sprintf("submodule %q", c.Subdir)
}

// discoverComponentDirs lists the consumable directories under rootDir: the
// root itself when it holds Terraform files, plus each modules/<name>
// directory that does.
func discoverComponentDirs(rootDir string) ([]componentDir, error) {
	var components []componentDir
	hasRoot, err := parser.DirHasTerraformFiles(rootDir)
	if err != nil {
		return nil, fmt.Errorf("reading module directory: %w", err)
	}
	if hasRoot {
		components = append(components, componentDir{})
	}

	entries, err := os.ReadDir(filepath.Join(rootDir, "modules"))
	if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("reading modules directory: %w", err)
	}
	seen := map[string]string{}
	for _, entry := range entries {
		name := entry.Name()
		if !entry.IsDir() || name[0] == '.' {
			continue
		}
		hasFiles, err := parser.DirHasTerraformFiles(filepath.Join(rootDir, "modules", name))
		if err != nil {
			return nil, fmt.Errorf("reading submodule directory %q: %w", name, err)
		}
		if !hasFiles {
			continue
		}
		d := componentDir{Subdir: path.Join("modules", name)}
		segment := d.module()
		if segment == "" {
			return nil, fmt.Errorf("submodule directory %q does not sanitize to a usable module name", name)
		}
		if prev, ok := seen[segment]; ok {
			return nil, fmt.Errorf("submodule directories %q and %q both map to module name %q", prev, name, segment)
		}
		seen[segment] = name
		components = append(components, d)
	}
	return components, nil
}

// requirementDirs lists the directories whose provider requirements a program
// directory serves: the directory itself, plus every submodule component
// directory when it is a component package (marked by PulumiPlugin.yaml).
func requirementDirs(programDir string) ([]string, error) {
	dirs := []string{programDir}
	if _, err := os.Stat(filepath.Join(programDir, "PulumiPlugin.yaml")); err != nil {
		return dirs, nil
	}
	comps, err := discoverComponentDirs(programDir)
	if err != nil {
		return nil, err
	}
	for _, c := range comps {
		if c.Subdir != "" {
			dirs = append(dirs, filepath.Join(programDir, filepath.FromSlash(c.Subdir)))
		}
	}
	return dirs, nil
}

// componentSource addresses a component directory through the module source that
// resolved to its package root, appending the component's root-relative path to
// the source's subdir. The root component is the source itself.
func componentSource(source string, c componentDir) string {
	if c.Subdir == "" {
		return source
	}
	pkgSource, subdir := getmodules.SplitPackageSubdir(source)
	// The query string must follow the //subdir for the result to re-split.
	pkgSource, query, hasQuery := strings.Cut(pkgSource, "?")
	s := pkgSource + "//" + path.Join(subdir, c.Subdir)
	if hasQuery {
		s += "?" + query
	}
	return s
}
