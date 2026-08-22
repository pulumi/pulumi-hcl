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

// Package server implements the Pulumi language runtime gRPC server for HCL.
package server

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/blang/semver"
	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/pulumi/pulumi-hcl/pkg/codegen"
	"github.com/pulumi/pulumi-hcl/pkg/hcl/ast"
	"github.com/pulumi/pulumi-hcl/pkg/hcl/bridge"
	"github.com/pulumi/pulumi-hcl/pkg/hcl/modules"
	"github.com/pulumi/pulumi-hcl/pkg/hcl/packages"
	"github.com/pulumi/pulumi-hcl/pkg/hcl/parser"
	"github.com/pulumi/pulumi-hcl/pkg/hcl/run"
	"github.com/pulumi/pulumi-hcl/pkg/version"
	"github.com/pulumi/pulumi-hcl/vendored/addrs"
	"github.com/pulumi/pulumi/pkg/v3/codegen/convert"
	"github.com/pulumi/pulumi/pkg/v3/codegen/pcl"
	"github.com/pulumi/pulumi/pkg/v3/codegen/schema"
	"github.com/pulumi/pulumi/sdk/v3/go/common/apitype"
	pulumiencoding "github.com/pulumi/pulumi/sdk/v3/go/common/encoding"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource/plugin"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource/urn"
	"github.com/pulumi/pulumi/sdk/v3/go/common/tokens"
	"github.com/pulumi/pulumi/sdk/v3/go/common/util/contract"
	"github.com/pulumi/pulumi/sdk/v3/go/common/util/fsutil"
	"github.com/pulumi/pulumi/sdk/v3/go/common/util/logging"
	"github.com/pulumi/pulumi/sdk/v3/go/common/util/rpcutil"
	"github.com/pulumi/pulumi/sdk/v3/go/common/workspace"
	"github.com/pulumi/pulumi/sdk/v3/go/property"
	pulumirpc "github.com/pulumi/pulumi/sdk/v3/proto/go"
	"github.com/zclconf/go-cty/cty"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/emptypb"
)

// LanguageHost implements the LanguageRuntimeServer gRPC interface.
type LanguageHost struct {
	pulumirpc.UnimplementedLanguageRuntimeServer
	engine  pulumirpc.EngineClient
	closers []io.Closer

	// alwaysRegisterProviders forces every `provider` block to be registered
	// as a resource. Test-only; set via WithAlwaysRegisterProviders.
	alwaysRegisterProviders bool
}

// Ensure LanguageHost implements the interface.
var _ pulumirpc.LanguageRuntimeServer = (*LanguageHost)(nil)

// Option configures a LanguageHost.
type Option func(*LanguageHost)

// WithAlwaysRegisterProviders forces every `provider` block to be registered
// as a resource even when no resource references it, bypassing Terraform's
// lazy provider-configure semantics.
//
// This exists ONLY for the language conformance tests, whose Pulumi-semantics
// fixtures declare explicit providers as resources and expect them in the
// snapshot. Do not use it in production: it would configure unused providers
// whose config is meant to be evaluated lazily.
func WithAlwaysRegisterProviders() Option {
	return func(h *LanguageHost) { h.alwaysRegisterProviders = true }
}

// NewLanguageHost creates a new HCL language host.
//
// engineAddress is the gRPC address of the engine. It may be empty when the
// engine attaches to an already-running host via PULUMI_DEBUG_LANGUAGES: in
// that mode the address is unknown at construction and arrives later through
// the [LanguageHost.Handshake] RPC.
//
// The returned [LanguageHost] should be closed.
func NewLanguageHost(engineAddress string, opts ...Option) (*LanguageHost, error) {
	host := &LanguageHost{}
	for _, opt := range opts {
		opt(host)
	}
	if engineAddress != "" {
		if err := host.connectEngine(engineAddress); err != nil {
			return nil, err
		}
	}
	return host, nil
}

// connectEngine dials the engine at engineAddress and records the connection
// for later cleanup. It is called either at construction (spawn mode) or from
// Handshake (attach mode).
func (host *LanguageHost) connectEngine(engineAddress string) error {
	engineConn, err := grpc.NewClient(
		engineAddress,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		rpcutil.GrpcChannelOptions(),
	)
	if err != nil {
		return fmt.Errorf("connecting to engine: %w", err)
	}
	host.engine = pulumirpc.NewEngineClient(engineConn)
	host.closers = append(host.closers, engineConn)
	return nil
}

// Handshake is the first RPC the engine sends when it attaches to an
// already-running host (PULUMI_DEBUG_LANGUAGES). The engine address is not
// known until this call, so the host establishes its engine connection here.
// In spawn mode the engine never calls Handshake; the address is supplied to
// NewLanguageHost as the binary's positional argument instead.
func (host *LanguageHost) Handshake(
	ctx context.Context, req *pulumirpc.LanguageHandshakeRequest,
) (*pulumirpc.LanguageHandshakeResponse, error) {
	if req.EngineAddress == "" {
		return nil, errors.New("language handshake request must contain an engine address")
	}
	if err := host.connectEngine(req.EngineAddress); err != nil {
		return nil, err
	}
	return &pulumirpc.LanguageHandshakeResponse{}, nil
}

func (host *LanguageHost) Close() error {
	errs := make([]error, len(host.closers))
	for i, v := range host.closers {
		errs[i] = v.Close()
	}
	return errors.Join(errs...)
}

// bridgePackageName is the plugin that serves any Terraform provider via
// parameterization.
const bridgePackageName = "terraform-provider"

// bridgePackageVersion pins the terraform-provider release install specs
// resolve to. Left unset, the CLI reuses whatever version is already in the
// plugin cache, so a stale install keeps serving long-fixed schema bugs
// (https://github.com/pulumi/pulumi-hcl/issues/204). Renovate bumps it on
// each release (see renovate.json5).
const bridgePackageVersion = "1.3.0" // renovate: github-releases pulumi/pulumi-terraform-provider

// GetRequiredPackages returns the packages required to run an HCL program,
// keyed by provider source — a local name may resolve to different sources
// in different modules, and those are distinct requirements. Pulumi-source
// providers are emitted as plain PackageDependency entries (or from the
// local SDK descriptor `pulumi package add` wrote). Non-Pulumi sources
// satisfied by a local SDK are emitted from that descriptor; the rest become
// PackageSpec entries that `pulumi install` resolves (per docs/providers.md).
func (host *LanguageHost) GetRequiredPackages(
	ctx context.Context,
	req *pulumirpc.GetRequiredPackagesRequest,
) (*pulumirpc.GetRequiredPackagesResponse, error) {
	logging.V(5).Infof("GetRequiredPackages: program=%s", req.Info.ProgramDirectory)

	sdks, err := readSDKInfos(req.Info.ProgramDirectory)
	if err != nil {
		return &pulumirpc.GetRequiredPackagesResponse{}, fmt.Errorf("unable to read SDKs folder: %w", err)
	}

	tfReqs, pulumiPkgs, err := programRequirements(ctx, req.Info.ProgramDirectory)
	if err != nil {
		return &pulumirpc.GetRequiredPackagesResponse{}, err
	}

	// Distinct sources resolving to one package name cannot both live at
	// sdks/<name>: `pulumi install` would overwrite one SDK with the other
	// and never converge.
	byName := map[string]string{}
	for _, source := range sortedKeys(tfReqs) {
		name := packageName("", source)
		if prev, ok := byName[name]; ok {
			return nil, fmt.Errorf(
				"provider sources %q and %q both resolve to package name %q; "+
					"distinct sources sharing a package name are not supported",
				tfReqs[prev].display, tfReqs[source].display, name)
		}
		byName[name] = source
	}

	var pkgs []*pulumirpc.PackageDependency
	var specs []*pulumirpc.PackageSpec
	emitted := map[string]bool{}
	emitPkg := func(dir string, info workspace.PackageDescriptor) {
		if emitted[dir] {
			return
		}
		emitted[dir] = true
		pkgs = append(pkgs, &pulumirpc.PackageDependency{
			Name:             info.Name,
			Version:          versionString(info.Version),
			Kind:             "resource",
			Server:           info.PluginDownloadURL,
			Parameterization: parameterizationProto(info.Parameterization),
			Extension:        parameterizationProto(info.ExtensionParameterization),
		})
	}

	for _, source := range sortedKeys(tfReqs) {
		if dir, desc, ok := descriptorForSource(source, sdks); ok {
			emitPkg(dir, desc)
			continue
		}
		params := []string{tfReqs[source].display}
		if constraint := tfReqs[source].constraint(); constraint != "" {
			params = append(params, constraint)
		}
		specs = append(specs, &pulumirpc.PackageSpec{
			Source:     bridgePackageName,
			Version:    bridgePackageVersion,
			Parameters: params,
		})
	}

	for _, name := range sortedKeys(pulumiPkgs) {
		// The SDK descriptor carries the parameterization and download URL
		// a required_providers entry alone lacks.
		if dir, desc, ok := descriptorForPulumiPackage(name, sdks); ok {
			emitPkg(dir, desc)
			continue
		}
		pkgs = append(pkgs, &pulumirpc.PackageDependency{
			Name:    name,
			Version: pulumiPkgs[name],
			Kind:    "resource",
		})
	}

	return &pulumirpc.GetRequiredPackagesResponse{Packages: pkgs, Specs: specs}, nil
}

