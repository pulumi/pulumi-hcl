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

// Package run implements the HCL program execution engine.
package run

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"math"
	"math/big"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/pulumi/pulumi-hcl/pkg/hcl/ast"
	"github.com/pulumi/pulumi-hcl/pkg/hcl/bridge"
	"github.com/pulumi/pulumi-hcl/pkg/hcl/eval"
	"github.com/pulumi/pulumi-hcl/pkg/hcl/graph"
	"github.com/pulumi/pulumi-hcl/pkg/hcl/modulepath"
	"github.com/pulumi/pulumi-hcl/pkg/hcl/modules"
	"github.com/pulumi/pulumi-hcl/pkg/hcl/packages"
	"github.com/pulumi/pulumi-hcl/pkg/hcl/transform"
	"github.com/pulumi/pulumi-hcl/pkg/potel"
	"github.com/pulumi/pulumi-hcl/pkg/util"
	"github.com/pulumi/pulumi/pkg/v3/codegen"
	"github.com/pulumi/pulumi/pkg/v3/codegen/schema"
	"github.com/pulumi/pulumi/pkg/v3/util/pdag"
	"github.com/pulumi/pulumi/sdk/v3/go/common/providers"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource/plugin"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource/urn"
	"github.com/pulumi/pulumi/sdk/v3/go/common/tokens"
	"github.com/pulumi/pulumi/sdk/v3/go/common/util/contract"
	"github.com/pulumi/pulumi/sdk/v3/go/common/util/logging"
	"github.com/pulumi/pulumi/sdk/v3/go/common/workspace"
	"github.com/pulumi/pulumi/sdk/v3/go/property"
	"github.com/zclconf/go-cty/cty"
	ctyconvert "github.com/zclconf/go-cty/cty/convert"
	"github.com/zclconf/go-cty/cty/function"
)

// PackageRef is an opaque reference returned by RegisterPackage that routes
// resource registrations to the correct parameterized provider instance.
type PackageRef string

// ResourceMonitor is the interface for registering resources with Pulumi.
// This matches the resource monitor interface used by the Pulumi engine.
type ResourceMonitor interface {
	// RegisterPackage registers a parameterized package with the engine and returns
	// a PackageRef that must be passed in subsequent resource registrations.
	RegisterPackage(ctx context.Context, pkg workspace.PackageDescriptor) (PackageRef, error)

	// RegisterResource registers a resource with Pulumi.
	RegisterResource(ctx context.Context, req RegisterResourceRequest) (*RegisterResourceResponse, error)

	// ReadResource reads the state of an existing resource. This is the
	// registration form used for stack references, which the engine resolves
	// against the backend rather than creating.
	ReadResource(ctx context.Context, req ReadResourceRequest) (*ReadResourceResponse, error)

	// Invoke invokes a provider function.
	Invoke(ctx context.Context, req InvokeRequest) (*InvokeResponse, error)

	// Call invokes a method on a resource.
	Call(ctx context.Context, req CallRequest) (*CallResponse, error)

	// RegisterResourceOutputs registers outputs on a resource (used for stack outputs).
	RegisterResourceOutputs(ctx context.Context, urn urn.URN, outputs property.Map) error

	// CheckPulumiVersion checks if the Pulumi CLI version satisfies the given version range.
	CheckPulumiVersion(ctx context.Context, versionRange string) error

	// RegisterResourceHook registers a named callback. Must be called before any
	// resource registration that binds the hook by name via Hooks.
	RegisterResourceHook(ctx context.Context, name string, callback ResourceHookFunction, opts ResourceHookOptions) error

	// ResolveURN returns the URN the engine will assign to a resource with
	// this (parent, token, name), and the name it registers under. The MLC
	// monitor rewrites both after this package sees them.
	ResolveURN(parent urn.URN, token, name string) (urn.URN, string)

	// LogWarning emits a non-fatal warning diagnostic to the engine.
	LogWarning(ctx context.Context, message string) error
}

// ResourceHookFunction is the engine-invoked hook callback. A non-nil return
// fails the operation; the error message is surfaced to the user.
type ResourceHookFunction func(ctx context.Context, args *ResourceHookArgs) error

type ResourceHookOptions struct {
	// OnDryRun controls whether the hook runs during previews.
	OnDryRun bool
}

type ResourceHookArgs struct {
	URN        string
	ID         string
	Name       string
	Type       string
	NewInputs  property.Map
	OldInputs  property.Map
	NewOutputs property.Map
	OldOutputs property.Map
}

type ResourceHookBinding struct {
	BeforeCreate []string
	BeforeUpdate []string
	AfterCreate  []string
	AfterUpdate  []string
	BeforeDelete []string
	AfterDelete  []string
	OnError      []string
}

// CustomTimeouts contains custom timeout values for resource operations.
type CustomTimeouts struct {
	Create float64 // Timeout in seconds for create operations
	Read   float64 // Timeout in seconds for read operations
	Update float64 // Timeout in seconds for update operations
	Delete float64 // Timeout in seconds for delete operations
}

// Alias represents a resource alias - either a URN string or a spec object.
type Alias struct {
	// URN is set for URN-based aliases.
	URN string
	// Spec is set for spec-based aliases.
	Spec *AliasSpec
}

// AliasSpec represents a resource alias specification.
type AliasSpec struct {
	Name      string
	Type      string
	Stack     string
	Project   string
	ParentURN string
	NoParent  bool
}

// RegisterResourceRequest contains the parameters for registering a resource.
type RegisterResourceRequest struct {
	Type                    string
	Name                    string
	Inputs                  property.Map
	Dependencies            []string
	PropertyDependencies    map[string][]string // Map from property key to list of URNs it depends on
	Custom                  bool
	Remote                  bool
	Protect                 bool
	IgnoreChanges           []property.Glob
	Aliases                 []Alias
	Provider                string
	Providers               map[string]string // Map from package name to provider reference (urn::id)
	Parent                  urn.URN
	DeleteBeforeReplace     bool
	DeleteBeforeReplaceDef  bool // True if DeleteBeforeReplace was explicitly set
	CustomTimeouts          *CustomTimeouts
	ImportId                string // Resource ID to import
	AdditionalSecretOutputs []string
	RetainOnDelete          *bool
	DeletedWith             string          // URN of the resource that, when deleted, causes this resource to be deleted
	ReplaceWith             []string        // URNs of resources whose replacement triggers replacement of this resource
	HideDiffs               []property.Glob // Property paths whose diffs should not be displayed
	ReplaceOnChanges        []property.Glob // Property paths that if changed should force a replacement
	ReplacementTrigger      property.Value  // Value whose change triggers replacement
	EnvVarMappings          map[string]string
	Version                 string
	PluginDownloadURL       string
	PackageRef              PackageRef
	Hooks                   *ResourceHookBinding
}

// RegisterResourceResponse contains the result of registering a resource.
type RegisterResourceResponse struct {
	URN     urn.URN
	ID      string
	Outputs property.Map
	Unknown bool // The engine elided the operation (e.g. a targeted update); outputs must resolve as unknown.
}

// ReadResourceRequest contains the parameters for reading an existing resource.
// A read honors only the subset of resource options the engine's ReadResource
// RPC accepts; options that imply lifecycle management (Protect, IgnoreChanges,
// CustomTimeouts, ...) have no meaning for a read and are not carried.
type ReadResourceRequest struct {
	Type                    string
	Name                    string
	ID                      string
	Inputs                  property.Map
	Parent                  urn.URN
	Dependencies            []string
	Provider                string
	Version                 string
	AdditionalSecretOutputs []string
	PluginDownloadURL       string
	PackageRef              PackageRef
}

// ReadResourceResponse contains the result of reading a resource. ID echoes the
// requested ID, since a read identifies the resource rather than minting one.
type ReadResourceResponse struct {
	URN     urn.URN
	ID      string
	Outputs property.Map
}

// InvokeRequest contains the parameters for invoking a function.
type InvokeRequest struct {
	Token             string
	Args              property.Map
	Provider          string
	Version           string
	PluginDownloadURL string
	PackageRef        PackageRef
	DependsOn         []string
}

// InvokeResponse contains the result of invoking a function.
type InvokeResponse struct {
	Return   property.Map
	Failures []string
	Unknown  bool
}

// CallRequest contains the parameters for invoking a method on a resource.
type CallRequest struct {
	Token      string
	Args       property.Map
	PackageRef PackageRef
}

// CallResponse contains the result of invoking a method on a resource.
type CallResponse struct {
	Return   property.Map
	Failures []string
}

// moduleInstance represents a single runtime instance of an inlined module.
type moduleInstance struct {
	// Path identifies this instance within the module nesting tree. The
	// leaf [modulepath.Step] carries the count index or for_each key, if
	// any.
	Path modulepath.Path
	// ModuleInfo describes the module call this instance belongs to,
	// shared by every instance of the call.
	ModuleInfo *graph.ModuleInfo
	// Name is the resolved Pulumi logical name of this instance's component:
	// a `pulumi { name = ... }` override when present, else the parent
	// instance's Name joined to this instance's step with ".". It prefixes
	// the derived names of everything inside the instance and is exposed to
	// the instance as pulumi.module.name.
	Name string
	// Config is the parsed configuration of the called module, shared by
	// every instance of the call.
	Config  *ast.Config
	EvalCtx *eval.Context        // per-instance evaluation context
	URN     urn.URN              // component URN
	Parent  *moduleInstance      // enclosing module instance (nil for root-level calls)
	Index   *int                 // count index (nil if not using count)
	EachKey *cty.Value           // for_each key (nil if not using for_each)
	EachVal *cty.Value           // for_each value (nil if not using for_each)
	mu      sync.Mutex           // protects Outputs
	Outputs map[string]cty.Value // collected output values
}

// outputObject returns the instance's collected outputs as an object value.
func (inst *moduleInstance) outputObject() cty.Value {
	inst.mu.Lock()
	defer inst.mu.Unlock()
	if len(inst.Outputs) == 0 {
		return cty.EmptyObjectVal
	}
	return cty.ObjectVal(inst.Outputs)
}

// instancePath builds the path for one instance of a module call named name
// within the enclosing instance at parentPath, with the leaf step carrying
// the runtime disambiguator (count index or for_each key, if any).
func instancePath(parentPath modulepath.Path, name string, index *int, eachKey *cty.Value) modulepath.Path {
	switch {
	case index != nil:
		return parentPath.Append(modulepath.NewIndexedStep(name, *index))
	case eachKey != nil:
		return parentPath.Append(modulepath.NewKeyedStep(name, eachKey.AsString()))
	default:
		return parentPath.Append(modulepath.NewStep(name))
	}
}

// moduleInstanceName resolves the Pulumi logical name of one module instance:
// a `pulumi { name = ... }` override evaluated in the calling scope (with the
// instance's count.index/each.key in scope) is the full name; a null or
// absent override derives the name from the parent instance's resolved name
// joined to this instance's own step with ".".
func moduleInstanceName(
	mod *ast.Module, parentEvalCtx *eval.Context, parentName string,
	instPath modulepath.Path, index *int, eachKey, eachVal *cty.Value,
) (string, error) {
	if mod.PulumiName != nil {
		hclCtx := parentEvalCtx.HCLContextWithIteration(index, eachKey, eachVal)
		name, ok, err := evaluatePulumiName(mod.PulumiName, hclCtx, "module "+mod.Name)
		if err != nil {
			return "", err
		}
		if ok {
			return name, nil
		}
	}
	_, leaf, ok := instPath.Parent()
	if !ok {
		return "", fmt.Errorf("module %s: instance path is empty", mod.Name)
	}
	return joinModuleName(parentName, leaf.LogicalName()), nil
}

// inheritableOpts holds the resource options that child resources can inherit from their parent.
type inheritableOpts struct {
	Protect        *bool
	RetainOnDelete *bool
}

// unknownTokenDiag turns a packages.NotFoundError or ProviderAsResourceError
// into an hcl.Diagnostic anchored at typeRange. Other errors pass through.
func unknownTokenDiag(kind string, typeRange hcl.Range, err error) error {
	var pae *packages.ProviderAsResourceError
	if errors.As(err, &pae) {
		return hcl.Diagnostics{{
			Severity: hcl.DiagError,
			Summary:  "Provider declared as a resource",
			Detail:   pae.Error(),
			Subject:  typeRange.Ptr(),
		}}
	}
	var nfe *packages.NotFoundError
	if !errors.As(err, &nfe) {
		return err
	}
	d := &hcl.Diagnostic{
		Severity: hcl.DiagError,
		Summary:  fmt.Sprintf("unknown %s type %q", kind, nfe.Token),
		Subject:  typeRange.Ptr(),
	}
	if nfe.Suggestion != "" {
		d.Detail = fmt.Sprintf("did you mean %q?", nfe.Suggestion)
	}
	return hcl.Diagnostics{d}
}

// Engine executes HCL programs against the Pulumi engine.
type Engine struct {
	// config is the parsed HCL configuration.
	config *ast.Config

	// evaluator handles expression evaluation.
	evaluator *eval.Evaluator

	// pkgLoader loads Pulumi package schemas.
	pkgLoader schema.ReferenceLoader

	// providerInfoSource resolves TF provider mappings (bridge ProviderInfo)
	// so HCL block/attribute names line up 1:1 with the TF source. May be nil
	// when the host can't reach a mapper, in which case the convention-based
	// transform path is used.
	providerInfoSource bridge.ProviderInfoSource

	// resolver resolves TF resource/data source types to Pulumi schemas and
	// bridge mappings, shared with schema generation so both resolve identically.
	resolver *packages.Resolver

	// resmon is the resource monitor for registering resources.
	resmon ResourceMonitor

	// resourceOutputs maps resource keys to their output values.
	resourceOutputs *util.SyncMap[graph.InstanceKey, cty.Value]

	// resourceInheritableOpts maps resource keys to the options that children can inherit.
	resourceInheritableOpts *util.SyncMap[graph.InstanceKey, inheritableOpts]

	// defaultProviders maps a package name to the urn::id of its un-aliased
	// `provider "<pkg>" {}` block. Resources whose package matches and that
	// omit an explicit `provider` arg use this so the provider block's config
	// flows through, instead of the engine spinning up an empty default.
	defaultProviders *util.SyncMap[string, string]

	// stackOutputs collects outputs to be registered on the stack.
	stackOutputs map[string]property.Value

	// stackURN is the URN of the root stack resource.
	stackURN urn.URN

	// projectName is the current project name.
	projectName string

	// stackName is the current stack name.
	stackName string

	// organization is the current organization name.
	organization string

	// packages maps a Pulumi package name to its descriptor: local SDKs and
	// required_providers pins. It is the source of the version a block's
	// requests carry (see pinnedVersion).
	packages map[string]workspace.PackageDescriptor

	// packageRefs maps parameterized package alias to its RegisterPackage ref.
	packageRefs map[string]PackageRef

	// dryRun indicates if this is a preview operation.
	dryRun bool

	// dispatcher is EngineOptions.DestroyDispatcher (or a private default).
	dispatcher *DestroyDispatcher

	// workDir is the working directory.
	workDir string

	// absolutePaths mirrors EngineOptions.AbsolutePaths for the child-module
	// contexts created during the run.
	absolutePaths bool

	// pulumiConfig contains Pulumi stack configuration values.
	pulumiConfig map[string]ConfigValue

	// tfvars holds the root variable values read from the variable-value files
	// loaded automatically from the program directory. See loadTfvars.
	tfvars map[string]*hcl.Attribute

	// moduleLoader loads and caches module configurations.
	moduleLoader *modules.Loader

	// moduleInstances maps module path → list of instances for inlined modules.
	moduleInstances *util.SyncMap[modulepath.Path, []*moduleInstance]

	parallel int

	// expansions maps each (block, module instance) cell to the expansion
	// skeleton scheduling its instances. See cells.go.
	expansions *util.SyncMap[expansionCell, *graph.BlockExpansion]

	// planBarrier is the plan/apply phase boundary: every plan-time data read
	// completes before it, and every resource expansion waits for it. See
	// materializeRootCells.
	planBarrier pdag.Node

	// cellURNs records each cell's registered instance URNs, consumed by
	// dependency-metadata recording. See urnRegistry.
	cellURNs *urnRegistry

	// pendingURNs holds the URNs of resource instances whose outputs came back
	// with unknowns, so a data source that `depends_on` one of them can defer
	// its read to apply. An update that leaves every output known does not land
	// here; catching those needs the engine's diff, not the outputs.
	pendingURNs *util.SyncMap[string, struct{}]

	// pendingModuleCalls holds the cells of the module calls that contain a
	// pending resource, so `depends_on` naming a module call — which covers
	// every resource the module contains — defers a read just as naming the
	// resource itself does. A call is keyed by the block key it has in the
	// calling scope, so every instance of an expanded call shares one entry.
	pendingModuleCalls *util.SyncMap[expansionCell, struct{}]

	// failedNodes tracks resource nodes that failed to register, keyed by instance key.
	// Dependent nodes check this map and are skipped when a dependency failed.
	failedNodes *util.SyncMap[graph.InstanceKey, error]

	// graph is the resolved dependency graph for the current run. Stored on
	// the engine so processNode handlers can consult its topology (e.g.
	// processProvider checks whether anything depends on a provider node
	// before registering it).
	graph *graph.Graph

	// forcedCBD holds the resource node keys that must be created before their
	// prior instance is destroyed because they, or a resource depending on
	// them, declared create_before_destroy. Computed once from the graph.
	forcedCBD map[graph.NodeKey]bool

	// alwaysRegisterProviders forces every `provider` block to be registered
	// as a resource even when nothing references it, bypassing Terraform's
	// lazy provider-configure semantics. Test-only; see
	// EngineOptions.AlwaysRegisterProviders.
	alwaysRegisterProviders bool
}

// ConfigValue is a value supplied for a root variable. See [UntypedConfigValue] or
// [TypedConfigValue] to create one.
type ConfigValue struct {
	// Exactly one form is set: untyped holds a raw string parsed according to the
	// consuming variable's declared type (the Pulumi config / TF_VAR_ path), while
	// typed holds an already-structured value (the Construct path) whose marks —
	// secrets in particular — are preserved as-is. secret carries the secret bit for
	// an untyped value, whose raw string cannot itself carry a mark; a typed value
	// records its secretness on the value instead.

	untyped *string
	secret  bool
	typed   cty.Value
}

// UntypedConfigValue builds a ConfigValue from a raw string. The string is
// parsed according to the consuming variable's declared type, mirroring how
// OpenTofu parses -var / TF_VAR_ values. secret marks the value sensitive.
func UntypedConfigValue(s string, secret bool) ConfigValue {
	return ConfigValue{untyped: &s, secret: secret}
}

// TypedConfigValue builds a ConfigValue from an already-typed value, preserving
// any marks (such as secrets) it carries. Used when the caller already holds
// structured values, e.g. component Construct inputs.
func TypedConfigValue(v cty.Value) ConfigValue {
	return ConfigValue{typed: v}
}

// EngineOptions configures the engine.
type EngineOptions struct {
	// ProjectName is the Pulumi project name.
	ProjectName string

	// StackName is the Pulumi stack name.
	StackName string

	// Organization is the Pulumi organization name.
	Organization string

	// Config contains the values supplied for root variables, keyed by variable
	// name (optionally project-prefixed)
	Config map[string]ConfigValue

	// RootModule indicates the engine runs the program as the root module,
	// which is the only module that automatically loads variable-value files
	// (`terraform.tfvars` and friends). Engines that run a module — a component
	// or a module resource — leave it false.
	RootModule bool

	// DryRun indicates this is a preview operation.
	DryRun bool

	// DestroyDispatcher is the deployment's destroy-provisioner dispatch
	// table, shared by every engine of the deployment. Nil gets a fresh,
	// engine-private dispatcher.
	DestroyDispatcher *DestroyDispatcher

	// ResourceMonitor is the resource monitor for registering resources.
	ResourceMonitor ResourceMonitor

	// WorkDir is the working directory (where the program files are).
	WorkDir string

	// RootDir is the project root directory (where Pulumi.yaml is).
	RootDir string

	// AbsolutePaths makes path.module and path.root evaluate to absolute
	// directories. The Construct entry points set it: the module tree lives
	// outside the Pulumi program (a module cache, a bundle unpack dir, or an
	// arbitrary local path) while provider plugins resolve relative paths
	// against the program directory, so the relative values a direct program
	// run renders would point outside the module tree.
	AbsolutePaths bool

	SchemaLoader schema.ReferenceLoader

	// ProviderInfoSource is the bridge mapping resolver. Optional; when nil
	// the engine falls back to convention-based name mapping.
	ProviderInfoSource bridge.ProviderInfoSource

	// Packages maps parameterized package alias to its descriptor.
	// The engine calls RegisterPackage on the resource monitor for each entry before running the program.
	Packages map[string]workspace.PackageDescriptor

	// ModuleLoader loads child module configurations. It must not be nil.
	ModuleLoader *modules.Loader

	Parallel int

	// AlwaysRegisterProviders forces every `provider` block to be registered
	// as a resource even when no resource references it. This exists ONLY for
	// the language conformance tests, whose Pulumi-semantics fixtures expect
	// explicitly-declared providers to appear in the snapshot. Production runs
	// must leave this false to preserve Terraform's lazy provider-configure
	// behavior (an unused provider whose config would fail is never
	// configured).
	AlwaysRegisterProviders bool
}

// NewEngine creates a new execution engine.
func NewEngine(ctx context.Context, config *ast.Config, opts *EngineOptions) (*Engine, error) {
	contract.Requiref(opts.SchemaLoader != nil, "opts.SchemaLoader", "EngineOptions.SchemaLoader cannot be nil")
	contract.Requiref(opts.WorkDir != "", "opts.WorkDir", "EngineOptions.WorkDir cannot be empty")
	contract.Requiref(opts.RootDir != "", "opts.RootDir", "EngineOptions.RootDir cannot be empty")
	contract.Requiref(opts.ModuleLoader != nil, "EngineOptions.ModuleLoader", "cannot be empty")

	evalCtx, err := newEvalContext(opts.AbsolutePaths, opts.WorkDir, opts.RootDir, opts.WorkDir,
		opts.StackName, opts.ProjectName, opts.Organization)
	if err != nil {
		return nil, fmt.Errorf("creating the root evaluation context: %w", err)
	}

	// Only the root module auto-loads variable-value files; a module's own
	// `terraform.tfvars` is inert, including when that module is consumed as a
	// Pulumi component.
	var tfvars map[string]*hcl.Attribute
	if opts.RootModule {
		tfvars, err = loadTfvars(opts.WorkDir)
		if err != nil {
			return nil, fmt.Errorf("loading variable values: %w", err)
		}
	}

	dispatcher := opts.DestroyDispatcher
	if dispatcher == nil {
		dispatcher = NewDestroyDispatcher()
	}

	return &Engine{
		config:                  config,
		dispatcher:              dispatcher,
		evaluator:               eval.NewEvaluator(evalCtx),
		pkgLoader:               opts.SchemaLoader,
		providerInfoSource:      opts.ProviderInfoSource,
		resmon:                  opts.ResourceMonitor,
		resourceOutputs:         util.NewSyncMap[graph.InstanceKey, cty.Value](),
		resourceInheritableOpts: util.NewSyncMap[graph.InstanceKey, inheritableOpts](),
		defaultProviders:        util.NewSyncMap[string, string](),
		stackOutputs:            make(map[string]property.Value),
		projectName:             opts.ProjectName,
		stackName:               opts.StackName,
		organization:            opts.Organization,
		dryRun:                  opts.DryRun,
		workDir:                 opts.WorkDir,
		absolutePaths:           opts.AbsolutePaths,
		pulumiConfig:            opts.Config,
		tfvars:                  tfvars,
		packages:                opts.Packages,
		packageRefs:             make(map[string]PackageRef),
		moduleLoader:            opts.ModuleLoader,
		moduleInstances:         util.NewSyncMap[modulepath.Path, []*moduleInstance](),
		parallel:                opts.Parallel,
		expansions:              util.NewSyncMap[expansionCell, *graph.BlockExpansion](),
		cellURNs:                newURNRegistry(),
		pendingURNs:             util.NewSyncMap[string, struct{}](),
		pendingModuleCalls:      util.NewSyncMap[expansionCell, struct{}](),
		failedNodes:             util.NewSyncMap[graph.InstanceKey, error](),
		alwaysRegisterProviders: opts.AlwaysRegisterProviders,
		resolver: packages.NewResolver(
			opts.SchemaLoader, opts.ProviderInfoSource, opts.Packages, knownProviders(config.Terraform)),
	}, nil
}

