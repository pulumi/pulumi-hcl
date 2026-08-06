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
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	p "github.com/pulumi/pulumi-go-provider"
	"github.com/pulumi/pulumi/pkg/v3/codegen/convert"
	pulumiSchema "github.com/pulumi/pulumi/pkg/v3/codegen/schema"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource/plugin"
	"github.com/pulumi/pulumi/sdk/v3/go/common/tokens"
	"github.com/pulumi/pulumi/sdk/v3/go/common/util/contract"
	"github.com/pulumi/pulumi/sdk/v3/go/common/util/logging"
	"github.com/pulumi/pulumi/sdk/v3/go/common/util/rpcutil"
	"github.com/pulumi/pulumi/sdk/v3/go/property"
	pulumirpc "github.com/pulumi/pulumi/sdk/v3/proto/go"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/pulumi/pulumi-hcl/pkg/grpcerr"
	"github.com/pulumi/pulumi-hcl/pkg/hcl/ast"
	"github.com/pulumi/pulumi-hcl/pkg/hcl/bridge"
	"github.com/pulumi/pulumi-hcl/pkg/hcl/modules"
	"github.com/pulumi/pulumi-hcl/pkg/hcl/packages"
	"github.com/pulumi/pulumi-hcl/pkg/hcl/resolve"
	"github.com/pulumi/pulumi-hcl/pkg/hcl/run"
	"github.com/pulumi/pulumi-hcl/pkg/hcl/transform"
	"github.com/pulumi/pulumi-hcl/pkg/potel"
)

// moduleProvider is the fully dynamic HCL provider. It serves the single
// hcl:index:Module resource: a component whose module source arrives as a plain
// input at Construct time, rather than being baked into a per-module schema.
//
// The services it needs to resolve and type a module's providers — a schema
// loader, a bridge mapper, and a package resolver — are supplied by the engine
// during Handshake, the only point at which they are available.
type moduleProvider struct {
	version      string
	moduleLoader *modules.Loader

	engine             pulumirpc.EngineClient
	schemaLoader       pulumiSchema.ReferenceLoader
	providerInfoSource bridge.ProviderInfoSource
	resolver           pulumirpc.PackageResolverClient

	// param is non-nil once the provider has been parameterized by a specific
	// module source (via Parameterize). It makes the provider serve that module's
	// typed component schema instead of the generic hcl:index:Module.
	param *parameterizedModule

	// hooks is provider-scoped so a `before_delete` hook survives past the
	// Construct call that registered it, to fire during `destroy --run-program`.
	hooks lazyCallbackServer

	// dispatchers holds each deployment's destroy-provisioner dispatcher,
	// shared by that deployment's Constructs.
	dispatchers dispatcherSet
}

// NewModuleProvider builds the fully dynamic HCL provider on top of the raw
// (non-infer) pulumi-go-provider Provider surface.
func NewModuleProvider(ctx context.Context, version string) p.Provider {
	m := &moduleProvider{
		version:      version,
		moduleLoader: modules.NewLoader(modules.LiveResolver(ctx)),
	}
	return grpcerr.Wrap(p.Provider{
		Handshake:    m.handshake,
		Parameterize: m.parameterize,
		GetSchema:    m.getSchema,
		Configure:    func(context.Context, p.ConfigureRequest) error { return nil },
		CheckConfig: func(_ context.Context, req p.CheckRequest) (p.CheckResponse, error) {
			return p.CheckResponse{Inputs: req.Inputs}, nil
		},
		DiffConfig: func(context.Context, p.DiffRequest) (p.DiffResponse, error) {
			return p.DiffResponse{}, nil
		},
		Construct: m.construct,
		Cancel:    m.cancel,
	})
}