// programRequirements resolves every provider referenced by the program's
// module tree and component directories to its source, skipping directories
// that fail to parse.
func programRequirements(
	ctx context.Context, programDir string,
) (tf map[string]*tfRequirement, pulumi map[string]string, err error) {
	dirs, err := requirementDirs(programDir)
	if err != nil {
		return nil, nil, err
	}
	p := parser.NewParser()
	loader := modules.NewLoader(modules.LiveResolver(ctx))
	tf = map[string]*tfRequirement{}
	pulumi = map[string]string{}
	aliases := map[string]*ast.RequiredProvider{}
	visited := map[string]struct{}{}
	for _, dir := range dirs {
		config, diags := p.ParseDirectory(dir)
		if diags.HasErrors() {
			continue
		}
		collectRequirementsRec(ctx, config, dir, tf, pulumi, aliases, loader, visited)
	}
	return tf, pulumi, nil
}

// descriptorForSource returns the on-disk SDK descriptor that satisfies the
// canonical provider source, and the sdks/ directory it lives in. A
// descriptor carrying a source (recorded by GeneratePackage) satisfies
// exactly that source; one without satisfies the source its directory is
// named after (installs name the SDK directory after the resolved package);
// an extension satisfies its base provider's source from any directory.
func descriptorForSource(
	source string, sdks map[string]sdkInfo,
) (string, workspace.PackageDescriptor, bool) {
	for _, dir := range sortedKeys(sdks) {
		if info := sdks[dir]; info.source != "" && canonicalSource(info.source) == source {
			return dir, info.desc, true
		}
	}
	name := packageName("", source)
	if info, ok := sdks[name]; ok && info.source == "" {
		return name, info.desc, true
	}
	// An extension's resource tokens live in the base provider's namespace,
	// so the base is served by the extension's SDK.
	for _, dir := range sortedKeys(sdks) {
		if desc := sdks[dir].desc; desc.ExtensionParameterization != nil && desc.Name == name {
			return dir, desc, true
		}
	}
	return "", workspace.PackageDescriptor{}, false
}

// bridgeRemoteSource returns the provider source a terraform-provider
// parameterization was resolved from, read from the bridge's parameter
// encoding ({"remote":{"url":...}}) — the only surviving record of the
// source, since GeneratePackage receives just the schema. Non-bridge
// parameters return "" and match by directory name instead.
func bridgeRemoteSource(desc workspace.PackageDescriptor) string {
	if desc.Name != bridgePackageName || desc.Parameterization == nil {
		return ""
	}
	var param struct {
		Remote struct {
			URL string `json:"url"`
		} `json:"remote"`
	}
	if json.Unmarshal(desc.Parameterization.Value, &param) != nil {
		return ""
	}
	return param.Remote.URL
}

// canonicalSource returns the fully-qualified form of a provider source
// ("aws" → "registry.opentofu.org/hashicorp/aws") so every spelling of one
// provider shares a single requirement; unparseable sources map to themselves.
func canonicalSource(source string) string {
	provider, diags := addrs.ParseProviderSourceString(source)
	if diags.HasErrors() {
		return source
	}
	return provider.String()
}

// tfProviderSource returns the source URL passed to the terraform-provider
// plugin: req.Source when set, else the OpenTofu-registry default
// "hashicorp/<alias>".
func tfProviderSource(alias string, req *ast.RequiredProvider) string {
	if req != nil && req.Source != "" {
		return req.Source
	}
	return "hashicorp/" + alias
}

// packageName returns the Pulumi package name for a required_providers
// entry: the last segment of the source ("pulumi/aws" → "aws",
// "hashicorp/simple" → "simple"), or the alias when source is unset.
func packageName(alias, source string) string {
	if source == "" {
		return alias
	}
	parts := strings.Split(source, "/")
	return parts[len(parts)-1]
}

func versionString(v *semver.Version) string {
	if v == nil {
		return ""
	}
	return v.String()
}

func parameterizationProto(p *workspace.Parameterization) *pulumirpc.PackageParameterization {
	if p == nil {
		return nil
	}
	return &pulumirpc.PackageParameterization{
		Name:    p.Name,
		Version: p.Version.String(),
		Value:   p.Value,
	}
}

// sdkInfo is one sdks/<dir>/hcl.sdk.json: the package descriptor plus the
// provider source it satisfies (recorded by GeneratePackage; empty for
// pre-existing SDKs and non-bridge parameterized packages).
type sdkInfo struct {
	desc   workspace.PackageDescriptor
	source string
}

// readSDKInfos reads every sdks/*/hcl.sdk.json under dir, keyed by SDK
// directory name.
func readSDKInfos(dir string) (map[string]sdkInfo, error) {
	sdksDir := filepath.Join(dir, "sdks")
	entries, err := os.ReadDir(sdksDir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	result := make(map[string]sdkInfo, len(entries))
	var errs []error
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		path := filepath.Join(sdksDir, entry.Name(), "hcl.sdk.json")
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		info, err := parseSDKInfo(path, data)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		result[entry.Name()] = info
	}
	return result, errors.Join(errs...)
}

// parseSDKInfo decodes the contents of one hcl.sdk.json; path only labels errors.
func parseSDKInfo(path string, data []byte) (sdkInfo, error) {
	var info sdkInfo
	var stamp struct {
		Source string `json:"source"`
	}
	if err := json.Unmarshal(data, &info.desc); err != nil {
		return sdkInfo{}, fmt.Errorf("%q: %w", path, err)
	}
	if err := json.Unmarshal(data, &stamp); err != nil {
		return sdkInfo{}, fmt.Errorf("%q: %w", path, err)
	}
	info.source = stamp.Source
	return info, nil
}

// sdkPackageName returns the package an SDK serves: the name of the schema
// it was generated from.
func sdkPackageName(desc workspace.PackageDescriptor) string {
	if desc.Parameterization != nil {
		return desc.Parameterization.Name
	}
	if desc.ExtensionParameterization != nil {
		return desc.ExtensionParameterization.Name
	}
	return desc.Name
}

// descriptorForPulumiPackage returns the SDK descriptor serving a Pulumi
// package and its sdks/ directory. The directory may be namespace-prefixed
// (sdks/<namespace>-<name>), so match on the descriptor. A terraform-provider
// descriptor serves a non-Pulumi source that happens to share the name.
func descriptorForPulumiPackage(
	name string, sdks map[string]sdkInfo,
) (string, workspace.PackageDescriptor, bool) {
	for _, dir := range sortedKeys(sdks) {
		desc := sdks[dir].desc
		if desc.Name != bridgePackageName && sdkPackageName(desc) == name {
			return dir, desc, true
		}
	}
	return "", workspace.PackageDescriptor{}, false
}