func newEvalContext(absolutePaths bool, moduleDir, rootDir, rootModuleDir, stack, project, organization string) (*eval.Context, error) {
	if absolutePaths {
		return eval.NewAbsolutePathContext(moduleDir, rootDir, rootModuleDir, stack, project, organization)
	}
	return eval.NewContext(moduleDir, rootDir, rootModuleDir, stack, project, organization)
}

// Run executes the HCL program.
func (e *Engine) Run(ctx context.Context) error {
	ctx, span := potel.Start(ctx, "Engine.Run")
	defer span.End()
	for alias, pkg := range e.packages {
		// A bare pin names no plugin beyond its version, which every request
		// carries; a ref would displace the schema's download URL.
		if pkg.Parameterization == nil && pkg.ExtensionParameterization == nil && pkg.PluginDownloadURL == "" {
			continue
		}
		ref, err := e.resmon.RegisterPackage(ctx, pkg)
		if err != nil {
			return fmt.Errorf("registering package %s: %w", alias, err)
		}
		e.packageRefs[alias] = ref
	}

	// Register the root stack resource to get its URN for outputs
	if err := e.registerStack(ctx); err != nil {
		return fmt.Errorf("registering stack: %w", err)
	}

	// Before any resource registration: a delete-before-replace delete fires
	// BeforeDelete hooks while the triggering registration is still blocked.
	e.registerDestroyDispatcher(ctx)

	if err := e.warnUndeclaredTfvars(ctx); err != nil {
		return err
	}

	if err := e.installProviderFunctions(ctx, e.evaluator.Context(), e.config, nil); err != nil {
		return err
	}

	// Build the dependency graph with module inlining
	g, err := graph.BuildFromConfig(e.config, &moduleLoaderAdapter{e.moduleLoader}, e.workDir)
	if err != nil {
		return fmt.Errorf("building dependency graph: %w", err)
	}

	// Validate the graph
	if errs := g.Validate(); len(errs) > 0 {
		return errors.Join(errs...)
	}
	e.graph = g
	e.forcedCBD = g.ForcedCreateBeforeDestroy()

	// Resource/data instances are scheduled by expansion cells; their graph
	// nodes serve as completion barriers only. Root cells materialize before
	// the walk; each module's cells materialize when its init node runs.
	if err := e.materializeRootCells(g); err != nil {
		return fmt.Errorf("materializing expansion cells: %w", err)
	}

	// Process nodes in parallel where possible
	if err := e.processGraph(ctx, g); err != nil {
		return err
	}

	// The engine deletes removed-block resources after the program exits;
	// their dispatcher entries must be recorded by then.
	if err := e.recordRemovedBlockEntries(ctx); err != nil {
		return err
	}

	// Collect errors from resources that failed to register but were not fatal
	// (i.e., we continued processing to allow independent resources to proceed).
	nodeErrs := slices.Collect(e.failedNodes.Values())

	// Evaluate check blocks after the program has exited.
	if err := e.evaluateChecks(ctx); err != nil && len(nodeErrs) == 0 {
		return err
	}

	// Process outputs (collect them into stackOutputs). Under continue-on-error
	// some resources failed but recover() can still surface fallback values, so
	// outputs are computed regardless; an output that cannot be computed on a
	// failed run is dropped rather than masking the resource failures below.
	for name, output := range e.config.Outputs {
		if err := e.processOutput(ctx, name, output); err != nil {
			if len(nodeErrs) > 0 {
				continue
			}
			return fmt.Errorf("processing output %s: %w", name, err)
		}
	}

	// Register stack outputs
	if err := e.registerStackOutputs(ctx); err != nil {
		return fmt.Errorf("registering stack outputs: %w", err)
	}

	// Surface resource failures; the engine turns these into a bail under
	// continue-on-error, after the recovered outputs above are registered.
	if len(nodeErrs) > 0 {
		return errors.Join(nodeErrs...)
	}

	return nil
}

// registerStack registers the root stack resource.
func (e *Engine) registerStack(ctx context.Context) error {
	if e.resmon == nil {
		return nil
	}

	stackName := fmt.Sprintf("%s-%s", e.projectName, e.stackName)
	resp, err := e.resmon.RegisterResource(ctx, RegisterResourceRequest{
		Type:   "pulumi:pulumi:Stack",
		Name:   stackName,
		Inputs: property.NewMap(nil),
	})
	if err != nil {
		return err
	}

	e.stackURN = resp.URN
	return nil
}

// registerStackOutputs registers all collected outputs on the stack.
func (e *Engine) registerStackOutputs(ctx context.Context) error {
	if e.resmon == nil || len(e.stackOutputs) == 0 {
		return nil
	}

	return e.resmon.RegisterResourceOutputs(ctx, e.stackURN, property.NewMap(e.stackOutputs))
}

// processNode processes a single node based on its type.
func (e *Engine) processNode(ctx context.Context, node *graph.Node) error {
	switch node.Type {
	case graph.NodeTypeVariable:
		return e.processVariable(ctx, node)
	case graph.NodeTypeVariableValidation:
		return e.processVariableValidation(node)
	case graph.NodeTypeLocal:
		return e.processLocal(ctx, node)
	case graph.NodeTypeResource, graph.NodeTypeDataSource:
		// Instances run in expansion cells (cells.go); the node itself is a
		// completion barrier ordered after every cell via CompleteBefore.
		return nil
	case graph.NodeTypeModuleInit:
		return e.processModuleInit(ctx, node)
	case graph.NodeTypeModule:
		return e.processModuleComplete(ctx, node)
	case graph.NodeTypeCall:
		return e.processCall(ctx, node)
	case graph.NodeTypeOutput:
		if node.ModuleInfo != nil {
			return e.processModuleOutput(ctx, node)
		}
		return nil
	case graph.NodeTypeProvider:
		return e.processProvider(ctx, node)
	case graph.NodeTypeBuiltin:
		return nil
	case graph.NodeTypeUnknown:
		return errors.New("unknown node type")
	default:
		return fmt.Errorf("unknown node type: %v", node.Type)
	}
}

func (e *Engine) processGraph(ctx context.Context, g *graph.Graph) error {
	check := func(ctx context.Context) error {
		return e.checkConfigPulumiVersion(ctx,
			e.config, e.evaluator.EvaluateExpression)
	}
	if err := g.InjectAfter(check, func(n *graph.Node) bool {
		return n.Type == graph.NodeTypeVariable && n.ModuleInfo == nil
	}); err != nil {
		return err
	}
	return g.Walk(ctx, e.processNode, e.parallel)
}

// warnUndeclaredTfvars reports every name set by a variable-value file that the
// root module does not declare. Such a value reaches nothing — in particular it
// does not reach a module input of the same name — so the only signal a user
// gets that the file is not doing what they meant is this warning.
func (e *Engine) warnUndeclaredTfvars(ctx context.Context) error {
	for _, varName := range slices.Sorted(maps.Keys(e.tfvars)) {
		if _, declared := e.config.Variables[varName]; declared {
			continue
		}
		err := e.warnf(ctx, "Value for undeclared variable: the root module does not declare a variable "+
			"named %q but a value was found in file %q. If you meant to use this value, add a \"variable\" "+
			"block to the configuration.", varName, e.tfvars[varName].Range.Filename)
		if err != nil {
			return err
		}
	}
	return nil
}

// varSource is where a root variable's value came from. Most of the resolution
// is source-independent; the few steps that are not name the source they mean.
type varSource int

const (
	sourceNone varSource = iota
	// sourceSupplied covers every value supplied from outside the program —
	// Pulumi stack config, a variable-value file, TF_VAR_<name> — which
	// resolution treats alike once the value is typed.
	sourceSupplied
	sourceConfigTyped
	sourceDefault
)

// variableDefault evaluates a variable's declared default.
func (e *Engine) variableDefault(v *ast.Variable) (cty.Value, error) {
	val, diags := e.evaluator.EvaluateExpression(v.Default)
	if diags.HasErrors() {
		return cty.NilVal, fmt.Errorf("evaluating variable default: %s", diags.Error())
	}
	return val, nil
}

// lookupConfig returns the Pulumi stack config value supplied for a root
// variable, preferring the project-prefixed key.
func (e *Engine) lookupConfig(varName string) (ConfigValue, bool) {
	if cv, ok := e.pulumiConfig[e.projectName+":"+varName]; ok {
		return cv, true
	}
	cv, ok := e.pulumiConfig[varName]
	return cv, ok
}

// processVariable processes a variable definition.
func (e *Engine) processVariable(ctx context.Context, node *graph.Node) error {
	v := node.Variable
	if v == nil {
		return fmt.Errorf("variable node missing Variable field")
	}

	// Module variable: evaluate input expression in parent context, store in each instance context.
	if node.ModuleInfo != nil {
		return e.processModuleVariable(node)
	}

	varName := v.Name
	var val cty.Value
	var isSecret bool
	source := sourceNone

	// Variable value precedence (highest to lowest):
	// 1. Pulumi stack config (projectName:<name>), which stands in for -var
	// 2. Automatically-loaded variable-value files (see loadTfvars)
	// 3. Environment variable TF_VAR_<name>
	// 4. Default value

	if e.evaluator.Context().HCLContext().Variables["var"].Type().HasAttribute(varName) {
		return fmt.Errorf("%q already evaluated", varName)
	}

	// A value that arrives as a raw string is parsed according to the declared
	// type. A variable declared without a type keeps its literal string value,
	// matching OpenTofu's VariableParseLiteral default; one declared with any
	// type — including `any` — parses its value as HCL (VariableParseHCL).
	parseString := func(s string) (cty.Value, error) {
		if v.TypeConstraint == cty.NilType {
			return cty.StringVal(s), nil
		}
		converted, err := convertStringToType(s, v.TypeConstraint)
		if err != nil {
			return cty.NilVal, fmt.Errorf("variable %q: %w", varName, err)
		}
		return converted, nil
	}

	var err error
	if cv, ok := e.lookupConfig(varName); ok {
		if cv.untyped != nil {
			if val, err = parseString(*cv.untyped); err != nil {
				return err
			}
			source = sourceSupplied
			isSecret = cv.secret
		} else {
			// An already-typed value; its marks (e.g. secrets) ride along.
			val = cv.typed
			source = sourceConfigTyped
		}
	} else if tfvarsVal, ok := e.tfvars[varName]; ok {
		if val, err = tfvarsValue(tfvarsVal); err != nil {
			return err
		}
		source = sourceSupplied
	} else if envVal, ok := os.LookupEnv("TF_VAR_" + varName); ok {
		// A variable set to the empty string is set: only an absent variable
		// falls through to the default.
		if val, err = parseString(envVal); err != nil {
			return err
		}
		source = sourceSupplied
	}

	// If no value from any source, use default. A variable without a default
	// is required, regardless of its `nullable` setting (which only governs
	// whether a *provided* value may be the null literal).
	if source == sourceNone {
		if v.Default == nil {
			return fmt.Errorf("variable %q is required but no value was provided. Set it in a "+
				"variable-value file such as terraform.tfvars, with the TF_VAR_%s environment "+
				"variable, or with Pulumi config: pulumi config set %s <value>",
				varName, varName, varName)
		}
		if val, err = e.variableDefault(v); err != nil {
			return err
		}
		source = sourceDefault
	}

	// A `nullable = false` variable rejects an explicit null value: the
	// default is substituted when one is declared, otherwise it is an
	// error.
	if val.IsNull() && !v.Nullable && source != sourceDefault {
		if v.Default == nil {
			return fmt.Errorf("variable %q must not be set to null: it is declared with nullable = false and has no default", varName)
		}
		if val, err = e.variableDefault(v); err != nil {
			return err
		}
		source = sourceDefault
	}

	// Fill in optional()-attribute defaults before sensitive marking.
	if v.TypeDefaults != nil && !val.IsNull() {
		val = v.TypeDefaults.Apply(val)
	}

	if v.TypeConstraint != cty.NilType && v.TypeConstraint != cty.DynamicPseudoType {
		converted, err := ctyconvert.Convert(val, v.TypeConstraint)
		if err != nil {
			if source == sourceDefault {
				return fmt.Errorf("variable %q: this default value is not compatible with the "+
					"variable's type constraint: %s", varName, err)
			}
			return fmt.Errorf("variable %q: the given value is not suitable for var.%s: %s",
				varName, varName, err)
		}
		val = converted
	}

	// A resource reference supplied as a typed config value — e.g. a component
	// input from a calling program — carries only its identity. Fetch the
	// referenced resource's state so the program can read its fields.
	if source == sourceConfigTyped {
		if u, ok := eval.ResourceReferenceURN(val); ok {
			resolved, err := e.resolveConfigResourceReference(ctx, val, u)
			if err != nil {
				return fmt.Errorf("variable %q: %w", varName, err)
			}
			val = resolved
		}
	}

	// Handle sensitive marking
	if v.Sensitive || isSecret {
		val = val.Mark(eval.SensitiveMark)
	}
	if v.Ephemeral {
		val = val.Mark(eval.EphemeralMark)
	}

	// Store in eval context (needed for validation which may reference var.<name>)
	e.evaluator.Context().SetVariable(varName, val)

	return runVariableValidations(e.evaluator, varName, v.Validations)
}

// processVariableValidation runs the validation rules of a variable whose
// rules reference other objects (e.g. a resource's computed output). The rules
// live on their own graph node, ordered after both the variable's value and
// the referenced objects, so consumers of the variable only observe a
// validated value.
func (e *Engine) processVariableValidation(node *graph.Node) error {
	v := node.Variable
	if v == nil {
		return fmt.Errorf("variable validation node missing Variable field")
	}
	if node.ModuleInfo != nil {
		return e.forEachModuleInstance(node, func(inst *moduleInstance) error {
			return runVariableValidations(eval.NewEvaluator(inst.EvalCtx), v.Name, v.Validations)
		})
	}
	return runVariableValidations(e.evaluator, v.Name, v.Validations)
}

// runVariableValidations evaluates a variable's `validation` rules against ev
// (whose context must already hold the variable's value). It returns an error
// for the first rule whose condition is known and false; unknown conditions are
// deferred.
func runVariableValidations(ev *eval.Evaluator, varName string, validations []*ast.Validation) error {
	for i, validation := range validations {
		condVal, diags := ev.EvaluateExpression(validation.Condition)
		if diags.HasErrors() {
			return fmt.Errorf("evaluating validation condition %d for variable %q: %s", i+1, varName, diags.Error())
		}
		condVal, _ = condVal.Unmark()
		if !condVal.IsKnown() {
			continue
		}

		condOK, err := conditionResultToBool(condVal)
		if err != nil {
			return fmt.Errorf("validation condition %d for variable %q: %s", i+1, varName, err)
		}

		if !condOK {
			errMsgVal, diags := ev.EvaluateExpression(validation.ErrorMessage)
			if diags.HasErrors() {
				return fmt.Errorf("validation failed for variable %q (could not evaluate error message: %s)",
					varName, diags.Error())
			}
			errMsg := "validation failed"
			if s := renderErrorMessage(errMsgVal); s != "" {
				errMsg = s
			}
			return fmt.Errorf("validation failed for variable %q: %s", varName, errMsg)
		}
	}

	return nil
}

// convertStringToType converts a string-sourced variable value (Pulumi config
// or a TF_VAR_ environment variable) to the variable's declared type.
func convertStringToType(s string, targetType cty.Type) (cty.Value, error) {
	var val cty.Value
	if targetType.IsPrimitiveType() {
		val = cty.StringVal(s)
	} else {
		expr, diags := hclsyntax.ParseExpression([]byte(s), "<variable value>", hcl.Pos{Line: 1, Column: 1})
		if diags.HasErrors() {
			return cty.NilVal, fmt.Errorf("cannot parse %q as an HCL expression: %s", s, diags.Error())
		}
		v, valDiags := expr.Value(nil)
		if valDiags.HasErrors() {
			return cty.NilVal, fmt.Errorf("cannot evaluate %q: %s", s, valDiags.Error())
		}
		val = v
	}

	converted, err := ctyconvert.Convert(val, targetType)
	if err != nil {
		return cty.NilVal, fmt.Errorf("cannot convert %q to %s: %w", s, targetType.FriendlyName(), err)
	}
	return converted, nil
}

// processLocal processes a local value definition.
func (e *Engine) processLocal(ctx context.Context, node *graph.Node) error {
	local := node.Local
	if local == nil {
		return fmt.Errorf("local node missing Local field")
	}

	if node.ModuleInfo != nil {
		return e.forEachModuleInstance(node, func(inst *moduleInstance) error {
			localName := strings.TrimPrefix(node.Key.ID, "local.")
			val, diags := local.Value.Value(inst.EvalCtx.HCLContext())
			if diags.HasErrors() {
				return fmt.Errorf("evaluating local value %s: %s", localName, diags.Error())
			}
			inst.EvalCtx.SetLocal(localName, val)
			return nil
		})
	}

	val, diags := e.evaluator.EvaluateExpression(local.Value)
	if diags.HasErrors() {
		return fmt.Errorf("evaluating local value: %s", diags.Error())
	}

	localName := node.Key.ID[6:] // Remove "local." prefix
	e.evaluator.Context().SetLocal(localName, val)

	return nil
}

// processProvider processes a provider configuration and registers it as a provider resource.
func (e *Engine) processProvider(ctx context.Context, node *graph.Node) error {
	provider := node.Provider
	if provider == nil {
		return fmt.Errorf("provider node missing Provider field")
	}

	// TF/tofu only configures providers that something actually uses; an
	// unused `provider` block is silently ignored even if its body would
	// fail validate/configure. The graph already captures every real use
	// as an in-edge to the provider node (explicit `provider = ...` refs
	// and root implicit-default refs), so no dependents means no use.
	// alwaysRegisterProviders (test-only) opts out so conformance fixtures
	// see explicitly-declared providers in the snapshot.
	if !e.alwaysRegisterProviders && e.graph != nil && !e.graph.HasDependents(node.Key) {
		return nil
	}

	if node.ModuleInfo != nil {
		return e.forEachModuleInstance(node, func(inst *moduleInstance) error {
			return e.registerProvider(ctx, node, provider, inst.EvalCtx, inst.URN, inst)
		})
	}

	return e.registerProvider(ctx, node, provider, e.evaluator.Context(), e.stackURN, nil)
}

// providerInstance carries one `for_each` instance of a provider block: the
// instance key and the element value bound to each.key/each.value while the
// block's config is evaluated.
type providerInstance struct {
	key   string
	value cty.Value
}

// registerProvider registers a provider block in evalCtx: one instance per
// for_each key when the block has `for_each`, otherwise a single
// configuration.
func (e *Engine) registerProvider(
	ctx context.Context, node *graph.Node, provider *ast.Provider,
	evalCtx *eval.Context, parentURN urn.URN, modInst *moduleInstance,
) error {
	if provider.ForEach == nil {
		return e.registerProviderInContext(ctx, node, provider, evalCtx, parentURN, modInst, nil)
	}

	forEach, unknown, _, diags := eval.NewEvaluator(evalCtx).EvaluateForEach(provider.ForEach)
	if diags.HasErrors() {
		return fmt.Errorf("evaluating for_each for provider %s: %s", node.Key, diags.Error())
	}
	// Provider instances must be configurable up front, so unlike a
	// resource's for_each an unknown value is an error even during preview.
	if unknown {
		return fmt.Errorf("%s: the for_each value depends on values that are not yet known", node.Key)
	}

	for _, key := range slices.Sorted(maps.Keys(forEach)) {
		inst := &providerInstance{key: key, value: forEach[key]}
		if err := e.registerProviderInContext(ctx, node, provider, evalCtx, parentURN, modInst, inst); err != nil {
			return err
		}
	}

	// An empty for_each still binds the provider address so a reference
	// evaluates to an empty collection rather than "no such attribute".
	if len(forEach) == 0 {
		evalCtx.SetResource(node.Key.ID, "", cty.EmptyObjectVal)
	}

	return nil
}

// resolvePassThroughProvider looks up a provider passed into a module via
// `providers = { <localKey> = <parentExpr> }` and returns the resolved
// URN::ID, or "" when the resource isn't in a module, there's no entry for
// localKey, or the parent expression doesn't yield a provider reference. An
// expression the parent's scope doesn't bind is chased recursively through
// the parent's own pass-through entries.
func (e *Engine) resolvePassThroughProvider(modInfo *graph.ModuleInfo, localKey string) string {
	if modInfo == nil || modInfo.Module == nil || localKey == "" {
		return ""
	}
	passExpr, ok := modInfo.Module.Providers[localKey]
	if !ok {
		return ""
	}
	if parentCtx := e.parentEvalContext(modInfo); parentCtx != nil {
		val, diags := eval.NewEvaluator(parentCtx).EvaluateExpression(passExpr)
		if !diags.HasErrors() {
			if ref, err := providerRefFromCty(val); err == nil {
				return ref
			}
		}
	}
	return e.resolvePassThroughProvider(e.parentModuleInfo(modInfo), providerExprKey(passExpr))
}

// resolvePassedProviders resolves a module call's `providers = { ... }`
// entries to "<urn>::<id>" references keyed by the provider's package name.
func (e *Engine) resolvePassedProviders(modInfo *graph.ModuleInfo, parentEvalCtx *eval.Context) map[string]string {
	var refs map[string]string
	for _, passExpr := range modInfo.Module.Providers {
		ref := ""
		val, diags := eval.NewEvaluator(parentEvalCtx).EvaluateExpression(passExpr)
		if !diags.HasErrors() {
			if r, err := providerRefFromCty(val); err == nil {
				ref = r
			}
		}
		if ref == "" {
			ref = e.resolvePassThroughProvider(e.parentModuleInfo(modInfo), providerExprKey(passExpr))
		}
		if ref == "" {
			continue
		}
		parsed, err := providers.ParseReference(ref)
		if err != nil {
			continue
		}
		if refs == nil {
			refs = make(map[string]string)
		}
		refs[parsed.URN().Type().Name().String()] = ref
	}
	return refs
}

// parentModuleInfo returns the ModuleInfo of the module call enclosing
// modInfo, or nil when the parent is the root config (or has no instances).
func (e *Engine) parentModuleInfo(modInfo *graph.ModuleInfo) *graph.ModuleInfo {
	if modInfo.ParentPath().IsRoot() {
		return nil
	}
	insts, ok := e.moduleInstances.Get(modInfo.ParentPath())
	if !ok || len(insts) == 0 {
		return nil
	}
	return insts[0].ModuleInfo
}

// resolveExplicitProvider resolves a resource/data source `provider = ...`
// expression to a "<urn>::<id>" reference. A pass-through entry of the
// instantiating module call wins over any local definition; otherwise the
// expression is evaluated in the resource's own scope. A bare `provider =
// name` reference whose default configuration is registered nowhere in scope
// resolves to "": the reference names the implicit empty default
// configuration, i.e. the engine default.
func (e *Engine) resolveExplicitProvider(
	expr hcl.Expression, evalCtx *eval.Context, modInfo *graph.ModuleInfo,
) (string, error) {
	if ref := e.resolvePassThroughProvider(modInfo, providerExprKey(expr)); ref != "" {
		return ref, nil
	}
	val, valDiags := eval.NewEvaluator(evalCtx).EvaluateExpression(expr)
	if !valDiags.HasErrors() {
		return providerRefFromCty(val)
	}
	vars := expr.Variables()
	if len(vars) != 1 || len(vars[0]) != 1 {
		return "", errors.New(valDiags.Error())
	}
	name := vars[0].RootName()
	if modInfo != nil {
		if ref := e.inheritedDefaultProvider(modInfo, name); ref != "" {
			return ref, nil
		}
		return "", nil
	}
	if ref, ok := e.defaultProviders.Get(e.providerPackageName(name)); ok {
		return ref, nil
	}
	return "", nil
}

// inheritedDefaultProvider walks up the module tree from modInfo, returning
// the nearest ancestor's un-aliased default provider config for the provider
// that modInfo calls name (URN::ID), or "" if none. An ancestor's default is
// its own registered block or one passed to it through its module call.
// Inheritance is by fully-qualified provider address, so each ancestor is
// searched under the name that ancestor gives the provider. The graph adds a
// matching edge so that block is registered before this resolves.
func (e *Engine) inheritedDefaultProvider(modInfo *graph.ModuleInfo, name string) string {
	fqn := graph.ProviderFQN(modInfo.Terraform, name)
	for path := modInfo.Path; ; {
		parent, _, ok := path.Parent()
		if !ok {
			return ""
		}
		var parentInfo *graph.ModuleInfo
		tfBlock := e.config.Terraform
		if insts, ok := e.moduleInstances.Get(parent); ok && len(insts) > 0 {
			parentInfo = insts[0].ModuleInfo
			tfBlock = parentInfo.Terraform
		}
		local := graph.LocalProviderName(tfBlock, fqn, name)
		if outputs, ok := e.nodeOutputs(graph.NodeKey{Module: parent, ID: local}); ok {
			if ref, err := providerRefFromCty(outputs); err == nil {
				return ref
			}
		}
		if parentInfo != nil {
			if ref := e.resolvePassThroughProvider(parentInfo, local); ref != "" {
				return ref
			}
		}
		path = parent
	}
}