// handshake captures the schema loader, bridge mapper, and package resolver the
// engine exposes. All three are required: the dynamic Module cannot resolve or
// type a module's providers without them.
func (m *moduleProvider) handshake(ctx context.Context, req p.HandshakeRequest) (p.HandshakeResponse, error) {
	if req.LoaderAddress == nil || *req.LoaderAddress == "" {
		return p.HandshakeResponse{}, fmt.Errorf("no loader target received during handshake")
	}
	if req.MapperAddress == nil || *req.MapperAddress == "" {
		return p.HandshakeResponse{}, fmt.Errorf("no mapper target received during handshake")
	}
	if req.ResolverAddress == nil || *req.ResolverAddress == "" {
		return p.HandshakeResponse{}, fmt.Errorf("no resolver target received during handshake")
	}

	schemaLoader, err := pulumiSchema.NewLoaderClient(*req.LoaderAddress)
	if err != nil {
		return p.HandshakeResponse{}, fmt.Errorf("dial schema loader at %s: %w", *req.LoaderAddress, err)
	}

	mapperClient, err := convert.NewMapperClient(*req.MapperAddress)
	if err != nil {
		return p.HandshakeResponse{}, fmt.Errorf("dial mapper at %s: %w", *req.MapperAddress, err)
	}

	resolverClient, err := NewPackageResolverClient(*req.ResolverAddress)
	if err != nil {
		return p.HandshakeResponse{}, fmt.Errorf("dial resolver at %s: %w", *req.ResolverAddress, err)
	}

	m.schemaLoader = schemaLoader
	m.providerInfoSource = bridge.NewCache(bridge.NewMapperSource(mapperClient))
	m.resolver = resolve.NewCache(resolverClient)
	if req.EngineAddress != "" && m.engine == nil {
		if engineConn, err := grpc.NewClient(req.EngineAddress,
			grpc.WithTransportCredentials(insecure.NewCredentials())); err == nil {
			m.engine = pulumirpc.NewEngineClient(engineConn)
		}
	}

	return p.HandshakeResponse{}, nil
}

// getSchema returns the static hcl:index:Module schema, or — once the provider
// has been parameterized — the parameterized module's typed component schema with
// its parameterization Value attached so a generated SDK can re-parameterize.
func (m *moduleProvider) getSchema(context.Context, p.GetSchemaRequest) (p.GetSchemaResponse, error) {
	spec := moduleResourceSchema(m.version)
	if m.param != nil {
		spec = m.param.schema.ToPulumiPackageSchema()
		spec.Namespace = m.param.namespace
		spec.Parameterization = &pulumiSchema.ParameterizationSpec{
			BaseProvider: pulumiSchema.BaseProviderSpec{Name: "hcl", Version: m.version},
			Parameter:    m.param.value,
		}
	}
	b, err := json.Marshal(spec)
	if err != nil {
		return p.GetSchemaResponse{}, fmt.Errorf("marshaling schema: %w", err)
	}
	return p.GetSchemaResponse{Schema: string(b)}, nil
}

// cancel removes any unpacked module bundle when the provider shuts down.
func (m *moduleProvider) cancel(context.Context) error {
	m.hooks.close()
	if m.param != nil && m.param.tempDir != "" {
		return os.RemoveAll(m.param.tempDir)
	}
	return nil
}