// sdkDescriptors projects sdkInfos down to the descriptor map the schema
// loader and run engine consume, keyed by package name.
func sdkDescriptors(infos map[string]sdkInfo) map[string]workspace.PackageDescriptor {
	descs := make(map[string]workspace.PackageDescriptor, len(infos))
	for _, info := range infos {
		descs[sdkPackageName(info.desc)] = info.desc
	}
	return descs
}

// missingNonPulumiSDKs returns the sorted non-Pulumi provider sources used
// by config (and its transitively-loaded modules) that no on-disk SDK
// satisfies. Empty workDir skips module recursion.
func missingNonPulumiSDKs(
	ctx context.Context, config *ast.Config, sdks map[string]sdkInfo, workDir string,
) []string {
	tfReqs, _, _ := collectRequirements(ctx, modules.NewLoader(modules.LiveResolver(ctx)), config, workDir)
	var missing []string
	for _, source := range sortedKeys(tfReqs) {
		if _, _, ok := descriptorForSource(source, sdks); !ok {
			missing = append(missing, tfReqs[source].display)
		}
	}
	return missing
}

func isBuiltinProvider(alias string) bool { return alias == "pulumi" || alias == "terraform" }

// tfRequirement accumulates what the module tree declares for one canonical
// provider source: the constraint union, and the shortest spelling seen for
// specs and error messages.
type tfRequirement struct {
	display     string
	constraints map[string]struct{}
}

// constraint joins the declared constraints in sorted order — deterministic
// regardless of walk order — the way tofu renders a merged requirement
// (", "-separated). The terraform-provider plugin intersects them at resolve
// time, erroring on an empty intersection just as tofu does.
func (r *tfRequirement) constraint() string { return strings.Join(sortedKeys(r.constraints), ", ") }

// collectRequirements walks config (recursing through `module` blocks when
// workDir is non-empty) and resolves every provider it references to a
// fully-qualified source, mirroring tofu's resolution. It returns:
//   - tf: non-Pulumi requirements keyed by canonical source;
//   - pulumi: pulumi/<name>-sourced packages mapped to their version;
//   - aliases: every provider local name referenced, mapped to its resolved
//     required_providers entry (nil for implicit providers), builtins
//     included. First declared entry wins across modules — key requirements
//     off tf/pulumi, never off aliases.
//
// Each local name is resolved to its source within the module that uses it (its
// own required_providers, else the "hashicorp/<name>" default), so a provider
// declared only in a child module still resolves from its declared source.
func collectRequirements(
	ctx context.Context, loader *modules.Loader, config *ast.Config, workDir string,
) (tf map[string]*tfRequirement, pulumi map[string]string, aliases map[string]*ast.RequiredProvider) {
	tf = map[string]*tfRequirement{}
	pulumi = map[string]string{}
	aliases = map[string]*ast.RequiredProvider{}
	collectRequirementsRec(ctx, config, workDir, tf, pulumi, aliases, loader, map[string]struct{}{})
	return tf, pulumi, aliases
}

func collectRequirementsRec(
	ctx context.Context,
	config *ast.Config, workDir string,
	tf map[string]*tfRequirement, pulumi map[string]string, aliases map[string]*ast.RequiredProvider,
	loader *modules.Loader, visited map[string]struct{},
) {
	if config == nil {
		return
	}
	required := map[string]*ast.RequiredProvider{}
	if config.Terraform != nil {
		required = config.Terraform.RequiredProviders
	}

	// known scopes resource/data type splitting to the providers this module
	// declares so that local names containing underscores (e.g. "snake_names")
	// aren't mis-split at the first underscore. Undeclared (implicit) providers
	// fall back to the first underscore-delimited segment inside PackageFromToken.
	known := make([]string, 0, len(required)+len(config.Providers))
	for alias := range required {
		known = append(known, alias)
	}
	for _, p := range config.Providers {
		known = append(known, p.Name)
	}

	add := func(alias string) {
		req := required[alias]
		if _, seen := aliases[alias]; !seen || req != nil {
			aliases[alias] = req
		}
		if isBuiltinProvider(alias) {
			return
		}
		if req.IsPulumi() {
			pulumi[packageName(alias, req.Source)] = req.Version
			return
		}
		source := tfProviderSource(alias, req)
		key := canonicalSource(source)
		r := tf[key]
		if r == nil {
			r = &tfRequirement{display: source, constraints: map[string]struct{}{}}
			tf[key] = r
		} else if len(source) < len(r.display) || (len(source) == len(r.display) && source < r.display) {
			r.display = source
		}
		if req != nil && req.Version != "" {
			r.constraints[req.Version] = struct{}{}
		}
	}

	for alias := range required {
		add(alias)
	}
	for _, p := range config.Providers {
		add(p.Name)
	}
	addType := func(tfType string) {
		if name, err := packages.PackageFromToken(known, tfType); err == nil && name != "" {
			add(name)
		}
	}
	for _, r := range config.Resources {
		addType(r.Type)
	}
	for _, d := range config.DataSources {
		addType(d.Type)
		// terraform_remote_state is served by the external pulumi-terraform
		// package, not the builtin `terraform` provider that add() skips.
		if d.Type == "terraform_remote_state" {
			pulumi[run.TerraformStatePackage] = run.TerraformStatePackageVersion
		}
	}
	// A removed block's provisioners resolve the removed resource's schema at
	// run time, so its provider is required even with no resource block left.
	// A module-prefixed target may name a module call that is itself gone, so
	// the type falls back to this config's provider names.
	for _, rem := range config.Removed {
		if rem.From.Type != "" {
			addType(rem.From.Type)
		}
	}

	if loader == nil {
		return
	}
	// Iterate in a stable order: this walk drives the archive layout when a module
	// is bundled for parameterization, so a map's random order would make the
	// bundle non-deterministic.
	for _, name := range slices.Sorted(maps.Keys(config.Modules)) {
		mod := config.Modules[name]
		if mod.Source == "" {
			continue
		}
		loaded, err := loader.LoadModule(ctx, mod.Source, mod.Version, workDir)
		if err != nil {
			// `pulumi install` surfaces a concrete error later; don't block on it here.
			logging.V(5).Infof("collectRequirements: loading module %q: %v", mod.Source, err)
			continue
		}
		if _, seen := visited[loaded.SourcePath]; seen {
			continue
		}
		visited[loaded.SourcePath] = struct{}{}
		collectRequirementsRec(ctx, loaded.Config, loaded.SourcePath, tf, pulumi, aliases, loader, visited)
	}
}

func sortedKeys[K cmp.Ordered, V any](m map[K]V) []K { return slices.Sorted(maps.Keys(m)) }