// parentEvalContext returns the eval.Context of the enclosing module
// instance (or the root context when modInfo's parent is root).
func (e *Engine) parentEvalContext(modInfo *graph.ModuleInfo) *eval.Context {
	if modInfo.ParentPath().IsRoot() {
		return e.evaluator.Context()
	}
	parentInsts, ok := e.moduleInstances.Get(modInfo.ParentPath())
	if !ok || len(parentInsts) == 0 {
		return nil
	}
	return parentInsts[0].EvalCtx
}

// providerExprKey returns "name" or "name.alias" from a provider-reference
// expression. Returns "" for anything that isn't a single one-or-two-step
// traversal. Mirrors graph.providerExprKey — duplicated to keep the run
// package free of internal graph helpers.
func providerExprKey(expr hcl.Expression) string {
	if expr == nil {
		return ""
	}
	traversals := expr.Variables()
	if len(traversals) != 1 {
		return ""
	}
	t := traversals[0]
	if len(t) == 0 {
		return ""
	}
	name := t.RootName()
	if len(t) == 1 {
		return name
	}
	if attr, ok := t[1].(hcl.TraverseAttr); ok {
		return name + "." + attr.Name
	}
	return name
}

// evalSchemaInputs evaluates schema inputs and reports successful values.
func evalSchemaInputs(
	hclCtx *hcl.EvalContext,
	onEvaluated func(resource.PropertyKey, cty.Value),
) transform.EvalFunc {
	return func(propKey resource.PropertyKey, expr hcl.Expression, extraVars map[string]cty.Value) (cty.Value, hcl.Diagnostics) {
		c := hclCtx
		if len(extraVars) > 0 {
			c = hclCtx.NewChild()
			c.Variables = extraVars
		}
		val, diags := expr.Value(c)
		if !diags.HasErrors() && onEvaluated != nil {
			onEvaluated(propKey, val)
		}
		return val, diags
	}
}

// evalResourceInputs evaluates resource inputs and records property dependencies.
func evalResourceInputs(
	hclCtx *hcl.EvalContext,
	inputProperties []*schema.Property,
	onEvaluated func(resource.PropertyKey, cty.Value),
) (transform.EvalFunc, map[string][]string) {
	plainInputProps := make(map[string]bool, len(inputProperties))
	for _, p := range inputProperties {
		plainInputProps[p.Name] = p.Plain
	}

	dependsOn := make(map[string][]string)
	record := func(propKey resource.PropertyKey, val cty.Value) {
		if onEvaluated != nil {
			onEvaluated(propKey, val)
		}

		prop := string(propKey)
		if _, ok := dependsOn[prop]; !ok {
			dependsOn[prop] = nil
		}
		if plainInputProps[prop] {
			return
		}
		for _, urn := range eval.CollectDepURNs(val) {
			idx, found := slices.BinarySearch(dependsOn[prop], urn)
			if !found {
				dependsOn[prop] = slices.Insert(dependsOn[prop], idx, urn)
			}
		}
	}
	return evalSchemaInputs(hclCtx, record), dependsOn
}

func (e *Engine) registerProviderInContext(
	ctx context.Context, node *graph.Node, provider *ast.Provider,
	evalCtx *eval.Context, parentURN urn.URN, modInst *moduleInstance,
	inst *providerInstance,
) error {
	pkgName := e.providerPackageName(provider.Name)
	typeToken := "pulumi:providers:" + pkgName

	if inst != nil {
		key := cty.StringVal(inst.key)
		evalCtx = evalCtx.WithIteration(nil, &key, &inst.value)
	}

	hclCtx := evalCtx.HCLContext()

	// Schema-aware eval is needed so schema.Property.Secret marks survive.
	pkg, perr := packages.ResolvePackage(ctx, e.pkgLoader, knownProviders(e.config.Terraform), "pulumi_providers_"+pkgName)
	if perr != nil {
		return fmt.Errorf("resolving provider package %s: %w", provider.Name, perr)
	}
	resSchema, perr := pkg.Provider()
	if perr != nil {
		return fmt.Errorf("resolving provider schema for %s: %w", provider.Name, perr)
	}

	providerMapping := e.resolver.ProviderConfigBodyMapping(ctx, pkgName)
	inputEval, dependsOn := evalResourceInputs(hclCtx, resSchema.InputProperties, nil)
	inputsMap, _, diags := transform.EvalResourceWithSchema(
		provider.Config, resSchema, providerMapping, inputEval)
	if diags.HasErrors() {
		return fmt.Errorf("evaluating provider %s config: %s", provider.Name, diags.Error())
	}
	inputs := make(map[string]property.Value, inputsMap.Len())
	inputsMap.AllStable(func(k string, v property.Value) bool {
		inputs[k] = v
		return true
	})

	logicalName := provider.Alias
	if logicalName == "" {
		logicalName = provider.Name
	}
	if inst != nil {
		logicalName = modulepath.NewKeyedStep(logicalName, inst.key).LogicalName()
	}
	if modInst != nil {
		logicalName = joinModuleName(modInst.Name, logicalName)
	}

	// Version comes from an explicit attribute, else required_providers.
	var version string
	if provider.Version != nil {
		val, vdiags := provider.Version.Value(hclCtx)
		if vdiags.HasErrors() {
			return fmt.Errorf("evaluating provider version: %s", vdiags.Error())
		}
		if val.Type() == cty.String {
			version = val.AsString()
		}
	}
	if version == "" {
		version = e.pinnedVersion(provider.Name)
	}

	req := RegisterResourceRequest{
		Type:       typeToken,
		Name:       logicalName,
		Custom:     true,
		Parent:     parentURN,
		Version:    version,
		PackageRef: e.packageRefs[pkgName],
	}
	if resSchema.PackageReference != nil {
		req.PluginDownloadURL = resSchema.PackageReference.PluginDownloadURL()
	}

	if provider.EnvVarMappings != nil {
		val, vdiags := provider.EnvVarMappings.Value(hclCtx)
		if vdiags.HasErrors() {
			return fmt.Errorf("evaluating env_var_mappings: %s", vdiags.Error())
		}
		mappings, err := transform.CtyToPropertyValue(val)
		if err != nil {
			return fmt.Errorf("converting env_var_mappings: %w", err)
		}
		if mappings.IsMap() {
			req.EnvVarMappings = make(map[string]string)
			mappings.AsMap().AllStable(func(k string, v property.Value) bool {
				if v.IsString() {
					req.EnvVarMappings[k] = v.AsString()
				}
				return true
			})
		}
	}

	if provider.PluginDownloadURL != nil {
		val, vdiags := provider.PluginDownloadURL.Value(hclCtx)
		if vdiags.HasErrors() {
			return fmt.Errorf("evaluating plugin_download_url: %s", vdiags.Error())
		}
		if val.Type() == cty.String {
			// Surface via Inputs so it appears as a top-level output;
			// the resource-option field (req.PluginDownloadURL) is
			// set below to the package's schema default.
			inputs["pluginDownloadURL"] = property.New(val.AsString())
		}
	}

	if provider.AdditionalSecretOutputs != nil {
		val, vdiags := provider.AdditionalSecretOutputs.Value(hclCtx)
		if vdiags.HasErrors() {
			return fmt.Errorf("evaluating additional_secret_outputs: %s", vdiags.Error())
		}
		if val.Type().IsTupleType() || val.Type().IsListType() {
			for it := val.ElementIterator(); it.Next(); {
				_, v := it.Element()
				if v.Type() == cty.String {
					req.AdditionalSecretOutputs = append(req.AdditionalSecretOutputs, v.AsString())
				}
			}
		}
	}

	req.Inputs = property.NewMap(inputs)
	req.PropertyDependencies = dependsOn
	for _, deps := range dependsOn {
		req.Dependencies = append(req.Dependencies, deps...)
	}
	req.Dependencies = e.cellURNs.widen(req.Dependencies)

	resp, err := e.resmon.RegisterResource(ctx, req)
	if err != nil {
		return fmt.Errorf("registering provider %s: %w", node.Key, err)
	}

	providerID := resp.ID

	// Use ResourceOutputToCty to match processResource's snake_case key emission.
	outputObj, err := transform.ResourceOutputToCty(resp.Outputs, resSchema, providerMapping, e.dryRun)
	if err != nil {
		return fmt.Errorf("converting provider outputs to HCL types: %w", err)
	}
	if e.dryRun && providerID == "" {
		outputObj["id"] = cty.UnknownVal(cty.String)
	} else {
		outputObj["id"] = cty.StringVal(providerID)
	}
	// Providers are resource references that `provider = X.alias` resolves
	// through the eval context, so their `urn` must stay visible (it is not
	// marked synthetic). Unlike a managed resource, a provider can't be
	// referenced as a value in HCL, so there's no user-facing iteration to leak
	// into.
	outputObj["urn"] = cty.StringVal(string(resp.URN))

	outputsKey := graph.InstanceKey{Node: node.Key}
	if inst != nil {
		outputsKey.Suffix = fmt.Sprintf("[%q]", inst.key)
	}
	e.resourceOutputs.Set(outputsKey, cty.ObjectVal(outputObj).Mark(eval.DepMark(resp.URN)))

	// Top-level un-aliased provider blocks become the default provider for
	// resources of the same package that don't set `provider` explicitly.
	if provider.Alias == "" && node.ModuleInfo == nil && providerID != "" {
		e.defaultProviders.Set(pkgName, string(resp.URN)+"::"+providerID)
	}

	markedProviderOutputs := cty.ObjectVal(outputObj).Mark(eval.DepMark(resp.URN))
	bareKey := node.Key.ID
	if inst != nil {
		// Instances assemble into an object keyed by each.key, so
		// `<name>.<alias>["key"]` selects one through ordinary indexing.
		evalCtx.SetEachResource(bareKey, inst.key, resp.URN, markedProviderOutputs)
	} else {
		evalCtx.SetResource(bareKey, resp.URN, markedProviderOutputs)
	}

	return nil
}

// registerResourceInstanceInContext registers a single resource instance with Pulumi.
func (e *Engine) registerResourceInstanceInContext(
	ctx context.Context,
	node *graph.Node,
	res *ast.Resource,
	resSchema *schema.Resource,
	instance *graph.ExpandedResource,
	evalCtx *eval.Context,
	parentURN urn.URN,
	modInst *moduleInstance,
	metaArgDeps []string,
) error {
	evalCtx = evalCtx.WithIteration(instance.Index, instance.EachKey, instance.EachValue)

	hclCtx := evalCtx.HCLContext()

	resourceMapping := e.resolver.ResourceBodyMapping(ctx, res.Type)
	// terraform_data's attributes are dynamically typed, so their cty types
	// (e.g. set-ness) survive the Pulumi property round-trip only via the
	// evaluated values captured here; see wrapTerraformDataInputs.
	var tdataEvaluated map[string]cty.Value
	if res.Type == packages.TerraformDataType {
		tdataEvaluated = map[string]cty.Value{}
	}
	inputEval, dependsOn := evalResourceInputs(hclCtx, resSchema.InputProperties,
		func(propKey resource.PropertyKey, val cty.Value) {
			if tdataEvaluated != nil {
				tdataEvaluated[string(propKey)] = val
			}
		})
	resourceInputs, ephemeralPaths, diags := transform.EvalResourceWithSchema(
		res.Config, resSchema, resourceMapping, inputEval)
	if diags.HasErrors() {
		return diags
	}
	if strings.HasPrefix(res.Type, "pulumi_providers_") && res.PluginDownloadURL != nil {
		val, valDiags := res.PluginDownloadURL.Value(hclCtx)
		if !valDiags.HasErrors() && val.Type() == cty.String {
			resourceInputs = resourceInputs.Set("pluginDownloadURL", property.New(val.AsString()))
		}
	}

	timeouts, timeoutsRole, err := evalTimeouts(res.Timeouts, resourceMapping, hclCtx)
	if err != nil {
		return err
	}
	if timeoutsRole == timeoutsInput && !timeouts.IsNull() {
		pv, err := transform.CtyToPropertyValue(timeouts)
		if err != nil {
			return fmt.Errorf("converting timeouts: %w", err)
		}
		prop := resourceMapping.Lookup("timeouts").PulumiName
		resourceInputs = resourceInputs.Set(prop, pv)
		for _, urn := range eval.CollectDepURNs(timeouts) {
			idx, found := slices.BinarySearch(dependsOn[prop], urn)
			if !found {
				dependsOn[prop] = slices.Insert(dependsOn[prop], idx, urn)
			}
		}
	}

	opts, err := e.buildResourceOptions(ctx, node, res, instance, evalCtx, parentURN, modInst, resourceMapping, resSchema.InputProperties, resSchema.Properties, resourceInputs, timeouts)
	if err != nil {
		return err
	}
	// The options are built from the unboxed inputs: terraform_data's
	// {type, value} boxing would hide the value shapes that
	// ignoreChangesApplies inspects.
	resourceInputs = wrapTerraformDataInputs(res.Type, resourceInputs, tdataEvaluated)
	opts.Custom = !resSchema.IsComponent
	opts.Remote = resSchema.IsComponent
	opts.PropertyDependencies = dependsOn

	// An ephemeral value is free to differ on every run, so a property it
	// flows into should not display a diff. The path arrives in the assembled
	// inputs' namespace (snake-cased Pulumi names, MaxItemsOne blocks already
	// flattened), which translateAttrPathTraversal maps to engine form.
	for _, p := range ephemeralPaths {
		glob, err := translateAttrPathTraversal(ctyPathTraversal(p), resourceMapping, resSchema.InputProperties)
		if err != nil {
			return fmt.Errorf("translating ephemeral property path on %q: %w", res.Type+"."+res.Name, err)
		}
		if !slices.Contains(opts.HideDiffs, glob) {
			opts.HideDiffs = append(opts.HideDiffs, glob)
		}
	}
	// The per-source collection may repeat URNs; widen dedups below.
	for _, deps := range dependsOn {
		opts.DependsOn = append(opts.DependsOn, deps...)
	}
	// count/for_each, lifecycle precondition/postcondition, and
	// provisioner/connection references are not tied to any body property, so
	// they stay out of PropertyDependencies but still gate ordering through
	// DependsOn.
	checkDeps := checkRuleDeps(res.Preconditions, hclCtx)
	checkDeps = append(checkDeps, checkRuleDeps(res.Postconditions, hclCtx)...)
	provDeps := provisionerDeps(res, hclCtx)
	opts.DependsOn = append(opts.DependsOn, slices.Concat(metaArgDeps, checkDeps, provDeps)...)
	// Widen every collected instance URN to its resource's registered
	// instance set: destroy ordering is resource-wide, so a reference to one
	// instance makes every sibling wait for this resource. Sources that
	// contribute no dependency (a destroy-time provisioner's self-reference,
	// say) stay excluded — widening only amplifies what was collected.
	opts.DependsOn = e.cellURNs.widen(opts.DependsOn)

	if opts.Version == "" {
		opts.Version = e.pinnedVersion(packageNameFromResourceType(res.Type))
	}

	if opts.PluginDownloadURL == "" && resSchema.PackageReference != nil {
		opts.PluginDownloadURL = resSchema.PackageReference.PluginDownloadURL()
	}

	opts.PackageRef = e.packageRefForResource(res.Type, resSchema)

	resourceName, err := e.resourceInstanceName(res, instance, hclCtx, modInst)
	if err != nil {
		return err
	}

	if len(res.Preconditions) > 0 {
		if err := e.bindPreconditionHooks(ctx, res, instance, evalCtx, opts, resourceName); err != nil {
			return err
		}
	}

	if len(res.Postconditions) > 0 {
		if err := e.bindPostconditionHooks(ctx, res, resSchema, resourceMapping, instance, evalCtx, opts, resourceName); err != nil {
			return err
		}
	}

	if len(res.Provisioners) > 0 || opts.PreventDestroy != preventDestroyAllow {
		if err := e.bindGlobalHooks(ctx, res, resSchema, resourceMapping, instance, evalCtx, opts, resourceName); err != nil {
			return err
		}
	}

	urn, id, outputs, unknown, err := e.registerResource(ctx, res.Type, resSchema.Token, resourceName, resourceInputs, opts)
	if err != nil {
		e.failedNodes.Set(instance.Key, fmt.Errorf("registering resource: %w", err))
		return nil
	}

	e.cellURNs.add(cellKey(node, modInst), string(urn))

	outputs = outputs.Delete("id", "urn")

	outputs, err = e.resolveResourceRefsInOutputs(ctx, outputs, resSchema)
	if err != nil {
		return fmt.Errorf("resolving resource references in outputs: %w", err)
	}

	outputObj, err := transform.ResourceOutputToCty(outputs, resSchema, resourceMapping, e.dryRun || unknown)
	if err != nil {
		return fmt.Errorf("converting resource outputs to HCL types: %w", err)
	}
	if err := unwrapTerraformDataOutputs(res.Type, outputObj, outputs); err != nil {
		return fmt.Errorf("converting resource outputs to HCL types: %w", err)
	}
	if (e.dryRun || unknown) && id == "" {
		outputObj["id"] = cty.UnknownVal(cty.String)
	} else {
		outputObj["id"] = cty.StringVal(id)
	}
	outputObj["urn"] = cty.StringVal(string(urn)).Mark(eval.SyntheticMark)
	if timeoutsRole == timeoutsAttribute {
		outputObj["timeouts"] = timeouts
	} else if timeoutsRole == timeoutsInput && !resourceMapping.Lookup("timeouts").TFBlock {
		// The SDK copies the configured timeouts into state on every plan and
		// apply, so state that lacks them (an imported resource: reads drop
		// timeouts) is repopulated from config. A concrete provider value wins.
		if cur, ok := outputObj["timeouts"]; ok {
			if unmarked, _ := cur.Unmark(); unmarked.IsNull() || !unmarked.IsKnown() {
				outputObj["timeouts"] = timeouts
			}
		}
	}

	markedOutputs := cty.ObjectVal(outputObj).Mark(eval.DepMark(urn))

	e.resourceOutputs.Set(instance.Key, markedOutputs)

	if !markedOutputs.IsWhollyKnown() {
		e.pendingURNs.Set(string(urn), struct{}{})
		for inst := modInst; inst != nil; inst = inst.Parent {
			e.pendingModuleCalls.Set(moduleCallCell(inst), struct{}{})
		}
	}

	var iOpts inheritableOpts
	if opts.Protect {
		iOpts.Protect = new(true)
	}
	iOpts.RetainOnDelete = opts.RetainOnDelete
	e.resourceInheritableOpts.Set(instance.Key, iOpts)

	baseKey := node.Key.ID
	if instance.Index != nil {
		evalCtx.SetCountResource(baseKey, *instance.Index, urn, markedOutputs)
	} else if instance.EachKey != nil {
		evalCtx.SetEachResource(baseKey, instance.EachKey.AsString(), urn, markedOutputs)
	} else {
		evalCtx.SetResource(baseKey, urn, markedOutputs)
	}

	return nil
}

// allTimeoutOps is the set of operations a `timeouts` block can configure.
var allTimeoutOps = []string{"create", "read", "update", "delete", "default"}

// timeoutsRole says how a resource's `timeouts` block is realized.
type timeoutsRole int

const (
	// timeoutsNone: the block is not part of the resource value.
	timeoutsNone timeoutsRole = iota
	// timeoutsAttribute: the schema strips the block, so the engine exposes
	// its value as a synthesized attribute of the resource.
	timeoutsAttribute
	// timeoutsInput: the schema declares a `timeouts` object of its own (the
	// wire schema keeps the SDK-injected block; Plugin Framework providers
	// define one), so the block is sent as an ordinary input property and
	// reads back through the resource's outputs.
	timeoutsInput
)

// timeoutsDeclaredOps returns the operations the resource's schema accepts a
// timeout for and how the block is realized. nil ops means a `timeouts` block
// is not accepted at all.
func timeoutsDeclaredOps(mapping *bridge.BodyMapping) ([]string, timeoutsRole) {
	if mapping == nil {
		// No schema knowledge: accept everything, expose nothing.
		return allTimeoutOps, timeoutsNone
	}
	if f := mapping.Lookup("timeouts"); f != nil {
		if f.Nested == nil {
			return nil, timeoutsNone
		}
		return slices.Sorted(maps.Keys(f.Nested.Fields)), timeoutsInput
	}
	if mapping.Timeouts != nil {
		return mapping.Timeouts, timeoutsAttribute
	}
	return nil, timeoutsNone
}

// evalTimeouts evaluates a `timeouts` block against the operations the
// resource's schema declares a timeout for, returning the value of the
// block: null when it is absent, and one string attribute per declared
// operation otherwise. A block on a resource that accepts none, or an
// argument for an operation it does not declare, is an error.
func evalTimeouts(t *ast.Timeouts, mapping *bridge.BodyMapping, hclCtx *hcl.EvalContext) (cty.Value, timeoutsRole, error) {
	ops, role := timeoutsDeclaredOps(mapping)
	if ops == nil {
		if t == nil {
			return cty.NilVal, timeoutsNone, nil
		}
		return cty.NilVal, timeoutsNone, fmt.Errorf("%s: Unsupported block type; Blocks of type \"timeouts\" are not expected here",
			t.DeclRange)
	}
	attrTypes := make(map[string]cty.Type, len(ops))
	for _, op := range ops {
		attrTypes[op] = cty.String
	}
	ty := cty.Object(attrTypes)
	if t == nil {
		return cty.NullVal(ty), role, nil
	}
	attrs := make(map[string]cty.Value, len(ops))
	for _, op := range ops {
		attrs[op] = cty.NullVal(cty.String)
	}
	for name, expr := range map[string]hcl.Expression{
		"create": t.Create, "read": t.Read, "update": t.Update, "delete": t.Delete, "default": t.Default,
	} {
		if expr == nil {
			continue
		}
		if !ty.HasAttribute(name) {
			return cty.NilVal, role, fmt.Errorf("%s: Unsupported argument; An argument named %q is not expected here",
				expr.Range(), name)
		}
		val, diags := expr.Value(hclCtx)
		if diags.HasErrors() {
			return cty.NilVal, role, fmt.Errorf("evaluating timeouts.%s: %s", name, diags.Error())
		}
		val, err := ctyconvert.Convert(val, cty.String)
		if err != nil {
			return cty.NilVal, role, fmt.Errorf("%s: Incorrect attribute value type; Inappropriate value for attribute %q: %s",
				expr.Range(), name, err)
		}
		attrs[name] = val
	}
	return cty.ObjectVal(attrs), role, nil
}

// customTimeoutsFromValue derives the Pulumi custom timeouts from an evaluated
// `timeouts` block; an operation without a timeout of its own takes `default`.
// Unknown timeouts are left unset. Returns nil when none is configured.
func customTimeoutsFromValue(timeouts cty.Value) (*CustomTimeouts, error) {
	if timeouts.IsNull() {
		return nil, nil
	}
	attr := func(name string) cty.Value {
		if !timeouts.Type().HasAttribute(name) {
			return cty.NullVal(cty.String)
		}
		v, _ := timeouts.GetAttr(name).Unmark()
		return v
	}
	dflt := attr("default")
	ct := &CustomTimeouts{}
	configured := false
	for _, op := range []struct {
		name string
		dst  *float64
	}{
		{"create", &ct.Create}, {"read", &ct.Read}, {"update", &ct.Update}, {"delete", &ct.Delete},
	} {
		v := attr(op.name)
		if v.IsNull() {
			v = dflt
		}
		if v.IsNull() || !v.IsKnown() {
			continue
		}
		d, err := time.ParseDuration(v.AsString())
		if err != nil {
			return nil, fmt.Errorf("parsing %q timeout: %w", op.name, err)
		}
		*op.dst = d.Seconds()
		configured = true
	}
	if !configured {
		return nil, nil
	}
	return ct, nil
}