// construct loads the module named by the "source" input, resolves the providers
// it references through the handshake resolver, runs it, and returns its outputs
// under the component's single "outputs" property.
func (m *moduleProvider) construct(ctx context.Context, req p.ConstructRequest) (p.ConstructResponse, error) {
	logging.V(5).Infof("Construct: type=%s name=%s", req.Urn.Type(), req.Urn.Name())
	if m.param != nil {
		return m.constructParameterized(ctx, req)
	}
	if req.Urn.Type() != moduleResourceToken {
		return p.ConstructResponse{}, fmt.Errorf("unknown resource type: %q", req.Urn.Type())
	}

	schemaLoader, providerInfoSource, resolver := m.schemaLoader, m.providerInfoSource, m.resolver
	if resolver == nil {
		return p.ConstructResponse{}, fmt.Errorf("Construct called before a successful Handshake")
	}

	sourceVal, ok := req.Inputs.GetOk("source")
	if !ok || !sourceVal.IsString() {
		return p.ConstructResponse{}, fmt.Errorf("module requires a plain string %q input", "source")
	}
	source := sourceVal.AsString()

	var version string
	if v, ok := req.Inputs.GetOk("version"); ok {
		if !v.IsString() {
			return p.ConstructResponse{}, fmt.Errorf("module %q input must be a plain string", "version")
		}
		version = v.AsString()
	}

	var inputs property.Map
	if v, ok := req.Inputs.GetOk("inputs"); ok && !v.IsComputed() {
		if !v.IsMap() {
			return p.ConstructResponse{}, fmt.Errorf("module %q input must be a map", "inputs")
		}
		inputs = v.AsMap()
	}

	loaded, err := m.moduleLoader.LoadModule(ctx, source, version, ".")
	if err != nil {
		return p.ConstructResponse{}, fmt.Errorf("loading module %q: %w", source, err)
	}

	if err := validateModuleInputs(inputs, loaded.Config); err != nil {
		return p.ConstructResponse{}, err
	}

	// Resolve every provider the module references to a concrete descriptor, the
	// dynamic equivalent of the on-disk sdks/ descriptors a source MLC reads.
	resolved, err := m.resolvePackages(ctx, m.moduleLoader, loaded.Config, loaded.SourcePath)
	if err != nil {
		return p.ConstructResponse{}, err
	}

	// A parameterization-aware loader lets the engine load the schemas of bridged
	// providers resolved above.
	loader := packages.NewParameterizationAwareLoader(schemaLoader, resolved)

	monitorConn, err := grpc.NewClient(req.MonitorEndpoint,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithUnaryInterceptor(rpcutil.OpenTracingClientInterceptor()),
		grpc.WithStreamInterceptor(rpcutil.OpenTracingStreamClientInterceptor()),
	)
	if err != nil {
		return p.ConstructResponse{}, fmt.Errorf("connecting to monitor: %w", err)
	}
	defer contract.IgnoreClose(monitorConn)

	componentInputs, err := plugin.MarshalProperties(
		resource.ToResourcePropertyMap(req.Inputs),
		constructMarshalOptions(),
	)
	if err != nil {
		return p.ConstructResponse{}, fmt.Errorf("marshaling inputs: %w", err)
	}

	hooks, err := m.hooks.get()
	if err != nil {
		return p.ConstructResponse{}, fmt.Errorf("starting hook callback server: %w", err)
	}
	resmon := m.newConstructMonitor(ctx, req,
		pulumirpc.NewResourceMonitorClient(monitorConn), componentInputs, wrapModuleOutputs, hooks)

	engineRun, err := run.NewEngine(ctx, loaded.Config, &run.EngineOptions{
		ProjectName:        string(req.Urn.Project()),
		StackName:          string(req.Urn.Stack()),
		DryRun:             req.DryRun,
		DestroyDispatcher:  m.dispatchers.get(req.MonitorEndpoint),
		WorkDir:            loaded.SourcePath,
		RootDir:            loaded.SourcePath,
		AbsolutePaths:      true,
		Config:             moduleConfig(string(req.Urn.Project()), inputs),
		ResourceMonitor:    resmon,
		SchemaLoader:       pulumiSchema.NewCachedLoader(loader),
		ProviderInfoSource: providerInfoSource,
		Packages:           resolved,
		ModuleLoader:       m.moduleLoader,
		Parallel:           int(req.Parallel),
	})
	if err != nil {
		return p.ConstructResponse{}, fmt.Errorf("creating engine: %w", err)
	}

	if err := engineRun.Run(ctx); err != nil {
		return p.ConstructResponse{}, fmt.Errorf("executing module %q: %w", source, err)
	}

	return p.ConstructResponse{
		Urn:   resmon.componentURN,
		State: resmon.outputs,
	}, nil
}