// Run executes an HCL program.
func (host *LanguageHost) Run(
	ctx context.Context,
	req *pulumirpc.RunRequest,
) (*pulumirpc.RunResponse, error) {
	logging.V(5).Infof("Run: program=%s, pwd=%s, stack=%s, project=%s",
		req.Info.EntryPoint, req.Pwd, req.Stack, req.Project)

	// Connect to the resource monitor
	monitorConn, err := grpc.NewClient(
		req.MonitorAddress,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		rpcutil.GrpcChannelOptions(),
	)
	if err != nil {
		return nil, fmt.Errorf("connecting to resource monitor: %w", err)
	}
	defer contract.IgnoreClose(monitorConn)

	// Create the resource monitor and engine clients
	monitorClient := pulumirpc.NewResourceMonitorClient(monitorConn)
	resmon := &resourceMonitorAdapter{
		monitorClient: monitorClient,
		engineClient:  host.engine,
		ctx:           ctx,
		stack:         req.Stack,
		project:       req.Project,
	}

	// Parse the HCL program
	p := parser.NewParser()
	config, diags := p.ParseDirectory(req.Info.ProgramDirectory)
	if diags.HasErrors() {
		return &pulumirpc.RunResponse{
			Error: diags.Error(),
		}, nil
	}

	if config.Terraform != nil && (config.Terraform.Component != nil || config.Terraform.Package != nil) {
		return &pulumirpc.RunResponse{
			Error: "pulumi.component and pulumi.package blocks are only valid in " +
				"multi-language component modules, not in regular programs",
		}, nil
	}

	secretConfigKeys := make(map[string]bool, len(req.ConfigSecretKeys))
	for _, k := range req.ConfigSecretKeys {
		secretConfigKeys[k] = true
	}
	configMap := make(map[string]run.ConfigValue, len(req.Config))
	for k, v := range req.Config {
		configMap[k] = run.UntypedConfigValue(v, secretConfigKeys[k])
	}

	schemaLoader, err := schema.NewLoaderClient(req.LoaderTarget)
	if err != nil {
		return nil, fmt.Errorf("unable to acquire gRPC schema loader: %w", err)
	}

	sdkInfos, err := readSDKInfos(req.Info.ProgramDirectory)
	if err != nil {
		return nil, fmt.Errorf("unable to read parameterization: %w", err)
	}
	paramDescriptors := sdkDescriptors(sdkInfos)

	if missing := missingNonPulumiSDKs(ctx, config, sdkInfos, req.Info.ProgramDirectory); len(missing) > 0 {
		return &pulumirpc.RunResponse{
			Error: fmt.Sprintf(
				"missing local SDK for non-Pulumi provider(s) %v; run `pulumi install` to fetch them",
				missing),
		}, nil
	}

	// Cache the underlying loader, then wrap with the parameterization-aware
	// loader so it stays the outermost layer — the resolver type-asserts it to
	// discover extension schemas that a token's base package omits.
	loader := schema.ReferenceLoader(schema.NewCachedLoader(schemaLoader))
	if len(paramDescriptors) > 0 {
		loader = packages.NewParameterizationAwareLoader(loader, paramDescriptors)
	}

	req.Parallel = max(req.Parallel, 1) // (req.Parallel <= 1) => serial

	if req.MapperTarget == "" {
		return nil, errors.New("Run: missing mapper_target; the Pulumi engine must supply a mapper server " +
			"so HCL can resolve TF provider mappings (requires pulumi >= v3.243)")
	}
	mapperClient, err := convert.NewMapperClient(req.MapperTarget)
	if err != nil {
		return nil, fmt.Errorf("dial mapper at %s: %w", req.MapperTarget, err)
	}
	providerInfoSource := bridge.NewCache(bridge.NewMapperSource(mapperClient))

	// Create and run the engine
	engine, err := run.NewEngine(ctx, config, &run.EngineOptions{
		ProjectName:             req.Project,
		StackName:               req.Stack,
		Organization:            req.Organization,
		Config:                  configMap,
		DryRun:                  req.DryRun,
		DestroyDispatcher:       run.NewDestroyDispatcher(),
		ResourceMonitor:         resmon,
		SchemaLoader:            loader,
		ProviderInfoSource:      providerInfoSource,
		WorkDir:                 req.Info.ProgramDirectory,
		RootDir:                 req.Info.RootDirectory,
		RootModule:              true,
		ModuleLoader:            modules.NewLoader(modules.LiveResolver(ctx)),
		Packages:                paramDescriptors,
		Parallel:                int(req.Parallel),
		AlwaysRegisterProviders: host.alwaysRegisterProviders,
	})
	if err != nil {
		return &pulumirpc.RunResponse{
			Error: err.Error(),
		}, nil
	}

	if err := engine.Run(ctx); err != nil {
		return &pulumirpc.RunResponse{
			Error: err.Error(),
		}, nil
	}

	return &pulumirpc.RunResponse{}, nil
}

// GetPluginInfo returns information about this language plugin.
func (host *LanguageHost) GetPluginInfo(
	ctx context.Context,
	req *emptypb.Empty,
) (*pulumirpc.PluginInfo, error) {
	return &pulumirpc.PluginInfo{
		Version: version.Version().String(),
	}, nil
}

// InstallDependencies installs dependencies for an HCL program.
func (host *LanguageHost) InstallDependencies(
	req *pulumirpc.InstallDependenciesRequest,
	server pulumirpc.LanguageRuntime_InstallDependenciesServer,
) error {
	// Provider plugins are installed by the CLI from the specs
	// GetRequiredPackages emits; there is nothing else to install.
	return nil
}

// RuntimeOptionsPrompts returns prompts for runtime options during `pulumi new`.
func (host *LanguageHost) RuntimeOptionsPrompts(
	ctx context.Context,
	req *pulumirpc.RuntimeOptionsRequest,
) (*pulumirpc.RuntimeOptionsResponse, error) {
	return &pulumirpc.RuntimeOptionsResponse{
		Prompts: []*pulumirpc.RuntimeOptionPrompt{},
	}, nil
}

// About returns information about the HCL runtime.
func (host *LanguageHost) About(
	ctx context.Context,
	req *pulumirpc.AboutRequest,
) (*pulumirpc.AboutResponse, error) {
	return &pulumirpc.AboutResponse{
		Executable: "pulumi-language-hcl",
		Version:    version.Version().String(),
	}, nil
}

// GetProgramDependencies returns the dependencies of an HCL program.
func (host *LanguageHost) GetProgramDependencies(
	ctx context.Context,
	req *pulumirpc.GetProgramDependenciesRequest,
) (*pulumirpc.GetProgramDependenciesResponse, error) {
	logging.V(5).Infof("GetProgramDependencies: program=%s", req.Info.ProgramDirectory)

	// Parse HCL files to extract provider dependencies
	p := parser.NewParser()
	config, diags := p.ParseDirectory(req.Info.ProgramDirectory)
	if diags.HasErrors() {
		return &pulumirpc.GetProgramDependenciesResponse{
			Dependencies: []*pulumirpc.DependencyInfo{},
		}, nil
	}

	var deps []*pulumirpc.DependencyInfo

	// Extract dependencies from pulumi.required_providers
	if config.Terraform != nil {
		for name, provider := range config.Terraform.RequiredProviders {
			dep := &pulumirpc.DependencyInfo{
				Name: name,
			}

			if provider.Version != "" {
				dep.Version = provider.Version
			}

			if provider.Source != "" {
				parts := strings.Split(provider.Source, "/")
				if len(parts) >= 2 {
					dep.Name = parts[len(parts)-1]
				}
			}

			deps = append(deps, dep)
		}
	}

	return &pulumirpc.GetProgramDependenciesResponse{
		Dependencies: deps,
	}, nil
}

// RunPlugin runs a plugin program (for component providers).
// This allows HCL modules to be consumed as component resources from other languages.
func (host *LanguageHost) RunPlugin(
	req *pulumirpc.RunPluginRequest,
	server pulumirpc.LanguageRuntime_RunPluginServer,
) error {
	logging.V(5).Infof("RunPlugin: pwd=%s args=%v", req.Pwd, req.Args)

	// Get the module path from the request
	modulePath := req.Pwd
	if req.Info != nil && req.Info.ProgramDirectory != "" {
		modulePath = req.Info.ProgramDirectory
	}

	// Create the provider (name and version are derived from the module's terraform {} block)
	provider, err := NewLocalProvider(server.Context(), modulePath, req.LoaderTarget)
	if err != nil {
		errBytes := fmt.Appendf(nil, "Error creating provider: %v\n", err)
		if err := server.Send(&pulumirpc.RunPluginResponse{
			Output: &pulumirpc.RunPluginResponse_Stderr{Stderr: errBytes},
		}); err != nil {
			return err
		}
		return server.Send(&pulumirpc.RunPluginResponse{
			Output: &pulumirpc.RunPluginResponse_Exitcode{Exitcode: 1},
		})
	}

	// Create gRPC server
	grpcServer := grpc.NewServer()
	pulumirpc.RegisterResourceProviderServer(grpcServer, provider)

	// Listen on a random port
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		errBytes := fmt.Appendf(nil, "Error listening: %v\n", err)
		if err := server.Send(&pulumirpc.RunPluginResponse{
			Output: &pulumirpc.RunPluginResponse_Stderr{Stderr: errBytes},
		}); err != nil {
			return err
		}
		return server.Send(&pulumirpc.RunPluginResponse{
			Output: &pulumirpc.RunPluginResponse_Exitcode{Exitcode: 1},
		})
	}

	// Output the port for the engine to connect
	port := lis.Addr().(*net.TCPAddr).Port
	portMsg := fmt.Sprintf("%d\n", port)
	if err := server.Send(&pulumirpc.RunPluginResponse{
		Output: &pulumirpc.RunPluginResponse_Stdout{Stdout: []byte(portMsg)},
	}); err != nil {
		return err
	}

	// Start serving in a goroutine
	errCh := make(chan error, 1)
	go func() {
		errCh <- grpcServer.Serve(lis)
	}()

	// Wait for context cancellation or server error
	ctx := server.Context()
	select {
	case <-ctx.Done():
		grpcServer.GracefulStop()
	case err := <-errCh:
		if err != nil {
			errBytes := fmt.Appendf(nil, "Server error: %v\n", err)
			if err := server.Send(&pulumirpc.RunPluginResponse{
				Output: &pulumirpc.RunPluginResponse_Stderr{Stderr: errBytes},
			}); err != nil {
				return err
			}
		}
	}

	return server.Send(&pulumirpc.RunPluginResponse{
		Output: &pulumirpc.RunPluginResponse_Exitcode{Exitcode: 0},
	})
}

