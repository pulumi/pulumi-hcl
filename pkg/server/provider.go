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
	"fmt"
	"path/filepath"

	"github.com/blang/semver"
	p "github.com/pulumi/pulumi-go-provider"
	"github.com/pulumi/pulumi-hcl/pkg/grpcerr"
	"github.com/pulumi/pulumi-hcl/pkg/hcl/ast"
	"github.com/pulumi/pulumi-hcl/pkg/hcl/bridge"
	"github.com/pulumi/pulumi-hcl/pkg/hcl/modules"
	"github.com/pulumi/pulumi-hcl/pkg/hcl/run"
	"github.com/pulumi/pulumi/pkg/v3/codegen/convert"
	pulumiSchema "github.com/pulumi/pulumi/pkg/v3/codegen/schema"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource/plugin"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource/urn"
	"github.com/pulumi/pulumi/sdk/v3/go/common/tokens"
	"github.com/pulumi/pulumi/sdk/v3/go/common/workspace"
	"github.com/pulumi/pulumi/sdk/v3/go/property"
	pulumirpc "github.com/pulumi/pulumi/sdk/v3/proto/go"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/structpb"
)

// NewLocalProvider serves the HCL module package at modulePath as a source
// MLC: the module provider born parameterized by the local package — one
// component per consumable directory — reading provider descriptors from the
// package's own sdks folder. addr is the engine's schema-loader target, which
// also serves the mapper.
func NewLocalProvider(ctx context.Context, modulePath, addr string) (pulumirpc.ResourceProviderServer, error) {
	loader := modules.NewLoader(modules.LiveResolver(ctx))
	pkgLoader, err := pulumiSchema.NewLoaderClient(addr)
	if err != nil {
		return nil, fmt.Errorf("unable to acquire schema loader: %w", err)
	}

	// The schema-loader target also serves the mapper service, so a component
	// can resolve the bridge mappings for the TF providers it uses internally.
	mapperClient, err := convert.NewMapperClient(addr)
	if err != nil {
		return nil, fmt.Errorf("unable to acquire mapper: %w", err)
	}
	providerInfoSource := bridge.NewCache(bridge.NewMapperSource(mapperClient))

	comps, resolvedVersion, err := localComponents(ctx, loader, modulePath)
	if err != nil {
		return nil, err
	}
	pkgName, rootToken, version, err := packageIdentity(comps, filepath.Base(modulePath), false, resolvedVersion)
	if err != nil {
		return nil, err
	}

	m := &moduleProvider{
		version:            version.String(),
		moduleLoader:       loader,
		schemaLoader:       pkgLoader,
		providerInfoSource: providerInfoSource,
	}
	components, spec, err := buildComponents(ctx, comps, pkgName, rootToken, version,
		m.componentBinderFactory(loader))
	if err != nil {
		return nil, err
	}

	m.param = &parameterizedModule{
		spec:       spec,
		components: components,
		loader:     loader,
		name:       pkgName,
	}
	return p.RawServer(pkgName, version.String(), m.asProvider())(nil)
}

// localComponents loads the components of the module package at modulePath
// and resolves the packages each registers against: the package's sdks folder
// is the shared descriptor pool, and each component's own required_providers
// pins fill in what the folder does not cover, as Run does for a root program.
func localComponents(
	ctx context.Context, loader *modules.Loader, modulePath string,
) ([]loadedComponent, string, error) {
	sdkInfos, err := readSDKInfos(modulePath)
	if err != nil {
		return nil, "", fmt.Errorf("reading parameterization: %w", err)
	}
	comps, resolvedVersion, err := loadComponents(ctx, loader, modulePath, "")
	if err != nil {
		return nil, "", fmt.Errorf("loading module: %w", err)
	}
	for i := range comps {
		comps[i].packages, _ = programPackages(ctx, loader, comps[i].loaded.Config, comps[i].loaded.SourcePath, sdkInfos)
	}
	return comps, resolvedVersion, nil
}