// buildResourceOptions builds resource options using the provided eval context and parent URN.
func (e *Engine) buildResourceOptions(
	ctx context.Context, node *graph.Node, res *ast.Resource, instance *graph.ExpandedResource,
	evalCtx *eval.Context, parentURN urn.URN,
	modInst *moduleInstance, resourceMapping *bridge.BodyMapping,
	inputProps, outputProps []*schema.Property, inputs property.Map, timeouts cty.Value,
) (*ResourceOptions, error) {
	modInfo := node.ModuleInfo
	opts := &ResourceOptions{}
	opts.Parent = parentURN

	// depends_on records every registered instance of the target block,
	// keyed or not — dependency metadata is resource-wide (see urnRegistry).
	// The registry lookup also covers expanded targets, whose instance
	// outputs are not addressable by the block key.
	miPath := modulepath.Root()
	if modInst != nil {
		miPath = modInst.Path
	}
	for _, dep := range res.DependsOn {
		block, ok := graph.TraversalKey(node.Key.Module, dep)
		if !ok {
			continue
		}
		opts.DependsOn = append(opts.DependsOn, e.cellURNs.get(expansionCell{block: block, mi: miPath})...)
	}

	hclCtx := evalCtx.HCLContext()

	// Handle lifecycle options
	if res.Lifecycle != nil {
		guard, err := evalPreventDestroy(res, hclCtx)
		if err != nil {
			return nil, err
		}
		opts.PreventDestroy = guard
		// ignore_changes maps to ignoreChanges. The traversal names are TF
		// (snake_case) attribute names; the Pulumi engine matches ignoreChanges
		// paths against Pulumi (camelCase) property names, so they must be
		// translated through the bridge mapping first.
		for _, ic := range res.Lifecycle.IgnoreChanges {
			icStr, err := translateAttrPathTraversal(ic, resourceMapping, inputProps)
			if err != nil {
				return nil, fmt.Errorf("invalid property path: %w", err)
			}
			if !ignoreChangesApplies(ic, resourceMapping, inputProps, inputs) {
				continue
			}
			opts.IgnoreChanges = append(opts.IgnoreChanges, icStr)
		}
		if res.Lifecycle.IgnoreAllChanges {
			opts.IgnoreChanges = []property.Glob{property.GlobFromSegments(property.Splat)}
		}
	}
	// create_before_destroy controls replacement order, with TF semantics:
	//   - true: create new, then delete old
	//   - false or absent: delete old, then create new (TF default)
	//
	// Mapped to Pulumi's inverse `deleteBeforeReplace`:
	//   - cbd=true  -> DeleteBeforeReplace=false
	//   - cbd=false -> DeleteBeforeReplace=true
	//   - cbd unset -> DeleteBeforeReplace=true (TF default, opposite of Pulumi's)
	//
	// create_before_destroy also propagates to a resource's dependencies, so a
	// dependency of a create-before-destroy resource is forced to the same
	// ordering even when it does not declare it (forcedCBD, computed from the
	// graph). This keeps every create in a replacement chain ahead of the deletes.
	cbd := res.Lifecycle != nil && res.Lifecycle.CreateBeforeDestroy != nil && *res.Lifecycle.CreateBeforeDestroy
	cbd = cbd || e.forcedCBD[node.Key]
	opts.DeleteBeforeReplaceDef = true
	opts.DeleteBeforeReplace = !cbd

	if key, ok := graph.TraversalKey(node.Key.Module, res.ResourceParent); ok {
		if outputs, ok := e.nodeOutputs(key); ok {
			if parentURN := ctyAsString(outputs.GetAttr("urn")); parentURN != "" {
				opts.Parent = urn.URN(parentURN) // TODO: Don't look at attrs for this
			}
		}
	}

	if res.Provider != nil {
		ref, err := e.resolveExplicitProvider(res.Provider, evalCtx, modInfo)
		if err != nil {
			return nil, fmt.Errorf("resolving provider for %s.%s: %w", res.Type, res.Name, err)
		}
		if ref != "" {
			opts.Provider = ref
		}
	} else if modInfo != nil {
		// Implicit default in a module: try a pass-through entry, then
		// the in-module `provider "<pkg>" {}` block, then an inherited
		// ancestor default. If none exist, fall through to Pulumi's
		// engine default.
		pkg := packageNameFromResourceType(res.Type)
		if ref := e.resolvePassThroughProvider(modInfo, pkg); ref != "" {
			opts.Provider = ref
		} else if outputs, ok := e.nodeOutputs(graph.NodeKey{Module: node.Key.Module, ID: pkg}); ok {
			if ref, err := providerRefFromCty(outputs); err == nil {
				opts.Provider = ref
			}
		} else if ref := e.inheritedDefaultProvider(modInfo, pkg); ref != "" {
			opts.Provider = ref
		}
	} else {
		if ref, ok := e.defaultProviders.Get(packageNameFromResourceType(res.Type)); ok {
			opts.Provider = ref
		}
	}

	for _, traversal := range res.Providers {
		key, ok := graph.TraversalKey(node.Key.Module, traversal)
		if !ok {
			continue
		}
		if providerOutputs, ok := e.nodeOutputs(key); ok {
			urn := ctyAsString(providerOutputs.GetAttr("urn"))
			id := ctyAsString(providerOutputs.GetAttr("id"))
			if urn != "" && id != "" {
				pkgName := packageNameFromResourceType(strings.SplitN(key.ID, ".", 2)[0])
				if opts.Providers == nil {
					opts.Providers = make(map[string]string)
				}
				opts.Providers[pkgName] = urn + "::" + id
			}
		}
	}

	customTimeouts, err := customTimeoutsFromValue(timeouts)
	if err != nil {
		return nil, err
	}
	opts.CustomTimeouts = customTimeouts

	// Handle moved blocks - resolve aliases from moved blocks that target this resource
	movedAliases := e.resolveMovedAliases(ctx, res, instance.Index, instance.EachKeyString(), modInst)
	opts.Aliases = append(opts.Aliases, movedAliases...)

	// Handle aliases attribute
	if res.Aliases != nil {
		aliases, err := e.evaluateAliases(res.Aliases)
		if err != nil {
			return nil, err
		}
		opts.Aliases = append(opts.Aliases, aliases...)
	}

	// Handle import blocks - resolve import ID from import blocks that target this resource
	importId, err := e.resolveImportId(res, instance.Index, instance.EachKeyString(), modInst)
	if err != nil {
		return nil, err
	}
	opts.ImportId = importId

	for _, t := range res.AdditionalSecretOutputs {
		name, err := translateSecretOutputName(t, resourceMapping, outputProps)
		if err != nil {
			return nil, err
		}
		opts.AdditionalSecretOutputs = append(opts.AdditionalSecretOutputs, name)
	}

	// Properties the schema declares as secret are marked as secret outputs, so
	// the engine stores and surfaces them as secrets just as the generated SDKs do.
	for _, p := range outputProps {
		if p.Secret && !slices.Contains(opts.AdditionalSecretOutputs, p.Name) {
			opts.AdditionalSecretOutputs = append(opts.AdditionalSecretOutputs, p.Name)
		}
	}

	if res.Protect != nil {
		val, diags := res.Protect.Value(hclCtx)
		val, _ = val.Unmark()
		if !diags.HasErrors() && val.Type() == cty.Bool && !val.IsNull() && val.IsKnown() {
			opts.Protect = val.True()
		}
	}

	if res.RetainOnDelete != nil {
		val, diags := res.RetainOnDelete.Value(hclCtx)
		val, _ = val.Unmark()
		if !diags.HasErrors() && val.Type() == cty.Bool && !val.IsNull() && val.IsKnown() {
			b := val.True()
			opts.RetainOnDelete = &b
		}
	}

	if key, ok := graph.TraversalKey(node.Key.Module, res.DeletedWith); ok {
		if outputs, ok := e.nodeOutputs(key); ok {
			if urn := ctyAsString(outputs.GetAttr("urn")); urn != "" {
				opts.DeletedWith = urn
			}
		}
	}

	for _, ref := range res.ReplaceWith {
		key, ok := graph.TraversalKey(node.Key.Module, ref)
		if !ok {
			continue
		}
		if outputs, ok := e.nodeOutputs(key); ok {
			if urn := ctyAsString(outputs.GetAttr("urn")); urn != "" {
				opts.ReplaceWith = append(opts.ReplaceWith, urn)
			}
		}
	}

	// hide_diffs and replace_on_changes name properties of this resource by
	// their attribute path. Like ignore_changes, the path may be written in TF
	// (snake_case) convention and must be translated to the Pulumi property
	// name the engine expects; a path already in Pulumi form passes through.
	for _, p := range res.HideDiff {
		glob, err := translateAttrPathTraversal(p, resourceMapping, inputProps)
		if err != nil {
			return nil, fmt.Errorf("invalid hide_diffs property path: %w", err)
		}
		opts.HideDiffs = append(opts.HideDiffs, glob)
	}
	for _, p := range res.ReplaceOnChanges {
		glob, err := translateAttrPathTraversal(p, resourceMapping, inputProps)
		if err != nil {
			return nil, fmt.Errorf("invalid replace_on_changes property path: %w", err)
		}
		opts.ReplaceOnChanges = append(opts.ReplaceOnChanges, glob)
	}

	// `lifecycle { replace_triggered_by = [a, b, ...] }` evaluates each
	// expression and feeds the result to RegisterResource as the
	// ReplacementTrigger property value: any element flipping triggers a
	// replacement. A single-element list is unwrapped to a scalar so the
	// trigger value round-trips with Pulumi's scalar `replacementTrigger`.
	// A reference in a trigger also establishes a dependency, as in TF.
	//
	// An element whose value is a whole resource is action-based, not
	// value-based: it must fire when the referenced resource is replaced even
	// if every attribute value is unchanged. The value trigger covers in-place
	// updates (an update implies the object value changed); the replacement
	// action is covered by also listing the referenced instances' URNs in
	// ReplaceWith.
	if res.Lifecycle != nil && len(res.Lifecycle.ReplaceTriggeredBy) > 0 {
		vals := make([]cty.Value, 0, len(res.Lifecycle.ReplaceTriggeredBy))
		for _, expr := range res.Lifecycle.ReplaceTriggeredBy {
			val, diags := expr.Value(hclCtx)
			if diags.HasErrors() {
				return nil, fmt.Errorf("evaluating replace_triggered_by on %q: %s",
					res.Type+"."+res.Name, diags.Error())
			}
			for _, dep := range eval.CollectDepURNs(val) {
				if !slices.Contains(opts.DependsOn, dep) {
					opts.DependsOn = append(opts.DependsOn, dep)
				}
			}
			vals = append(vals, val)
			opts.ReplaceWith = append(opts.ReplaceWith, resourceURNsFromValue(val)...)
		}
		var triggerVal cty.Value
		if len(vals) == 1 {
			triggerVal = vals[0]
		} else {
			triggerVal = cty.TupleVal(vals)
		}
		pv, err := transform.CtyToPropertyValue(triggerVal)
		if err != nil {
			return nil, fmt.Errorf("converting replace_triggered_by on %q: %w",
				res.Type+"."+res.Name, err)
		}
		opts.ReplacementTrigger = pv
	}

	// Handle inline import_id attribute
	if res.ImportID != "" {
		opts.ImportId = res.ImportID
	}

	if res.EnvVarMappings != nil {
		val, diags := res.EnvVarMappings.Value(hclCtx)
		val, _ = val.UnmarkDeep()
		if !diags.HasErrors() && (val.Type().IsObjectType() || val.Type().IsMapType()) &&
			!val.IsNull() && val.IsKnown() {
			mappings := make(map[string]string)
			for k, v := range val.AsValueMap() {
				if v.Type() == cty.String && !v.IsNull() && v.IsKnown() {
					mappings[k] = v.AsString()
				}
			}
			if len(mappings) > 0 {
				opts.EnvVarMappings = mappings
			}
		}
	}

	if res.Version != nil {
		val, diags := res.Version.Value(hclCtx)
		val, _ = val.Unmark()
		if !diags.HasErrors() && val.Type() == cty.String && !val.IsNull() && val.IsKnown() {
			opts.Version = val.AsString()
		}
	}

	if res.PluginDownloadURL != nil && !strings.HasPrefix(res.Type, "pulumi_providers_") {
		val, diags := res.PluginDownloadURL.Value(hclCtx)
		val, _ = val.Unmark()
		if !diags.HasErrors() && val.Type() == cty.String && !val.IsNull() && val.IsKnown() {
			opts.PluginDownloadURL = val.AsString()
		}
	}

	if key, ok := graph.TraversalKey(node.Key.Module, res.ResourceParent); ok {
		if parentOpts, ok := e.resourceInheritableOpts.Get(graph.InstanceKey{Node: key}); ok {
			if res.Protect == nil && parentOpts.Protect != nil && *parentOpts.Protect {
				opts.Protect = true
			}
			if res.RetainOnDelete == nil && parentOpts.RetainOnDelete != nil {
				opts.RetainOnDelete = parentOpts.RetainOnDelete
			}
		}
	}

	return opts, nil
}

// preventDestroyGuard is the delete-time decision for a resource's
// lifecycle.prevent_destroy; it is consulted only when a destroy is planned, so
// create and update are always inert.
type preventDestroyGuard int

const (
	// preventDestroyAllow lets the delete proceed: unset, false, or unknown.
	preventDestroyAllow preventDestroyGuard = iota
	// preventDestroyRefuse refuses the delete: prevent_destroy is true.
	preventDestroyRefuse
	// preventDestroyNull errors the delete: prevent_destroy is a known null.
	preventDestroyNull
)

// evalPreventDestroy evaluates res's lifecycle.prevent_destroy in hclCtx into a
// delete-time guard. An unknown value allows the delete; a known null becomes
// preventDestroyNull, surfaced by the delete hook rather than failing the
// create. References to per-instance symbols are rejected statically: the guard
// must be evaluable for instances that have already been removed from the
// configuration, whose per-instance data is gone.
func evalPreventDestroy(res *ast.Resource, hclCtx *hcl.EvalContext) (preventDestroyGuard, error) {
	if res.Lifecycle == nil || res.Lifecycle.PreventDestroy == nil {
		return preventDestroyAllow, nil
	}
	for _, trav := range res.Lifecycle.PreventDestroy.Variables() {
		if root := trav.RootName(); root == "count" || root == "each" {
			return preventDestroyAllow, fmt.Errorf(
				"invalid reference in prevent_destroy on %q: the argument cannot refer to %s.*, "+
					"because it must be evaluable for instances that have already been removed from the configuration",
				res.Type+"."+res.Name, root)
		}
	}
	val, diags := res.Lifecycle.PreventDestroy.Value(hclCtx)
	if diags.HasErrors() {
		return preventDestroyAllow, fmt.Errorf("evaluating prevent_destroy on %q: %s",
			res.Type+"."+res.Name, diags.Error())
	}
	val, _ = val.Unmark()
	val, err := ctyconvert.Convert(val, cty.Bool)
	if err != nil {
		return preventDestroyAllow, fmt.Errorf("invalid prevent_destroy value on %q: %w",
			res.Type+"."+res.Name, err)
	}
	switch {
	case !val.IsKnown():
		return preventDestroyAllow, nil
	case val.IsNull():
		return preventDestroyNull, nil
	case val.True():
		return preventDestroyRefuse, nil
	default:
		return preventDestroyAllow, nil
	}
}

// preventDestroyRefusal is returned from the destroy dispatcher for a guarded
// instance; tfcompat tests compare the wording across runtimes.
func preventDestroyRefusal(addr string) error {
	return fmt.Errorf(
		"resource instance %s has prevent_destroy set, but the plan calls for it to be destroyed. "+
			"To proceed, disable prevent_destroy for this resource", addr)
}

// preventDestroyNullRefusal is returned when a to-be-destroyed instance's
// prevent_destroy is a known null.
func preventDestroyNullRefusal(addr string) error {
	return fmt.Errorf(
		"resource instance %s has prevent_destroy set to null; when making a dynamic "+
			"decision to allow destroy, use false instead", addr)
}

// resolveMovedAliases finds the `moved` blocks that rename the resource instance
// being registered and returns the aliases recording its prior addresses, so a
// rename is treated as a move rather than a replacement.
//
// A moved block's addresses are relative to the module it is written in, so the
// resolver walks the resource's own module and every ancestor module. It handles
// resource renames within a module (including `count`/`for_each` instance-key
// changes), changes of the resource's type, moves of a resource between the
// root and a module or between two modules, and resources carried along when an
// enclosing module call is renamed or re-keyed (the matching component alias is
// attached in processModuleInit).
//
// Moved blocks chain: a prior address may itself be the `to` of another moved
// block (a -> b, then b -> c). Every address along the chain is aliased, so the
// engine matches whichever of them the state holds.
func (e *Engine) resolveMovedAliases(
	ctx context.Context, res *ast.Resource, index *int, eachKey *string, modInst *moduleInstance,
) []Alias {
	var aliases []Alias

	// resPath is the resource's module instance path (with any count/for_each
	// keys), which is what a resolved moved address is matched against.
	resPath := modulepath.Root()
	if modInst != nil {
		resPath = modInst.Path
	}

	// A resource address at some point along the chain of moves.
	type address struct {
		path    modulepath.Path
		typ     string
		name    string
		index   *int
		eachKey *string
		// token is the prior type's Pulumi token, set only when the move also
		// changes the resource's type.
		token string
	}
	type addrKey struct {
		typ    string
		prefix string
	}
	mkAddrKey := func(a address) addrKey {
		return addrKey{a.typ, modulepath.NewAddress(a.path, instanceStep(a.name, a.index, a.eachKey)).LogicalName()}
	}

	work := []address{{path: resPath, typ: res.Type, name: res.Name, index: index, eachKey: eachKey}}
	seen := map[addrKey]bool{mkAddrKey(work[0]): true}

	// sameModule holds the addresses the resource had within its own module,
	// current one first: the names to reconstruct under a prior module path when
	// the enclosing module call is renamed in the same apply.
	sameModule := []address{work[0]}

	for len(work) > 0 {
		cur := work[0]
		work = work[1:]
		for _, scope := range ancestorPaths(cur.path) {
			for _, moved := range e.graph.MovedBlocks(scope) {
				to, ok := ast.ParseTargetAddr(moved.To)
				if !ok || to.Type == "" { // skip whole-module-call moves
					continue
				}
				toPath := scope.Join(to.Module)
				if toPath != cur.path || to.Type != cur.typ || to.Name != cur.name {
					continue
				}
				from, ok := ast.ParseTargetAddr(moved.From)
				if !ok || from.Type == "" {
					continue
				}
				fromPath := scope.Join(from.Module)

				// Determine the prior instance key. A keyed endpoint on either side
				// makes this an instance move taking the prior key from `from` (an
				// unkeyed endpoint paired with a keyed one names the no-key
				// instance); with both endpoints unkeyed it is a whole-resource
				// rename that maps every instance to the same key.
				var priorIdx *int
				var priorEach *string
				switch {
				case to.Keyed():
					if !instanceKeysEqual(cur.index, cur.eachKey, to.KeyIndex, to.KeyEach) {
						continue
					}
					priorIdx, priorEach = from.KeyIndex, from.KeyEach
				case from.Keyed():
					if cur.index != nil || cur.eachKey != nil {
						continue
					}
					priorIdx, priorEach = from.KeyIndex, from.KeyEach
				default:
					priorIdx, priorEach = cur.index, cur.eachKey
				}

				prior := address{path: fromPath, typ: from.Type, name: from.Name, index: priorIdx, eachKey: priorEach}
				if seen[mkAddrKey(prior)] {
					continue
				}

				// A `moved` may also change the resource's type; the alias then
				// carries the prior type's token so the engine matches the old URN.
				if from.Type != res.Type {
					priorRes, err := e.resolver.ResolveResource(ctx, from.Type)
					if err != nil {
						logging.V(5).Infof("moved: cannot resolve prior type %q: %v", from.Type, err)
						continue
					}
					prior.token = priorRes.Token
				}

				seen[mkAddrKey(prior)] = true
				work = append(work, prior)
				if fromPath == resPath {
					sameModule = append(sameModule, prior)
				}

				// The prior name is the resource's own name under its prior module
				// path; the prior parent is described relative to where it is now.
				name := modulepath.NewAddress(fromPath, instanceStep(from.Name, priorIdx, priorEach)).LogicalName()
				parentURN, noParent, ok := e.priorParentSpec(fromPath, resPath, modInst)
				if !ok {
					continue
				}

				aliases = append(aliases, Alias{Spec: &AliasSpec{
					Name:      name,
					Type:      prior.token,
					ParentURN: parentURN,
					NoParent:  noParent,
				}})

				// The module the resource moved out of may itself be renamed in
				// the same apply; the object then lived under the pre-rename
				// module path, parented to the same component under its
				// pre-rename name. A prior address in the resource's own module
				// is handled below instead, where the parent is the resource's
				// own component and Pulumi supplies it from the component alias.
				if fromPath != resPath {
					for _, oldPath := range e.oldModulePaths(fromPath) {
						aliases = append(aliases, Alias{Spec: &AliasSpec{
							Name:      modulepath.NewAddress(oldPath, instanceStep(from.Name, priorIdx, priorEach)).LogicalName(),
							Type:      prior.token,
							ParentURN: string(urn.URN(parentURN).Rename(oldPath.LogicalName())),
						}})
					}
				}
			}
		}
	}

	// A `moved` block that renames an enclosing module call moves this resource
	// with it. The resource is aliased to each name it held within the module —
	// its current one and any it is being renamed from in the same apply — under
	// each prior module path; Pulumi combines that with the renamed component's
	// own alias to recover the old URN.
	for _, oldPath := range e.oldModulePaths(resPath) {
		for _, a := range sameModule {
			name := modulepath.NewAddress(oldPath, instanceStep(a.name, a.index, a.eachKey)).LogicalName()
			aliases = append(aliases, Alias{Spec: &AliasSpec{Name: name, Type: a.token}})
		}
	}

	return aliases
}

// movedFromModuleInits returns the init nodes of the still-configured modules
// that `moved` blocks name as prior homes of the block's resource, following
// chained moves. The resource's cell is ordered after those inits so that
// resolveMovedAliases finds the prior module's component already registered
// when it resolves the prior parent URN (priorComponentURN); without that
// ordering the alias silently degrades whenever the resource wins the race.
//
// The walk is the static shadow of resolveMovedAliases's chain walk: instance
// keys are ignored, so it over-approximates to every instance of the named
// modules, which only adds ordering.
func (e *Engine) movedFromModuleInits(node *graph.Node) []pdag.Node {
	res := node.Resource
	if res == nil {
		return nil
	}
	resPath := modulepath.Root()
	if node.ModuleInfo != nil {
		resPath = node.ModuleInfo.Path
	}
	type addr struct {
		path      modulepath.Path
		typ, name string
	}
	start := addr{resPath, res.Type, res.Name}
	work := []addr{start}
	seen := map[addr]bool{start: true}
	needInit := map[modulepath.Path]bool{}
	for len(work) > 0 {
		cur := work[0]
		work = work[1:]
		for _, scope := range ancestorPaths(cur.path) {
			for _, moved := range e.graph.MovedBlocks(scope) {
				to, ok := ast.ParseTargetAddr(moved.To)
				if !ok || to.Type == "" { // skip whole-module-call moves
					continue
				}
				if scope.Join(to.Module).Config() != cur.path || to.Type != cur.typ || to.Name != cur.name {
					continue
				}
				from, ok := ast.ParseTargetAddr(moved.From)
				if !ok || from.Type == "" {
					continue
				}
				prior := addr{scope.Join(from.Module).Config(), from.Type, from.Name}
				if seen[prior] {
					continue
				}
				seen[prior] = true
				work = append(work, prior)
				if prior.path != resPath && !prior.path.IsRoot() {
					needInit[prior.path] = true
				}
			}
		}
	}
	var inits []pdag.Node
	for path := range needInit {
		if init, ok := e.graph.KeyNode(graph.NodeKey{Module: path, ID: "__init__"}); ok {
			inits = append(inits, init)
		}
	}
	return inits
}

// oldModulePaths applies the whole-module-call `moved` blocks that rename a
// module enclosing (or equal to) path, following chained renames, and returns
// every prior module path the object lived at, nearest rename first. It
// returns nil when none applies.
func (e *Engine) oldModulePaths(path modulepath.Path) []modulepath.Path {
	var prior []modulepath.Path
	seen := map[modulepath.Path]bool{path: true}
	for {
		next, ok := e.priorModulePath(path)
		if !ok || seen[next] {
			return prior
		}
		seen[next] = true
		prior = append(prior, next)
		path = next
	}
}