// GenerateProgram generates an HCL program from a PCL program.
func (host *LanguageHost) GenerateProgram(
	ctx context.Context,
	req *pulumirpc.GenerateProgramRequest,
) (*pulumirpc.GenerateProgramResponse, error) {
	if len(req.Source) == 0 {
		return &pulumirpc.GenerateProgramResponse{
			Source: map[string][]byte{"main.tf": {}},
		}, nil
	}

	// Write source files to a temp directory so that BindDirectory can resolve
	// component references (which require a directory path on the filesystem).
	tmpDir, err := os.MkdirTemp("", "hcl-generate-*")
	if err != nil {
		return nil, fmt.Errorf("creating temp directory: %w", err)
	}
	defer contract.IgnoreError(os.RemoveAll(tmpDir))
	for k, v := range req.Source {
		p := filepath.Join(tmpDir, k)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return nil, fmt.Errorf("creating directory for %s: %w", k, err)
		}
		if err := os.WriteFile(p, []byte(v), 0o644); err != nil {
			return nil, fmt.Errorf("writing %s: %w", k, err)
		}
	}

	loaderClient, err := schema.NewLoaderClient(req.LoaderTarget)
	if err != nil {
		return nil, fmt.Errorf("unable to aquire loader: %w", err)
	}
	var binderOpts []pcl.BindOption
	if !req.Strict {
		binderOpts = append(binderOpts, pcl.NonStrictBindOptions()...)
	}
	program, bindDiags, err := pcl.BindDirectory(tmpDir, schema.NewCachedLoader(loaderClient), binderOpts...)
	if err != nil {
		return nil, fmt.Errorf("binding program: %w", err)
	}
	if bindDiags.HasErrors() {
		return &pulumirpc.GenerateProgramResponse{
			Diagnostics: plugin.HclDiagnosticsToRPCDiagnostics(bindDiags),
		}, nil
	}

	files, genDiags, err := codegen.GenerateProgram(program)
	if err != nil {
		return nil, fmt.Errorf("generating program: %w", err)
	}

	// Include sdks/<alias>/hcl.sdk.json for parameterized packages so that
	// ConvertProgram can load their schemas when the round-trip test writes these
	// files to a temp directory and then calls ConvertProgram on that directory.
	for _, ref := range program.PackageReferences() {
		if ref.Name() == "pulumi" {
			continue
		}
		pkg, pkgErr := ref.Definition()
		if pkgErr != nil || pkg.Parameterization == nil {
			continue
		}
		baseVersion := pkg.Parameterization.BasePlugin.Version
		var paramVersion semver.Version
		if pkg.Version != nil {
			paramVersion = *pkg.Version
		}
		desc := workspace.PackageDescriptor{
			PluginDescriptor: workspace.PluginDescriptor{
				Name:    pkg.Parameterization.BasePlugin.Name,
				Version: &baseVersion,
				Kind:    apitype.ResourcePlugin,
			},
			Parameterization: &workspace.Parameterization{
				Name:    pkg.Name,
				Version: paramVersion,
				Value:   pkg.Parameterization.Parameter,
			},
		}
		data, marshalErr := json.Marshal(desc)
		if marshalErr != nil {
			continue
		}
		files["sdks/"+pkg.Name+"/hcl.sdk.json"] = data
	}

	return &pulumirpc.GenerateProgramResponse{
		Diagnostics: plugin.HclDiagnosticsToRPCDiagnostics(genDiags),
		Source:      files,
	}, nil
}

// GenerateProject generates a complete HCL project.
func (host *LanguageHost) GenerateProject(
	ctx context.Context,
	req *pulumirpc.GenerateProjectRequest,
) (*pulumirpc.GenerateProjectResponse, error) {
	logging.V(5).Infof("GenerateProject: sourceDirectory=%s, targetDirectory=%s",
		req.SourceDirectory, req.TargetDirectory)

	loaderClient, err := schema.NewLoaderClient(req.LoaderTarget)
	if err != nil {
		return nil, fmt.Errorf("unable to aquire loader: %w", err)
	}
	var binderOpts []pcl.BindOption
	if !req.Strict {
		binderOpts = append(binderOpts, pcl.NonStrictBindOptions()...)
	}

	var project workspace.Project
	if err := json.Unmarshal([]byte(req.Project), &project); err != nil {
		return nil, err
	}

	project.Runtime = workspace.NewProjectRuntimeInfo("hcl", nil)

	projectBytes, err := pulumiencoding.YAML.Marshal(project)
	if err != nil {
		return nil, err
	}

	if err := os.MkdirAll(req.TargetDirectory, 0o755); err != nil {
		return nil, fmt.Errorf("create target directory: %w", err)
	}

	if err := os.WriteFile(filepath.Join(req.TargetDirectory, "Pulumi.yaml"), projectBytes, 0o600); err != nil {
		return nil, fmt.Errorf("write Pulumi.yaml: %w", err)
	}

	// Determine where to write program files. When the project specifies a
	// "main" subdirectory, generated code goes into that subdirectory.
	programDir := req.TargetDirectory
	if project.Main != "" {
		programDir = filepath.Join(req.TargetDirectory, project.Main)
		if err := os.MkdirAll(programDir, 0o755); err != nil {
			return nil, fmt.Errorf("creating main directory: %w", err)
		}

	}

	program, bindDiags, err := pcl.BindDirectory(req.SourceDirectory, schema.NewCachedLoader(loaderClient), binderOpts...)
	if err != nil {
		return nil, fmt.Errorf("binding directory: %w", err)
	}
	if bindDiags.HasErrors() {
		return &pulumirpc.GenerateProjectResponse{
			Diagnostics: plugin.HclDiagnosticsToRPCDiagnostics(bindDiags),
		}, nil
	}

	files, genDiags, err := codegen.GenerateProgram(program)
	if err != nil {
		return nil, fmt.Errorf("generating program: %w", err)
	}

	for name, content := range files {
		path := filepath.Join(programDir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, fmt.Errorf("creating directory for %s: %w", name, err)
		}
		if err := os.WriteFile(path, content, 0o644); err != nil {
			return nil, fmt.Errorf("writing %s: %w", name, err)
		}
	}

	// For each parameterized local dependency, store the hcl.sdk.json so that
	// GetRequiredPackages and Run can find the parameterization info later.
	for alias, artifactPath := range req.LocalDependencies {
		data, err := os.ReadFile(filepath.Join(artifactPath, "hcl.sdk.json"))
		if err != nil {
			continue
		}
		var desc workspace.PackageDescriptor
		if err := json.Unmarshal(data, &desc); err != nil {
			continue
		}
		if desc.Parameterization == nil && desc.ExtensionParameterization == nil {
			continue
		}
		sdkDir := filepath.Join(programDir, "sdks", alias)
		if err := os.MkdirAll(sdkDir, 0o755); err != nil {
			return nil, fmt.Errorf("creating sdk dir for %s: %w", alias, err)
		}
		if err := os.WriteFile(filepath.Join(sdkDir, "hcl.sdk.json"), data, 0o644); err != nil {
			return nil, fmt.Errorf("writing hcl.sdk.json for %s: %w", alias, err)
		}
	}

	return &pulumirpc.GenerateProjectResponse{
		Diagnostics: plugin.HclDiagnosticsToRPCDiagnostics(genDiags),
	}, nil
}