// constructParameterized runs the parameterized module: it maps the typed
// component inputs to the module's HCL variables, runs the engine against the
// bundled (offline) module tree, and exposes the module's outputs under their
// typed schema property names. It mirrors HCLProvider.Construct, the local-path
// MLC equivalent.
func (m *moduleProvider) constructParameterized(ctx context.Context, req p.ConstructRequest) (p.ConstructResponse, error) {
	if m.resolver == nil {
		return p.ConstructResponse{}, fmt.Errorf("Construct called before a successful Handshake")
	}
	param := m.param

	loaded, err := param.loader.LoadModule(ctx, param.rootSource, param.rootVersion, ".")
	if err != nil {
		return p.ConstructResponse{}, fmt.Errorf("loading module %q: %w", param.name, err)
	}

	monitorConn, err := grpc.NewClient(req.MonitorEndpoint,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithUnaryInterceptor(rpcutil.OpenTracingClientInterceptor()),
		grpc.WithStreamInterceptor(rpcutil.OpenTracingStreamClientInterceptor()),
	)
	if err != nil {
		return p.ConstructResponse{}, fmt.Errorf("connecting to monitor: %w", err)
	}
	defer contract.IgnoreClose(monitorConn)

	componentInputs, err := plugin.MarshalProperties(
		resource.ToResourcePropertyMap(req.Inputs), constructMarshalOptions())
	if err != nil {
		return p.ConstructResponse{}, fmt.Errorf("marshaling inputs: %w", err)
	}

	hooks, err := m.hooks.get()
	if err != nil {
		return p.ConstructResponse{}, fmt.Errorf("starting hook callback server: %w", err)
	}
	resmon := m.newConstructMonitor(ctx, req,
		pulumirpc.NewResourceMonitorClient(monitorConn), componentInputs, param.schema.OutputsToPulumi, hooks)

	loader := pulumiSchema.NewCachedLoader(packages.NewParameterizationAwareLoader(
		m.schemaLoader, param.packages))

	engineRun, err := run.NewEngine(ctx, loaded.Config, &run.EngineOptions{
		ProjectName:        string(req.Urn.Project()),
		StackName:          string(req.Urn.Stack()),
		DryRun:             req.DryRun,
		DestroyDispatcher:  m.dispatchers.get(req.MonitorEndpoint),
		WorkDir:            loaded.SourcePath,
		RootDir:            loaded.SourcePath,
		AbsolutePaths:      true,
		Config:             moduleConfig(string(req.Urn.Project()), param.schema.InputsToHCL(req.Inputs)),
		ResourceMonitor:    resmon,
		SchemaLoader:       loader,
		ProviderInfoSource: m.providerInfoSource,
		Packages:           param.packages,
		ModuleLoader:       param.loader,
		Parallel:           int(req.Parallel),
	})
	if err != nil {
		return p.ConstructResponse{}, fmt.Errorf("creating engine: %w", err)
	}

	if err := engineRun.Run(ctx); err != nil {
		return p.ConstructResponse{}, fmt.Errorf("executing module %q: %w", param.name, err)
	}

	return p.ConstructResponse{
		Urn:   resmon.componentURN,
		State: resmon.outputs,
	}, nil
}

// newConstructMonitor builds the resource monitor that intercepts the engine's
// stack registration and re-emits it as the component resource the Construct
// caller expects. mapOutputs shapes the module's top-level outputs into the
// component's output property map.
func (m *moduleProvider) newConstructMonitor(
	ctx context.Context, req p.ConstructRequest, client pulumirpc.ResourceMonitorClient,
	componentInputs *structpb.Struct, mapOutputs func(property.Map) property.Map, hooks *callbackServer,
) *constructResourceMonitor {
	return &constructResourceMonitor{
		client:                  client,
		engine:                  m.engine,
		hooks:                   hooks,
		ctx:                     ctx,
		parentURN:               string(req.Parent),
		componentType:           string(req.Urn.Type()),
		componentName:           string(req.Urn.Name()),
		componentInputs:         componentInputs,
		aliases:                 aliasURNsToProto(req.Aliases),
		protect:                 req.Protect,
		dependencies:            urnsToStrings(req.Dependencies),
		providers:               providersToProto(req.Providers),
		additionalSecretOutputs: req.AdditionalSecretOutputs,
		deletedWith:             string(req.DeletedWith),
		deleteBeforeReplace:     req.DeleteBeforeReplace,
		ignoreChanges:           req.IgnoreChanges,
		replaceOnChanges:        req.ReplaceOnChanges,
		retainOnDelete:          req.RetainOnDelete,
		customTimeouts:          customTimeoutsToProto(req.CustomTimeouts),
		mapOutputs:              mapOutputs,
	}
}