// priorModulePath applies the first whole-module-call `moved` block that
// renames a module enclosing (or equal to) path, returning the module path the
// object lived at before that rename. ok is false when none applies.
func (e *Engine) priorModulePath(path modulepath.Path) (_ modulepath.Path, ok bool) {
	for _, scope := range ancestorPaths(path) {
		for _, moved := range e.graph.MovedBlocks(scope) {
			to, ok := ast.ParseTargetAddr(moved.To)
			if !ok || to.Type != "" || to.Module.IsRoot() {
				continue // not a whole-module-call address
			}
			from, ok := ast.ParseTargetAddr(moved.From)
			if !ok || from.Type != "" || from.Module.IsRoot() {
				continue
			}
			toPath := scope.Join(to.Module)
			suffix, ok := stripModulePrefix(path, toPath)
			if !ok {
				continue
			}
			fromPath := scope.Join(from.Module)
			for _, s := range suffix {
				fromPath = fromPath.Append(s)
			}
			return fromPath, true
		}
	}
	return path, false
}

// moduleComponentAliases returns the aliases for a module instance's component
// resource when `moved` blocks rename its call, so the component (and its
// children) are recognized as moved rather than replaced.
func (e *Engine) moduleComponentAliases(instPath modulepath.Path) []Alias {
	var aliases []Alias
	for _, oldPath := range e.oldModulePaths(instPath) {
		aliases = append(aliases, Alias{Spec: &AliasSpec{Name: oldPath.LogicalName()}})
	}
	return aliases
}

// priorParentSpec describes the parent of a resource at its prior moved address,
// relative to where it is registered now: the parent is unchanged within the
// same module, was the stack (NoParent) when the prior address was the root, or
// is a specific module component otherwise. ok is false when that prior
// component cannot be resolved.
func (e *Engine) priorParentSpec(
	fromPath, resPath modulepath.Path, modInst *moduleInstance,
) (parentURN string, noParent, ok bool) {
	switch {
	case fromPath == resPath:
		return "", false, true // parent unchanged; the alias inherits it
	case fromPath.IsRoot():
		return "", true, true // prior parent was the stack
	default:
		u, ok := e.priorComponentURN(fromPath, modInst)
		return string(u), false, ok
	}
}

// priorComponentURN returns the component URN of the module a resource moved out
// of. It uses the module's live component when that module still exists in the
// run; otherwise, when the resource now lives in a sibling module of the same
// source, it derives the URN from that sibling's component (same component type).
func (e *Engine) priorComponentURN(
	fromPath modulepath.Path, modInst *moduleInstance,
) (urn.URN, bool) {
	if insts, ok := e.moduleInstances.Get(fromPath); ok && len(insts) > 0 {
		return insts[0].URN, true
	}
	if modInst != nil {
		// Same component type as the resource's own module, so renaming the
		// resource's component URN to the prior module's name yields it.
		return modInst.URN.Rename(fromPath.LogicalName()), true
	}
	return "", false
}

// pathSteps returns p's steps, root first.
func pathSteps(p modulepath.Path) []modulepath.Step {
	var steps []modulepath.Step
	p.Steps(func(s modulepath.Step) bool {
		steps = append(steps, s)
		return true
	})
	return steps
}

// stripModulePrefix returns the steps of path that follow prefix, and whether
// prefix is a prefix of path.
func stripModulePrefix(path, prefix modulepath.Path) ([]modulepath.Step, bool) {
	ps, pre := pathSteps(path), pathSteps(prefix)
	if len(pre) > len(ps) {
		return nil, false
	}
	for i := range pre {
		if ps[i] != pre[i] {
			return nil, false
		}
	}
	return ps[len(pre):], true
}

// ancestorPaths returns p and all of its ancestors, root first.
func ancestorPaths(p modulepath.Path) []modulepath.Path {
	var chain []modulepath.Path
	for {
		chain = append(chain, p)
		parent, _, ok := p.Parent()
		if !ok {
			break
		}
		p = parent
	}
	slices.Reverse(chain)
	return chain
}

// instanceKeysEqual reports whether two resource instance keys name the same
// instance. Keys of different kinds (count vs for_each) never match, and two
// unkeyed (single-instance) addresses do not either.
func instanceKeysEqual(aIdx *int, aEach *string, bIdx *int, bEach *string) bool {
	switch {
	case aIdx != nil && bIdx != nil:
		return *aIdx == *bIdx
	case aEach != nil && bEach != nil:
		return *aEach == *bEach
	default:
		return false
	}
}

// resolveImportId finds the import block that targets this resource instance
// and returns its import ID. An import address names a single instance: its
// module-call steps must match the resource's module instance path, and on a
// count/for_each resource each instance takes only the ID of the import block
// keyed with its own instance key. An import block with for_each expands into
// one import per element, with each.key/each.value in scope for `to` and `id`.
func (e *Engine) resolveImportId(
	res *ast.Resource, index *int, eachKey *string, modInst *moduleInstance,
) (string, error) {
	resPath := modulepath.Root()
	if modInst != nil {
		resPath = modInst.Path
	}

	for _, imp := range e.config.Imports {
		evs := []*eval.Evaluator{e.evaluator}
		if imp.ForEach != nil {
			var err error
			evs, err = e.expandImportForEach(imp.ForEach)
			if err != nil {
				return "", err
			}
		}
		for _, ev := range evs {
			id, matched, err := e.matchImport(imp, ev, res, index, eachKey, resPath)
			if matched || err != nil {
				return id, err
			}
		}
	}

	return "", nil
}

// expandImportForEach expands an import block's for_each into one evaluator
// per element, each carrying that element's each.key/each.value. Beyond the
// map, object and set a resource's for_each accepts, an import block also
// accepts a tuple, whose each.key is the element's index — so a tuple can key
// a counted resource directly.
func (e *Engine) expandImportForEach(expr hcl.Expression) ([]*eval.Evaluator, error) {
	newEvaluator := func(k, v cty.Value) *eval.Evaluator {
		return eval.NewEvaluator(e.evaluator.Context().WithIteration(nil, &k, &v))
	}

	val, diags := e.evaluator.Evaluate(expr)
	if diags.HasErrors() {
		return nil, diags
	}
	if unmarked, _ := val.Unmark(); !val.HasMark(eval.SensitiveMark) &&
		unmarked.IsKnown() && !unmarked.IsNull() && unmarked.Type().IsTupleType() {
		evs := make([]*eval.Evaluator, 0, unmarked.LengthInt())
		for i, v := range unmarked.AsValueSlice() {
			evs = append(evs, newEvaluator(cty.NumberIntVal(int64(i)), v))
		}
		return evs, nil
	}

	forEach, unknown, _, diags := e.evaluator.EvaluateForEach(expr)
	if diags.HasErrors() {
		return nil, diags
	}
	if unknown {
		return nil, hcl.Diagnostics{{
			Severity: hcl.DiagError,
			Summary:  "Invalid for_each argument",
			Detail:   `The import block "for_each" argument depends on resource attributes that cannot be determined until apply.`,
			Subject:  expr.Range().Ptr(),
		}}
	}
	evs := make([]*eval.Evaluator, 0, len(forEach))
	for k, v := range forEach {
		evs = append(evs, newEvaluator(cty.StringVal(k), v))
	}
	return evs, nil
}

// matchImport reports whether imp targets the given resource instance and, if
// so, returns its import ID. ev carries each.key/each.value when the import
// block has for_each.
func (e *Engine) matchImport(
	imp *ast.Import, ev *eval.Evaluator,
	res *ast.Resource, index *int, eachKey *string, resPath modulepath.Path,
) (string, bool, error) {
	traversal, diags := importToTraversal(ev, imp.To)
	if diags.HasErrors() {
		return "", false, diags
	}
	to, ok := ast.ParseTargetAddr(traversal)
	if !ok || to.Type != res.Type || to.Name != res.Name {
		return "", false, nil
	}
	if to.Module != resPath {
		return "", false, nil
	}
	if to.Keyed() || index != nil || eachKey != nil {
		if !instanceKeysEqual(index, eachKey, to.KeyIndex, to.KeyEach) {
			return "", false, nil
		}
	}
	id, err := e.evaluateImportId(imp, ev)
	return id, true, err
}

// importToTraversal converts an import block's `to` expression into a static
// traversal. Index keys may be arbitrary expressions (e.g. each.key); they are
// evaluated with ev and must produce a known, non-null, non-sensitive string
// or number.
func importToTraversal(ev *eval.Evaluator, expr hcl.Expression) (hcl.Traversal, hcl.Diagnostics) {
	if t, diags := hcl.AbsTraversalForExpr(expr); !diags.HasErrors() {
		return t, nil
	}
	switch e := expr.(type) {
	case *hclsyntax.IndexExpr:
		t, diags := importToTraversal(ev, e.Collection)
		if diags.HasErrors() {
			return nil, diags
		}
		key, keyDiags := importIndexKey(ev, e.Key)
		if keyDiags.HasErrors() {
			return nil, keyDiags
		}
		return append(t, key), nil
	case *hclsyntax.RelativeTraversalExpr:
		t, diags := importToTraversal(ev, e.Source)
		if diags.HasErrors() {
			return nil, diags
		}
		return append(t, e.Traversal...), nil
	default:
		return nil, hcl.Diagnostics{{
			Severity: hcl.DiagError,
			Summary:  "Invalid import address expression",
			Detail:   "Import address must be a reference to a resource's address, and only allows for indexing with dynamic keys.",
			Subject:  expr.Range().Ptr(),
		}}
	}
}

// importIndexKey evaluates an index-key expression of an import address into a
// TraverseIndex step.
func importIndexKey(ev *eval.Evaluator, expr hcl.Expression) (hcl.TraverseIndex, hcl.Diagnostics) {
	idx := hcl.TraverseIndex{SrcRange: expr.Range()}
	errf := func(detail string) hcl.Diagnostics {
		return hcl.Diagnostics{{
			Severity: hcl.DiagError,
			Summary:  "Import block 'to' address contains an invalid key",
			Detail:   detail,
			Subject:  expr.Range().Ptr(),
		}}
	}
	val, diags := ev.EvaluateExpression(expr)
	if diags.HasErrors() {
		return idx, diags
	}
	sensitive := val.HasMark(eval.SensitiveMark)
	val, _ = val.Unmark()
	switch {
	case !val.IsKnown():
		return idx, errf("The index of an import target address must be known at plan time.")
	case val.IsNull():
		return idx, errf("The index of an import target address cannot be null.")
	case val.Type() != cty.String && val.Type() != cty.Number:
		return idx, errf("The index of an import target address must be a string or a number.")
	case sensitive:
		return idx, errf("The index of an import target address cannot be sensitive.")
	}
	idx.Key = val
	return idx, nil
}

// evaluateImportId evaluates an import block's id expression, which must
// produce a known, non-null, non-sensitive string at plan time. ev carries
// each.key/each.value when the import block has for_each.
func (e *Engine) evaluateImportId(imp *ast.Import, ev *eval.Evaluator) (string, error) {
	errf := func(detail string) error {
		var subject *hcl.Range
		if imp.Id != nil {
			subject = imp.Id.Range().Ptr()
		} else {
			subject = imp.DeclRange.Ptr()
		}
		return hcl.Diagnostics{{
			Severity: hcl.DiagError,
			Summary:  "Invalid import id argument",
			Detail:   detail,
			Subject:  subject,
		}}
	}

	if imp.Id == nil {
		return "", errf("The import ID cannot be null.")
	}
	val, diags := ev.EvaluateExpression(imp.Id)
	if diags.HasErrors() {
		return "", diags
	}
	sensitive := val.HasMark(eval.SensitiveMark)
	val, _ = val.Unmark()
	if val.IsNull() {
		return "", errf("The import ID cannot be null.")
	}
	if !val.IsKnown() {
		return "", errf(`The import block "id" argument depends on resource attributes that cannot be determined until apply.`)
	}
	if sensitive {
		return "", errf("The import ID cannot be sensitive.")
	}
	converted, err := ctyconvert.Convert(val, cty.String)
	if err != nil {
		return "", errf(fmt.Sprintf("The import ID value is unsuitable: %s.", err))
	}
	return converted.AsString(), nil
}

// evaluateAliases evaluates the aliases expression and returns a list of Alias values.
// Each alias can be a URN string or an object with spec fields.
func (e *Engine) evaluateAliases(expr hcl.Expression) ([]Alias, error) {
	val, diags := expr.Value(e.evaluator.Context().HCLContext())
	if diags.HasErrors() {
		return nil, diags
	}
	if val.IsNull() {
		return nil, nil
	}
	if !val.Type().IsListType() && !val.Type().IsTupleType() {
		return nil, fmt.Errorf("aliases must be a list")
	}
	var aliases []Alias
	it := val.ElementIterator()
	for it.Next() {
		_, elem := it.Element()
		if elem.Type() == cty.String {
			aliases = append(aliases, Alias{URN: ctyAsString(elem)})
		} else if elem.Type().IsObjectType() {
			spec := &AliasSpec{}
			objType := elem.Type()
			if objType.HasAttribute("name") {
				spec.Name = ctyAsString(elem.GetAttr("name"))
			}
			if objType.HasAttribute("type") {
				spec.Type = ctyAsString(elem.GetAttr("type"))
			}
			if objType.HasAttribute("stack") {
				spec.Stack = ctyAsString(elem.GetAttr("stack"))
			}
			if objType.HasAttribute("project") {
				spec.Project = ctyAsString(elem.GetAttr("project"))
			}
			if objType.HasAttribute("parent_urn") {
				spec.ParentURN = ctyAsString(elem.GetAttr("parent_urn"))
			}
			if objType.HasAttribute("no_parent") {
				if v, _ := elem.GetAttr("no_parent").Unmark(); v.Type() == cty.Bool && !v.IsNull() && v.IsKnown() {
					spec.NoParent = v.True()
				}
			}
			aliases = append(aliases, Alias{Spec: spec})
		}
	}
	return aliases, nil
}

// packageNameFromResourceType extracts the provider package name from an HCL resource type.
// For example, "config_resource" returns "config" and "pulumi_providers_config" returns "config".
func packageNameFromResourceType(token string) string {
	if name, ok := strings.CutPrefix(token, "pulumi_providers_"); ok {
		return name
	}
	return strings.SplitN(token, "_", 2)[0]
}

// pinnedVersion returns the version the program pins for the provider with
// the required_providers local name local, or "" when it pins none. The pin
// reaches the schema loader through the same package map, so a block is type
// checked against the version its requests carry.
func (e *Engine) pinnedVersion(local string) string {
	desc, ok := e.packages[e.providerPackageName(local)]
	if !ok || desc.Version == nil {
		return ""
	}
	return desc.Version.String()
}

// packageRefForType returns the RegisterPackage ref for the given HCL resource type, or empty if none.
func (e *Engine) packageRefForType(hclToken string) PackageRef {
	return e.packageRefs[packageNameFromResourceType(hclToken)]
}

// packageRefForResource returns the registered package ref for a resource. An
// extension resource's token lives in the base provider's namespace, but its
// resolved schema names the extension package (e.g. "myext"), which is the one
// registered as a parameterized package — using it lets the engine record the
// resource's ExtensionRef.
func (e *Engine) packageRefForResource(hclToken string, resSchema *schema.Resource) PackageRef {
	if resSchema != nil && resSchema.PackageReference != nil {
		if ref, ok := e.packageRefs[resSchema.PackageReference.Name()]; ok {
			return ref
		}
	}
	return e.packageRefForType(hclToken)
}

// providerPackageName maps a provider's required_providers local name to its
// Pulumi package name: the basename of the entry's source ("hashicorp/simple"
// → "simple"), or the local name itself when no entry renames it.
func providerPackageName(tfBlock *ast.Terraform, local string) string {
	if tfBlock != nil {
		if req, ok := tfBlock.RequiredProviders[local]; ok && req.Source != "" {
			parts := strings.Split(req.Source, "/")
			return parts[len(parts)-1]
		}
	}
	return local
}

func (e *Engine) providerPackageName(local string) string {
	return providerPackageName(e.config.Terraform, local)
}

func knownProviders(tfBlock *ast.Terraform) []string {
	if tfBlock == nil {
		return nil
	}
	providers := make([]string, 0, len(tfBlock.RequiredProviders))
	for name := range tfBlock.RequiredProviders {
		providers = append(providers, name)
	}
	return providers
}

// nodeOutputs returns the outputs of a block's single (suffixless) instance.
func (e *Engine) nodeOutputs(key graph.NodeKey) (cty.Value, bool) {
	return e.resourceOutputs.Get(graph.InstanceKey{Node: key})
}

// hasFailedDependency reports whether any dependency of res is in failedNodes.
// When true, the resource should be skipped so that only genuinely independent
// resources are registered with the engine.
func (e *Engine) hasFailedDependency(res *ast.Resource) bool {
	// Check explicit depends_on traversals.
	for _, dep := range res.DependsOn {
		if key, ok := graph.TraversalKey(modulepath.Root(), dep); ok {
			if _, failed := e.failedNodes.Get(graph.InstanceKey{Node: key}); failed {
				return true
			}
		}
	}
	// Check resource body expressions. A dependency reached only through a
	// recover(value, recovery) value argument does not gate the resource: if it
	// failed, recover() supplies the recovery value instead.
	if res.Config != nil {
		attrs, _ := res.Config.JustAttributes()
		for _, attr := range attrs {
			for _, depKey := range eval.NonRecoverableDependencies(attr.Expr) {
				if _, failed := e.failedNodes.Get(graph.InstanceKey{Node: graph.NodeKey{ID: depKey}}); failed {
					return true
				}
			}
		}
	}
	return false
}

// instanceStep builds the modulepath step for one instance of a block from
// the expansion's optional count index or for_each key.
func instanceStep(logicalName string, index *int, eachKey *string) modulepath.Step {
	switch {
	case index != nil:
		return modulepath.NewIndexedStep(logicalName, *index)
	case eachKey != nil:
		return modulepath.NewKeyedStep(logicalName, *eachKey)
	default:
		return modulepath.NewStep(logicalName)
	}
}

// joinModuleName joins an enclosing module instance's resolved name and a
// child name with ".". An empty parent (the root) leaves the name bare.
func joinModuleName(parentName, name string) string {
	if parentName == "" {
		return name
	}
	return parentName + "." + name
}

// evaluatePulumiName evaluates a `pulumi { name = ... }` override expression.
// ok is false when the expression evaluates to null, which means "no
// override"; subject names the block for error messages.
func evaluatePulumiName(expr hcl.Expression, hclCtx *hcl.EvalContext, subject string) (name string, ok bool, err error) {
	val, diags := expr.Value(hclCtx)
	if diags.HasErrors() {
		return "", false, fmt.Errorf("evaluating pulumi name for %s: %s", subject, diags.Error())
	}
	val, _ = val.Unmark()
	if val.IsNull() {
		return "", false, nil
	}
	if !val.IsKnown() || val.Type() != cty.String {
		return "", false, fmt.Errorf("pulumi name for %s must be a known string", subject)
	}
	return val.AsString(), true, nil
}

// resourceInstanceName computes the Pulumi resource name for one resource
// instance. A `pulumi { name = ... }` override is evaluated per instance
// (count.index/each.key and pulumi.module.name are in scope) and is the full
// logical name — no module prefix is applied; a null override falls back to
// the derived name. Derived names are prefixed with the enclosing module
// instance's resolved name (e.g. "many" or "many[0]") joined with ".".
func (*Engine) resourceInstanceName(
	res *ast.Resource, instance *graph.ExpandedResource, hclCtx *hcl.EvalContext, modInst *moduleInstance,
) (string, error) {
	if res.PulumiName != nil {
		name, ok, err := evaluatePulumiName(res.PulumiName, hclCtx, res.Type+"."+res.Name)
		if err != nil {
			return "", err
		}
		if ok {
			return name, nil
		}
	}
	name := instanceStep(res.Name, instance.Index, instance.EachKeyString()).LogicalName()
	if modInst == nil {
		return name, nil
	}
	return joinModuleName(modInst.Name, name), nil
}

// translateAttrPathTraversal formats an attribute-path traversal (e.g. an
// ignore_changes entry) into the dotted/bracketed path the Pulumi engine
// expects, translating each TF (snake_case) attribute segment to its Pulumi
// (camelCase) name.
func translateAttrPathTraversal(
	traversal hcl.Traversal, mapping *bridge.BodyMapping, props []*schema.Property,
) (property.Glob, error) {
	if len(traversal) == 0 {
		return property.Glob{}, nil
	}
	segments := make([]property.GlobSegment, 0, len(traversal))
	resolver := attrPathNameResolver{mapping: mapping, props: props}
	prevSingularBlock := false
	for _, step := range traversal {
		switch s := step.(type) {
		case hcl.TraverseRoot:
			name, singularBlock, err := resolver.next(s.Name)
			if err != nil {
				return property.Glob{}, err
			}
			segments = append(segments, property.NewSegment(name))
			prevSingularBlock = singularBlock
		case hcl.TraverseAttr:
			name, singularBlock, err := resolver.next(s.Name)
			if err != nil {
				return property.Glob{}, err
			}
			segments = append(segments, property.NewSegment(name))
			prevSingularBlock = singularBlock
		case hcl.TraverseIndex:
			// The bridge flattens a MaxItems=1 block to a single object, so the
			// TF list index addressing it (settings[0]) has no Pulumi
			// counterpart: the flattened object property already stands in for
			// the sole element, and the index is dropped so the translated path
			// (settings.mode) matches.
			if prevSingularBlock {
				prevSingularBlock = false
				continue
			}
			// Index segments are dynamic map keys or list indices, not schema
			// properties: they are emitted verbatim and not validated (the
			// preceding attribute already advanced the resolver past the
			// collection), matching OpenTofu, which accepts foo["bar"] without
			// checking the key.
			key := s.Key
			if key.Type() == cty.String {
				segments = append(segments, property.NewSegment(key.AsString()))
			} else if key.Type() == cty.Number {
				i64, acc := key.AsBigFloat().Int64()
				if acc != big.Exact || i64 > math.MaxInt {
					return property.Glob{}, fmt.Errorf("unrepresentable path segment %s", key.AsBigFloat())
				}
				segments = append(segments, property.NewSegment(int(i64)))
			}
		}
	}

	return property.GlobFromSegments(segments...), nil
}

// ignoreChangesApplies reports whether an ignore_changes path resolves inside
// the evaluated inputs. OpenTofu silently skips an entry whose path does not
// apply to the configuration — removing a whole block un-ignores the
// attributes inside it, so e.g. a ForceNew change there is planned as a
// replacement again. Passing such an entry to the engine anyway would keep
// suppressing the diff, because the engine ignores by state paths too.
//
// Mirroring OpenTofu's check: a trailing map key is exempt (a key may be
// ignored while being added or removed), a missing leaf attribute still
// resolves (it decodes as null in OpenTofu's config), and an unknown value
// cannot be disproven, so those keep the entry. OpenTofu also requires the
// path to resolve in the prior state, which is not visible here.
func ignoreChangesApplies(
	traversal hcl.Traversal, mapping *bridge.BodyMapping, props []*schema.Property, inputs property.Map,
) bool {
	if len(traversal) > 1 {
		if idx, ok := traversal[len(traversal)-1].(hcl.TraverseIndex); ok && idx.Key.Type() == cty.String {
			traversal = traversal[:len(traversal)-1]
		}
	}
	resolver := attrPathNameResolver{mapping: mapping, props: props}
	v := property.New(inputs)
	prevSingularBlock := false
	for i, step := range traversal {
		if v.IsComputed() {
			return true
		}
		var tfName string
		switch s := step.(type) {
		case hcl.TraverseRoot:
			tfName = s.Name
		case hcl.TraverseAttr:
			tfName = s.Name
		case hcl.TraverseIndex:
			if prevSingularBlock {
				prevSingularBlock = false
				continue
			}
			switch {
			case s.Key.Type() == cty.Number && v.IsArray():
				idx, acc := s.Key.AsBigFloat().Int64()
				if acc != big.Exact || idx < 0 || idx >= int64(v.AsArray().Len()) {
					return false
				}
				v = v.AsArray().Get(int(idx))
			case s.Key.Type() == cty.String && v.IsMap():
				el, ok := v.AsMap().GetOk(s.Key.AsString())
				if !ok {
					return false
				}
				v = el
			default:
				return false
			}
			continue
		}
		name, singularBlock, err := resolver.next(tfName)
		if err != nil {
			return false
		}
		if !v.IsMap() {
			return false
		}
		el, ok := v.AsMap().GetOk(name)
		if !ok {
			return i == len(traversal)-1
		}
		v = el
		prevSingularBlock = singularBlock
	}
	return true
}