// GeneratePackage generates SDK bindings for a Pulumi package.
//
// HCL doesn't need generated SDKs — it uses provider schemas directly. However,
// we write an hcl.sdk.json file containing the package descriptor so that
// GetRequiredPackages can discover which packages a project depends on.
func (host *LanguageHost) GeneratePackage(
	ctx context.Context,
	req *pulumirpc.GeneratePackageRequest,
) (*pulumirpc.GeneratePackageResponse, error) {
	desc, err := packageDescriptorFromSchema([]byte(req.Schema))
	if err != nil {
		return nil, fmt.Errorf("parsing schema for package descriptor: %w", err)
	}

	out := struct {
		workspace.PackageDescriptor
		Source string `json:"source,omitempty"`
	}{desc, bridgeRemoteSource(desc)}

	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshaling package descriptor: %w", err)
	}
	data = append(data, '\n')

	if err := os.WriteFile(filepath.Join(req.Directory, "hcl.sdk.json"), data, 0o644); err != nil {
		return nil, fmt.Errorf("writing hcl.sdk.json: %w", err)
	}

	return &pulumirpc.GeneratePackageResponse{}, nil
}

// packageDescriptorFromSchema extracts a workspace.PackageDescriptor from a JSON
// schema blob. This mirrors the logic in the test framework's interface.go that
// builds expected PackageDescriptors from schema Package definitions.
func packageDescriptorFromSchema(schemaJSON []byte) (workspace.PackageDescriptor, error) {
	var spec schema.PartialPackageSpec
	if err := json.Unmarshal(schemaJSON, &spec); err != nil {
		return workspace.PackageDescriptor{}, fmt.Errorf("unmarshaling schema: %w", err)
	}

	desc := workspace.PackageDescriptor{
		PluginDescriptor: workspace.PluginDescriptor{
			Name:              spec.Name,
			Kind:              apitype.ResourcePlugin,
			PluginDownloadURL: spec.PluginDownloadURL,
		},
	}

	if spec.Version != "" {
		v, err := semver.Parse(spec.Version)
		if err != nil {
			return workspace.PackageDescriptor{}, fmt.Errorf("parsing version %q: %w", spec.Version, err)
		}
		desc.Version = &v
	}

	if spec.Parameterization != nil {
		baseVersion, err := semver.Parse(spec.Parameterization.BaseProvider.Version)
		if err != nil {
			return workspace.PackageDescriptor{}, fmt.Errorf(
				"parsing base provider version %q: %w", spec.Parameterization.BaseProvider.Version, err)
		}
		desc.Parameterization = &workspace.Parameterization{
			Name:    desc.Name,
			Version: *desc.Version,
			Value:   spec.Parameterization.Parameter,
		}
		desc.Name = spec.Parameterization.BaseProvider.Name
		desc.Version = &baseVersion
	}

	// An extension parameterizes a base provider without replacing it: its
	// resource tokens live in the base provider's namespace, so the descriptor
	// names the base provider and carries the extension in the extension slot.
	if spec.ExtensionParameterization != nil {
		baseVersion, err := semver.Parse(spec.ExtensionParameterization.BaseProvider.Version)
		if err != nil {
			return workspace.PackageDescriptor{}, fmt.Errorf(
				"parsing base provider version %q: %w", spec.ExtensionParameterization.BaseProvider.Version, err)
		}
		desc.ExtensionParameterization = &workspace.Parameterization{
			Name:    desc.Name,
			Version: *desc.Version,
			Value:   spec.ExtensionParameterization.Parameter,
		}
		desc.Name = spec.ExtensionParameterization.BaseProvider.Name
		desc.Version = &baseVersion
	}

	return desc, nil
}

// Pack packages an HCL program into a deployable artifact.
func (host *LanguageHost) Pack(
	ctx context.Context,
	req *pulumirpc.PackRequest,
) (*pulumirpc.PackResponse, error) {
	logging.V(5).Infof("Pack: packageDirectory=%s, destinationDirectory=%s",
		req.PackageDirectory, req.DestinationDirectory)

	// Create a named subdirectory within the destination so that multiple packages packed into
	// the same destination directory don't overwrite each other's files.
	pkgName := filepath.Base(req.PackageDirectory)
	artifactPath := filepath.Join(req.DestinationDirectory, pkgName+".sdk")
	if err := os.MkdirAll(artifactPath, 0o755); err != nil {
		return nil, fmt.Errorf("creating artifact directory: %w", err)
	}

	if err := fsutil.CopyFile(artifactPath, req.PackageDirectory, nil); err != nil {
		return nil, err
	}

	return &pulumirpc.PackResponse{
		ArtifactPath: artifactPath,
	}, nil
}

// Link has no project file to edit; it reports the `required_providers`
// entries for linked SDKs the program does not reference yet.
func (host *LanguageHost) Link(
	ctx context.Context,
	req *pulumirpc.LinkRequest,
) (*pulumirpc.LinkResponse, error) {
	tfReqs, pulumiPkgs, err := programRequirements(ctx, req.Info.ProgramDirectory)
	if err != nil {
		return nil, err
	}
	f := hclwrite.NewEmptyFile()
	reqProviders := f.Body().AppendNewBlock("terraform", nil).Body().AppendNewBlock("required_providers", nil)
	unreferenced := 0
	for _, dep := range req.Packages {
		if dep.Package.GetName() == "pulumi" { // built in; no hcl.sdk.json
			continue
		}
		path := filepath.Join(req.Info.RootDirectory, dep.Path, "hcl.sdk.json")
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		info, err := parseSDKInfo(path, data)
		if err != nil {
			return nil, err
		}
		source := requiredProviderSource(info)
		name := packageName("", source)
		if _, ok := tfReqs[canonicalSource(source)]; ok && info.source != "" {
			continue
		}
		if _, ok := pulumiPkgs[name]; ok && info.source == "" {
			continue
		}
		unreferenced++
		reqProviders.Body().SetAttributeValue(name, cty.ObjectVal(map[string]cty.Value{
			"source": cty.StringVal(source),
		}))
	}
	switch unreferenced {
	case 0:
		return &pulumirpc.LinkResponse{}, nil
	case 1:
		return &pulumirpc.LinkResponse{
			ImportInstructions: "You can use the package in your HCL program with:\n\n" + string(f.Bytes()),
		}, nil
	default:
		return &pulumirpc.LinkResponse{
			ImportInstructions: "You can use the packages in your HCL program with:\n\n" + string(f.Bytes()),
		}, nil
	}
}

// requiredProviderSource returns the `source` that resolves to the SDK
// described by info.
func requiredProviderSource(info sdkInfo) string {
	if info.source != "" {
		return info.source
	}
	if p := info.desc.Parameterization; p != nil {
		return "pulumi/" + p.Name
	}
	return "pulumi/" + info.desc.Name
}

// Ensure resourceMonitorAdapter implements the interface.
var _ run.ResourceMonitor = (*resourceMonitorAdapter)(nil)

// resourceMonitorAdapter adapts the Pulumi gRPC resource monitor to our interface.
type resourceMonitorAdapter struct {
	monitorClient pulumirpc.ResourceMonitorClient
	engineClient  pulumirpc.EngineClient
	ctx           context.Context

	stack   string
	project string

	hookMu  sync.Mutex
	hookCBS *callbackServer
}