// moduleIdentity derives a module's component token and version. The terraform
// `package` and `component` blocks take precedence; absent them defaultName
// supplies the package name and the version the module's source resolved to
// supplies the version, falling back to "0.0.0-dev" for sources that carry no
// version (local paths, git, http).
//
// The module segment defaults to "index" and the component name defaults to
// "Module", so a module with no component block yields the token
// "<package>:index:Module", referenced in HCL as `<package>_module` (mirroring
// the dynamic hcl:index:Module resource).
func moduleIdentity(loaded *modules.LoadedModule, defaultName string, forceDefault bool) (tokens.Type, semver.Version, error) {
	module := "index"
	pkgName := defaultName
	pkgVersion := loaded.Version
	if pkgVersion == "" {
		pkgVersion = "0.0.0-dev"
	}
	var explicitComponentName string
	if tf := loaded.Config.Terraform; tf != nil {
		if comp := tf.Component; comp != nil {
			explicitComponentName = comp.Name
			if comp.Module != "" {
				module = comp.Module
			}
		}
		if pkg := tf.Package; pkg != nil {
			if pkg.Name != "" && !forceDefault {
				pkgName = pkg.Name
			}
			if pkg.Version != "" {
				pkgVersion = pkg.Version
			}
		}
	}
	componentName := "Module"
	if explicitComponentName != "" {
		componentName = explicitComponentName
	}
	version, err := semver.ParseTolerant(pkgVersion)
	if err != nil {
		return "", version, grpcerr.Errorf(codes.InvalidArgument, "parsing module version %q: %w", pkgVersion, err)
	}
	return tokens.Type(fmt.Sprintf("%s:%s:%s", pkgName, module, componentName)), version, nil
}

// moduleLoaderAdapter adapts *modules.Loader to schema.ModuleLoader, so schema
// generation can recursively type references to child modules.
type moduleLoaderAdapter struct {
	loader *modules.Loader
}

func (a moduleLoaderAdapter) LoadModule(
	ctx context.Context, source, versionConstraint, callerDir string,
) (*ast.Config, string, error) {
	// schema.ModuleLoader carries no context, so there is none to thread here.
	m, err := a.loader.LoadModule(ctx, source, versionConstraint, callerDir)
	if err != nil {
		return nil, "", err
	}
	return m.Config, m.SourcePath, nil
}

// constructResourceMonitor wraps the resource monitor for Construct calls.
// It intercepts the engine's stack registration and converts it into the
// actual component resource registration expected by the Pulumi engine.
type constructResourceMonitor struct {
	client          pulumirpc.ResourceMonitorClient
	engine          pulumirpc.EngineClient
	ctx             context.Context
	parentURN       string
	componentType   string
	componentName   string
	componentInputs *structpb.Struct
	// Resource options from the ConstructRequest that must be forwarded
	// when registering the component resource with the engine.
	aliases                 []*pulumirpc.Alias
	protect                 *bool
	dependencies            []string
	providers               map[string]string
	ignoreChanges           []string
	additionalSecretOutputs []string
	deleteBeforeReplace     *bool
	deletedWith             string
	retainOnDelete          *bool
	replaceOnChanges        []string
	replaceWith             []string
	customTimeouts          *pulumirpc.ConstructRequest_CustomTimeouts
	replacementTrigger      *structpb.Value
	resourceHooks           *pulumirpc.RegisterResourceRequest_ResourceHooksBinding

	componentURN urn.URN
	outputs      property.Map

	// mapOutputs transforms the HCL module's top-level outputs into the
	// component's output property map before they are registered.
	mapOutputs func(property.Map) property.Map

	// hooks is the provider-owned callback server (not this monitor's): a
	// `before_delete` hook fires after this Construct call has returned, so the
	// callback server must outlive any single construct.
	hooks *callbackServer
}

// constructMarshalOptions are the property (un)marshal options used on every
// hop across the Construct boundary.
func constructMarshalOptions() plugin.MarshalOptions {
	return plugin.MarshalOptions{
		KeepUnknowns:     true,
		KeepSecrets:      true,
		KeepResources:    true,
		KeepOutputValues: true,
		KeepByteString:   true,
	}
}