// ctyPathTraversal converts a cty.Path over an evaluated inputs object into an
// attribute-path traversal, so it can be translated to engine form by
// translateAttrPathTraversal like a hide_diffs entry.
func ctyPathTraversal(p cty.Path) hcl.Traversal {
	t := make(hcl.Traversal, 0, len(p))
	for _, step := range p {
		switch s := step.(type) {
		case cty.GetAttrStep:
			t = append(t, hcl.TraverseAttr{Name: s.Name})
		case cty.IndexStep:
			t = append(t, hcl.TraverseIndex{Key: s.Key})
		}
	}
	return t
}

// translateSecretOutputName translates a single additional_secret_outputs entry
// from its TF (snake_case) name to its Pulumi name. Unlike hide_diffs and
// replace_on_changes, an additional_secret_outputs entry names a single
// top-level output property rather than a nested path, so a multi-segment
// traversal is rejected.
func translateSecretOutputName(
	t hcl.Traversal, mapping *bridge.BodyMapping, props []*schema.Property,
) (string, error) {
	if len(t) != 1 {
		return "", fmt.Errorf(
			"invalid additional_secret_outputs entry %#v: expected a single top-level property name",
			formatAttrTraversal(t))
	}
	var name string
	switch s := t[0].(type) {
	case hcl.TraverseRoot:
		name = s.Name
	case hcl.TraverseAttr:
		name = s.Name
	default:
		return "", fmt.Errorf("invalid additional_secret_outputs entry: expected a property name")
	}
	resolver := attrPathNameResolver{mapping: mapping, props: props}
	pulumiName, _, err := resolver.next(name)
	return pulumiName, err
}

// formatAttrTraversal renders an attribute-path traversal as a dotted/bracketed
// string for diagnostics.
func formatAttrTraversal(t hcl.Traversal) string {
	var b strings.Builder
	for i, step := range t {
		switch s := step.(type) {
		case hcl.TraverseRoot:
			b.WriteString(s.Name)
		case hcl.TraverseAttr:
			if i > 0 {
				b.WriteByte('.')
			}
			b.WriteString(s.Name)
		case hcl.TraverseIndex:
			if s.Key.Type() == cty.String {
				fmt.Fprintf(&b, "[%q]", s.Key.AsString())
			} else if s.Key.Type() == cty.Number {
				if i64, acc := s.Key.AsBigFloat().Int64(); acc == big.Exact {
					fmt.Fprintf(&b, "[%d]", i64)
				}
			}
		}
	}
	return b.String()
}

// attrPathNameResolver walks an attribute path, translating each attribute
// segment from its TF name to its Pulumi name and descending into the nested
// schema for the next segment.
type attrPathNameResolver struct {
	mapping *bridge.BodyMapping
	props   []*schema.Property
}

// next translates one TF (snake_case) attribute-name segment to its Pulumi name
// and advances the resolver into the nested schema for the following segment. It
// reports whether the resolved field is a MaxItems=1 field flattened to a single
// Pulumi value, so the caller can drop the TF list index that follows it.
func (r *attrPathNameResolver) next(tfName string) (name string, singular bool, err error) {
	if fm := r.mapping.Lookup(tfName); fm != nil {
		r.mapping, r.props = fm.Nested, nil
		return fm.PulumiName, fm.MaxItemsOne, nil
	}
	pulumiName, prop := transform.PulumiCaseFromSnakeCase(tfName, r.props)
	if prop != nil {
		r.mapping, r.props = nil, objectProperties(prop.Type)
		_, isObject := codegen.UnwrapType(prop.Type).(*schema.ObjectType)
		return pulumiName, isObject, nil
	}
	if r.mapping != nil || len(r.props) > 0 {
		return "", false, fmt.Errorf("unknown property %q", tfName)
	}
	return tfName, false, nil
}

// objectProperties returns the nested properties of an object-typed schema,
// unwrapping array/map element types and optional wrappers, or nil when the
// type has no named properties.
func objectProperties(t schema.Type) []*schema.Property {
	switch tt := t.(type) {
	case *schema.ObjectType:
		return tt.Properties
	case *schema.ArrayType:
		return objectProperties(tt.ElementType)
	case *schema.MapType:
		return objectProperties(tt.ElementType)
	case *schema.OptionalType:
		return objectProperties(tt.ElementType)
	default:
		return nil
	}
}

// ResourceOptions contains resource registration options.
type ResourceOptions struct {
	Custom                  bool
	Remote                  bool
	DependsOn               []string
	PropertyDependencies    map[string][]string
	Protect                 bool
	IgnoreChanges           []property.Glob
	Aliases                 []Alias
	Provider                string
	Providers               map[string]string // Map from package name to provider reference (urn::id)
	Parent                  urn.URN
	DeleteBeforeReplace     bool
	DeleteBeforeReplaceDef  bool // True if DeleteBeforeReplace was explicitly set
	CustomTimeouts          *CustomTimeouts
	ImportId                string
	AdditionalSecretOutputs []string
	RetainOnDelete          *bool
	DeletedWith             string          // URN of the resource that, when deleted, causes this resource to be deleted
	ReplaceWith             []string        // URNs of resources whose replacement triggers replacement of this resource
	HideDiffs               []property.Glob // Property paths whose diffs should not be displayed
	ReplaceOnChanges        []property.Glob // Property paths that if changed should force a replacement
	ReplacementTrigger      property.Value  // Value whose change triggers replacement
	EnvVarMappings          map[string]string
	Version                 string
	PluginDownloadURL       string
	PackageRef              PackageRef
	Hooks                   *ResourceHookBinding

	// PreventDestroy is enforced by the destroy dispatcher hook, not the
	// engine: the guard must be re-evaluated from current configuration on
	// every run, while an engine option would persist in state. Never
	// forwarded to RegisterResource.
	PreventDestroy preventDestroyGuard
}

// registerResource registers a resource with the Pulumi engine. tfType is the
// HCL resource type, used only to detect builtins (like terraform_data) that are
// lowered onto a different engine resource at this schemaless boundary.
func (e *Engine) registerResource(
	ctx context.Context,
	tfType string,
	typeToken string,
	name string,
	inputs property.Map,
	opts *ResourceOptions,
) (urn.URN, string, property.Map, bool, error) {
	inputs = lowerTerraformDataInputs(tfType, inputs, opts)

	if typeToken == stackReferenceType {
		u, id, outputs, err := e.readResource(ctx, typeToken, name, inputs, opts)
		return u, id, outputs, false, err
	}

	// Register with the resource monitor
	resp, err := e.resmon.RegisterResource(ctx, RegisterResourceRequest{
		Type:                    typeToken,
		Name:                    name,
		Inputs:                  inputs,
		Dependencies:            opts.DependsOn,
		PropertyDependencies:    opts.PropertyDependencies,
		Custom:                  opts.Custom,
		Remote:                  opts.Remote,
		Protect:                 opts.Protect,
		IgnoreChanges:           opts.IgnoreChanges,
		Aliases:                 opts.Aliases,
		Provider:                opts.Provider,
		Providers:               opts.Providers,
		Parent:                  opts.Parent,
		DeleteBeforeReplace:     opts.DeleteBeforeReplace,
		DeleteBeforeReplaceDef:  opts.DeleteBeforeReplaceDef,
		CustomTimeouts:          opts.CustomTimeouts,
		ImportId:                opts.ImportId,
		AdditionalSecretOutputs: opts.AdditionalSecretOutputs,
		RetainOnDelete:          opts.RetainOnDelete,
		DeletedWith:             opts.DeletedWith,
		ReplaceWith:             opts.ReplaceWith,
		HideDiffs:               opts.HideDiffs,
		ReplaceOnChanges:        opts.ReplaceOnChanges,
		ReplacementTrigger:      opts.ReplacementTrigger,
		EnvVarMappings:          opts.EnvVarMappings,
		Version:                 opts.Version,
		PluginDownloadURL:       opts.PluginDownloadURL,
		PackageRef:              opts.PackageRef,
		Hooks:                   opts.Hooks,
	})
	if err != nil {
		return "", "", property.Map{}, false, err
	}

	return resp.URN, resp.ID, lowerTerraformDataOutputs(tfType, resp.Outputs, opts), resp.Unknown, nil
}

// stackReferenceType is the builtin resource the engine resolves against the
// backend. It is registered with a Read rather than a Create: the modern Pulumi
// SDKs read stack references (Go's ReadResource, Node's id-bearing
// CustomResource), and Create is deprecated for this type.
const stackReferenceType = "pulumi:pulumi:StackReference"

// readResource registers a resource via a Read. The ID is taken from the "name"
// input (the fully-qualified stack name), matching how the SDKs identify a
// stack reference.
func (e *Engine) readResource(
	ctx context.Context,
	typeToken string,
	name string,
	inputs property.Map,
	opts *ResourceOptions,
) (urn.URN, string, property.Map, error) {
	nameVal, ok := inputs.GetOk("name")
	if !ok || !nameVal.IsString() {
		return "", "", property.Map{}, fmt.Errorf("%s %q requires a string \"name\" input", typeToken, name)
	}

	resp, err := e.resmon.ReadResource(ctx, ReadResourceRequest{
		Type:                    typeToken,
		Name:                    name,
		ID:                      nameVal.AsString(),
		Inputs:                  inputs,
		Parent:                  opts.Parent,
		Dependencies:            opts.DependsOn,
		Provider:                opts.Provider,
		Version:                 opts.Version,
		AdditionalSecretOutputs: opts.AdditionalSecretOutputs,
		PluginDownloadURL:       opts.PluginDownloadURL,
		PackageRef:              opts.PackageRef,
	})
	if err != nil {
		return "", "", property.Map{}, err
	}

	return resp.URN, resp.ID, resp.Outputs, nil
}

// invokeDataSourceOnce performs a single data-source invocation using the
// current state of evalCtx (which may have each/count set by the caller).
// The returned outputs are marked with the URN deps gathered from inputs
// and explicit depends_on, so downstream reads of data.X.Y carry them
// without a separate dependency map.
func (e *Engine) invokeDataSourceOnce(
	ctx context.Context, node *graph.Node, ds *ast.DataSource, funcSchema *schema.Function,
	evalCtx *eval.Context, mi *moduleInstance,
) (cty.Value, error) {
	hclCtx := evalCtx.HCLContext()

	miPath := modulepath.Root()
	if mi != nil {
		miPath = mi.Path
	}

	// Failed preconditions prevent the read; unknown conditions defer.
	if err := evaluatePreconditions(ds.Preconditions, hclCtx, node.Key.String()); err != nil {
		return cty.NilVal, err
	}

	depMarks := cty.ValueMarks{}
	var depURNs []string
	addURN := func(urn string) {
		if urn == "" {
			return
		}
		depMarks[eval.DepMark(urn)] = struct{}{}
		depURNs = append(depURNs, urn)
	}

	// Check-rule references establish dependencies, as in TF; carry them on
	// the outputs alongside the input-derived deps so downstream readers
	// inherit them.
	for _, urn := range checkRuleDeps(ds.Preconditions, hclCtx) {
		addURN(urn)
	}
	for _, urn := range checkRuleDeps(ds.Postconditions, hclCtx) {
		addURN(urn)
	}

	dataSourceMapping := e.resolver.DataSourceBodyMapping(ctx, ds.Type)
	timeouts, timeoutsRole, err := evalTimeouts(ds.Timeouts, dataSourceMapping, hclCtx)
	if err != nil {
		return cty.NilVal, err
	}
	inputEval := evalSchemaInputs(hclCtx, func(_ resource.PropertyKey, val cty.Value) {
		for _, urn := range eval.CollectDepURNs(val) {
			addURN(urn)
		}
	})
	inputs, diags := transform.EvalFunctionWithSchema(
		ds.Config, funcSchema, dataSourceMapping, inputEval)
	if diags.HasErrors() {
		return cty.NilVal, diags
	}
	if timeoutsRole == timeoutsInput && !timeouts.IsNull() {
		pv, err := transform.CtyToPropertyValue(timeouts)
		if err != nil {
			return cty.NilVal, fmt.Errorf("converting timeouts: %w", err)
		}
		inputs = inputs.Set(dataSourceMapping.Lookup("timeouts").PulumiName, pv)
		for _, urn := range eval.CollectDepURNs(timeouts) {
			addURN(urn)
		}
	}

	invokeReq := InvokeRequest{
		Token:      funcSchema.Token,
		Args:       inputs,
		Version:    e.pinnedVersion(packageNameFromResourceType(ds.Type)),
		PackageRef: e.packageRefForType(ds.Type),
	}

	if ds.Provider != nil {
		ref, err := e.resolveExplicitProvider(ds.Provider, evalCtx, node.ModuleInfo)
		if err != nil {
			return cty.NilVal, fmt.Errorf("resolving provider for data %s.%s: %w", ds.Type, ds.Name, err)
		}
		if ref != "" {
			invokeReq.Provider = ref
		}
	} else if node.ModuleInfo != nil {
		pkg := packageNameFromResourceType(ds.Type)
		if ref := e.resolvePassThroughProvider(node.ModuleInfo, pkg); ref != "" {
			invokeReq.Provider = ref
		} else if outputs, ok := e.nodeOutputs(graph.NodeKey{Module: node.Key.Module, ID: pkg}); ok {
			if ref, err := providerRefFromCty(outputs); err == nil {
				invokeReq.Provider = ref
			}
		} else if ref := e.inheritedDefaultProvider(node.ModuleInfo, pkg); ref != "" {
			invokeReq.Provider = ref
		}
	} else {
		if ref, ok := e.defaultProviders.Get(packageNameFromResourceType(ds.Type)); ok {
			invokeReq.Provider = ref
		}
	}

	if ds.PluginDownloadURL != nil {
		val, valDiags := ds.PluginDownloadURL.Value(hclCtx)
		if !valDiags.HasErrors() && val.Type() == cty.String {
			invokeReq.PluginDownloadURL = ctyAsString(val)
		}
	}

	// depends_on marks the outputs with every registered instance URN of the
	// target block, keyed or not — dependency metadata is resource-wide (see
	// buildResourceOptions).
	dependsOnPending := e.moduleDependsOnPending(mi)
	for _, dep := range ds.DependsOn {
		block, ok := graph.TraversalKey(node.Key.Module, dep)
		if !ok {
			continue
		}
		cell := expansionCell{block: block, mi: miPath}
		for _, urn := range e.cellURNs.get(cell) {
			addURN(urn)
		}
		if e.cellPending(cell) {
			dependsOnPending = true
		}
	}

	// A data source carrying custom conditions widens the decision to its whole
	// dependency set, direct and indirect: a condition can only be made to pass
	// by an upstream change, so a pending resource reached through another data
	// source counts too. Without conditions a `depends_on` naming a data source
	// is no reason to wait — a read has no side effects of its own.
	if len(ds.Preconditions) > 0 || len(ds.Postconditions) > 0 {
		for _, dep := range ds.DependsOn {
			val, diags := dep.TraverseAbs(hclCtx)
			if diags.HasErrors() {
				continue
			}
			for _, urn := range eval.CollectDepURNs(val) {
				addURN(urn)
			}
		}
		for _, urn := range depURNs {
			if _, pending := e.pendingURNs.Get(urn); pending {
				dependsOnPending = true
			}
		}
	}

	// The engine gates the invoke on the created-ness of its dependencies.
	invokeReq.DependsOn = depURNs

	// Match the Node.js / Python SDK behavior: during preview, if any input
	// to the invoke is unknown, skip the provider call and synthesize an
	// all-unknown result. A pending `depends_on` target defers the read too,
	// even with every input known: the read would observe state that the
	// dependency has not produced yet.
	var outputs property.Map
	if !e.dryRun || (!property.New(inputs).HasComputed() && !dependsOnPending) {
		var err error
		outputs, err = e.invokeFunction(ctx, ds.Type, invokeReq)
		if err != nil {
			return cty.NilVal, fmt.Errorf("invoking data source: %w", err)
		}
	}

	ctyOutputs, err := transform.FunctionOutputToCty(outputs, funcSchema, dataSourceMapping, e.dryRun)
	if err != nil {
		return cty.NilVal, fmt.Errorf("converting function outputs to HCL types: %w", err)
	}
	if timeoutsRole == timeoutsAttribute && ctyOutputs.Type().IsObjectType() && ctyOutputs.IsKnown() && !ctyOutputs.IsNull() {
		attrs := ctyOutputs.AsValueMap()
		if attrs == nil {
			attrs = map[string]cty.Value{}
		}
		attrs["timeouts"] = timeouts
		ctyOutputs = cty.ObjectVal(attrs)
	}

	if err := evaluatePostconditionValues(ds.Postconditions, hclCtx, ctyOutputs, node.Key.String()); err != nil {
		return cty.NilVal, err
	}

	return ctyOutputs.WithMarks(depMarks), nil
}

// moduleDependsOnPending reports whether any enclosing module call
// `depends_on`s a resource whose changes have not been applied yet. A module's
// depends_on covers everything the module contains, so such a dependency
// defers the reads inside it just as one written on the data block would.
func (e *Engine) moduleDependsOnPending(mi *moduleInstance) bool {
	for inst := mi; inst != nil; inst = inst.Parent {
		for _, dep := range inst.ModuleInfo.Module.DependsOn {
			// The targets are addressed from the calling module's scope.
			block, ok := graph.TraversalKey(inst.ModuleInfo.ParentPath(), dep)
			if !ok {
				continue
			}
			cell := expansionCell{block: block, mi: parentMIPath(inst)}
			if e.cellPending(cell) {
				return true
			}
		}
	}
	return false
}

// cellPending reports whether the block cell resolves to anything whose changes
// have not been applied yet: a resource instance with unknowns in its outputs,
// or a module call containing one.
func (e *Engine) cellPending(cell expansionCell) bool {
	if _, pending := e.pendingModuleCalls.Get(cell); pending {
		return true
	}
	for _, urn := range e.cellURNs.get(cell) {
		if _, pending := e.pendingURNs.Get(urn); pending {
			return true
		}
	}
	return false
}

// moduleCallCell is the cell of the module call inst instantiates, keyed as the
// calling scope addresses it: every instance of an expanded call shares it,
// matching how a `depends_on` entry naming the call covers the whole call.
func moduleCallCell(inst *moduleInstance) expansionCell {
	return expansionCell{
		block: graph.NodeKey{Module: inst.ModuleInfo.ParentPath(), ID: "module." + inst.ModuleInfo.ModuleName()},
		mi:    parentMIPath(inst),
	}
}

// parentMIPath is the instance path of the module instance enclosing inst, or
// the root when inst was called from the root.
func parentMIPath(inst *moduleInstance) modulepath.Path {
	if inst.Parent == nil {
		return modulepath.Root()
	}
	return inst.Parent.Path
}

// processCall processes a call block (method invocation on a resource).
func (e *Engine) processCall(ctx context.Context, node *graph.Node) error {
	call := node.Call
	if call == nil {
		return fmt.Errorf("call node missing Call field")
	}

	// Find the resource or provider being called by logical name
	var resKey string
	var resType string
	var resSchema *schema.Resource
	var isProviderResource bool // true for resource "pulumi_providers_*" blocks

	for k, res := range e.config.Resources {
		if res.Name == call.ResourceName {
			resKey = k
			resType = res.Type
			var err error
			resSchema, err = e.resolver.ResolveResource(ctx, res.Type)
			if err != nil {
				if diag := unknownTokenDiag("resource", res.TypeRange, err); diag != err {
					return diag
				}
				return fmt.Errorf("resolving resource type %s for call: %w", res.Type, err)
			}
			isProviderResource = strings.HasPrefix(res.Type, "pulumi_providers_")
			break
		}
	}

	if resKey == "" {
		// Try providers — config.Providers is keyed by provider.Key(),
		// so we search by alias / name instead of by ResourceName directly.
		var matched *ast.Provider
		for _, p := range e.config.Providers {
			if p.Alias == call.ResourceName || (p.Alias == "" && p.Name == call.ResourceName) {
				matched = p
				break
			}
		}
		if matched != nil {
			resKey = matched.Key()
			providerToken := "pulumi_providers_" + e.providerPackageName(matched.Name)
			resType = providerToken
			pkg, err := packages.ResolvePackage(ctx, e.pkgLoader, knownProviders(e.config.Terraform), providerToken)
			if err != nil {
				return fmt.Errorf("resolving provider package for call: %w", err)
			}
			resSchema, err = pkg.Provider()
			if err != nil {
				return fmt.Errorf("resolving provider schema for call: %w", err)
			}
		}
	}

	if resKey == "" {
		return fmt.Errorf("call block references unknown resource or provider %q", call.ResourceName)
	}

	// Find the method in the resource schema by matching snake_case name
	var method *schema.Method
	for _, m := range resSchema.Methods {
		if transform.SnakeCaseFromPulumiCase(m.Name) == call.MethodName {
			method = m
			break
		}
	}
	if method == nil {
		return fmt.Errorf("resource %q has no method %q", call.ResourceName, call.MethodName)
	}

	// Look up resource outputs to get URN and ID
	outputs, ok := e.nodeOutputs(graph.NodeKey{ID: resKey})
	if !ok {
		return fmt.Errorf("resource %q outputs not found", resKey)
	}

	urnStr := ctyAsString(outputs.GetAttr("urn"))
	if urnStr == "" {
		return fmt.Errorf("resource %q missing URN", resKey)
	}
	urn := resource.URN(urnStr)

	// Build __self__ resource reference
	var selfID property.Value
	if resSchema.IsComponent && !isProviderResource {
		selfID = property.New(property.Null)
	} else {
		idVal, _ := outputs.GetAttr("id").Unmark()
		if idVal.Type() == cty.String && idVal.IsKnown() && !idVal.IsNull() {
			selfID = property.New(idVal.AsString())
		} else if idVal.Type() == cty.String {
			selfID = property.New(property.Computed)
		} else {
			selfID = property.New(property.Null)
		}
	}
	selfRef := property.New(property.ResourceReference{
		URN: urn,
		ID:  selfID,
	})

	// Evaluate call arguments using the function schema, excluding __self__ which is
	// provided by the runtime (not the HCL body).
	filteredFunc := *method.Function
	if filteredFunc.Inputs != nil {
		filteredInputs := *filteredFunc.Inputs
		filteredInputs.Properties = slices.DeleteFunc(
			slices.Clone(filteredInputs.Properties),
			func(p *schema.Property) bool { return p.Name == "__self__" },
		)
		filteredFunc.Inputs = &filteredInputs
	}

	userArgs, diags := transform.EvalFunctionWithSchema(
		call.Config, &filteredFunc, nil,
		evalSchemaInputs(e.evaluator.Context().HCLContext(), nil))
	if diags.HasErrors() {
		return fmt.Errorf("evaluating call arguments for %s.%s: %s", call.ResourceName, call.MethodName, diags.Error())
	}

	ret, err := e.callMethod(ctx, CallRequest{
		Token:      method.Function.Token,
		Args:       userArgs.Set("__self__", selfRef),
		PackageRef: e.packageRefForType(resType),
	})
	if err != nil {
		return fmt.Errorf("calling method %s.%s: %w", call.ResourceName, call.MethodName, err)
	}

	// Convert return values to cty
	ctyOutputs, err := transform.FunctionOutputToCty(ret, method.Function, nil, e.dryRun)
	if err != nil {
		return fmt.Errorf("converting call outputs to HCL types: %w", err)
	}

	// Store outputs keyed as "resourceName.methodName"
	callKey := ast.CallKey(call.ResourceName, call.MethodName)
	e.evaluator.Context().SetCall(callKey, ctyOutputs)

	return nil
}

// callMethod calls a method on a resource via the resource monitor.
func (e *Engine) callMethod(ctx context.Context, req CallRequest) (property.Map, error) {
	resp, err := e.resmon.Call(ctx, req)
	if err != nil {
		return property.Map{}, err
	}

	if len(resp.Failures) > 0 {
		return property.Map{}, fmt.Errorf("method call failed: %v", resp.Failures)
	}

	return resp.Return, nil
}

// installProviderFunctions builds the provider-defined function table for the
// module described by config and installs it on evalCtx, keyed as
// provider::<localname>::<name>. Only providers the parser saw function calls
// on are resolved, so provider schemas keep loading lazily. When the parser
// could not scan every file (JSON syntax), it falls back to (leniently)
// resolving every declared provider. modInfo is nil for the root module.
func (e *Engine) installProviderFunctions(
	ctx context.Context, evalCtx *eval.Context, config *ast.Config, modInfo *graph.ModuleInfo,
) error {
	table, err := e.providerFunctionTable(ctx, config, modInfo)
	if err != nil {
		return err
	}
	if len(table) > 0 {
		evalCtx.SetProviderFunctions(table)
	}
	return nil
}