// ResolveURN replicates the engine's URN generation; the langhost path
// registers names verbatim.
func (r *resourceMonitorAdapter) ResolveURN(parent urn.URN, token, name string) (urn.URN, string) {
	parentType := tokens.Type("")
	if parent != "" && parent.QualifiedType() != resource.RootStackType {
		parentType = parent.QualifiedType()
	}
	return urn.New(tokens.QName(r.stack), tokens.PackageName(r.project), parentType, tokens.Type(token), name), name
}

// RegisterPackage registers a parameterized package with the engine.
func (r *resourceMonitorAdapter) RegisterPackage(
	ctx context.Context,
	pkg workspace.PackageDescriptor,
) (run.PackageRef, error) {
	return registerPackage(ctx, r.monitorClient, pkg)
}

// registerPackage registers a parameterized package with the engine via the
// resource monitor and returns the ref that routes subsequent resource
// registrations to the matching provider instance. Shared by the Run-path
// monitor and the Construct-path monitor so a component's bridged providers
// register the same way a root program's do.
func registerPackage(
	ctx context.Context,
	client pulumirpc.ResourceMonitorClient,
	pkg workspace.PackageDescriptor,
) (run.PackageRef, error) {
	versionStr := ""
	if pkg.Version != nil {
		versionStr = pkg.Version.String()
	}
	req := &pulumirpc.RegisterPackageRequest{
		Name:    pkg.Name,
		Version: versionStr,
	}
	if pkg.Parameterization != nil {
		req.Parameterization = &pulumirpc.Parameterization{
			Name:    pkg.Parameterization.Name,
			Version: pkg.Parameterization.Version.String(),
			Value:   pkg.Parameterization.Value,
		}
	}
	if pkg.ExtensionParameterization != nil {
		req.Extension = &pulumirpc.Parameterization{
			Name:    pkg.ExtensionParameterization.Name,
			Version: pkg.ExtensionParameterization.Version.String(),
			Value:   pkg.ExtensionParameterization.Value,
		}
	}
	resp, err := client.RegisterPackage(ctx, req)
	if err != nil {
		return "", fmt.Errorf("registering package %s: %w", pkg.Name, err)
	}
	return run.PackageRef(resp.Ref), nil
}

// globsToPropertyPaths marshals property globs to the dotted/bracketed
// property-path strings the resource monitor protocol expects. The globs are
// kept in their structured form through the engine and only flattened here, at
// the wire boundary.
func globsToPropertyPaths(globs []property.Glob) ([]string, error) {
	if len(globs) == 0 {
		return nil, nil
	}
	paths := make([]string, len(globs))
	for i, g := range globs {
		text, err := g.MarshalText()
		if err != nil {
			return nil, err
		}
		paths[i] = string(text)
	}
	return paths, nil
}

// RegisterResource registers a resource with Pulumi.
func (r *resourceMonitorAdapter) RegisterResource(
	ctx context.Context,
	req run.RegisterResourceRequest,
) (*run.RegisterResourceResponse, error) {
	// Convert inputs to protobuf struct
	inputsStruct, err := plugin.MarshalProperties(resource.ToResourcePropertyMap(req.Inputs), r.pluginOptions())
	if err != nil {
		return nil, fmt.Errorf("marshaling inputs: %w", err)
	}

	var aliases []*pulumirpc.Alias
	for _, a := range req.Aliases {
		if a.URN != "" {
			aliases = append(aliases, &pulumirpc.Alias{
				Alias: &pulumirpc.Alias_Urn{Urn: a.URN},
			})
		} else if a.Spec != nil {
			spec := &pulumirpc.Alias_Spec{
				Name:    a.Spec.Name,
				Type:    a.Spec.Type,
				Stack:   a.Spec.Stack,
				Project: a.Spec.Project,
			}
			if a.Spec.NoParent {
				spec.Parent = &pulumirpc.Alias_Spec_NoParent{NoParent: true}
			} else if a.Spec.ParentURN != "" {
				spec.Parent = &pulumirpc.Alias_Spec_ParentUrn{ParentUrn: a.Spec.ParentURN}
			}
			aliases = append(aliases, &pulumirpc.Alias{
				Alias: &pulumirpc.Alias_Spec_{Spec: spec},
			})
		}
	}

	// Convert PropertyDependencies to protobuf format
	propDeps := make(map[string]*pulumirpc.RegisterResourceRequest_PropertyDependencies)
	for prop, urns := range req.PropertyDependencies {
		propDeps[prop] = &pulumirpc.RegisterResourceRequest_PropertyDependencies{
			Urns: urns,
		}
	}

	ignoreChanges, err := globsToPropertyPaths(req.IgnoreChanges)
	if err != nil {
		return nil, fmt.Errorf("marshaling ignoreChanges: %w", err)
	}
	hideDiffs, err := globsToPropertyPaths(req.HideDiffs)
	if err != nil {
		return nil, fmt.Errorf("marshaling hideDiffs: %w", err)
	}
	replaceOnChanges, err := globsToPropertyPaths(req.ReplaceOnChanges)
	if err != nil {
		return nil, fmt.Errorf("marshaling replaceOnChanges: %w", err)
	}

	// Build the registration request
	registerReq := &pulumirpc.RegisterResourceRequest{
		Type:                       req.Type,
		Name:                       req.Name,
		Custom:                     req.Custom,
		Remote:                     req.Remote,
		Object:                     inputsStruct,
		Protect:                    &req.Protect,
		Dependencies:               req.Dependencies,
		PropertyDependencies:       propDeps,
		Provider:                   req.Provider,
		Providers:                  req.Providers,
		Parent:                     string(req.Parent),
		IgnoreChanges:              ignoreChanges,
		Aliases:                    aliases,
		AcceptSecrets:              true,
		AcceptResources:            true,
		AcceptsByteString:          true,
		SupportsPartialValues:      true,
		DeleteBeforeReplace:        req.DeleteBeforeReplace,
		DeleteBeforeReplaceDefined: req.DeleteBeforeReplaceDef,
		ImportId:                   req.ImportId,
		AdditionalSecretOutputs:    req.AdditionalSecretOutputs,
		RetainOnDelete:             req.RetainOnDelete,
		DeletedWith:                req.DeletedWith,
		ReplaceWith:                req.ReplaceWith,
		HideDiffs:                  hideDiffs,
		ReplaceOnChanges:           replaceOnChanges,
		EnvVarMappings:             req.EnvVarMappings,
		Version:                    req.Version,
		PluginDownloadURL:          req.PluginDownloadURL,
		PackageRef:                 string(req.PackageRef),
	}

	// Add custom timeouts if specified
	if req.CustomTimeouts != nil {
		registerReq.CustomTimeouts = &pulumirpc.RegisterResourceRequest_CustomTimeouts{
			Create: formatTimeoutSeconds(req.CustomTimeouts.Create),
			Read:   formatTimeoutSeconds(req.CustomTimeouts.Read),
			Update: formatTimeoutSeconds(req.CustomTimeouts.Update),
			Delete: formatTimeoutSeconds(req.CustomTimeouts.Delete),
		}
	}

	registerReq.Hooks = hooksToProto(req.Hooks)

	// Add replacement trigger if specified
	if !req.ReplacementTrigger.IsNull() {
		trigger, err := plugin.MarshalPropertyValue("replacement_trigger",
			resource.ToResourcePropertyValue(req.ReplacementTrigger),
			r.pluginOptions())
		if err != nil {
			return nil, fmt.Errorf("marshaling replacement trigger: %w", err)
		}
		registerReq.ReplacementTrigger = trigger
	}

	// Call the resource monitor
	resp, err := r.monitorClient.RegisterResource(ctx, registerReq)
	if err != nil {
		return nil, fmt.Errorf("registering resource: %w", err)
	}

	// Unmarshal outputs
	outputs, err := plugin.UnmarshalProperties(resp.Object, r.pluginOptions())
	if err != nil {
		return nil, fmt.Errorf("unmarshaling outputs: %w", err)
	}

	return &run.RegisterResourceResponse{
		URN:     urn.URN(resp.Urn),
		ID:      resp.Id,
		Outputs: resource.FromResourcePropertyMap(outputs),
		Unknown: resp.Unknown,
	}, nil
}