// RequirementSpecs turns every provider the module tree references into a
// resolver request keyed by its local name. Pulumi-sourced providers resolve by
// package name; bridged Terraform providers resolve as a parameterization of the
// terraform-provider plugin, matching how GetRequiredPackages emits install
// specs. Built-in providers (pulumi, terraform) are handled by the engine and
// are not resolved.
func RequirementSpecs(
	ctx context.Context, loader *modules.Loader, config *ast.Config, workDir string,
) []resolve.Request {
	ctx, span := potel.Start(ctx, "requirementSpecs")
	defer span.End()
	tf, pulumi, aliases := collectRequirements(ctx, loader, config, workDir)
	var reqs []resolve.Request
	for _, alias := range sortedKeys(aliases) {
		if isBuiltinProvider(alias) {
			continue
		}
		req := aliases[alias]
		if req.IsPulumi() {
			name := packageName(alias, req.Source)
			reqs = append(reqs, resolve.Request{
				Alias: alias,
				Spec:  &pulumirpc.PackageSpec{Source: name, Version: pulumi[name]},
			})
			continue
		}
		source := tfProviderSource(alias, req)
		params := []string{source}
		if r := tf[canonicalSource(source)]; r != nil {
			if c := r.constraint(); c != "" {
				params = append(params, c)
			}
		}
		reqs = append(reqs, resolve.Request{
			Alias: alias,
			Spec:  &pulumirpc.PackageSpec{Source: "terraform-provider", Parameters: params},
		})
	}
	return reqs
}

// validateModuleInputs rejects any input that does not name a variable the
// module declares. Without this an input the module never reads — a typo, or a
// stale input left over after the module changed — is silently dropped rather
// than surfaced. Invalid values for declared variables (type mismatches, failed
// `validation` blocks) are caught later by the engine.
func validateModuleInputs(inputs property.Map, config *ast.Config) error {
	var unknown []string
	for k := range inputs.All {
		if _, ok := config.Variables[k]; !ok {
			unknown = append(unknown, k)
		}
	}
	sort.Strings(unknown)
	plural := "s"
	switch len(unknown) {
	case 0:
		return nil
	case 1:
		plural = ""
		fallthrough
	default:
		return fmt.Errorf("module has no variables declared for input%s: %s",
			plural, strings.Join(unknown, ", "))
	}
}

// moduleConfig maps the module's input variables to the engine's config, keyed
// <project>:<variable>. The values are passed through as already-typed cty values, so
// structure, unknowns, and marks are preserved across the component boundary.
func moduleConfig(project string, inputs property.Map) map[string]run.ConfigValue {
	config := make(map[string]run.ConfigValue, inputs.Len())
	for k, v := range inputs.All {
		config[project+":"+string(k)] = run.TypedConfigValue(transform.PropertyValueToCty(v))
	}
	return config
}

// wrapModuleOutputs exposes the module's top-level outputs under the component's
// single untyped "outputs" property.
func wrapModuleOutputs(outputs property.Map) property.Map {
	return property.NewMap(map[string]property.Value{
		"outputs": property.New(outputs.AsMap()),
	})
}

func aliasURNsToProto(urns []resource.URN) []*pulumirpc.Alias {
	if len(urns) == 0 {
		return nil
	}
	out := make([]*pulumirpc.Alias, len(urns))
	for i, u := range urns {
		out[i] = &pulumirpc.Alias{Alias: &pulumirpc.Alias_Urn{Urn: string(u)}}
	}
	return out
}

func urnsToStrings(urns []resource.URN) []string {
	if len(urns) == 0 {
		return nil
	}
	out := make([]string, len(urns))
	for i, u := range urns {
		out[i] = string(u)
	}
	return out
}

func providersToProto(provs map[tokens.Package]p.ProviderReference) map[string]string {
	if len(provs) == 0 {
		return nil
	}
	out := make(map[string]string, len(provs))
	for pkg, ref := range provs {
		out[string(pkg)] = string(ref.Urn) + "::" + string(ref.ID)
	}
	return out
}

func customTimeoutsToProto(ct *resource.CustomTimeouts) *pulumirpc.ConstructRequest_CustomTimeouts {
	if ct == nil {
		return nil
	}
	return &pulumirpc.ConstructRequest_CustomTimeouts{
		Create: formatTimeoutSeconds(ct.Create),
		Update: formatTimeoutSeconds(ct.Update),
		Delete: formatTimeoutSeconds(ct.Delete),
	}
}