// providerFunctionTable resolves and projects the provider-defined functions
// the module described by config can call. See installProviderFunctions.
func (e *Engine) providerFunctionTable(
	ctx context.Context, config *ast.Config, modInfo *graph.ModuleInfo,
) (map[string]function.Function, error) {
	referenced := map[string]struct{}{}
	for _, name := range config.ProviderFunctionCalls {
		referenced[name] = struct{}{}
	}
	lenient := false
	if config.ProviderFunctionCallsIncomplete && config.Terraform != nil {
		lenient = true
		for name := range config.Terraform.RequiredProviders {
			referenced[name] = struct{}{}
		}
	}
	if len(referenced) == 0 {
		return nil, nil
	}

	table := map[string]function.Function{}
	for providerName := range referenced {
		if config.Terraform != nil {
			if req, ok := config.Terraform.RequiredProviders[providerName]; ok && packages.IsTerraformProviderSource(req.Source) {
				for tfName, fn := range eval.TerraformProviderFunctions() {
					table[ast.ProviderFunctionName(providerName, tfName)] = fn
				}
				continue
			}
		}
		fns, err := e.resolver.ProviderFunctions(ctx, providerPackageName(config.Terraform, providerName))
		if err != nil {
			if lenient {
				logging.V(5).Infof("provider functions for %q unavailable: %v", providerName, err)
				continue
			}
			return nil, fmt.Errorf("resolving provider functions for %q: %w", providerName, err)
		}
		for tfName, fnSchema := range fns {
			f, err := transform.ProviderFunction(fnSchema.Function, fnSchema.Variadic, e.dryRun,
				e.providerFunctionImpl(ctx, providerName, fnSchema.Function, modInfo))
			if err != nil {
				return nil, fmt.Errorf("projecting function provider::%s::%s: %w", providerName, tfName, err)
			}
			table[ast.ProviderFunctionName(providerName, tfName)] = f
		}
	}
	return table, nil
}

// providerFunctionImpl returns the invoke callback behind a provider-defined
// function. Provider routing mirrors a data source with no explicit
// `provider` argument: the instantiating module call's pass-through
// providers, then the module's own un-aliased provider block, then an
// ancestor's, and otherwise the package's default provider. During preview a
// call whose converted arguments carry unknowns is skipped, which the
// projection turns into an unknown result.
func (e *Engine) providerFunctionImpl(
	ctx context.Context, providerName string, fnSchema *schema.Function, modInfo *graph.ModuleInfo,
) transform.ProviderFunctionImpl {
	return func(args property.Map) (property.Map, error) {
		if e.dryRun && property.New(args).HasComputed() {
			return property.Map{}, nil
		}
		req := InvokeRequest{
			Token:      fnSchema.Token,
			Args:       args,
			Version:    e.pinnedVersion(providerName),
			PackageRef: e.packageRefs[e.providerPackageName(providerName)],
		}
		if modInfo != nil {
			if ref := e.resolvePassThroughProvider(modInfo, providerName); ref != "" {
				req.Provider = ref
			} else if outputs, ok := e.nodeOutputs(graph.NodeKey{Module: modInfo.Path, ID: providerName}); ok {
				if ref, err := providerRefFromCty(outputs); err == nil {
					req.Provider = ref
				}
			} else if ref := e.inheritedDefaultProvider(modInfo, providerName); ref != "" {
				req.Provider = ref
			}
		} else if ref, ok := e.defaultProviders.Get(e.providerPackageName(providerName)); ok {
			req.Provider = ref
		}

		resp, err := e.resmon.Invoke(ctx, req)
		if err != nil {
			return property.Map{}, err
		}
		if len(resp.Failures) > 0 {
			return property.Map{}, fmt.Errorf("%s", strings.Join(resp.Failures, "; "))
		}
		return resp.Return, nil
	}
}

// invokeFunction invokes a Pulumi function (data source).
func (e *Engine) invokeFunction(ctx context.Context, tfType string, req InvokeRequest) (property.Map, error) {
	dsArgs := req.Args
	req, defaults, err := lowerRemoteStateInvoke(tfType, req)
	if err != nil {
		return property.Map{}, err
	}

	resp, err := e.resmon.Invoke(ctx, req)
	if err != nil {
		return property.Map{}, err
	}

	if len(resp.Failures) > 0 {
		return property.Map{}, fmt.Errorf("function invocation failed: %v", resp.Failures)
	}

	if resp.Unknown {
		return property.Map{}, nil
	}

	if tfType == packages.RemoteStateType {
		return remoteStateResult(dsArgs, defaults, resp.Return), nil
	}
	return resp.Return, nil
}

func (e *Engine) getResourceState(ctx context.Context, ref property.ResourceReference) (property.Map, error) {
	result, err := e.resmon.Invoke(ctx, InvokeRequest{
		Token: "pulumi:pulumi:getResource",
		Args:  property.NewMap(map[string]property.Value{"urn": property.New(string(ref.URN))}),
	})
	if err != nil {
		return property.Map{}, err
	}
	stateVal, ok := result.Return.GetOk("state")
	if !ok || !stateVal.IsMap() {
		return property.Map{}, nil
	}
	return stateVal.AsMap(), nil
}

func (e *Engine) resolveResourceRefsInOutputs(
	ctx context.Context,
	outputs property.Map,
	resSchema *schema.Resource,
) (property.Map, error) {
	resolved := outputs
	for _, p := range resSchema.Properties {
		resType, ok := codegen.UnwrapType(p.Type).(*schema.ResourceType)
		if !ok {
			continue
		}
		v, ok := resolved.GetOk(p.Name)
		if !ok || !v.IsResourceReference() {
			continue
		}
		ref := v.AsResourceReference()
		refMap := property.NewMap(map[string]property.Value{"__ref": property.New(ref)})
		if e.resmon != nil && resType.Resource != nil {
			if state, err := e.getResourceState(ctx, ref); err == nil {
				for _, sp := range resType.Resource.Properties {
					if sv, ok := state.GetOk(sp.Name); ok {
						refMap = refMap.Set(sp.Name, sv)
					}
				}
			}
		}
		resolved = resolved.Set(p.Name, property.New(refMap))
	}
	return resolved, nil
}

// resolveConfigResourceReference enriches a resource reference supplied as a
// typed config value with the referenced resource's state, so a program
// consuming this module as a component can read the resource's fields, not just
// its id. The referenced resource lives in the calling program, so its state is
// fetched through the monitor, keyed on the URN; this works even when the
// reference's id is still unknown during preview.
func (e *Engine) resolveConfigResourceReference(ctx context.Context, val cty.Value, u urn.URN) (cty.Value, error) {
	contract.Requiref(e.resmon != nil, "e.resmon", "cannot resolve a resource reference without a resource monitor")
	unmarked, marks := val.Unmark()
	contract.Assertf(unmarked.Type().IsObjectType() && unmarked.Type().HasAttribute("id"),
		"a resource reference must be an object with an id attribute, got %s", unmarked.Type().FriendlyName())

	idAttr, _ := unmarked.GetAttr("id").Unmark()
	ref := property.ResourceReference{URN: u, ID: property.New(property.Null)}
	switch {
	case !idAttr.IsKnown() || idAttr.IsNull():
		// A preview-unknown or component reference has no usable id;
		// getResource keys on the URN.
	case idAttr.Type() == cty.String:
		ref.ID = property.New(idAttr.AsString())
	default:
		return cty.Value{}, fmt.Errorf("resource reference %s has a non-string id of type %s",
			u, idAttr.Type().FriendlyName())
	}

	state, err := e.getResourceState(ctx, ref)
	if err != nil {
		return cty.Value{}, fmt.Errorf("fetching state of resource reference %s: %w", u, err)
	}
	attrs := transform.PropertyMapToCty(state).AsValueMap()
	if attrs == nil {
		attrs = map[string]cty.Value{}
	}
	attrs["id"] = unmarked.GetAttr("id")

	obj := cty.ObjectVal(attrs).WithMarks(marks)
	return eval.MarkResourceReference(obj, u), nil
}

// processModule processes a module call.
// Terraform modules map to Pulumi component resources. The module's resources
// become children of the component, and module outputs are collected for references.
// moduleLoaderAdapter adapts modules.Loader to graph.ModuleLoader.
type moduleLoaderAdapter struct {
	loader *modules.Loader
}

func (a *moduleLoaderAdapter) LoadModule(source, version, workDir string) (*graph.LoadedModule, error) {
	// graph.ModuleLoader carries no context, so there is none to thread here.
	loaded, err := a.loader.LoadModule(context.TODO(), source, version, workDir)
	if err != nil {
		return nil, err
	}
	return &graph.LoadedModule{
		Config:     loaded.Config,
		SourcePath: loaded.SourcePath,
	}, nil
}

// forEachModuleInstance iterates over all instances of the module identified by node.ModuleInfo.Path.
func (e *Engine) forEachModuleInstance(node *graph.Node, fn func(inst *moduleInstance) error) error {
	instances, ok := e.moduleInstances.Get(node.ModuleInfo.Path)
	if !ok {
		return fmt.Errorf("no module instances for %q", graph.ModuleAddress(node.ModuleInfo.Path))
	}
	for _, inst := range instances {
		if err := fn(inst); err != nil {
			return err
		}
	}
	return nil
}

// processModuleVariable evaluates a module variable's input expression in the parent context
// and stores the result in each module instance's eval context.
func (e *Engine) processModuleVariable(node *graph.Node) error {
	v := node.Variable
	modInfo := node.ModuleInfo
	varName := v.Name

	moduleInputAttrs, _ := modInfo.Module.Config.JustAttributes()
	inputAttr, hasInput := moduleInputAttrs[varName]

	return e.forEachModuleInstance(node, func(inst *moduleInstance) error {
		var val cty.Value

		if hasInput {
			// The input expression lives in the enclosing module instance's
			// scope so that expressions like var.name resolve correctly; for
			// root-level calls that is the root evaluator context.
			parentEvalCtx := e.evaluator.Context()
			if inst.Parent != nil {
				parentEvalCtx = inst.Parent.EvalCtx
			}
			var diags hcl.Diagnostics
			hclCtx := parentEvalCtx.HCLContextWithIteration(inst.Index, inst.EachKey, inst.EachVal)
			val, diags = inputAttr.Expr.Value(hclCtx)
			if diags.HasErrors() {
				return fmt.Errorf("evaluating module input %s: %s", varName, diags.Error())
			}
		} else {
			// No input: the variable takes its default. Only root variables
			// are set from outside the program, so nothing else applies here.
			if v.Default != nil {
				var diags hcl.Diagnostics
				val, diags = v.Default.Value(inst.EvalCtx.HCLContext())
				if diags.HasErrors() {
					return fmt.Errorf("evaluating variable default for %s: %s", varName, diags.Error())
				}
			} else {
				return fmt.Errorf("variable %q is required but no value was provided", varName)
			}
		}

		// A `nullable = false` variable rejects an explicit null argument: the
		// default is substituted when one is declared, otherwise it is an
		// error. Matches Terraform/OpenTofu.
		if val.IsNull() && !v.Nullable {
			if v.Default == nil {
				return fmt.Errorf("variable %q must not be set to null: it is declared with nullable = false and has no default", varName)
			}
			var diags hcl.Diagnostics
			val, diags = v.Default.Value(inst.EvalCtx.HCLContext())
			if diags.HasErrors() {
				return fmt.Errorf("evaluating variable default for %s: %s", varName, diags.Error())
			}
		}

		// Fill in optional()-attribute defaults before type conversion so
		// the result satisfies the declared object shape.
		if v.TypeDefaults != nil && !val.IsNull() {
			val = v.TypeDefaults.Apply(val)
		}

		// Coerce the value to match the variable's type constraint.
		if v.TypeConstraint != cty.NilType {
			if converted, err := ctyconvert.Convert(val, v.TypeConstraint); err == nil {
				val = converted
			}
		}

		if v.Sensitive {
			val = val.Mark(eval.SensitiveMark)
		}
		if v.Ephemeral {
			val = val.Mark(eval.EphemeralMark)
		}

		inst.EvalCtx.SetVariable(varName, val)

		return runVariableValidations(eval.NewEvaluator(inst.EvalCtx), varName, v.Validations)
	})
}

// processModuleInit processes a module init node: registers component resources and creates instances.
func (e *Engine) processModuleInit(ctx context.Context, node *graph.Node) error {
	modInfo := node.ModuleInfo
	mod := modInfo.Module

	componentType := fmt.Sprintf("components:index:%s", modules.ComponentTypeName(modules.SourceName(mod.Source)))

	// A nested module call runs once per instance of the enclosing module; a
	// root-level call runs in the single root scope (a nil parent instance).
	// When the parent has zero instances (count=0 / for_each empty) the
	// entire inner subtree must be skipped — registering an empty instances
	// slice lets downstream per-instance work (vars, locals, nested modules,
	// resources) loop zero times instead of falling back to the root context.
	parents := []*moduleInstance{nil}
	if !modInfo.ParentPath().IsRoot() {
		parentInstances, ok := e.moduleInstances.Get(modInfo.ParentPath())
		if !ok || len(parentInstances) == 0 {
			e.moduleInstances.Set(modInfo.Path, nil)
			return nil
		}
		parents = parentInstances
	}

	// Load the child module to get variable type constraints for input coercion.
	loaderWorkDir := modInfo.ParentSourcePath
	if loaderWorkDir == "" {
		loaderWorkDir = e.workDir
	}
	childMod, err := e.moduleLoader.LoadModule(ctx, mod.Source, mod.Version, loaderWorkDir)
	if err != nil {
		return fmt.Errorf("loading module %s for input types: %w", mod.Source, err)
	}

	if err := e.checkModulePulumiVersion(ctx, mod.Source, childMod.Config); err != nil {
		return err
	}

	// One table serves every instance: the functions a module can call depend
	// on its config and call site, not on the count/for_each iteration.
	moduleFunctions, err := e.providerFunctionTable(ctx, childMod.Config, modInfo)
	if err != nil {
		return fmt.Errorf("module %s: %w", mod.Source, err)
	}

	var instances []*moduleInstance
	for _, parent := range parents {
		insts, err := e.initModuleCallIn(ctx, node, childMod, moduleFunctions, componentType, parent)
		if err != nil {
			return err
		}
		instances = append(instances, insts...)
	}
	e.moduleInstances.Set(modInfo.Path, instances)
	return e.materializeModuleCells(modInfo, instances)
}

// initModuleCallIn creates the instances of one module call within a single
// instance of the enclosing module (nil parent = the root config): it
// evaluates the call's inputs and count/for_each in that instance's scope and
// registers one component resource per resulting instance.
func (e *Engine) initModuleCallIn(
	ctx context.Context, node *graph.Node, childMod *modules.LoadedModule,
	moduleFunctions map[string]function.Function, componentType string,
	parent *moduleInstance,
) ([]*moduleInstance, error) {
	modInfo := node.ModuleInfo
	mod := modInfo.Module

	parentURN, parentEvalCtx, parentPath, parentName := e.stackURN, e.evaluator.Context(), modulepath.Root(), ""
	if parent != nil {
		parentURN, parentEvalCtx, parentPath, parentName = parent.URN, parent.EvalCtx, parent.Path, parent.Name
	}

	// Evaluate module inputs for the component resource registration
	inputs := make(map[string]property.Value)
	inputDeps := make(map[string][]string)
	attrs, _ := mod.Config.JustAttributes()
	for name, attr := range attrs {
		val, diags := attr.Expr.Value(parentEvalCtx.HCLContext())
		if diags.HasErrors() {
			continue
		}
		// Coerce to the variable's declared type if available.
		if v, ok := childMod.Config.Variables[name]; ok && v.TypeConstraint != cty.NilType {
			if converted, convErr := ctyconvert.Convert(val, v.TypeConstraint); convErr == nil {
				val = converted
			}
		}
		pv, err := transform.CtyToPropertyValue(val)
		if err == nil {
			inputs[name] = pv
			inputDeps[name] = eval.CollectDepURNs(val)
		}
	}

	// The engine propagates a component's providers map to remote components
	// registered under it.
	passedProviders := e.resolvePassedProviders(modInfo, parentEvalCtx)

	newInstance := func(index *int, eachKey, eachVal *cty.Value) (*moduleInstance, error) {
		instPath := instancePath(parentPath, modInfo.ModuleName(), index, eachKey)
		instName, err := moduleInstanceName(mod, parentEvalCtx, parentName, instPath, index, eachKey, eachVal)
		if err != nil {
			return nil, err
		}
		componentOpts := &ResourceOptions{
			Parent:               parentURN,
			Providers:            passedProviders,
			PropertyDependencies: inputDeps,
		}
		for _, deps := range inputDeps {
			componentOpts.DependsOn = append(componentOpts.DependsOn, deps...)
		}
		componentOpts.DependsOn = e.cellURNs.widen(componentOpts.DependsOn)
		componentOpts.Aliases = e.moduleComponentAliases(instPath)
		if mod.Protect != nil {
			hclCtx := parentEvalCtx.HCLContextWithIteration(index, eachKey, eachVal)
			val, diags := mod.Protect.Value(hclCtx)
			val, _ = val.Unmark()
			if !diags.HasErrors() && val.Type() == cty.Bool && !val.IsNull() && val.IsKnown() {
				componentOpts.Protect = val.True()
			}
		}
		componentURN, _, _, err := e.registerComponentResource(ctx, componentType, instName, property.NewMap(inputs), componentOpts)
		if err != nil {
			return nil, fmt.Errorf("registering module component %s: %w", instPath.String(), err)
		}
		instCtx, err := newEvalContext(
			e.absolutePaths, modInfo.SourcePath, e.workDir, e.workDir,
			e.stackName, e.projectName, e.organization,
		)
		if err != nil {
			return nil, fmt.Errorf("creating the module evaluation context: %w", err)
		}
		instCtx.SetProviderFunctions(moduleFunctions)
		instCtx.SetModuleName(instName)
		return &moduleInstance{
			Path:       instPath,
			ModuleInfo: modInfo,
			Name:       instName,
			Config:     childMod.Config,
			EvalCtx:    instCtx,
			URN:        componentURN,
			Parent:     parent,
			Index:      index,
			EachKey:    eachKey,
			EachVal:    eachVal,
			Outputs:    make(map[string]cty.Value),
		}, nil
	}

	// No count/for_each: single instance.
	if mod.Count == nil && mod.ForEach == nil {
		inst, err := newInstance(nil, nil, nil)
		if err != nil {
			return nil, err
		}
		return []*moduleInstance{inst}, nil
	}

	if mod.Count != nil {
		count, _, unknown, _, diags := eval.NewEvaluator(parentEvalCtx).EvaluateCount(mod.Count)
		if diags.HasErrors() {
			return nil, fmt.Errorf("evaluating module count: %s", diags.Error())
		}
		if unknown {
			// TODO: support unknown module expansion during preview the way
			// resources do (register no instances, bind outputs to unknown).
			return nil, fmt.Errorf("%s: the count value depends on values that are not yet known", node.Key)
		}
		var instances []*moduleInstance
		for idx := range count {
			inst, err := newInstance(&idx, nil, nil)
			if err != nil {
				return nil, err
			}
			inst.EvalCtx.SetCount(idx)
			instances = append(instances, inst)
		}
		return instances, nil
	}

	forEach, unknown, _, diags := eval.NewEvaluator(parentEvalCtx).EvaluateForEach(mod.ForEach)
	if diags.HasErrors() {
		return nil, fmt.Errorf("evaluating module for_each: %s", diags.Error())
	}
	if unknown {
		// TODO: support unknown module expansion during preview the way
		// resources do (register no instances, bind outputs to unknown).
		return nil, fmt.Errorf("%s: the for_each value depends on values that are not yet known", node.Key)
	}

	var instances []*moduleInstance
	for _, ks := range slices.Sorted(maps.Keys(forEach)) {
		k := cty.StringVal(ks)
		v := forEach[ks]
		inst, err := newInstance(nil, &k, &v)
		if err != nil {
			return nil, err
		}
		inst.EvalCtx.SetEach(k, v)
		instances = append(instances, inst)
	}
	return instances, nil
}

// processModuleOutput evaluates a module output in each instance and stores it in the parent context.
func (e *Engine) processModuleOutput(_ context.Context, node *graph.Node) error {
	output := node.Output
	modInfo := node.ModuleInfo
	outputName := strings.TrimPrefix(node.Key.ID, "output.")

	err := e.forEachModuleInstance(node, func(inst *moduleInstance) error {
		if err := runOutputPreconditions(output, inst.EvalCtx.HCLContext(), outputName); err != nil {
			return err
		}
		val, diags := output.Value.Value(inst.EvalCtx.HCLContext())
		if diags.HasErrors() {
			return fmt.Errorf("evaluating module output %s: %s", outputName, diags.Error())
		}
		// A `sensitive = true` output carries the mark into the calling module,
		// so a reference to it stays sensitive; likewise for `ephemeral = true`.
		if output.Sensitive {
			val = val.Mark(eval.SensitiveMark)
		}
		if output.Ephemeral {
			val = val.Mark(eval.EphemeralMark)
		}
		inst.mu.Lock()
		inst.Outputs[outputName] = val
		inst.mu.Unlock()
		return nil
	})
	if err != nil {
		return err
	}

	// Eagerly publish outputs to the parent contexts so other module variables
	// can reference them before the completion node runs.
	instances, ok := e.moduleInstances.Get(modInfo.Path)
	if !ok {
		return nil
	}
	e.publishModuleValue(modInfo, instances)
	return nil
}

// publishModuleValue assembles the value of `module.<name>` from the
// instances' collected outputs and publishes it into each enclosing module
// instance's eval context (or the root context for a top-level call). Each
// enclosing instance sees only its own instances of the call.
func (e *Engine) publishModuleValue(modInfo *graph.ModuleInfo, instances []*moduleInstance) {
	parents := []*moduleInstance{nil}
	if !modInfo.ParentPath().IsRoot() {
		parentInstances, ok := e.moduleInstances.Get(modInfo.ParentPath())
		if !ok {
			return
		}
		parents = parentInstances
	}

	mod := modInfo.Module
	name := modInfo.ModuleName()
	for _, parent := range parents {
		parentCtx := e.evaluator.Context()
		if parent != nil {
			parentCtx = parent.EvalCtx
		}
		var children []*moduleInstance
		for _, inst := range instances {
			if inst.Parent == parent {
				children = append(children, inst)
			}
		}

		switch {
		case mod.Count != nil:
			tupleVals := make([]cty.Value, len(children))
			for i, inst := range children {
				tupleVals[i] = inst.outputObject()
			}
			if len(tupleVals) > 0 {
				parentCtx.SetModule(name, cty.TupleVal(tupleVals))
			} else {
				parentCtx.SetModule(name, cty.EmptyTupleVal)
			}
		case mod.ForEach != nil:
			mapVals := make(map[string]cty.Value, len(children))
			for _, inst := range children {
				if inst.EachKey == nil {
					continue
				}
				mapVals[inst.EachKey.AsString()] = inst.outputObject()
			}
			if len(mapVals) > 0 {
				parentCtx.SetModule(name, cty.ObjectVal(mapVals))
			} else {
				parentCtx.SetModule(name, cty.EmptyObjectVal)
			}
		default:
			if len(children) == 1 {
				inst := children[0]
				inst.mu.Lock()
				outs := maps.Clone(inst.Outputs)
				inst.mu.Unlock()
				if len(outs) == 0 {
					parentCtx.SetModule(name, cty.EmptyObjectVal)
				}
				for k, v := range outs {
					parentCtx.SetModuleOutput(name, k, v)
				}
			}
		}
	}
}

// processModuleComplete handles the module completion node: registers component outputs
// and assembles the full module value in the parent context.
func (e *Engine) processModuleComplete(ctx context.Context, node *graph.Node) error {
	modInfo := node.ModuleInfo
	if modInfo == nil {
		return fmt.Errorf("module completion node missing ModuleInfo")
	}

	instances, ok := e.moduleInstances.Get(modInfo.Path)
	if !ok {
		return fmt.Errorf("no module instances for %q", graph.ModuleAddress(modInfo.Path))
	}

	// Register component outputs and collect per-instance output objects.
	for _, inst := range instances {
		if e.resmon != nil {
			outputProps := make(map[string]property.Value)
			for k, v := range inst.Outputs {
				pv, err := transform.CtyToPropertyValue(v)
				if err == nil {
					outputProps[k] = pv
				}
			}
			if err := e.resmon.RegisterResourceOutputs(ctx, inst.URN, property.NewMap(outputProps)); err != nil {
				return fmt.Errorf("registering module outputs: %w", err)
			}
		}
	}

	e.publishModuleValue(modInfo, instances)
	return nil
}