// RegisterResource registers a resource.
func (m *constructResourceMonitor) RegisterResource(
	ctx context.Context,
	req run.RegisterResourceRequest,
) (*run.RegisterResourceResponse, error) {
	// Intercept the engine's internal stack registration and convert it to
	// the component resource that the Construct caller expects.
	if req.Type == "pulumi:pulumi:Stack" {
		registerReq := &pulumirpc.RegisterResourceRequest{
			Type:                    m.componentType,
			Name:                    m.componentName,
			Parent:                  m.parentURN,
			Object:                  m.componentInputs,
			Aliases:                 m.aliases,
			Protect:                 m.protect,
			Dependencies:            m.dependencies,
			Providers:               m.providers,
			IgnoreChanges:           m.ignoreChanges,
			AdditionalSecretOutputs: m.additionalSecretOutputs,
			DeletedWith:             m.deletedWith,
			RetainOnDelete:          m.retainOnDelete,
			ReplaceOnChanges:        m.replaceOnChanges,
			ReplaceWith:             m.replaceWith,
			Hooks:                   m.resourceHooks,
			AcceptSecrets:           true,
			AcceptResources:         true,
			AcceptsByteString:       true,
		}
		if m.deleteBeforeReplace != nil {
			registerReq.DeleteBeforeReplace = *m.deleteBeforeReplace
			registerReq.DeleteBeforeReplaceDefined = true
		}
		if m.customTimeouts != nil {
			registerReq.CustomTimeouts = &pulumirpc.RegisterResourceRequest_CustomTimeouts{
				Create: m.customTimeouts.Create,
				Read:   m.customTimeouts.Read,
				Update: m.customTimeouts.Update,
				Delete: m.customTimeouts.Delete,
			}
		}
		if m.replacementTrigger != nil {
			registerReq.ReplacementTrigger = m.replacementTrigger
		}
		resp, err := m.client.RegisterResource(ctx, registerReq)
		if err != nil {
			return nil, err
		}
		m.componentURN = urn.URN(resp.Urn)
		return &run.RegisterResourceResponse{
			URN: urn.URN(resp.Urn),
			ID:  resp.Id,
		}, nil
	}

	// Convert PropertyMap to protobuf
	inputs, err := plugin.MarshalProperties(resource.ToResourcePropertyMap(req.Inputs), constructMarshalOptions())
	if err != nil {
		return nil, fmt.Errorf("marshaling inputs: %w", err)
	}

	// Use parent from request or fall back to component URN
	parent := req.Parent
	if parent == "" {
		parent = m.componentURN
	}

	// Prefix child resource names with the component name to ensure URN uniqueness
	// across multiple instances of the same component type. This mirrors what other
	// Pulumi SDKs do (e.g. NodeJS uses `${name}-child`).
	name := m.componentName + "-" + req.Name

	ignoreChanges, err := globsToPropertyPaths(req.IgnoreChanges)
	if err != nil {
		return nil, fmt.Errorf("marshaling ignoreChanges: %w", err)
	}

	resp, err := m.client.RegisterResource(ctx, &pulumirpc.RegisterResourceRequest{
		Type:                req.Type,
		Name:                name,
		Custom:              req.Custom,
		Object:              inputs,
		Parent:              string(parent),
		Dependencies:        req.Dependencies,
		Provider:            req.Provider,
		Providers:           req.Providers,
		Protect:             &req.Protect,
		DeleteBeforeReplace: req.DeleteBeforeReplace,
		IgnoreChanges:       ignoreChanges,
		PackageRef:          string(req.PackageRef),
		Version:             req.Version,
		PluginDownloadURL:   req.PluginDownloadURL,
		Hooks:               hooksToProto(req.Hooks),
		AcceptSecrets:       true,
		AcceptResources:     true,
		AcceptsByteString:   true,
	})
	if err != nil {
		return nil, err
	}

	// Convert outputs back to PropertyMap
	outputs, err := plugin.UnmarshalProperties(resp.Object, constructMarshalOptions())
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
func (m *constructResourceMonitor) ReadResource(
	ctx context.Context,
	req run.ReadResourceRequest,
) (*run.ReadResourceResponse, error) {
	properties, err := plugin.MarshalProperties(resource.ToResourcePropertyMap(req.Inputs), plugin.MarshalOptions{
		KeepSecrets:    true,
		KeepResources:  true,
		KeepByteString: true,
	})
	if err != nil {
		return nil, fmt.Errorf("marshaling inputs: %w", err)
	}

	parent := req.Parent
	if parent == "" {
		parent = m.componentURN
	}

	resp, err := m.client.ReadResource(ctx, &pulumirpc.ReadResourceRequest{
		Id:                      req.ID,
		Type:                    req.Type,
		Name:                    m.componentName + "-" + req.Name,
		Parent:                  string(parent),
		Properties:              properties,
		Dependencies:            req.Dependencies,
		Provider:                req.Provider,
		Version:                 req.Version,
		AdditionalSecretOutputs: req.AdditionalSecretOutputs,
		PluginDownloadURL:       req.PluginDownloadURL,
		PackageRef:              string(req.PackageRef),
		AcceptSecrets:           true,
		AcceptResources:         true,
		AcceptsByteString:       true,
	})
	if err != nil {
		return nil, err
	}

	outputs, err := plugin.UnmarshalProperties(resp.Properties, plugin.MarshalOptions{
		KeepSecrets:    true,
		KeepResources:  true,
		KeepByteString: true,
	})
	if err != nil {
		return nil, fmt.Errorf("unmarshaling outputs: %w", err)
	}

	return &run.ReadResourceResponse{
		URN:     urn.URN(resp.Urn),
		ID:      req.ID,
		Outputs: resource.FromResourcePropertyMap(outputs),
	}, nil
}

// RegisterResourceOutputs registers resource outputs.
func (m *constructResourceMonitor) RegisterResourceOutputs(
	ctx context.Context,
	urn urn.URN,
	outputs property.Map,
) error {
	// The component's outputs are keyed by the HCL module's snake_case output
	// names (with snake_case object fields); expose them under the camelCase
	// names the schema declares, at every nesting depth. Child resources already
	// carry their own correctly-cased property names.
	if urn == m.componentURN {
		outputs = m.mapOutputs(outputs)
		m.outputs = outputs
	}

	outputsStruct, err := plugin.MarshalProperties(resource.ToResourcePropertyMap(outputs), constructMarshalOptions())
	if err != nil {
		return fmt.Errorf("marshaling outputs: %w", err)
	}

	_, err = m.client.RegisterResourceOutputs(ctx, &pulumirpc.RegisterResourceOutputsRequest{
		Urn:     string(urn),
		Outputs: outputsStruct,
	})
	return err
}

// Invoke invokes a provider function.
func (m *constructResourceMonitor) Invoke(
	ctx context.Context,
	req run.InvokeRequest,
) (*run.InvokeResponse, error) {
	argsStruct, err := plugin.MarshalProperties(resource.ToResourcePropertyMap(req.Args), constructMarshalOptions())
	if err != nil {
		return nil, fmt.Errorf("marshaling args: %w", err)
	}

	resp, err := m.client.Invoke(ctx, &pulumirpc.ResourceInvokeRequest{
		Tok:               req.Token,
		Args:              argsStruct,
		Provider:          req.Provider,
		Version:           req.Version,
		PluginDownloadURL: req.PluginDownloadURL,
		PackageRef:        string(req.PackageRef),
		AcceptResources:   true,
		AcceptsByteString: true,
		DependsOn:         req.DependsOn,
	})
	if err != nil {
		return nil, err
	}

	var failures []string
	for _, f := range resp.Failures {
		failures = append(failures, f.Reason)
	}

	ret, err := plugin.UnmarshalProperties(resp.Return, constructMarshalOptions())
	if err != nil {
		return nil, fmt.Errorf("unmarshaling return: %w", err)
	}

	return &run.InvokeResponse{
		Return:   resource.FromResourcePropertyMap(ret),
		Failures: failures,
		Unknown:  resp.Unknown,
	}, nil
}

// Call invokes a method on a resource.
func (m *constructResourceMonitor) Call(
	ctx context.Context,
	req run.CallRequest,
) (*run.CallResponse, error) {
	argsStruct, err := plugin.MarshalProperties(resource.ToResourcePropertyMap(req.Args), constructMarshalOptions())
	if err != nil {
		return nil, fmt.Errorf("marshaling args: %w", err)
	}

	resp, err := m.client.Call(ctx, &pulumirpc.ResourceCallRequest{
		Tok:               req.Token,
		Args:              argsStruct,
		PackageRef:        string(req.PackageRef),
		AcceptsByteString: true,
	})
	if err != nil {
		return nil, fmt.Errorf("calling method: %w", err)
	}

	var failures []string
	for _, f := range resp.Failures {
		failures = append(failures, f.Reason)
	}

	ret, err := plugin.UnmarshalProperties(resp.Return, constructMarshalOptions())
	if err != nil {
		return nil, fmt.Errorf("unmarshaling return: %w", err)
	}

	return &run.CallResponse{
		Return:   resource.FromResourcePropertyMap(ret),
		Failures: failures,
	}, nil
}

// CheckPulumiVersion checks if the Pulumi CLI version satisfies the given version range.
func (m *constructResourceMonitor) CheckPulumiVersion(ctx context.Context, versionRange string) error {
	return nil
}

func (m *constructResourceMonitor) RegisterPackage(
	ctx context.Context,
	pkg workspace.PackageDescriptor,
) (run.PackageRef, error) {
	return registerPackage(ctx, m.client, pkg)
}

// RegisterResourceHook hosts the callback on the provider-owned callback server
// and registers it with the engine's monitor.
func (m *constructResourceMonitor) RegisterResourceHook(
	ctx context.Context, name string, callback run.ResourceHookFunction, opts run.ResourceHookOptions,
) error {
	if m.hooks == nil {
		return fmt.Errorf("resource hooks are not supported: no callback server is available")
	}
	cb, err := m.hooks.register(resourceHookCallback(callback))
	if err != nil {
		return fmt.Errorf("registering hook callback: %w", err)
	}
	_, err = m.client.RegisterResourceHook(ctx, &pulumirpc.RegisterResourceHookRequest{
		Name:     name,
		Callback: cb,
		OnDryRun: opts.OnDryRun,
	})
	if err != nil {
		return fmt.Errorf("registering hook %q: %w", name, err)
	}
	return nil
}

// LogWarning emits a non-fatal warning diagnostic to the engine.
func (m *constructResourceMonitor) LogWarning(ctx context.Context, message string) error {
	if m.engine == nil {
		return nil
	}
	_, err := m.engine.Log(ctx, &pulumirpc.LogRequest{
		Severity: pulumirpc.LogSeverity_WARNING,
		Message:  message,
	})
	return err
}

// Ensure constructResourceMonitor implements run.ResourceMonitor.
var _ run.ResourceMonitor = (*constructResourceMonitor)(nil)

// ResolveURN mirrors RegisterResource's rewrites (parent fallback, name
// prefix), then replicates the engine's URN generation. The component URN is
// set before any resource the engine registers.
func (m *constructResourceMonitor) ResolveURN(parent urn.URN, token, name string) (urn.URN, string) {
	if parent == "" {
		parent = m.componentURN
	}
	name = m.componentName + "-" + name
	parentType := tokens.Type("")
	if parent != "" && parent.QualifiedType() != resource.RootStackType {
		parentType = parent.QualifiedType()
	}
	return urn.New(m.componentURN.Stack(), m.componentURN.Project(), parentType, tokens.Type(token), name), name
}