// ReadResource reads the state of an existing resource.
func (r *resourceMonitorAdapter) ReadResource(
	ctx context.Context,
	req run.ReadResourceRequest,
) (*run.ReadResourceResponse, error) {
	inputsStruct, err := plugin.MarshalProperties(resource.ToResourcePropertyMap(req.Inputs), r.pluginOptions())
	if err != nil {
		return nil, fmt.Errorf("marshaling inputs: %w", err)
	}

	resp, err := r.monitorClient.ReadResource(ctx, &pulumirpc.ReadResourceRequest{
		Id:                      req.ID,
		Type:                    req.Type,
		Name:                    req.Name,
		Parent:                  string(req.Parent),
		Properties:              inputsStruct,
		Dependencies:            req.Dependencies,
		Provider:                req.Provider,
		Version:                 req.Version,
		AcceptSecrets:           true,
		AdditionalSecretOutputs: req.AdditionalSecretOutputs,
		AcceptResources:         true,
		AcceptsByteString:       true,
		PluginDownloadURL:       req.PluginDownloadURL,
		PackageRef:              string(req.PackageRef),
	})
	if err != nil {
		return nil, fmt.Errorf("reading resource: %w", err)
	}

	outputs, err := plugin.UnmarshalProperties(resp.Properties, r.pluginOptions())
	if err != nil {
		return nil, fmt.Errorf("unmarshaling outputs: %w", err)
	}

	return &run.ReadResourceResponse{
		URN:     urn.URN(resp.Urn),
		ID:      req.ID,
		Outputs: resource.FromResourcePropertyMap(outputs),
	}, nil
}

// Invoke invokes a provider function.
func (r *resourceMonitorAdapter) Invoke(
	ctx context.Context,
	req run.InvokeRequest,
) (*run.InvokeResponse, error) {
	// Convert args to protobuf struct
	argsStruct, err := plugin.MarshalProperties(resource.ToResourcePropertyMap(req.Args), r.pluginOptions())
	if err != nil {
		return nil, fmt.Errorf("marshaling args: %w", err)
	}

	// Build the invoke request
	invokeReq := &pulumirpc.ResourceInvokeRequest{
		Tok:               req.Token,
		Args:              argsStruct,
		Provider:          req.Provider,
		Version:           req.Version,
		PluginDownloadURL: req.PluginDownloadURL,
		AcceptResources:   true,
		AcceptsByteString: true,
		PackageRef:        string(req.PackageRef),
		DependsOn:         req.DependsOn,
	}

	// Call the resource monitor
	resp, err := r.monitorClient.Invoke(ctx, invokeReq)
	if err != nil {
		return nil, fmt.Errorf("invoking function: %w", err)
	}

	// Unmarshal return value
	returnVal, err := plugin.UnmarshalProperties(resp.Return, r.pluginOptions())
	if err != nil {
		return nil, fmt.Errorf("unmarshaling return value: %w", err)
	}

	// Convert failures
	var failures []string
	for _, f := range resp.Failures {
		failures = append(failures, fmt.Sprintf("%s: %s", f.Property, f.Reason))
	}

	return &run.InvokeResponse{
		Return:   resource.FromResourcePropertyMap(returnVal),
		Failures: failures,
		Unknown:  resp.Unknown,
	}, nil
}

// RegisterResourceOutputs registers outputs on a resource (used for stack outputs).
func (r *resourceMonitorAdapter) RegisterResourceOutputs(
	ctx context.Context,
	urn urn.URN,
	outputs property.Map,
) error {
	// Convert outputs to protobuf struct
	outputsStruct, err := plugin.MarshalProperties(resource.ToResourcePropertyMap(outputs), r.pluginOptions())
	if err != nil {
		return fmt.Errorf("marshaling outputs: %w", err)
	}

	// Call the resource monitor
	_, err = r.monitorClient.RegisterResourceOutputs(ctx, &pulumirpc.RegisterResourceOutputsRequest{
		Urn:     string(urn),
		Outputs: outputsStruct,
	})
	if err != nil {
		return fmt.Errorf("registering resource outputs: %w", err)
	}

	return nil
}

// CheckPulumiVersion checks if the Pulumi CLI version satisfies the given version range.
func (r *resourceMonitorAdapter) CheckPulumiVersion(ctx context.Context, versionRange string) error {
	// Call the engine's RequirePulumiVersion RPC method
	_, err := r.engineClient.RequirePulumiVersion(ctx, &pulumirpc.RequirePulumiVersionRequest{
		PulumiVersionRange: versionRange,
	})
	return err
}

// LogWarning emits a non-fatal warning diagnostic to the engine.
func (r *resourceMonitorAdapter) LogWarning(ctx context.Context, message string) error {
	_, err := r.engineClient.Log(ctx, &pulumirpc.LogRequest{
		Severity: pulumirpc.LogSeverity_WARNING,
		Message:  message,
	})
	return err
}

// Call invokes a method on a resource.
func (r *resourceMonitorAdapter) Call(
	ctx context.Context,
	req run.CallRequest,
) (*run.CallResponse, error) {
	argsStruct, err := plugin.MarshalProperties(resource.ToResourcePropertyMap(req.Args), r.pluginOptions())
	if err != nil {
		return nil, fmt.Errorf("marshaling args: %w", err)
	}

	resp, err := r.monitorClient.Call(ctx, &pulumirpc.ResourceCallRequest{
		Tok:               req.Token,
		Args:              argsStruct,
		PackageRef:        string(req.PackageRef),
		AcceptsByteString: true,
	})
	if err != nil {
		return nil, fmt.Errorf("calling method: %w", err)
	}

	returnVal, err := plugin.UnmarshalProperties(resp.Return, r.pluginOptions())
	if err != nil {
		return nil, fmt.Errorf("unmarshaling return value: %w", err)
	}

	var failures []string
	for _, f := range resp.Failures {
		failures = append(failures, fmt.Sprintf("%s: %s", f.Property, f.Reason))
	}

	return &run.CallResponse{
		Return:   resource.FromResourcePropertyMap(returnVal),
		Failures: failures,
	}, nil
}

func (r *resourceMonitorAdapter) RegisterResourceHook(
	ctx context.Context, name string, callback run.ResourceHookFunction, opts run.ResourceHookOptions,
) error {
	r.hookMu.Lock()
	if r.hookCBS == nil {
		cbs, err := newCallbackServer()
		if err != nil {
			r.hookMu.Unlock()
			return err
		}
		r.hookCBS = cbs
	}
	cbs := r.hookCBS
	r.hookMu.Unlock()

	cb, err := cbs.register(resourceHookCallback(callback))
	if err != nil {
		return fmt.Errorf("registering hook callback: %w", err)
	}
	_, err = r.monitorClient.RegisterResourceHook(ctx, &pulumirpc.RegisterResourceHookRequest{
		Name:     name,
		Callback: cb,
		OnDryRun: opts.OnDryRun,
	})
	if err != nil {
		return fmt.Errorf("registering hook %q: %w", name, err)
	}
	return nil
}

func (*resourceMonitorAdapter) pluginOptions() plugin.MarshalOptions {
	return plugin.MarshalOptions{
		KeepUnknowns:   true,
		KeepSecrets:    true,
		KeepResources:  true,
		KeepByteString: true,
	}
}

// formatTimeoutSeconds converts a timeout in seconds to a duration string.
// Returns empty string if seconds is 0.
func formatTimeoutSeconds(seconds float64) string {
	if seconds == 0 {
		return ""
	}
	return time.Duration(seconds * float64(time.Second)).String()
}

func NewPackageResolverClient(target string) (pulumirpc.PackageResolverClient, error) {
	contract.Assertf(target != "", "unexpected empty target for package resolver")

	dialOpts := append(
		rpcutil.TracingInterceptorDialOptions(),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		rpcutil.GrpcChannelOptions(),
	)
	conn, err := grpc.NewClient(target, dialOpts...)
	if err != nil {
		return nil, err
	}
	return pulumirpc.NewPackageResolverClient(conn), nil
}