// registerComponentResource registers a component (non-custom) resource.
func (e *Engine) registerComponentResource(
	ctx context.Context,
	typeToken string,
	name string,
	inputs property.Map,
	opts *ResourceOptions,
) (urn.URN, string, property.Map, error) {
	if e.resmon == nil {
		urn := urn.New(tokens.QName(e.stackName), tokens.PackageName(e.projectName),
			"", tokens.Type(typeToken), name)
		return urn, "", inputs, nil
	}

	deps := opts.DependsOn
	resp, err := e.resmon.RegisterResource(ctx, RegisterResourceRequest{
		Type:                 typeToken,
		Name:                 name,
		Inputs:               inputs,
		Dependencies:         deps,
		PropertyDependencies: opts.PropertyDependencies,
		Parent:               opts.Parent,
		Protect:              opts.Protect,
		Providers:            opts.Providers,
	})
	if err != nil {
		return "", "", property.Map{}, err
	}

	return resp.URN, resp.ID, resp.Outputs, nil
}

// processOutput processes an output definition.
func (e *Engine) processOutput(_ context.Context, name string, output *ast.Output) error {
	if err := runOutputPreconditions(output, e.evaluator.Context().HCLContext(), name); err != nil {
		return err
	}

	// Evaluate the output value, intercepting can() calls.
	val, diags := e.evaluator.EvaluateExpression(output.Value)
	if diags.HasErrors() {
		return fmt.Errorf("evaluating output value: %s", diags.Error())
	}

	// Root outputs that evaluate to null are removed entirely; nothing can
	// reference a root output, so omitting it is always safe.
	if val.IsNull() {
		return nil
	}

	// Convert to PropertyValue
	pv, err := transform.CtyToPropertyValue(val)
	if err != nil {
		return fmt.Errorf("converting output value: %w", err)
	}

	// Sensitive and ephemeral outputs are both persisted as secrets.
	if output.Sensitive || output.Ephemeral {
		pv = pv.WithSecret(true)
	}

	// Store the output for later registration on the stack
	e.stackOutputs[name] = pv

	return nil
}

// providerRefFromCty extracts a "<urn>::<id>" provider reference from an evaluated
// `provider` attribute value.
//
// The value must be one of:
//   - a resource-outputs object with direct `urn` and `id` string attributes
//     (provider blocks like `aws.west` or pulumi_providers_* resources), or
//   - a resource reference carrying its URN in a resourceMark (e.g., the result
//     of a `call.<resource>.<method>` that returns a provider), with its id in
//     the object's id attribute.
func providerRefFromCty(val cty.Value) (string, error) {
	// Capture the reference's URN before the unmark below strips its resourceMark.
	refURN, isRef := eval.ResourceReferenceURN(val)

	if val.IsMarked() {
		val, _ = val.Unmark()
	}
	if !val.IsKnown() {
		return "", errors.New("provider value is not yet known")
	}
	if val.IsNull() {
		return "", errors.New("provider value is null")
	}
	if !val.Type().IsObjectType() {
		return "", fmt.Errorf("provider value must be an object, got %s", val.Type().FriendlyName())
	}
	if val.Type().HasAttribute("urn") && val.Type().HasAttribute("id") {
		urn := ctyAsString(val.GetAttr("urn"))
		id := providerIDFromCty(val.GetAttr("id"))
		if urn == "" || id == "" {
			return "", fmt.Errorf("provider value urn/id must be non-empty strings, got urn=%q id=%q", urn, id)
		}
		return urn + "::" + id, nil
	}
	if isRef {
		id := ""
		if val.Type().HasAttribute("id") {
			id = providerIDFromCty(val.GetAttr("id"))
		}
		return string(refURN) + "::" + id, nil
	}
	return "", errors.New("provider value is not a resource reference")
}

// providerIDFromCty renders a provider id attribute for use in a provider
// reference. A preview-unknown id becomes the engine's unknown-id sentinel,
// matching how the SDKs reference providers whose id is not yet known.
func providerIDFromCty(v cty.Value) string {
	if v.IsMarked() {
		v, _ = v.Unmark()
	}
	if !v.IsKnown() {
		return plugin.UnknownStringValue
	}
	if v.IsNull() || v.Type() != cty.String {
		return ""
	}
	return v.AsString()
}

// Validate validates an HCL configuration without executing it.
func Validate(config *ast.Config) []error {
	var errs []error

	g, err := graph.BuildFromConfig(config, nil, "")
	if err != nil {
		errs = append(errs, err)
		return errs
	}

	errs = append(errs, g.Validate()...)

	// Additional validation
	// TODO: Type checking, schema validation, etc.

	return errs
}

// checkRuleDeps collects the URNs of the resources referenced by the condition
// and error_message expressions of the given check rules. A reference in a
// precondition/postcondition establishes a dependency, as in TF, even though
// the rules themselves are only evaluated later by hooks. Each referenced
// variable is resolved through the eval context instead of evaluating the
// expression, so no function fires early while values reached through locals
// still carry their DepMarks. Traversals that do not resolve are skipped —
// notably self, which is only in scope once a postcondition hook fires.
func checkRuleDeps(rules []*ast.CheckRule, hclCtx *hcl.EvalContext) []string {
	var deps []string
	for _, rule := range rules {
		for _, expr := range []hcl.Expression{rule.Condition, rule.ErrorMessage} {
			if expr == nil {
				continue
			}
			for _, traversal := range expr.Variables() {
				val, diags := traversal.TraverseAbs(hclCtx)
				if diags.HasErrors() {
					continue
				}
				deps = append(deps, eval.CollectDepURNs(val)...)
			}
		}
	}
	return deps
}

// bodyTraversals returns every variable traversal in body, recursing into
// nested blocks and both halves of an escaped body.
func bodyTraversals(body hcl.Body) []hcl.Traversal {
	if body == nil {
		return nil
	}
	if eb, ok := body.(*ast.EscapedBody); ok {
		return append(bodyTraversals(eb.Base), bodyTraversals(eb.Escape)...)
	}
	var traversals []hcl.Traversal
	attrs, _ := body.JustAttributes()
	for _, attr := range attrs {
		traversals = append(traversals, attr.Expr.Variables()...)
	}
	if syntaxBody, ok := body.(*hclsyntax.Body); ok {
		for _, block := range syntaxBody.Blocks {
			traversals = append(traversals, bodyTraversals(block.Body)...)
		}
	}
	return traversals
}

func provisionerDeps(res *ast.Resource, hclCtx *hcl.EvalContext) []string {
	var deps []string
	collect := func(body hcl.Body) {
		for _, traversal := range bodyTraversals(body) {
			val, diags := traversal.TraverseAbs(hclCtx)
			if diags.HasErrors() {
				continue
			}
			deps = append(deps, eval.CollectDepURNs(val)...)
		}
	}
	if res.Connection != nil {
		collect(res.Connection.Config)
	}
	for _, prov := range res.Provisioners {
		if prov.When == "destroy" {
			continue
		}
		if prov.Connection != nil {
			collect(prov.Connection.Config)
		}
		collect(prov.Config)
	}
	return deps
}

// bindPreconditionHooks registers a single hook that evaluates every
// precondition and binds it to BeforeCreate/BeforeUpdate so a false condition
// blocks the operation. All rules are evaluated in one hook because the engine
// stops at the first failing hook, while every failing rule must be reported.
//
// The HCL eval context is snapshotted at registration so the async callback
// sees the per-instance count/each/self that was in scope here — by the time
// the callback fires, processing has moved on. Other resources' outputs are
// pinned too; graph order guarantees all referenced resources are settled
// by registration time. Unknown values defer (return nil) per TF's
// "known after apply" semantics.
func (e *Engine) bindPreconditionHooks(
	ctx context.Context,
	res *ast.Resource,
	instance *graph.ExpandedResource,
	evalCtx *eval.Context,
	opts *ResourceOptions,
	resourceName string,
) error {
	if len(res.Preconditions) == 0 {
		return nil
	}
	if opts.Hooks == nil {
		opts.Hooks = &ResourceHookBinding{}
	}
	hclSnapshot := evalCtx.HCLContext()
	hookName := fmt.Sprintf("%s.%s:precondition", res.Type, resourceName)
	callback := func(_ context.Context, _ *ResourceHookArgs) error {
		return evaluatePreconditions(res.Preconditions, hclSnapshot, instance.Key.String())
	}
	if err := e.resmon.RegisterResourceHook(ctx, hookName, callback, ResourceHookOptions{
		OnDryRun: true,
	}); err != nil {
		return fmt.Errorf("registering precondition hook: %w", err)
	}
	opts.Hooks.BeforeCreate = append(opts.Hooks.BeforeCreate, hookName)
	opts.Hooks.BeforeUpdate = append(opts.Hooks.BeforeUpdate, hookName)
	return nil
}

// bindPostconditionHooks registers a single hook that evaluates every
// postcondition and binds it to AfterCreate/AfterUpdate. The hook fires after
// the resource is created or updated; the engine supplies the resource's new
// outputs as `self` in the callback. All rules are evaluated in one hook
// because the engine stops at the first failing hook, while every failing rule
// must be reported. Failed postconditions surface as deployment errors but do
// not unwind the resource registration — same as TF.
func (e *Engine) bindPostconditionHooks(
	ctx context.Context,
	res *ast.Resource,
	resSchema *schema.Resource,
	mapping *bridge.BodyMapping,
	instance *graph.ExpandedResource,
	evalCtx *eval.Context,
	opts *ResourceOptions,
	resourceName string,
) error {
	if len(res.Postconditions) == 0 {
		return nil
	}
	if opts.Hooks == nil {
		opts.Hooks = &ResourceHookBinding{}
	}
	hclSnapshot := evalCtx.HCLContext()
	dryRun := e.dryRun
	hookName := fmt.Sprintf("%s.%s:postcondition", res.Type, resourceName)
	callback := func(_ context.Context, args *ResourceHookArgs) error {
		// Hooks receive raw engine outputs, so terraform_data's surface is
		// adapted the same way as on the registration path: property-level
		// lowering here, wrapper unboxing after re-expansion.
		outputs := lowerTerraformDataOutputs(res.Type, args.NewOutputs, opts)
		return evaluatePostconditions(res.Postconditions, hclSnapshot, outputs, res.Type, resSchema, mapping, dryRun, instance.Key.String())
	}
	if err := e.resmon.RegisterResourceHook(ctx, hookName, callback, ResourceHookOptions{
		OnDryRun: true,
	}); err != nil {
		return fmt.Errorf("registering postcondition hook: %w", err)
	}
	opts.Hooks.AfterCreate = append(opts.Hooks.AfterCreate, hookName)
	opts.Hooks.AfterUpdate = append(opts.Hooks.AfterUpdate, hookName)
	return nil
}

// evaluatePostconditions evaluates every postcondition with `self` bound to
// the engine-supplied NewOutputs, joining the failures so every failing rule
// is reported.
func evaluatePostconditions(
	rules []*ast.CheckRule, hclCtx *hcl.EvalContext, newOutputs property.Map, tfType string,
	resSchema *schema.Resource, mapping *bridge.BodyMapping, dryRun bool, resourceName string,
) error {
	outputObj, err := transform.ResourceOutputToCty(newOutputs, resSchema, mapping, dryRun)
	if err != nil {
		return fmt.Errorf("converting outputs for postconditions on %s: %w", resourceName, err)
	}
	if err := unwrapTerraformDataOutputs(tfType, outputObj, newOutputs); err != nil {
		return fmt.Errorf("converting outputs for postconditions on %s: %w", resourceName, err)
	}
	return evaluatePostconditionValues(rules, hclCtx, cty.ObjectVal(outputObj), resourceName)
}

// evaluatePostconditionValues evaluates every postcondition with `self` bound
// to the given value, joining the failures so every failing rule is reported.
func evaluatePostconditionValues(
	rules []*ast.CheckRule, hclCtx *hcl.EvalContext, self cty.Value, resourceName string,
) error {
	var errs []error
	for i, rule := range rules {
		errs = append(errs, evaluatePostconditionValue(rule, hclCtx, self, i+1, resourceName))
	}
	return errors.Join(errs...)
}

// evaluatePostconditionValue evaluates a postcondition with `self` bound to
// the given value. Unknown conditions defer (return nil).
func evaluatePostconditionValue(
	rule *ast.CheckRule, hclCtx *hcl.EvalContext, self cty.Value, index int, resourceName string,
) error {
	selfCtx := hclCtx.NewChild()
	selfCtx.Variables = map[string]cty.Value{"self": self}
	condVal, diags := rule.Condition.Value(selfCtx)
	if diags.HasErrors() {
		return fmt.Errorf("evaluating postcondition %d for %s: %s", index, resourceName, diags.Error())
	}
	condVal, _ = condVal.Unmark()
	if !condVal.IsKnown() {
		return nil
	}
	ok, err := conditionResultToBool(condVal)
	if err != nil {
		return fmt.Errorf("postcondition %d for %s: %s", index, resourceName, err)
	}
	if ok {
		return nil
	}
	msgVal, msgDiags := rule.ErrorMessage.Value(selfCtx)
	if msgDiags.HasErrors() {
		return fmt.Errorf("postcondition %d for %s failed (could not evaluate error message: %s)",
			index, resourceName, msgDiags.Error())
	}
	msg := "postcondition check failed"
	if s := renderErrorMessage(msgVal); s != "" {
		msg = s
	}
	return fmt.Errorf("postcondition for %s: %s", resourceName, msg)
}

// conditionResultToBool mirrors OpenTofu's handling of a condition result
// (variable validation, precondition, or postcondition). OpenTofu converts the
// result to bool, so any value convertible to bool — notably the strings
// "true"/"false" — is a valid condition value, not a type error. A null result
// is rejected, matching OpenTofu's "must return either true or false, not null".
func conditionResultToBool(v cty.Value) (bool, error) {
	converted, err := ctyconvert.Convert(v, cty.Bool)
	if err != nil {
		return false, fmt.Errorf("condition must be a boolean: %s", err)
	}
	if converted.IsNull() {
		return false, fmt.Errorf("condition must return either true or false, not null")
	}
	return converted.True(), nil
}

// runOutputPreconditions evaluates an output's `precondition` rules against
// hclCtx, reporting every rule whose condition is known and false; unknown
// conditions are deferred.
func runOutputPreconditions(output *ast.Output, hclCtx *hcl.EvalContext, name string) error {
	return evaluatePreconditions(output.Preconditions, hclCtx, fmt.Sprintf("output %q", name))
}

// evaluatePreconditions evaluates every rule and joins the failures so every
// failing rule is reported, not just the first.
func evaluatePreconditions(rules []*ast.CheckRule, hclCtx *hcl.EvalContext, resourceName string) error {
	var errs []error
	for i, rule := range rules {
		errs = append(errs, evaluatePrecondition(rule, hclCtx, i+1, resourceName))
	}
	return errors.Join(errs...)
}

// evaluatePrecondition returns nil when the rule holds or its condition is
// unknown (deferred), or a formatted error when it fails.
func evaluatePrecondition(rule *ast.CheckRule, hclCtx *hcl.EvalContext, index int, resourceName string) error {
	condVal, diags := rule.Condition.Value(hclCtx)
	if diags.HasErrors() {
		return fmt.Errorf("evaluating precondition %d for %s: %s", index, resourceName, diags.Error())
	}
	condVal, _ = condVal.Unmark()
	if !condVal.IsKnown() {
		return nil
	}
	ok, err := conditionResultToBool(condVal)
	if err != nil {
		return fmt.Errorf("precondition %d for %s: %s", index, resourceName, err)
	}
	if ok {
		return nil
	}
	msgVal, msgDiags := rule.ErrorMessage.Value(hclCtx)
	if msgDiags.HasErrors() {
		return fmt.Errorf("precondition %d for %s failed (could not evaluate error message: %s)",
			index, resourceName, msgDiags.Error())
	}
	msg := "precondition check failed"
	if s := renderErrorMessage(msgVal); s != "" {
		msg = s
	}
	return fmt.Errorf("precondition for %s: %s", resourceName, msg)
}

// evaluateChecks evaluates every check block after all resources have settled.
// Checks declared in child modules are evaluated once per module instance, in
// that instance's scope. Both scoped-data-source errors and failed assertions
// emit warnings and processing continues, matching Terraform: check blocks are
// the only custom condition that does not block the operation.
func (e *Engine) evaluateChecks(ctx context.Context) error {
	contract.Assertf(e.resmon != nil, "e.resmon cannot be nil")
	var errs []error
	errs = append(errs, e.evaluateConfigChecks(ctx, "", e.config, e.evaluator.Context()))
	var instances []*moduleInstance
	for insts := range e.moduleInstances.Values() {
		instances = append(instances, insts...)
	}
	slices.SortFunc(instances, func(a, b *moduleInstance) int {
		return modulepath.Compare(a.Path, b.Path)
	})
	for _, inst := range instances {
		errs = append(errs, e.evaluateConfigChecks(
			ctx, graph.ModuleAddress(inst.Path)+".", inst.Config, inst.EvalCtx))
	}
	return errors.Join(errs...)
}

// evaluateConfigChecks evaluates the check blocks of one config in the scope
// given by evalCtx. addrPrefix qualifies the reported check name with its
// module instance address ("" for the root config).
func (e *Engine) evaluateConfigChecks(
	ctx context.Context, addrPrefix string, config *ast.Config, evalCtx *eval.Context,
) error {
	names := slices.Collect(maps.Keys(config.Checks))
	slices.Sort(names)
	var errs []error
	for _, name := range names {
		errs = append(errs, e.evaluateCheck(ctx, addrPrefix+name, config.Checks[name], evalCtx))
	}
	return errors.Join(errs...)
}

// evaluateCheck evaluates one check block. Its scoped data source is read into a
// cloned context so it is visible only to this check's assertions and cannot
// collide with a top-level data source of the same address.
func (e *Engine) evaluateCheck(ctx context.Context, name string, check *ast.Check, evalCtx *eval.Context) error {
	var errs []error
	if ds := check.DataResource; ds != nil {
		evalCtx = evalCtx.Clone()
		if err := e.readScopedDataSource(ctx, ds, evalCtx); err != nil {
			// A scoped data source error is masked as a warning, matching TF.
			errs = append(errs, e.warnf(ctx, "check %q data %q.%q: %s", name, ds.Type, ds.Name, err))
		}
	}
	evaluator := eval.NewEvaluator(evalCtx)
	for _, assert := range check.Asserts {
		if msg := evaluateCheckAssert(evaluator, assert); msg != "" {
			errs = append(errs, e.warnf(ctx, "check %q assertion failed: %s", name, msg))
		}
	}
	return errors.Join(errs...)
}

// readScopedDataSource reads a check's scoped data source into evalCtx, making
// it available as data.<type>.<name> within that context only. Scoped data
// sources are single reads: they run after the walk, synchronously, outside
// the expansion-cell machinery.
func (e *Engine) readScopedDataSource(ctx context.Context, ds *ast.DataSource, evalCtx *eval.Context) error {
	node := &graph.Node{
		Key:        graph.NodeKey{ID: "data." + ast.ResourceKey(ds.Type, ds.Name)},
		Type:       graph.NodeTypeDataSource,
		DataSource: ds,
	}
	funcSchema, err := e.resolver.ResolveFunction(ctx, ds.Type)
	if err != nil {
		if diag := unknownTokenDiag("data source", ds.TypeRange, err); diag != err {
			return diag
		}
		return fmt.Errorf("resolving data source type %s: %w", ds.Type, err)
	}
	ctyOutputs, err := e.invokeDataSourceOnce(ctx, node, ds, funcSchema, evalCtx, nil)
	if err != nil {
		return err
	}
	evalCtx.SetDataSource(ast.ResourceKey(ds.Type, ds.Name), ctyOutputs)
	return nil
}

// warnf emits a best-effort warning diagnostic. A transport failure emitting it
// must not turn a non-blocking check into a failed operation.
func (e *Engine) warnf(ctx context.Context, format string, args ...any) error {
	return e.resmon.LogWarning(ctx, fmt.Sprintf(format, args...))
}

// evaluateCheckAssert evaluates a single check assertion. It returns the
// assertion's error message when the condition is known and false (or its
// condition cannot be evaluated, which Terraform masks as a warning), and ""
// when the assertion holds or its condition is not yet known.
func evaluateCheckAssert(evaluator *eval.Evaluator, rule *ast.CheckRule) string {
	condVal, diags := evaluator.EvaluateExpression(rule.Condition)
	if diags.HasErrors() {
		return fmt.Sprintf("could not evaluate condition: %s", diags.Error())
	}
	condVal, _ = condVal.Unmark()
	if !condVal.IsKnown() {
		return ""
	}
	ok, err := conditionResultToBool(condVal)
	if err != nil {
		return err.Error()
	}
	if ok {
		return ""
	}
	msgVal, msgDiags := evaluator.EvaluateExpression(rule.ErrorMessage)
	if msgDiags.HasErrors() {
		return "assertion failed (could not evaluate error message)"
	}
	if msg := renderErrorMessage(msgVal); msg != "" {
		return msg
	}
	return "assertion failed"
}

// checkModulePulumiVersion enforces a child module's declared version
// constraints. The module's own scope does not exist yet when its call is
// initialized, so the constraint expressions evaluate statically.
func (e *Engine) checkModulePulumiVersion(ctx context.Context, source string, config *ast.Config) error {
	err := e.checkConfigPulumiVersion(ctx, config, func(expr hcl.Expression) (cty.Value, hcl.Diagnostics) {
		return expr.Value(nil)
	})
	if err != nil {
		return fmt.Errorf("module %s: %w", source, err)
	}
	return nil
}

func (e *Engine) checkConfigPulumiVersion(
	ctx context.Context, config *ast.Config,
	evalExpr func(hcl.Expression) (cty.Value, hcl.Diagnostics),
) error {
	var exprs []hcl.Expression
	if tf := config.Terraform; tf != nil && tf.RequiredVersionRange != nil {
		exprs = append(exprs, tf.RequiredVersionRange)
	}
	if l := config.Language; l != nil {
		exprs = append(exprs, l.CompatibleWithPulumi)
	}

	for _, expr := range exprs {
		versionVal, diags := evalExpr(expr)
		if diags.HasErrors() {
			return fmt.Errorf("evaluating Pulumi version constraint: %s", diags.Error())
		}
		if versionVal.Type() != cty.String {
			return fmt.Errorf("the Pulumi version constraint must be a string, got %s",
				versionVal.Type().FriendlyName())
		}
		versionRange := versionVal.AsString()
		if versionRange == "" {
			continue
		}
		if err := e.resmon.CheckPulumiVersion(ctx, versionRange); err != nil {
			return err
		}
	}
	return nil
}

// ctyAsString reads a cty value as a string, tolerating marks (resource
// output leaves carry DepMarks) and returning "" for null / unknown /
// non-string. Use the cty API directly if you need to distinguish those.
// resourceURNsFromValue extracts the URNs of the resource instances val
// represents when it is a whole-resource value: a single instance carries a
// resource reference mark, while a reference to a resource with count or
// for_each yields a tuple or object of such instance values. An attribute of a
// resource inherits the mark but not its hash, so it yields no URNs.
func resourceURNsFromValue(val cty.Value) []string {
	if u, ok := eval.ResourceReferenceURN(val); ok {
		return []string{string(u)}
	}
	val, _ = val.Unmark()
	if val.IsNull() || !val.IsKnown() || !val.CanIterateElements() {
		return nil
	}
	var urns []string
	for it := val.ElementIterator(); it.Next(); {
		_, el := it.Element()
		if u, ok := eval.ResourceReferenceURN(el); ok {
			urns = append(urns, string(u))
		}
	}
	return urns
}

func ctyAsString(v cty.Value) string {
	if v.IsMarked() {
		v, _ = v.Unmark()
	}
	if v.IsNull() || !v.IsKnown() || v.Type() != cty.String {
		return ""
	}
	return v.AsString()
}

// sensitiveErrorMessageRef is the text substituted for a custom-condition
// error_message that interpolates a sensitive value. Rendering the real message
// would leak the secret, so the reference is reported instead.
const sensitiveErrorMessageRef = "Error message refers to sensitive values"

// renderErrorMessage reads a custom-condition error_message (variable
// validation, precondition, postcondition, or check assertion). It refuses to
// render a message that carries the sensitive mark — returning
// sensitiveErrorMessageRef rather than the interpolated secret — while still
// tolerating the non-sensitive marks (e.g. DepMarks) that resource outputs
// carry. A null / unknown / non-string value yields "" so the caller can fall
// back to its own default message.
func renderErrorMessage(v cty.Value) string {
	if v.HasMark(eval.SensitiveMark) || v.HasMark(eval.EphemeralMark) {
		return sensitiveErrorMessageRef
	}
	return ctyAsString(v)
}
