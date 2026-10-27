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

// Package converter converts HCL programs to PCL (Pulumi Configuration Language).
package converter

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/pulumi/pulumi-hcl/pkg/hcl/ast"
	"github.com/pulumi/pulumi-hcl/pkg/hcl/bridge"
	"github.com/pulumi/pulumi-hcl/pkg/hcl/comments"
	"github.com/pulumi/pulumi-hcl/pkg/hcl/packages"
	"github.com/pulumi/pulumi-hcl/pkg/hcl/parser"
	"github.com/pulumi/pulumi-hcl/pkg/hcl/transform"
	"github.com/pulumi/pulumi/pkg/v3/codegen"
	"github.com/pulumi/pulumi/pkg/v3/codegen/convert"
	"github.com/pulumi/pulumi/pkg/v3/codegen/schema"
	"github.com/pulumi/pulumi/pkg/v3/resource/plugin"
	"github.com/pulumi/pulumi/sdk/v3/go/common/util/contract"
	"github.com/pulumi/pulumi/sdk/v3/go/common/workspace"
	"github.com/zclconf/go-cty/cty"
	"gopkg.in/yaml.v3"
)

type hclConverter struct {
	projectDir string
}

// New returns a new HCL->PCL converter.
func New() plugin.Converter { return &hclConverter{} }

// NewInDir returns a converter for the project rooted at dir. [New] locates
// the project from the working directory instead, which is what the plugin
// binary needs; NewInDir is for callers that already know where it is.
func NewInDir(dir string) plugin.Converter { return &hclConverter{projectDir: dir} }

func (*hclConverter) Close() error { return nil }

func (*hclConverter) ConvertProgram(
	ctx context.Context, req *plugin.ConvertProgramRequest,
) (*plugin.ConvertProgramResponse, error) {
	if req.LoaderTarget == "" {
		return nil, fmt.Errorf("missing loader address")
	}
	if req.MapperTarget == "" {
		return nil, errors.New("missing mapper address")
	}
	mapperClient, err := convert.NewMapperClient(req.MapperTarget)
	if err != nil {
		return nil, fmt.Errorf("dial mapper at %s: %w", req.MapperTarget, err)
	}
	providerInfoSource := bridge.NewCache(bridge.NewMapperSource(mapperClient))

	client, err := schema.NewLoaderClient(req.LoaderTarget)
	if err != nil {
		return nil, fmt.Errorf("creating loader client: %w", err)
	}
	defer contract.IgnoreClose(client)

	paramInfos, err := readParameterizationInfos(req.SourceDirectory)
	if err != nil {
		return nil, fmt.Errorf("reading parameterization infos: %w", err)
	}

	loader := schema.ReferenceLoader(schema.NewCachedLoader(client))
	if len(paramInfos) > 0 {
		loader = packages.NewParameterizationAwareLoader(loader, paramInfos)
	}

	// Read source Pulumi.yaml, extract project name, write target Pulumi.yaml with runtime: pcl.
	pulumiYAMLBytes, err := os.ReadFile(filepath.Join(req.SourceDirectory, "Pulumi.yaml"))
	if err != nil {
		return nil, fmt.Errorf("reading Pulumi.yaml: %w", err)
	}

	var project map[string]any
	if err := yaml.Unmarshal(pulumiYAMLBytes, &project); err != nil {
		return nil, fmt.Errorf("parsing Pulumi.yaml: %w", err)
	}
	project["runtime"] = "pcl"

	targetYAML, err := yaml.Marshal(project)
	if err != nil {
		return nil, fmt.Errorf("marshaling Pulumi.yaml: %w", err)
	}

	if err := os.MkdirAll(req.TargetDirectory, 0o755); err != nil {
		return nil, fmt.Errorf("creating target directory: %w", err)
	}

	if err := os.WriteFile(filepath.Join(req.TargetDirectory, "Pulumi.yaml"), targetYAML, 0o644); err != nil {
		return nil, fmt.Errorf("writing Pulumi.yaml: %w", err)
	}

	// Collect every .tf file in the source tree.
	type hclSource struct {
		relPath string
		content []byte
	}
	var sources []hclSource
	err = filepath.WalkDir(req.SourceDirectory, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			if strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".tf") {
			return nil
		}
		relPath, err := filepath.Rel(req.SourceDirectory, path)
		if err != nil {
			return fmt.Errorf("computing relative path for %s: %w", path, err)
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("reading %s: %w", relPath, err)
		}
		sources = append(sources, hclSource{relPath: relPath, content: content})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walking source directory: %w", err)
	}

	// Sort for deterministic phase ordering across files.
	sort.Slice(sources, func(i, j int) bool { return sources[i].relPath < sources[j].relPath })

	// Parse every file up front so the project transformer sees the full symbol table.
	type parsedHCL struct {
		relPath string
		content []byte
		body    *hclsyntax.Body
	}
	var allDiags hcl.Diagnostics
	parsed := make([]parsedHCL, 0, len(sources))
	bodies := make([]*hclsyntax.Body, 0, len(sources))
	contents := make(map[string][]byte, len(sources))
	for _, s := range sources {
		f, parseDiags := hclsyntax.ParseConfig(s.content, s.relPath, hcl.Pos{Line: 1, Column: 1})
		if parseDiags.HasErrors() {
			return &plugin.ConvertProgramResponse{Diagnostics: parseDiags}, nil
		}
		body, ok := f.Body.(*hclsyntax.Body)
		if !ok {
			continue
		}
		parsed = append(parsed, parsedHCL{relPath: s.relPath, content: s.content, body: body})
		bodies = append(bodies, body)
		contents[s.relPath] = s.content
	}

	ft, scanDiags := newProjectTransformer(ctx, loader, providerInfoSource, paramInfos, bodies, contents)
	allDiags = append(allDiags, scanDiags...)
	if scanDiags.HasErrors() {
		return &plugin.ConvertProgramResponse{Diagnostics: allDiags}, nil
	}

	for _, p := range parsed {
		out := hclwrite.NewEmptyFile()
		// Only emit package blocks for root-level files.
		var pi map[string]workspace.PackageDescriptor
		if filepath.Dir(p.relPath) == "." {
			pi = paramInfos
		}
		diags, emitErr := ft.emitFile(ctx, p.content, p.relPath, p.body, out.Body(), pi)
		if emitErr != nil {
			return nil, emitErr
		}
		allDiags = append(allDiags, diags...)

		dstRelPath := strings.TrimSuffix(p.relPath, ".tf") + ".pp"
		dstPath := filepath.Join(req.TargetDirectory, dstRelPath)
		if err := os.MkdirAll(filepath.Dir(dstPath), 0o755); err != nil {
			return nil, fmt.Errorf("creating directory for %s: %w", dstRelPath, err)
		}
		dstFile, err := os.Create(dstPath)
		if err != nil {
			return nil, fmt.Errorf("opening %s: %w", dstRelPath, err)
		}
		if _, err := out.WriteTo(dstFile); err != nil {
			contract.IgnoreClose(dstFile)
			return nil, fmt.Errorf("writing to %s: %w", dstRelPath, err)
		}
		contract.IgnoreClose(dstFile)
	}

	return &plugin.ConvertProgramResponse{Diagnostics: allDiags}, nil
}

func (*hclConverter) ConvertSnippet(ctx context.Context, req *plugin.ConvertSnippetRequest) (*plugin.ConvertSnippetResponse, error) {
	// TODO[https://github.com/pulumi/pulumi-hcl/issues/151]: Implement
	return nil, plugin.ErrNotYetImplemented
}

// callReference identifies a call block by resource name and method name.
type callReference struct {
	resourceName string
	methodName   string
}

// dataReference identifies a data block by HCL type and logical name.
type dataReference struct {
	hclType string
	name    string
}

// fileTransformer holds context for converting a single HCL file to PCL.
type fileTransformer struct {
	// sources maps each contributing source filename to its raw bytes so that
	// expression byte slicing (e.g. for notImplemented() fall-throughs) works
	// correctly when expressions from one file are inlined into another.
	sources        map[string][]byte
	knownHCLTypes  map[string]bool // set of HCL type labels used in resource blocks
	stackRefNames  map[string]bool // set of logical names of pulumi_stackreference resources
	knownProviders []string        // provider names from pulumi.required_providers
	// providerAliases is the set of "<pkg>.<alias>" pairs declared by
	// `provider` blocks; used to detect provider references in traversals.
	providerAliases map[string]bool
	callBlocks      map[callReference]*hclsyntax.Body
	dataBlocks      map[dataReference]*hclsyntax.Body
	dataTokens      map[string]string // key: hclType, value: resolved PCL token
	loader          schema.ReferenceLoader
	resolver        *packages.Resolver
	resourceSchemas map[string]*schema.Resource // cache: HCL type label → resolved schema resource
	functionSchemas map[string]*schema.Function // cache: HCL type label → resolved schema function
	// resourceMappings and functionMappings hold the bridge mapping of each
	// entry of resourceSchemas and functionSchemas, or nil when it has none.
	resourceMappings map[string]*bridge.BodyMapping
	functionMappings map[string]*bridge.BodyMapping
	// providerFunctions maps a provider-defined function call name
	// ("provider::<local>::<fn>") to its resolved schema; a nil value marks a
	// name that could not be resolved so it falls back to notImplemented.
	providerFunctions map[string]*schema.Function
	nameRewrites      map[string]string // logical name → sanitized PCL identifier (only for invalid names)
	// dataPlans records, for each data block, if its invoke is inlined or
	// hoisted to a local.
	dataPlans map[dataReference]*dataPlan
	// rangeSubst is the substitution for each.* and count.* while the body
	// of a data block is emitted, or nil when they convert to range.*.
	rangeSubst *rangeSubst
	// comments holds leading comments lexed from the source file, keyed by
	// the source start byte of the syntactic element that should carry them.
	comments *comments.Map
}

// emitLeadingComments writes any source comments that immediately precede the
// element at the given source start byte to body.
func (ft *fileTransformer) emitLeadingComments(body *hclwrite.Body, startByte int) {
	ft.comments.Emit(body, startByte)
}

// srcBytes returns the raw bytes of the source file r came from. Using r's
// own filename (rather than the active file's bytes) is essential when
// expressions from one file are inlined into another.
func (ft *fileTransformer) srcBytes(r hcl.Range) []byte {
	src := ft.sources[r.Filename]
	if src == nil {
		return nil
	}
	return src[r.Start.Byte:r.End.Byte]
}

// withTrailing returns valueTokens with the same-line trailing comment for the
// attribute at startByte (if any) appended, ready to be passed to
// hclwrite.Body.SetAttributeRaw so the comment stays on the same line.
func (ft *fileTransformer) withTrailing(valueTokens hclwrite.Tokens, startByte int) hclwrite.Tokens {
	return ft.comments.AppendTrailing(valueTokens, startByte)
}

// newProjectTransformer creates a fileTransformer whose symbol tables are
// populated from every body in the project. sources maps the filename of each
// body to its raw bytes.
func newProjectTransformer(
	ctx context.Context, loader schema.ReferenceLoader, providerInfoSource bridge.ProviderInfoSource,
	pkgs map[string]workspace.PackageDescriptor, bodies []*hclsyntax.Body, sources map[string][]byte,
) (*fileTransformer, hcl.Diagnostics) {
	ft := &fileTransformer{
		sources:           sources,
		knownHCLTypes:     make(map[string]bool),
		stackRefNames:     make(map[string]bool),
		callBlocks:        make(map[callReference]*hclsyntax.Body),
		dataBlocks:        make(map[dataReference]*hclsyntax.Body),
		dataPlans:         make(map[dataReference]*dataPlan),
		dataTokens:        make(map[string]string),
		loader:            loader,
		resourceSchemas:   make(map[string]*schema.Resource),
		resourceMappings:  make(map[string]*bridge.BodyMapping),
		functionMappings:  make(map[string]*bridge.BodyMapping),
		nameRewrites:      make(map[string]string),
		functionSchemas:   make(map[string]*schema.Function),
		providerFunctions: make(map[string]*schema.Function),
		providerAliases:   make(map[string]bool),
	}
	// Phase 1: collect pulumi.required_providers across every body so that
	// later resolution sees the full set regardless of which file declared it.
	for _, body := range bodies {
		ft.scanProviders(body)
	}
	ft.resolver = packages.NewResolver(loader, providerInfoSource, pkgs, ft.knownProviders)
	// Phase 2: scan symbols (resources, data, call) using the merged provider set.
	var diags hcl.Diagnostics
	for _, body := range bodies {
		diags = append(diags, ft.scanSymbols(ctx, body)...)
	}
	// Phase 2.5: resolve provider-defined function calls (provider::<local>::<fn>)
	// that appear anywhere in expressions, so they eject as positional invokes.
	for _, body := range bodies {
		diags = append(diags, ft.scanProviderFunctions(ctx, body)...)
	}
	// Phase 3: compute name rewrites against the union of declarations.
	ft.computeNameRewrites(bodies)
	// Phase 4: decide which data blocks are inlined and which are hoisted to
	// locals. This needs the name rewrites to pick unused local names.
	ft.planDataBlocks(bodies)
	return ft, diags
}

func (ft *fileTransformer) scanProviders(body *hclsyntax.Body) {
	seen := make(map[string]bool, len(ft.knownProviders))
	for _, name := range ft.knownProviders {
		seen[name] = true
	}
	for _, block := range body.Blocks {
		if block.Type != "terraform" {
			continue
		}
		for _, sub := range block.Body.Blocks {
			if sub.Type != "required_providers" {
				continue
			}
			for name := range sub.Body.Attributes {
				if seen[name] {
					continue
				}
				seen[name] = true
				ft.knownProviders = append(ft.knownProviders, name)
			}
		}
	}
}

func (ft *fileTransformer) scanSymbols(ctx context.Context, body *hclsyntax.Body) hcl.Diagnostics {
	var diags hcl.Diagnostics
	for _, block := range body.Blocks {
		if block.Type == "resource" && len(block.Labels) >= 1 {
			ft.knownHCLTypes[block.Labels[0]] = true
			if block.Labels[0] == "pulumi_stackreference" && len(block.Labels) >= 2 {
				ft.stackRefNames[block.Labels[1]] = true
			}
		}
		if block.Type == "provider" && len(block.Labels) >= 1 {
			alias := block.Labels[0]
			if attr, ok := block.Body.Attributes["alias"]; ok {
				if val, vdiags := attr.Expr.Value(nil); !vdiags.HasErrors() && val.Type() == cty.String {
					alias = val.AsString()
				}
			}
			ft.providerAliases[block.Labels[0]+"."+alias] = true
		}
		if block.Type == "call" && len(block.Labels) == 2 {
			ft.callBlocks[callReference{block.Labels[0], block.Labels[1]}] = block.Body
		}
		if block.Type == "data" && len(block.Labels) == 2 {
			hclType := block.Labels[0]
			name := block.Labels[1]
			ft.dataBlocks[dataReference{hclType, name}] = block.Body
			if _, seen := ft.dataTokens[hclType]; !seen {
				fn, err := ft.resolver.ResolveFunction(ctx, hclType)
				if err != nil {
					diags = append(diags, &hcl.Diagnostic{
						Severity: hcl.DiagError,
						Summary:  "unknown data source type",
						Detail:   fmt.Sprintf("cannot convert HCL type %q to PCL token: %v", hclType, err),
						Subject:  block.TypeRange.Ptr(),
					})
					ft.dataTokens[hclType] = ""
				} else {
					ft.dataTokens[hclType] = fn.Token
					ft.functionSchemas[hclType] = fn
					ft.functionMappings[hclType] = ft.resolver.DataSourceBodyMapping(ctx, hclType)
				}
			}
		}
	}
	return diags
}

// scanProviderFunctions walks every expression in body, resolving each
// provider-defined function call (provider::<local>::<fn>) to its Pulumi schema
// so transformExpr can eject it as a positional invoke.
func (ft *fileTransformer) scanProviderFunctions(ctx context.Context, body *hclsyntax.Body) hcl.Diagnostics {
	var diags hcl.Diagnostics
	_ = hclsyntax.VisitAll(body, func(n hclsyntax.Node) hcl.Diagnostics {
		call, ok := n.(*hclsyntax.FunctionCallExpr)
		if !ok {
			return nil
		}
		local, fnName, ok := ast.ParseProviderFunctionName(call.Name)
		if !ok {
			return nil
		}
		if _, seen := ft.providerFunctions[call.Name]; seen {
			return nil
		}
		// The provider function's Pulumi token resolves from the same
		// "<local>_<fn>" HCL form that data sources use.
		fn, err := ft.resolver.ResolveFunction(ctx, local+"_"+fnName)
		if err != nil {
			diags = append(diags, &hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  "unknown provider function",
				Detail:   fmt.Sprintf("cannot convert %q to a PCL invoke: %v", call.Name, err),
				Subject:  call.Range().Ptr(),
			})
			ft.providerFunctions[call.Name] = nil
			return nil
		}
		ft.providerFunctions[call.Name] = fn
		return nil
	})
	return diags
}

// blockLogicalName returns the referenceable name label of a resource,
// variable, output, or module block, or "" for other block types.
func blockLogicalName(block *hclsyntax.Block) string {
	switch block.Type {
	case "resource":
		if len(block.Labels) >= 2 {
			return block.Labels[1]
		}
	case "variable", "output", "module":
		if len(block.Labels) >= 1 {
			return block.Labels[0]
		}
	}
	return ""
}

func (ft *fileTransformer) computeNameRewrites(bodies []*hclsyntax.Body) {
	usedNames := make(map[string]bool)
	// First pass: collect all names that are already valid identifiers.
	for _, body := range bodies {
		for _, block := range body.Blocks {
			if name := blockLogicalName(block); name != "" && hclsyntax.ValidIdentifier(name) {
				usedNames[name] = true
			}
		}
	}
	// Second pass: generate sanitized names for invalid identifiers.
	for _, body := range bodies {
		for _, block := range body.Blocks {
			// Outputs are never referenced in expressions, so they don't need rewriting.
			if block.Type == "output" {
				continue
			}
			name := blockLogicalName(block)
			if name == "" || hclsyntax.ValidIdentifier(name) {
				continue
			}
			if _, seen := ft.nameRewrites[name]; seen {
				continue
			}
			sanitized := sanitizeIdentifier(name)
			for usedNames[sanitized] {
				sanitized = sanitized + "_"
			}
			usedNames[sanitized] = true
			ft.nameRewrites[name] = sanitized
		}
	}
}

// sanitizeIdentifier converts a string into a valid HCL identifier by replacing
// invalid characters with underscores.
func sanitizeIdentifier(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || r == '_':
			b.WriteRune(r)
		case r >= '0' && r <= '9':
			if b.Len() == 0 {
				b.WriteRune('_')
			}
			b.WriteRune(r)
		case r == '-':
			if b.Len() == 0 {
				b.WriteRune('_')
			}
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	if b.Len() == 0 {
		return "_"
	}
	return b.String()
}

// resolveHCLType resolves an HCL resource type label (e.g., "pulumi_stash") to a schema
// resource, caching the result for subsequent calls.
func (ft *fileTransformer) resolveHCLType(ctx context.Context, hclType string) (*schema.Resource, error) {
	if res, ok := ft.resourceSchemas[hclType]; ok {
		return res, nil
	}
	res, err := ft.resolver.ResolveResource(ctx, hclType)
	if err != nil {
		return nil, fmt.Errorf("resolving resource type %q: %w", hclType, err)
	}
	ft.resourceSchemas[hclType] = res
	ft.resourceMappings[hclType] = ft.resolver.ResourceBodyMapping(ctx, hclType)
	return res, nil
}

// invokeOptionHCLToPCL maps HCL data-block attribute names that represent invoke
// options (written by genInvokeOptions) to their PCL invoke-options object keys.
var invokeOptionHCLToPCL = map[string]string{
	"depends_on":          "dependsOn",
	"parent":              "parent",
	"plugin_download_url": "pluginDownloadUrl",
	"provider":            "provider",
	"version":             "version",
}

// resourceOptionHCLToPCL maps HCL attribute names (written by genResourceOptions)
// to the corresponding PCL options-block attribute names (camelCase).
var resourceOptionHCLToPCL = map[string]string{
	"aliases":                   "aliases",
	"additional_secret_outputs": "additionalSecretOutputs",
	"count":                     "range",
	"deleted_with":              "deletedWith",
	"depends_on":                "dependsOn",
	"env_var_mappings":          "envVarMappings",
	"for_each":                  "range",
	"hide_diffs":                "hideDiffs",
	"import_id":                 "import",
	"parent":                    "parent",
	"plugin_download_url":       "pluginDownloadURL",
	"protect":                   "protect",
	"provider":                  "provider",
	"providers":                 "providers",
	"range":                     "range",
	"replace_on_changes":        "replaceOnChanges",
	"replace_with":              "replaceWith",
	"retain_on_delete":          "retainOnDelete",
	"version":                   "version",
}

// The predicates below report whether a top-level attribute of a block is a
// meta-argument rather than block-type-specific configuration. They match the
// parser: every other option lives in the nested `pulumi` block, so a
// top-level attribute with an option's name is an input property.

func isResourceMeta(name string) bool { return parser.ResourceReservedNames[name] }

func isDataMeta(name string) bool { return parser.DataReservedNames[name] }

func isModuleMeta(name string) bool { return parser.ModuleReservedNames[name] }

func isProviderMeta(name string) bool { return parser.ProviderReservedNames[name] }

// emitFile writes the PCL form of body to out using ft as a (possibly
// project-wide) symbol table. The caller owns construction of ft and only
// per-file context (src, filename, body, paramInfos) flows through here.
func (ft *fileTransformer) emitFile(
	ctx context.Context,
	src []byte, filename string, body *hclsyntax.Body, out *hclwrite.Body,
	paramInfos map[string]workspace.PackageDescriptor,
) (hcl.Diagnostics, error) {
	ft.comments = comments.Build(src, filename, body, "data", "call")

	var resultDiags hcl.Diagnostics

	for _, alias := range slices.Sorted(maps.Keys(paramInfos)) {
		emitPackageBlock(out, alias, paramInfos[alias])
	}

	for _, block := range body.Blocks {
		ft.emitLeadingComments(out, block.Range().Start.Byte)
		switch block.Type {

		case "call":
			// Call blocks are inlined into expressions as call() function calls; skip block output.

		case "data":
			// A data block becomes an invoke. The invoke is inlined at its
			// reference, or hoisted to a local here.
			if len(block.Labels) != 2 {
				continue
			}
			plan := ft.dataPlans[dataReference{block.Labels[0], block.Labels[1]}]
			if plan == nil || plan.local == "" {
				continue
			}
			out.SetAttributeRaw(plan.local, ft.hoistedInvokeTokens(block.Labels[0], block.Labels[1], block.Body, plan))
			out.AppendNewline()

		case "variable":
			if len(block.Labels) == 0 {
				continue
			}
			name := block.Labels[0]
			pclName := name
			if rewritten, ok := ft.nameRewrites[name]; ok {
				pclName = rewritten
			}
			labels := []string{pclName}

			if typeAttr, ok := block.Body.Attributes["type"]; ok {
				// A PCL config with no type label is already `any`, so omit an
				// explicit `any` rather than emit the redundant label.
				if pclType := convertHCLTypeExpr(src, typeAttr.Expr); pclType != "any" {
					labels = append(labels, pclType)
				}
			}
			blk := out.AppendNewBlock("config", labels)
			if pclName != name {
				blk.Body().SetAttributeRaw("__logicalName", hclwrite.TokensForValue(cty.StringVal(name)))
			}
			for _, attr := range sortedAttributes(block.Body.Attributes) {
				if attr.Name == "type" {
					continue
				}
				start := attr.Range().Start.Byte
				ft.emitLeadingComments(blk.Body(), start)
				blk.Body().SetAttributeRaw(attr.Name, ft.withTrailing(ft.transformExpr(attr.Expr), start))
			}
			out.AppendNewline()

		case "locals":
			for _, attr := range sortedAttributes(block.Body.Attributes) {
				start := attr.Range().Start.Byte
				ft.emitLeadingComments(out, start)
				out.SetAttributeRaw(attr.Name, ft.withTrailing(ft.transformExpr(attr.Expr), start))
				out.AppendNewline()
			}

		case "output":
			if len(block.Labels) == 0 {
				continue
			}
			name := block.Labels[0]
			blk := out.AppendNewBlock("output", []string{name})
			if !hclsyntax.ValidIdentifier(name) {
				blk.Body().SetAttributeRaw("__logicalName", hclwrite.TokensForValue(cty.StringVal(name)))
			}
			for _, attr := range sortedAttributes(block.Body.Attributes) {
				start := attr.Range().Start.Byte
				ft.emitLeadingComments(blk.Body(), start)
				blk.Body().SetAttributeRaw(attr.Name, ft.withTrailing(ft.transformExpr(attr.Expr), start))
			}
			out.AppendNewline()

		case "module":
			if len(block.Labels) == 0 {
				continue
			}
			logicalName := block.Labels[0]
			pclName := logicalName
			if rewritten, ok := ft.nameRewrites[logicalName]; ok {
				pclName = rewritten
			}
			sourceAttr, ok := block.Body.Attributes["source"]
			if !ok {
				resultDiags = append(resultDiags, &hcl.Diagnostic{
					Severity: hcl.DiagError,
					Summary:  "missing source attribute",
					Detail:   "module block requires a \"source\" attribute",
					Subject:  block.TypeRange.Ptr(),
				})
				continue
			}
			sourceVal, valDiags := sourceAttr.Expr.Value(nil)
			if valDiags.HasErrors() {
				resultDiags = append(resultDiags, valDiags...)
				continue
			}
			if sourceVal.Type() != cty.String {
				resultDiags = append(resultDiags, &hcl.Diagnostic{
					Severity: hcl.DiagError,
					Summary:  "invalid source attribute",
					Detail:   "module \"source\" must be a string",
					Subject:  sourceAttr.Expr.Range().Ptr(),
				})
				continue
			}
			blk := out.AppendNewBlock("component", []string{pclName, sourceVal.AsString()})
			if pclName != logicalName {
				blk.Body().SetAttributeRaw("__logicalName", hclwrite.TokensForValue(cty.StringVal(logicalName)))
			}
			var rangeExpr, providersExpr hclsyntax.Expression
			var rangeType schema.Type
			for _, attr := range block.Body.Attributes {
				switch attr.Name {
				case "count":
					rangeExpr = attr.Expr
				case "for_each":
					rangeExpr, rangeType = attr.Expr, forEachType
				case "providers":
					providersExpr = attr.Expr
				}
			}
			for _, attr := range inputAttributes(block.Body, isModuleMeta) {
				start := attr.Range().Start.Byte
				ft.emitLeadingComments(blk.Body(), start)
				blk.Body().SetAttributeRaw(attr.Name, ft.withTrailing(ft.transformExpr(attr.Expr), start))
			}
			if rangeExpr != nil || providersExpr != nil {
				optBlk := blk.Body().AppendNewBlock("options", nil)
				if rangeExpr != nil {
					optBlk.Body().SetAttributeRaw("range", ft.transformTypedExpr(rangeExpr, rangeType, nil))
				}
				if providersExpr != nil {
					optBlk.Body().SetAttributeRaw("providers", ft.transformExpr(providersExpr))
				}
			}
			out.AppendNewline()

		case "resource":
			if len(block.Labels) < 2 {
				resultDiags = append(resultDiags, &hcl.Diagnostic{
					Severity: hcl.DiagError,
					Summary:  "malformed resource block",
					Detail:   "resource block requires exactly 2 labels: <type> and <name>",
					Subject:  block.TypeRange.Ptr(),
				})
				continue
			}
			hclType := block.Labels[0]
			logicalName := block.Labels[1]
			pclName := logicalName
			if rewritten, ok := ft.nameRewrites[logicalName]; ok {
				pclName = rewritten
			}

			res, err := ft.resolveHCLType(ctx, hclType)
			if err != nil {
				resultDiags = append(resultDiags, &hcl.Diagnostic{
					Severity: hcl.DiagError,
					Summary:  "unknown resource type",
					Detail:   fmt.Sprintf("cannot convert HCL type %q to PCL token: %v", hclType, err),
					Subject:  block.TypeRange.Ptr(),
				})
				continue
			}

			blk := out.AppendNewBlock("resource", []string{pclName, res.Token})
			if pclName != logicalName {
				blk.Body().SetAttributeRaw("__logicalName", hclwrite.TokensForValue(cty.StringVal(logicalName)))
			}

			mapping := ft.resourceMappings[hclType]
			// Emit input properties first (skip resource option attributes).
			for _, attr := range inputAttributes(block.Body, isResourceMeta) {
				name, prop, nested := lookupProperty(attr.Name, res.InputProperties, mapping)
				start := attr.Range().Start.Byte
				ft.emitLeadingComments(blk.Body(), start)
				value := ft.transformTypedExpr(attr.Expr, propertyType(prop), nested)
				blk.Body().SetAttributeRaw(name, ft.withTrailing(value, start))
			}

			// Collect resource options from attributes and sub-blocks.
			type optEntry struct {
				name   string
				tokens hclwrite.Tokens
			}
			var opts []optEntry
			for _, attr := range sortedAttributes(block.Body.Attributes) {
				if pclName, isOpt := resourceOptionHCLToPCL[attr.Name]; isOpt && isResourceMeta(attr.Name) {
					var tokens hclwrite.Tokens
					if attr.Name == "for_each" {
						tokens = ft.transformForEachExpr(attr.Expr)
					} else {
						tokens = ft.transformExpr(attr.Expr)
					}
					opts = append(opts, optEntry{pclName, tokens})
				}
			}
			// Separate special sub-blocks (lifecycle, timeouts, dynamic) from
			// property sub-blocks (e.g. details { ... }) that represent
			// array-of-object input properties.
			//
			// Track whether `create_before_destroy` was found anywhere; if not, we
			// emit `deleteBeforeReplace = true` so the PCL matches TF's default of
			// delete-then-create (Pulumi's default is the opposite).
			var propertyBlocks []*hclsyntax.Block
			sawCreateBeforeDestroy := false
			for _, subBlock := range block.Body.Blocks {
				switch subBlock.Type {
				case "pulumi":
					// Pulumi-specific resource options live in their
					// own block; map them the same way as top-level
					// attributes.
					for _, attr := range sortedAttributes(subBlock.Body.Attributes) {
						pclName, isOpt := resourceOptionHCLToPCL[attr.Name]
						if !isOpt {
							continue
						}
						var tokens hclwrite.Tokens
						switch attr.Name {
						case "additional_secret_outputs":
							tokens = ft.transformPropertyPathList(attr.Expr, res.Properties, mapping)
						case "hide_diffs":
							tokens = ft.transformPropertyPathList(attr.Expr, res.Properties, mapping)
						case "replace_on_changes":
							tokens = ft.transformPropertyPathList(attr.Expr, res.InputProperties, mapping)
						default:
							tokens = ft.transformExpr(attr.Expr)
						}
						opts = append(opts, optEntry{pclName, tokens})
					}
				case "lifecycle":
					for _, attr := range sortedAttributes(subBlock.Body.Attributes) {
						switch attr.Name {
						case "prevent_destroy":
							opts = append(opts, optEntry{"protect", ft.transformExpr(attr.Expr)})
						case "ignore_changes":
							opts = append(opts, optEntry{"ignoreChanges", ft.transformPropertyPathList(attr.Expr, res.InputProperties, mapping)})
						case "create_before_destroy":
							sawCreateBeforeDestroy = true
							// `create_before_destroy = true` matches Pulumi's default and
							// converts to no PCL option; anything else inverts to recover
							// `deleteBeforeReplace`.
							if isBoolLiteral(attr.Expr, true) {
								continue
							}
							opts = append(opts, optEntry{"deleteBeforeReplace", invertTokens(ft.transformExpr(attr.Expr))})
						case "replace_triggered_by":
							// PCL's `replacementTrigger` is a single expression. If the TF
							// list has exactly one element, unwrap it; otherwise keep the
							// tuple so all elements influence the trigger value.
							opts = append(opts, optEntry{"replacementTrigger", ft.unwrapSingletonTupleExpr(attr.Expr)})
						default:
							resultDiags = append(resultDiags, &hcl.Diagnostic{
								Severity: hcl.DiagError,
								Summary:  "unsupported lifecycle attribute",
								Detail:   fmt.Sprintf("lifecycle attribute %q is not supported by the HCL converter", attr.Name),
								Subject:  attr.NameRange.Ptr(),
							})
						}
					}
				case "timeouts":
					var timeoutAttrs []hclwrite.ObjectAttrTokens
					for _, attr := range sortedAttributes(subBlock.Body.Attributes) {
						timeoutAttrs = append(timeoutAttrs, hclwrite.ObjectAttrTokens{
							Name:  hclwrite.TokensForIdentifier(attr.Name),
							Value: ft.transformExpr(attr.Expr),
						})
					}
					if len(timeoutAttrs) > 0 {
						opts = append(opts, optEntry{"customTimeouts", hclwrite.TokensForObject(timeoutAttrs)})
					}
				default:
					propertyBlocks = append(propertyBlocks, subBlock)
				}
			}
			resultDiags = append(resultDiags, ft.emitPropertyBlocks(blk.Body(), propertyBlocks, res.InputProperties, mapping)...)
			if !sawCreateBeforeDestroy {
				opts = append(opts, optEntry{
					"deleteBeforeReplace",
					hclwrite.Tokens{{Type: hclsyntax.TokenIdent, Bytes: []byte("true")}},
				})
			}
			if len(opts) > 0 {
				optBlk := blk.Body().AppendNewBlock("options", nil)
				for _, o := range opts {
					optBlk.Body().SetAttributeRaw(o.name, o.tokens)
				}
			}
			out.AppendNewline()

		case "provider":
			// `provider "<pkg>" { ... }` → PCL `resource "<alias>" "pulumi:providers:<pkg>" { ... }`.
			if len(block.Labels) < 1 {
				resultDiags = append(resultDiags, &hcl.Diagnostic{
					Severity: hcl.DiagError,
					Summary:  "malformed provider block",
					Detail:   "provider block requires exactly 1 label: <name>",
					Subject:  block.TypeRange.Ptr(),
				})
				continue
			}
			pkgName := block.Labels[0]
			providerToken := "pulumi:providers:" + pkgName

			resourceName := providerResourceName(block)

			pkg, perr := packages.ResolvePackage(ctx, ft.loader, ft.knownProviders, "pulumi_providers_"+pkgName)
			if perr != nil {
				resultDiags = append(resultDiags, &hcl.Diagnostic{
					Severity: hcl.DiagError,
					Summary:  "unknown provider",
					Detail:   fmt.Sprintf("cannot resolve provider %q: %v", pkgName, perr),
					Subject:  block.TypeRange.Ptr(),
				})
				continue
			}
			providerRes, perr := pkg.Provider()
			if perr != nil {
				resultDiags = append(resultDiags, &hcl.Diagnostic{
					Severity: hcl.DiagError,
					Summary:  "unknown provider",
					Detail:   fmt.Sprintf("cannot load provider schema for %q: %v", pkgName, perr),
					Subject:  block.TypeRange.Ptr(),
				})
				continue
			}

			blk := out.AppendNewBlock("resource", []string{resourceName, providerToken})

			type optEntry struct {
				name   string
				tokens hclwrite.Tokens
			}
			var opts []optEntry
			if forEach, ok := block.Body.Attributes["for_each"]; ok {
				opts = append(opts, optEntry{"range", ft.transformForEachExpr(forEach.Expr)})
			}
			mapping := ft.resolver.ProviderConfigBodyMapping(ctx, pkgName)
			for _, attr := range inputAttributes(block.Body, isProviderMeta) {
				name, prop, nested := lookupProperty(attr.Name, providerRes.InputProperties, mapping)
				start := attr.Range().Start.Byte
				ft.emitLeadingComments(blk.Body(), start)
				value := ft.transformTypedExpr(attr.Expr, propertyType(prop), nested)
				blk.Body().SetAttributeRaw(name, ft.withTrailing(value, start))
			}
			// Pulumi-specific provider options live in a nested `pulumi` block.
			var propertyBlocks []*hclsyntax.Block
			for _, subBlock := range block.Body.Blocks {
				if subBlock.Type != "pulumi" {
					propertyBlocks = append(propertyBlocks, subBlock)
					continue
				}
				for _, attr := range sortedAttributes(subBlock.Body.Attributes) {
					switch attr.Name {
					case "env_var_mappings":
						opts = append(opts, optEntry{"envVarMappings", ft.transformExpr(attr.Expr)})
					case "plugin_download_url":
						opts = append(opts, optEntry{"pluginDownloadURL", ft.transformExpr(attr.Expr)})
					case "version":
						opts = append(opts, optEntry{"version", ft.transformExpr(attr.Expr)})
					case "additional_secret_outputs":
						opts = append(opts, optEntry{"additionalSecretOutputs", ft.transformPropertyPathList(attr.Expr, providerRes.Properties, mapping)})
					}
				}
			}
			resultDiags = append(resultDiags, ft.emitPropertyBlocks(blk.Body(), propertyBlocks, providerRes.InputProperties, mapping)...)
			if len(opts) > 0 {
				optBlk := blk.Body().AppendNewBlock("options", nil)
				for _, o := range opts {
					optBlk.Body().SetAttributeRaw(o.name, o.tokens)
				}
			}
			out.AppendNewline()

		case "terraform":
			// required_providers is parsed for symbol resolution but has no PCL
			// equivalent at this position (PCL declares providers via top-level
			// `package "<alias>" { ... }` blocks instead). Emit a PCL pulumi
			// block only when there are non-provider attributes to carry over.
			if len(block.Body.Attributes) == 0 {
				continue
			}
			blk := out.AppendNewBlock("pulumi", nil)
			for _, attr := range sortedAttributes(block.Body.Attributes) {
				name, _ := transform.PulumiCaseFromSnakeCase(attr.Name, nil)
				start := attr.Range().Start.Byte
				ft.emitLeadingComments(blk.Body(), start)
				blk.Body().SetAttributeRaw(name, ft.withTrailing(ft.transformExpr(attr.Expr), start))
			}
			out.AppendNewline()

		default:
			resultDiags = append(resultDiags, &hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  "unsupported block type",
				Detail:   fmt.Sprintf("block type %q is not supported by the HCL converter", block.Type),
				Subject:  block.TypeRange.Ptr(),
			})
		}
	}

	// Top-level attributes (uncommon in HCL input, but pass through with transforms).
	for _, attr := range sortedAttributes(body.Attributes) {
		start := attr.Range().Start.Byte
		ft.emitLeadingComments(out, start)
		out.SetAttributeRaw(attr.Name, ft.withTrailing(ft.transformExpr(attr.Expr), start))
		out.AppendNewline()
	}

	return resultDiags, nil
}

// providerResourceName returns the name of the PCL provider resource for a
// `provider` block with at least one label: its literal alias, or else its
// package name.
func providerResourceName(block *hclsyntax.Block) string {
	if attr, ok := block.Body.Attributes["alias"]; ok {
		if val, diags := attr.Expr.Value(nil); !diags.HasErrors() && val.Type() == cty.String {
			return val.AsString()
		}
	}
	return block.Labels[0]
}

// sortedAttributes returns attributes sorted by source position.
func sortedAttributes(attrs hclsyntax.Attributes) []*hclsyntax.Attribute {
	return sortAttributes(slices.Collect(maps.Values(attrs)))
}

// sortAttributes sorts attrs by source position.
func sortAttributes(attrs []*hclsyntax.Attribute) []*hclsyntax.Attribute {
	sort.Slice(attrs, func(i, j int) bool {
		return attrs[i].NameRange.Start.Byte < attrs[j].NameRange.Start.Byte
	})
	return attrs
}

// inputAttributes returns body's block-type-specific attributes in source
// order: the top-level attributes for which isMeta is false, plus every
// attribute of the `_` escaping block, whatever its name.
func inputAttributes(body *hclsyntax.Body, isMeta func(name string) bool) []*hclsyntax.Attribute {
	var result []*hclsyntax.Attribute
	for _, attr := range body.Attributes {
		if !isMeta(attr.Name) {
			result = append(result, attr)
		}
	}
	for _, block := range body.Blocks {
		if block.Type == "_" {
			result = slices.AppendSeq(result, maps.Values(block.Body.Attributes))
		}
	}
	return sortAttributes(result)
}

// convertHCLTypeExpr converts an HCL type constraint expression to a PCL type string.
func convertHCLTypeExpr(src []byte, expr hclsyntax.Expression) string {
	return convertHCLTypeExprInner(src, expr, false)
}

// convertHCLTypeExprInner converts an HCL type expression to a PCL type string.
// inCollection is true when the expression is a type argument inside a collection
// type (map, list, set, object), where HCL's "number" maps to PCL's "int".
func convertHCLTypeExprInner(src []byte, expr hclsyntax.Expression, inCollection bool) string {
	switch e := expr.(type) {
	case *hclsyntax.ScopeTraversalExpr:
		switch e.Traversal.RootName() {
		case "string":
			return "string"
		case "bool":
			return "bool"
		case "number":
			if inCollection {
				return "int"
			}
			return "number"
		case "any":
			return "any"
		}
	case *hclsyntax.FunctionCallExpr:
		args := make([]string, len(e.Args))
		for i, arg := range e.Args {
			args[i] = convertHCLTypeExprInner(src, arg, true)
		}
		return e.Name + "(" + strings.Join(args, ", ") + ")"
	case *hclsyntax.ObjectConsExpr:
		parts := make([]string, len(e.Items))
		for i, item := range e.Items {
			key := convertHCLTypeExprInner(src, item.KeyExpr, false)
			val := convertHCLTypeExprInner(src, item.ValueExpr, true)
			parts[i] = key + "=" + val
		}
		return "{" + strings.Join(parts, ", ") + "}"
	}
	// Fallback: use source bytes.
	return string(src[expr.Range().Start.Byte:expr.Range().End.Byte])
}

// transformExpr converts an HCL expression to PCL hclwrite tokens by walking the AST.
func (ft *fileTransformer) transformExpr(expr hclsyntax.Expression) hclwrite.Tokens {
	return ft.transformTypedExpr(expr, nil, nil)
}

// transformTypedExpr converts an HCL expression that is assigned to the schema
// type t. The keys of an object literal get their names from t and from m, the
// bridge mapping of the fields of t (nil when there is none). This is the
// inverse of the generator, which also passes the type through object and
// tuple literals only. A nil t is an unknown type.
func (ft *fileTransformer) transformTypedExpr(
	expr hclsyntax.Expression, t schema.Type, m *bridge.BodyMapping,
) hclwrite.Tokens {
	switch e := expr.(type) {
	case *hclsyntax.ScopeTraversalExpr:
		return ft.transformTraversal(e)
	case *hclsyntax.FunctionCallExpr:
		// abspath(path.root) is the encoding generate.go emits for PCL's
		// cwd() (program working directory); recognize it here so a
		// round-trip restores the original PCL call.
		if e.Name == "abspath" && len(e.Args) == 1 {
			if trav, ok := e.Args[0].(*hclsyntax.ScopeTraversalExpr); ok &&
				len(trav.Traversal) == 2 && trav.Traversal.RootName() == "path" {
				if attr, ok := trav.Traversal[1].(hcl.TraverseAttr); ok && attr.Name == "root" {
					return hclwrite.TokensForFunctionCall("cwd")
				}
			}
		}
		// pulumiresourceurn(<resource>) is the encoding generate.go emits
		// for PCL's `<resource>.urn`; restore the attribute on eject.
		if e.Name == "pulumiresourceurn" && len(e.Args) == 1 {
			return append(ft.transformExpr(e.Args[0]),
				&hclwrite.Token{Type: hclsyntax.TokenDot, Bytes: []byte(".")},
				&hclwrite.Token{Type: hclsyntax.TokenIdent, Bytes: []byte("urn")},
			)
		}
		// A provider-defined function call ejects as a positional invoke:
		// provider::<local>::<fn>(a, b) → invoke("<token>", a, b).
		if _, _, ok := ast.ParseProviderFunctionName(e.Name); ok {
			if fn := ft.providerFunctions[e.Name]; fn != nil {
				args := make([]hclwrite.Tokens, 0, len(e.Args)+1)
				args = append(args, hclwrite.TokensForValue(cty.StringVal(fn.Token)))
				for _, arg := range e.Args {
					args = append(args, ft.transformExpr(arg))
				}
				return hclwrite.TokensForFunctionCall("invoke", args...)
			}
		}
		pclName := transformFunctionName(e.Name)
		if !pclSupportedFunctions[pclName] {
			r := e.Range()
			originalExpr := string(ft.srcBytes(r))
			return hclwrite.TokensForFunctionCall("notImplemented",
				hclwrite.TokensForValue(cty.StringVal(originalExpr)))
		}
		args := make([]hclwrite.Tokens, len(e.Args))
		for i, arg := range e.Args {
			args[i] = ft.transformExpr(arg)
		}
		return hclwrite.TokensForFunctionCall(pclName, args...)
	case *hclsyntax.TemplateExpr:
		return ft.transformTemplate(e)
	case *hclsyntax.TemplateWrapExpr:
		return ft.transformExpr(e.Wrapped)
	case *hclsyntax.TupleConsExpr:
		// The bridge flattens a MaxItems=1 list to its one element, or to
		// null when the list is empty, and types it as that element.
		if _, ok := codegen.UnwrapType(t).(*schema.ObjectType); ok {
			switch len(e.Exprs) {
			case 0:
				return hclwrite.TokensForValue(cty.NullVal(cty.DynamicPseudoType))
			case 1:
				return ft.transformTypedExpr(e.Exprs[0], t, m)
			}
		}
		elemType := elementTypeOf(t)
		var elems []hclwrite.Tokens
		for _, item := range e.Exprs {
			elems = append(elems, ft.transformTypedExpr(item, elemType, m))
		}
		return hclwrite.TokensForTuple(elems)
	case *hclsyntax.ObjectConsKeyExpr:
		return ft.transformExpr(e.Wrapped)
	case *hclsyntax.ObjectConsExpr:
		var attrs []hclwrite.ObjectAttrTokens
		for _, item := range e.Items {
			key, valueType, valueMapping := ft.transformObjectKey(item.KeyExpr, t, m)
			attrs = append(attrs, hclwrite.ObjectAttrTokens{
				Name:  key,
				Value: ft.transformTypedExpr(item.ValueExpr, valueType, valueMapping),
			})
		}
		return hclwrite.TokensForObject(attrs)
	case *hclsyntax.BinaryOpExpr:
		lhs := ft.transformExpr(e.LHS)
		op := binaryOpToken(e.Op)
		op.SpacesBefore = 1
		rhs := ft.transformExpr(e.RHS)
		if len(rhs) > 0 {
			rhs[0].SpacesBefore = 1
		}
		return append(append(lhs, op), rhs...)
	case *hclsyntax.UnaryOpExpr:
		op := unaryOpToken(e.Op)
		val := ft.transformExpr(e.Val)
		if len(val) > 0 {
			val[0].SpacesBefore = 1
		}
		return append(hclwrite.Tokens{op}, val...)
	case *hclsyntax.ConditionalExpr:
		cond := ft.transformExpr(e.Condition)
		trueVal := ft.transformExpr(e.TrueResult)
		falseVal := ft.transformExpr(e.FalseResult)
		tokens := cond
		tokens = append(tokens, &hclwrite.Token{
			Type: hclsyntax.TokenQuestion, Bytes: []byte("?"), SpacesBefore: 1,
		})
		if len(trueVal) > 0 {
			trueVal[0].SpacesBefore = 1
		}
		tokens = append(tokens, trueVal...)
		tokens = append(tokens, &hclwrite.Token{
			Type: hclsyntax.TokenColon, Bytes: []byte(":"), SpacesBefore: 1,
		})
		if len(falseVal) > 0 {
			falseVal[0].SpacesBefore = 1
		}
		tokens = append(tokens, falseVal...)
		return tokens
	case *hclsyntax.ForExpr:
		return ft.transformForExpr(e)
	case *hclsyntax.SplatExpr:
		return ft.transformSplatExpr(e)
	case *hclsyntax.IndexExpr:
		return ft.transformIndexExpr(e)
	case *hclsyntax.RelativeTraversalExpr:
		return ft.transformRelativeTraversal(e)
	case *hclsyntax.ParenthesesExpr:
		tokens := hclwrite.Tokens{{Type: hclsyntax.TokenOParen, Bytes: []byte("(")}}
		tokens = append(tokens, ft.transformExpr(e.Expression)...)
		return append(tokens, &hclwrite.Token{Type: hclsyntax.TokenCParen, Bytes: []byte(")")})
	default:
		r := expr.Range()
		return hclwrite.Tokens{
			&hclwrite.Token{Type: hclsyntax.TokenIdent, Bytes: ft.srcBytes(r)},
		}
	}
}

// transformObjectKey converts the key of an object literal that is assigned to
// the schema type t, where m is the bridge mapping of the fields of t. It
// returns the tokens of the key, and the schema type and bridge mapping of the
// value that the key holds.
//
// An identifier key (e.g. bool_array) is the snake_case name of a property,
// unless t is a map. A key of a map is data, so it does not change. A quoted key
// (e.g. "my key") is a map key for all types.
func (ft *fileTransformer) transformObjectKey(
	key hclsyntax.Expression, t schema.Type, m *bridge.BodyMapping,
) (hclwrite.Tokens, schema.Type, *bridge.BodyMapping) {
	name := hcl.ExprAsKeyword(key)
	if mt, ok := codegen.UnwrapType(t).(*schema.MapType); ok {
		if name != "" {
			return hclwrite.TokensForIdentifier(name), mt.ElementType, m
		}
		return ft.transformExpr(key), mt.ElementType, m
	}
	if name != "" {
		pclName, prop, nested := lookupProperty(name, propertiesOf(t), m)
		return hclwrite.TokensForIdentifier(pclName), propertyType(prop), nested
	}
	return ft.transformExpr(key), nil, nil
}

// lookupProperty returns the PCL name of the HCL name tfName, its property in
// props (nil when none matches), and the bridge mapping of its nested fields.
// m is the bridge mapping of props, or nil.
func lookupProperty(
	tfName string, props []*schema.Property, m *bridge.BodyMapping,
) (string, *schema.Property, *bridge.BodyMapping) {
	name, prop := transform.ResolvePulumiProperty(tfName, props, m)
	var nested *bridge.BodyMapping
	if fm := m.Lookup(tfName); fm != nil {
		nested = fm.Nested
	}
	return name, prop, nested
}

// propertyType returns the schema type of p. It returns nil when p is nil.
func propertyType(p *schema.Property) schema.Type {
	if p == nil {
		return nil
	}
	return p.Type
}

// transformIndexExpr converts an HCL index expression (collection[key]) to PCL tokens.
// When the collection is a ranged data block (one with for_each/count) whose
// invoke is inlined, the key is the iteration key of an enclosing range over
// the same collection. So we drop the index and inline the invoke: the
// inlined invoke's arguments already close over that range.
func (ft *fileTransformer) transformIndexExpr(e *hclsyntax.IndexExpr) hclwrite.Tokens {
	if coll, ok := e.Collection.(*hclsyntax.ScopeTraversalExpr); ok &&
		coll.Traversal.RootName() == "data" && len(coll.Traversal) == 3 {
		typeAttr, ok1 := coll.Traversal[1].(hcl.TraverseAttr)
		nameAttr, ok2 := coll.Traversal[2].(hcl.TraverseAttr)
		if ok1 && ok2 {
			if plan := ft.dataPlans[dataReference{typeAttr.Name, nameAttr.Name}]; plan != nil &&
				plan.ranged && plan.local == "" {
				if plan.subst != nil {
					saved := ft.rangeSubst
					ft.rangeSubst = plan.subst
					defer func() { ft.rangeSubst = saved }()
				}
				return ft.invokeExprTokens(typeAttr.Name, nameAttr.Name)
			}
		}
	}

	tokens := ft.transformExpr(e.Collection)
	tokens = append(tokens, &hclwrite.Token{Type: hclsyntax.TokenOBrack, Bytes: []byte("[")})
	tokens = append(tokens, ft.transformExpr(e.Key)...)
	tokens = append(tokens, &hclwrite.Token{Type: hclsyntax.TokenCBrack, Bytes: []byte("]")})
	return tokens
}

// transformRelativeTraversal converts an HCL relative traversal expression
// (source.attr...) to PCL tokens. The source is transformed recursively, then
// the trailing traversal steps are appended with schema-aware name mapping.
func (ft *fileTransformer) transformRelativeTraversal(e *hclsyntax.RelativeTraversalExpr) hclwrite.Tokens {
	tokens := ft.transformExpr(e.Source)
	props, mapping := ft.relativeTraversalSourceProps(e.Source)
	converted, block := mapSteps(e.Traversal, props, mapping)
	tokens = append(tokens, hclwrite.TokensForTraversal(converted)...)
	if block != nil {
		return blockListTokens(block, tokens)
	}
	return tokens
}

// relativeTraversalSourceProps returns the schema properties of the value
// produced by `source` and their bridge mapping, used to map subsequent
// traversal attribute names.
func (ft *fileTransformer) relativeTraversalSourceProps(
	source hclsyntax.Expression,
) ([]*schema.Property, *bridge.BodyMapping) {
	if idx, ok := source.(*hclsyntax.IndexExpr); ok {
		source = idx.Collection
	}
	coll, ok := source.(*hclsyntax.ScopeTraversalExpr)
	if !ok || coll.Traversal.RootName() != "data" || len(coll.Traversal) < 3 {
		return nil, nil
	}
	typeAttr, ok := coll.Traversal[1].(hcl.TraverseAttr)
	if !ok {
		return nil, nil
	}
	return ft.dataResultProps(typeAttr.Name)
}

// dataResultProps returns the schema properties of the result of the data
// source hclType and their bridge mapping.
func (ft *fileTransformer) dataResultProps(hclType string) ([]*schema.Property, *bridge.BodyMapping) {
	var props []*schema.Property
	if fn := ft.functionSchemas[hclType]; fn != nil && fn.ReturnType != nil {
		props = propertiesOf(fn.ReturnType)
	}
	return props, ft.functionMappings[hclType]
}

// dataSteps returns the steps of the data reference trav (data.<type>.<name>
// ...) that read the result of one invoke. For a ranged data block, idx is
// the index that selects that result, or nil.
func (ft *fileTransformer) dataSteps(ref dataReference, trav hcl.Traversal) (idx *hcl.TraverseIndex, steps hcl.Traversal) {
	steps = trav[3:]
	if len(steps) > 0 && rangeAttribute(ft.dataBlocks[ref], isDataMeta) != nil {
		if i, ok := steps[0].(hcl.TraverseIndex); ok {
			return &i, steps[1:]
		}
	}
	return nil, steps
}

// mapSteps maps steps, which read a value with the schema properties props
// and the bridge mapping m. block is as for [schemaAwareTraversalAttrs].
func mapSteps(
	steps hcl.Traversal, props []*schema.Property, m *bridge.BodyMapping,
) (hcl.Traversal, *bridge.FieldMapping) {
	dummy := append(hcl.Traversal{hcl.TraverseRoot{Name: "_"}}, steps...)
	converted, block := schemaAwareTraversalAttrs(dummy, props, m)
	return converted[1:], block
}

// transformSplatExpr converts a splat expression (source[*].attr) to PCL tokens.
// The HCL and PCL syntax are identical, so we just need to transform the
// sub-expressions while preserving the [*] structure.
func (ft *fileTransformer) transformSplatExpr(e *hclsyntax.SplatExpr) hclwrite.Tokens {
	// A PCL splat reads a MaxItems=1 block, which the bridge flattens to one
	// object or null, the same way HCL reads the block's list. So the source
	// is the block itself, not the list that HCL reads.
	var tokens hclwrite.Tokens
	if src, ok := e.Source.(*hclsyntax.ScopeTraversalExpr); ok {
		tokens, _ = ft.transformTraversalRef(src)
	} else {
		tokens = ft.transformExpr(e.Source)
	}
	tokens = append(tokens,
		&hclwrite.Token{Type: hclsyntax.TokenOBrack, Bytes: []byte("[")},
		&hclwrite.Token{Type: hclsyntax.TokenStar, Bytes: []byte("*")},
		&hclwrite.Token{Type: hclsyntax.TokenCBrack, Bytes: []byte("]")},
	)

	// Resolve the schema element type so we can do schema-aware name mapping
	// on the traversal after [*].
	var elementProps []*schema.Property
	var elementMapping *bridge.BodyMapping
	if src, ok := e.Source.(*hclsyntax.ScopeTraversalExpr); ok {
		elementProps, elementMapping = ft.splatElementProps(src.Traversal)
	}

	tokens = append(tokens, ft.transformSplatEach(e.Each, elementProps, elementMapping)...)
	return tokens
}

// splatElementProps resolves the schema properties of the element type at the
// end of a resource traversal, and their bridge mapping. For example, given
// traversal "my_res.name.detail_items" where detail_items is Array<Object>,
// this returns the Object's properties.
func (ft *fileTransformer) splatElementProps(trav hcl.Traversal) ([]*schema.Property, *bridge.BodyMapping) {
	root := trav.RootName()
	res := ft.resourceSchemas[root]
	if res == nil {
		return nil, nil
	}

	// Walk the traversal (skipping root + resource name) to find the type at
	// the splat point.
	var currentType schema.Type = &schema.ObjectType{Properties: res.Properties}
	mapping := ft.resourceMappings[root]
	// flattened is true when the current step is a MaxItems=1 block, which
	// the bridge flattens to its one element.
	var flattened bool
	for _, step := range trav[2:] {
		switch attr := step.(type) {
		case hcl.TraverseAttr:
			fm := mapping.Lookup(attr.Name)
			_, prop, nested := lookupProperty(attr.Name, propertiesOf(currentType), mapping)
			if prop == nil {
				return nil, nil
			}
			currentType, mapping = prop.Type, nested
			flattened = fm != nil && fm.MaxItemsOne
		case hcl.TraverseIndex:
			if !flattened {
				currentType = elementTypeOf(currentType)
			}
			flattened = false
		}
		if currentType == nil {
			return nil, nil
		}
	}

	// A splat over a MaxItems=1 block reads its one element, which PCL
	// splats the same way.
	if flattened {
		return propertiesOf(currentType), mapping
	}
	// The splat operates on an array/list — unwrap to get element properties.
	return propertiesOf(elementTypeOf(currentType)), mapping
}

// transformSplatEach transforms the "each" part of a splat expression,
// producing tokens for the traversal after [*]. It uses schema properties
// (when available) to do schema-aware name mapping via schemaAwareTraversalAttrs.
func (ft *fileTransformer) transformSplatEach(
	expr hclsyntax.Expression, elementProps []*schema.Property, elementMapping *bridge.BodyMapping,
) hclwrite.Tokens {
	switch e := expr.(type) {
	case *hclsyntax.RelativeTraversalExpr:
		tokens := ft.transformSplatEach(e.Source, elementProps, elementMapping)
		// TODO[https://github.com/pulumi/pulumi-hcl/issues/729]: A splat whose
		// elements end on a MaxItems=1 block reads a list of objects in PCL,
		// where HCL reads a list of lists.
		converted, _ := mapSteps(e.Traversal, elementProps, elementMapping)
		return append(tokens, hclwrite.TokensForTraversal(converted)...)
	case *hclsyntax.AnonSymbolExpr:
		return nil
	default:
		r := expr.Range()
		return hclwrite.Tokens{
			&hclwrite.Token{Type: hclsyntax.TokenIdent, Bytes: ft.srcBytes(r)},
		}
	}
}

func (ft *fileTransformer) transformTemplate(e *hclsyntax.TemplateExpr) hclwrite.Tokens {
	if open, indented, ok := ft.detectHeredocOpen(e.SrcRange); ok {
		return ft.transformHeredocTemplate(e, open, indented)
	}
	tokens := hclwrite.Tokens{
		&hclwrite.Token{Type: hclsyntax.TokenOQuote, Bytes: []byte{'"'}},
	}
	for _, part := range e.Parts {
		if lit, ok := part.(*hclsyntax.LiteralValueExpr); ok {
			r := lit.SrcRange
			tokens = append(tokens, &hclwrite.Token{
				Type:  hclsyntax.TokenQuotedLit,
				Bytes: ft.srcBytes(r),
			})
		} else {
			tokens = append(tokens,
				&hclwrite.Token{Type: hclsyntax.TokenTemplateInterp, Bytes: []byte("${")},
			)
			tokens = append(tokens, ft.transformExpr(part)...)
			tokens = append(tokens,
				&hclwrite.Token{Type: hclsyntax.TokenTemplateSeqEnd, Bytes: []byte("}")},
			)
		}
	}
	tokens = append(tokens, &hclwrite.Token{Type: hclsyntax.TokenCQuote, Bytes: []byte{'"'}})
	return tokens
}

// detectHeredocOpen inspects the source bytes of a template expression to see
// whether the expression originated from a heredoc (`<<DELIM` or `<<-DELIM`).
// Returns the bare delimiter and whether the indent-stripping form was used.
// The TemplateExpr AST node does not record this information, so we recover it
// from the source.
func (ft *fileTransformer) detectHeredocOpen(r hcl.Range) (delim string, indented bool, ok bool) {
	src := ft.srcBytes(r)
	if !bytes.HasPrefix(src, []byte("<<")) {
		return "", false, false
	}
	rest := src[2:]
	if len(rest) > 0 && rest[0] == '-' {
		indented = true
		rest = rest[1:]
	}
	line, _, found := bytes.Cut(rest, []byte("\n"))
	if !found {
		return "", false, false
	}
	delim = strings.TrimRight(string(line), "\r")
	if delim == "" {
		return "", false, false
	}
	return delim, indented, true
}

// transformHeredocTemplate emits PCL tokens that re-create a heredoc with the
// original delimiter and indent-strip style. Literal parts are taken verbatim
// from the source so embedded `${`-escapes and indentation are preserved; the
// runtime parse value will match the input.
func (ft *fileTransformer) transformHeredocTemplate(
	e *hclsyntax.TemplateExpr, delim string, indented bool,
) hclwrite.Tokens {
	prefix := "<<"
	if indented {
		prefix = "<<-"
	}
	tokens := hclwrite.Tokens{
		{Type: hclsyntax.TokenOHeredoc, Bytes: []byte(prefix + delim + "\n")},
	}
	for _, part := range e.Parts {
		if lit, ok := part.(*hclsyntax.LiteralValueExpr); ok {
			tokens = append(tokens, &hclwrite.Token{
				Type:  hclsyntax.TokenStringLit,
				Bytes: ft.srcBytes(lit.SrcRange),
			})
			continue
		}
		tokens = append(tokens,
			&hclwrite.Token{Type: hclsyntax.TokenTemplateInterp, Bytes: []byte("${")},
		)
		tokens = append(tokens, ft.transformExpr(part)...)
		tokens = append(tokens,
			&hclwrite.Token{Type: hclsyntax.TokenTemplateSeqEnd, Bytes: []byte("}")},
		)
	}
	tokens = append(tokens, &hclwrite.Token{Type: hclsyntax.TokenCHeredoc, Bytes: []byte(delim + "\n")})
	return tokens
}

func binaryOpToken(op *hclsyntax.Operation) *hclwrite.Token {
	switch op {
	case hclsyntax.OpAdd:
		return &hclwrite.Token{Type: hclsyntax.TokenPlus, Bytes: []byte("+")}
	case hclsyntax.OpSubtract:
		return &hclwrite.Token{Type: hclsyntax.TokenMinus, Bytes: []byte("-")}
	case hclsyntax.OpMultiply:
		return &hclwrite.Token{Type: hclsyntax.TokenStar, Bytes: []byte("*")}
	case hclsyntax.OpDivide:
		return &hclwrite.Token{Type: hclsyntax.TokenSlash, Bytes: []byte("/")}
	case hclsyntax.OpModulo:
		return &hclwrite.Token{Type: hclsyntax.TokenPercent, Bytes: []byte("%")}
	case hclsyntax.OpLogicalAnd:
		return &hclwrite.Token{Type: hclsyntax.TokenAnd, Bytes: []byte("&&")}
	case hclsyntax.OpLogicalOr:
		return &hclwrite.Token{Type: hclsyntax.TokenOr, Bytes: []byte("||")}
	case hclsyntax.OpEqual:
		return &hclwrite.Token{Type: hclsyntax.TokenEqualOp, Bytes: []byte("==")}
	case hclsyntax.OpNotEqual:
		return &hclwrite.Token{Type: hclsyntax.TokenNotEqual, Bytes: []byte("!=")}
	case hclsyntax.OpGreaterThan:
		return &hclwrite.Token{Type: hclsyntax.TokenGreaterThan, Bytes: []byte(">")}
	case hclsyntax.OpGreaterThanOrEqual:
		return &hclwrite.Token{Type: hclsyntax.TokenGreaterThanEq, Bytes: []byte(">=")}
	case hclsyntax.OpLessThan:
		return &hclwrite.Token{Type: hclsyntax.TokenLessThan, Bytes: []byte("<")}
	case hclsyntax.OpLessThanOrEqual:
		return &hclwrite.Token{Type: hclsyntax.TokenLessThanEq, Bytes: []byte("<=")}
	default:
		panic(fmt.Sprintf("unsupported binary operation: %v", op))
	}
}

func unaryOpToken(op *hclsyntax.Operation) *hclwrite.Token {
	switch op {
	case hclsyntax.OpNegate:
		return &hclwrite.Token{Type: hclsyntax.TokenMinus, Bytes: []byte("-")}
	case hclsyntax.OpLogicalNot:
		return &hclwrite.Token{Type: hclsyntax.TokenBang, Bytes: []byte("!")}
	default:
		panic(fmt.Sprintf("unsupported unary operation: %v", op))
	}
}

// transformTraversal converts an HCL scope traversal to PCL tokens.
func (ft *fileTransformer) transformTraversal(e *hclsyntax.ScopeTraversalExpr) hclwrite.Tokens {
	tokens, block := ft.transformTraversalRef(e)
	if block != nil {
		return blockListTokens(block, tokens)
	}
	return tokens
}

// transformTraversalRef converts an HCL scope traversal to PCL tokens. block
// is as for [schemaAwareTraversalAttrs]: if it is not nil, the tokens are the
// one element or null that the bridge flattens the block to.
func (ft *fileTransformer) transformTraversalRef(
	e *hclsyntax.ScopeTraversalExpr,
) (tokens hclwrite.Tokens, block *bridge.FieldMapping) {
	root := e.Traversal.RootName()

	// Provider references: `<pkg>.<alias>.x` → `<alias>.x` in PCL.
	// Trailing snake_case attrs become camelCase; the synthetic provider
	// outputs need a casing hint so `URL` doesn't get mangled to `Url`.
	// Defer to the call-inlining path when the alias names a call block.
	if ft.isKnownProvider(root) && len(e.Traversal) >= 2 {
		if attr, ok := e.Traversal[1].(hcl.TraverseAttr); ok &&
			ft.providerAliases[root+"."+attr.Name] {
			isCallMethod := false
			if len(e.Traversal) >= 3 {
				if method, ok := e.Traversal[2].(hcl.TraverseAttr); ok {
					if _, hasCall := ft.callBlocks[callReference{attr.Name, method.Name}]; hasCall {
						isCallMethod = true
					}
				}
			}
			if !isCallMethod {
				stripped := stripRoot(e.Traversal)
				for i := 1; i < len(stripped); i++ {
					if a, ok := stripped[i].(hcl.TraverseAttr); ok {
						name, _ := transform.PulumiCaseFromSnakeCase(a.Name, providerSyntheticOutputs)
						stripped[i] = hcl.TraverseAttr{Name: name, SrcRange: a.SrcRange}
					}
				}
				return hclwrite.TokensForTraversal(stripped), nil
			}
		}
	}

	// Resource traversal: strip the HCL type prefix (e.g., "pulumi_stash.myRes.prop" → "myRes.prop"),
	// and convert property attribute names from snake_case to camelCase.
	if ft.knownHCLTypes[root] {
		stripped := ft.rewriteTraversalRoot(stripRoot(e.Traversal))
		// StackReference: <type>.<name>.outputs["key"] → getOutput(<name>, "key")
		if len(stripped) == 3 {
			logicalName, ok1 := stripped[0].(hcl.TraverseRoot)
			attr, ok2 := stripped[1].(hcl.TraverseAttr)
			idx, ok3 := stripped[2].(hcl.TraverseIndex)
			if ok1 && ok2 && ok3 && attr.Name == "outputs" &&
				ft.stackRefNames[logicalName.Name] && idx.Key.Type() == cty.String {
				refTokens := hclwrite.TokensForTraversal(hcl.Traversal{hcl.TraverseRoot{Name: logicalName.Name}})
				keyTokens := hclwrite.TokensForValue(idx.Key)
				return hclwrite.TokensForFunctionCall("getOutput", refTokens, keyTokens), nil
			}
		}
		converted, block := ft.traversalAttrs(stripped, root)
		return hclwrite.TokensForTraversal(converted), block
	}

	switch root {
	case "data":
		// data.hclType.name[.prop...] → invoke("token", {args...})[.prop...]
		// or <local>[.prop...] when the invoke is hoisted to a local.
		if len(e.Traversal) >= 3 {
			typeAttr, ok1 := e.Traversal[1].(hcl.TraverseAttr)
			nameAttr, ok2 := e.Traversal[2].(hcl.TraverseAttr)
			if ok1 && ok2 {
				ref := dataReference{typeAttr.Name, nameAttr.Name}
				idx, steps := ft.dataSteps(ref, e.Traversal)
				var tokens hclwrite.Tokens
				if plan := ft.dataPlans[ref]; plan != nil && plan.local != "" {
					tokens = hclwrite.TokensForTraversal(hcl.Traversal{hcl.TraverseRoot{Name: plan.local}})
					if idx != nil {
						tokens = append(tokens, hclwrite.TokensForTraversal(hcl.Traversal{*idx})...)
					}
				} else {
					// The planner inlines a ranged data block only at a
					// reference data.<type>.<name>[key] with no other steps.
					contract.Assertf(idx == nil, "inlined data reference %s has an index", ref.name)
					tokens = ft.invokeExprTokens(typeAttr.Name, nameAttr.Name)
				}
				props, mapping := ft.dataResultProps(typeAttr.Name)
				converted, block := mapSteps(steps, props, mapping)
				return append(tokens, hclwrite.TokensForTraversal(converted)...), block
			}
		}
	case "call":
		// call.resourceName.methodName[.prop...] → call(resourceName, "methodName", {args...})[.prop...]
		if len(e.Traversal) >= 3 {
			resAttr, ok1 := e.Traversal[1].(hcl.TraverseAttr)
			methodAttr, ok2 := e.Traversal[2].(hcl.TraverseAttr)
			if ok1 && ok2 {
				tokens := ft.callExprTokens(resAttr.Name, methodAttr.Name)
				for _, step := range e.Traversal[3:] {
					if attr, ok := step.(hcl.TraverseAttr); ok {
						tokens = append(tokens,
							&hclwrite.Token{Type: hclsyntax.TokenDot, Bytes: []byte(".")},
							&hclwrite.Token{Type: hclsyntax.TokenIdent, Bytes: []byte(attr.Name)},
						)
					}
				}
				return tokens, nil
			}
		}
	case "count":
		if s := ft.rangeSubst; s != nil && s.index != "" &&
			traversalString(e.Traversal[:min(2, len(e.Traversal))]) == "count.index" {
			return hclwrite.TokensForTraversal(substRoot(e.Traversal, s.index)), nil
		}
		// count.index → range.value
		trav := make(hcl.Traversal, len(e.Traversal))
		trav[0] = hcl.TraverseRoot{Name: "range"}
		for i := 1; i < len(e.Traversal); i++ {
			if attr, ok := e.Traversal[i].(hcl.TraverseAttr); ok && attr.Name == "index" {
				trav[i] = hcl.TraverseAttr{Name: "value"}
			} else {
				trav[i] = e.Traversal[i]
			}
		}
		return hclwrite.TokensForTraversal(trav), nil
	case "each":
		if s := ft.rangeSubst; s != nil {
			switch traversalString(e.Traversal[:min(2, len(e.Traversal))]) {
			case "each.key":
				return hclwrite.TokensForTraversal(substRoot(e.Traversal, s.key)), nil
			case "each.value":
				return hclwrite.TokensForTraversal(substRoot(e.Traversal, s.value)), nil
			}
		}
		// each.key → range.key, each.value → range.value
		trav := make(hcl.Traversal, len(e.Traversal))
		trav[0] = hcl.TraverseRoot{Name: "range"}
		copy(trav[1:], e.Traversal[1:])
		return hclwrite.TokensForTraversal(trav), nil
	case "var", "local", "module":
		return hclwrite.TokensForTraversal(ft.rewriteTraversalRoot(stripRoot(e.Traversal))), nil
	case "pulumi":
		if len(e.Traversal) >= 2 {
			if attr, ok := e.Traversal[1].(hcl.TraverseAttr); ok {
				switch attr.Name {
				case "stack":
					return hclwrite.TokensForFunctionCall("stack"), nil
				case "project":
					return hclwrite.TokensForFunctionCall("project"), nil
				case "organization":
					return hclwrite.TokensForFunctionCall("organization"), nil
				}
			}
		}
	case "path":
		if len(e.Traversal) >= 2 {
			if attr, ok := e.Traversal[1].(hcl.TraverseAttr); ok {
				switch attr.Name {
				case "cwd":
					// PCL's rootDirectory() generates to path.cwd; invert.
					return hclwrite.TokensForFunctionCall("rootDirectory"), nil
				}
				// Bare path.root / path.module follow Terraform semantics
				// (path.root is "."), so leave them untouched. PCL's cwd()
				// comes through as abspath(path.root) and is handled in
				// the FunctionCallExpr branch.
			}
		}
	}
	return hclwrite.TokensForTraversal(e.Traversal), nil
}

// schemaAwareTraversalAttrs converts traversal attribute names from snake_case
// to their schema property names, tracking the schema type and the bridge
// mapping through each step. m is the bridge mapping of props, or nil.
//
// The bridge flattens a MaxItems=1 block to its one element, or to null when
// the block is absent, but HCL reads the block as a list. So the [0] after
// such a block is removed. If the traversal ends on such a block, block is
// its mapping, and the caller must write the value as the list that HCL reads
// (see [blockListTokens]).
func schemaAwareTraversalAttrs(
	trav hcl.Traversal, props []*schema.Property, m *bridge.BodyMapping,
) (result hcl.Traversal, block *bridge.FieldMapping) {
	if len(trav) <= 1 {
		return trav, nil
	}
	result = hcl.Traversal{trav[0]}
	// currentType tracks the schema type at the current traversal position.
	// We start with a synthetic object wrapping the top-level properties.
	var currentType schema.Type
	if len(props) > 0 {
		currentType = &schema.ObjectType{Properties: props}
	}
	for _, step := range trav[1:] {
		switch step := step.(type) {
		case hcl.TraverseAttr:
			block = nil
			if mt, ok := codegen.UnwrapType(currentType).(*schema.MapType); ok {
				// A map key written as an attribute is data, not a property
				// name, so it keeps its name as an index.
				result = append(result, hcl.TraverseIndex{Key: cty.StringVal(step.Name), SrcRange: step.SrcRange})
				currentType = mt.ElementType
				continue
			}
			if fm := m.Lookup(step.Name); fm != nil && fm.MaxItemsOne {
				block = fm
			}
			name, matched, nested := lookupProperty(step.Name, propertiesOf(currentType), m)
			result = append(result, hcl.TraverseAttr{Name: name})
			m = nested
			currentType = propertyType(matched)
		case hcl.TraverseIndex:
			if block != nil && isZeroIndex(step.Key) {
				block = nil
				continue
			}
			block = nil
			result = append(result, step)
			currentType = elementTypeOf(currentType)
		}
	}
	return result, block
}

// blockListTokens writes the value that HCL reads for the MaxItems=1 field
// fm. ref is the one element or null that the bridge flattens the field to.
func blockListTokens(fm *bridge.FieldMapping, ref hclwrite.Tokens) hclwrite.Tokens {
	unset := hclwrite.TokensForTuple(nil)
	if transform.UnsetSingularIsNull(fm) {
		unset = hclwrite.TokensForIdentifier("null")
	}
	out := hclwrite.Tokens{{Type: hclsyntax.TokenOParen, Bytes: []byte("(")}}
	out = append(out, ref...)
	out = append(out, &hclwrite.Token{Type: hclsyntax.TokenEqualOp, Bytes: []byte("==")})
	out = append(out, hclwrite.TokensForIdentifier("null")...)
	out = append(out, &hclwrite.Token{Type: hclsyntax.TokenQuestion, Bytes: []byte("?")})
	out = append(out, unset...)
	out = append(out, &hclwrite.Token{Type: hclsyntax.TokenColon, Bytes: []byte(":")})
	out = append(out, hclwrite.TokensForTuple([]hclwrite.Tokens{ref})...)
	return append(out, &hclwrite.Token{Type: hclsyntax.TokenCParen, Bytes: []byte(")")})
}

// isZeroIndex reports whether key is the number 0.
func isZeroIndex(key cty.Value) bool {
	return key.IsKnown() && key.Type() == cty.Number && key.RawEquals(cty.Zero)
}

// propertiesOf extracts []*schema.Property from a schema type.
func propertiesOf(t schema.Type) []*schema.Property {
	if t == nil {
		return nil
	}
	if obj, ok := codegen.UnwrapType(t).(*schema.ObjectType); ok {
		return obj.Properties
	}
	return nil
}

// elementTypeOf unwraps one level of container (map or array).
func elementTypeOf(t schema.Type) schema.Type {
	switch t := codegen.UnwrapType(t).(type) {
	case *schema.MapType:
		return t.ElementType
	case *schema.ArrayType:
		return t.ElementType
	default:
		return nil
	}
}

// traversalAttrs is [schemaAwareTraversalAttrs] against the schema and bridge
// mapping cached for hclType. The root (logical resource name) is left
// unchanged.
func (ft *fileTransformer) traversalAttrs(trav hcl.Traversal, hclType string) (hcl.Traversal, *bridge.FieldMapping) {
	var props []*schema.Property
	if res := ft.resourceSchemas[hclType]; res != nil {
		props = res.Properties
	}
	return schemaAwareTraversalAttrs(trav, props, ft.resourceMappings[hclType])
}

// stripRoot converts a traversal like var.name.field to name.field by promoting the
// second element to the root.
func stripRoot(trav hcl.Traversal) hcl.Traversal {
	if len(trav) < 2 {
		return trav
	}
	var name string
	switch step := trav[1].(type) {
	case hcl.TraverseAttr:
		name = step.Name
	case hcl.TraverseIndex:
		if step.Key.Type() == cty.String {
			name = step.Key.AsString()
		} else {
			return trav
		}
	default:
		return trav
	}
	result := make(hcl.Traversal, len(trav)-1)
	result[0] = hcl.TraverseRoot{Name: name}
	copy(result[1:], trav[2:])
	return result
}

// providerSyntheticOutputs disambiguates provider outputs whose snake_case
// HCL form has uppercase-acronym components (`URL` → `Url` by default).
var providerSyntheticOutputs = []*schema.Property{
	{Name: "id", Type: schema.StringType},
	{Name: "urn", Type: schema.StringType},
	{Name: "version", Type: schema.StringType},
	{Name: "pluginDownloadURL", Type: schema.StringType},
}

func (ft *fileTransformer) isKnownProvider(name string) bool {
	return slices.Contains(ft.knownProviders, name)
}

// rewriteTraversalRoot replaces the root name of a traversal if the name has
// a rewrite mapping (i.e. it was not a valid PCL identifier).
func (ft *fileTransformer) rewriteTraversalRoot(trav hcl.Traversal) hcl.Traversal {
	if len(trav) == 0 {
		return trav
	}
	if rewritten, ok := ft.nameRewrites[trav.RootName()]; ok {
		result := make(hcl.Traversal, len(trav))
		copy(result, trav)
		result[0] = hcl.TraverseRoot{Name: rewritten}
		return result
	}
	return trav
}

// callExprTokens generates PCL tokens for call(resourceName, "camelMethod", {args...}).
// It looks up the matching call block to extract the argument object.
func (ft *fileTransformer) callExprTokens(resourceName, snakeMethod string) hclwrite.Tokens {
	camelMethod, _ := transform.PulumiCaseFromSnakeCase(snakeMethod, nil)
	resTokens := hclwrite.TokensForTraversal(hcl.Traversal{hcl.TraverseRoot{Name: resourceName}})
	methodTokens := hclwrite.TokensForValue(cty.StringVal(camelMethod))

	var argsTokens hclwrite.Tokens
	if body, ok := ft.callBlocks[callReference{resourceName, snakeMethod}]; ok && len(body.Attributes) > 0 {
		var attrs []hclwrite.ObjectAttrTokens
		for _, attr := range sortedAttributes(body.Attributes) {
			name, _ := transform.PulumiCaseFromSnakeCase(attr.Name, nil)
			attrs = append(attrs, hclwrite.ObjectAttrTokens{
				Name:  hclwrite.TokensForIdentifier(name),
				Value: ft.transformExpr(attr.Expr),
			})
		}
		argsTokens = hclwrite.TokensForObject(attrs)
	} else {
		argsTokens = hclwrite.TokensForObject(nil)
	}

	return hclwrite.TokensForFunctionCall("call", resTokens, methodTokens, argsTokens)
}

// invokeExprTokens generates PCL tokens for invoke("token", {args...}) or
// invoke("token", {args...}, {opts...}) when invoke options are present.
// It looks up the matching data block to extract the argument and option objects.
func (ft *fileTransformer) invokeExprTokens(hclType, dsName string) hclwrite.Tokens {
	tokenTokens := hclwrite.TokensForValue(cty.StringVal(ft.dataTokens[hclType]))

	var inputProps []*schema.Property
	if fn := ft.functionSchemas[hclType]; fn != nil && fn.Inputs != nil {
		inputProps = fn.Inputs.Properties
	}
	mapping := ft.functionMappings[hclType]

	var argAttrs, optAttrs []hclwrite.ObjectAttrTokens
	if body, ok := ft.dataBlocks[dataReference{hclType, dsName}]; ok {
		for _, attr := range inputAttributes(body, isDataMeta) {
			name, prop, nested := lookupProperty(attr.Name, inputProps, mapping)
			argAttrs = append(argAttrs, hclwrite.ObjectAttrTokens{
				Name:  hclwrite.TokensForIdentifier(name),
				Value: ft.transformTypedExpr(attr.Expr, propertyType(prop), nested),
			})
		}
		for _, attr := range sortedAttributes(body.Attributes) {
			if pclName, isOpt := invokeOptionHCLToPCL[attr.Name]; isOpt && isDataMeta(attr.Name) {
				optAttrs = append(optAttrs, hclwrite.ObjectAttrTokens{
					Name:  hclwrite.TokensForIdentifier(pclName),
					Value: ft.transformExpr(attr.Expr),
				})
			}
		}
		// Pulumi-specific invoke options live in a nested `pulumi` block; the
		// remaining blocks, including those in the `_` escaping block, are
		// array-of-object input properties.
		var inputBlocks []*hclsyntax.Block
		for _, b := range body.Blocks {
			switch b.Type {
			case "_":
				inputBlocks = append(inputBlocks, b.Body.Blocks...)
			case "pulumi":
				for _, attr := range sortedAttributes(b.Body.Attributes) {
					if pclName, isOpt := invokeOptionHCLToPCL[attr.Name]; isOpt {
						optAttrs = append(optAttrs, hclwrite.ObjectAttrTokens{
							Name:  hclwrite.TokensForIdentifier(pclName),
							Value: ft.transformExpr(attr.Expr),
						})
					}
				}
			default:
				inputBlocks = append(inputBlocks, b)
			}
		}
		argAttrs = append(argAttrs, ft.blocksToObjectAttrs(inputBlocks, inputProps, mapping)...)
	}

	argsTokens := hclwrite.TokensForObject(argAttrs)
	if len(optAttrs) == 0 {
		return hclwrite.TokensForFunctionCall("invoke", tokenTokens, argsTokens)
	}
	return hclwrite.TokensForFunctionCall("invoke", tokenTokens, argsTokens, hclwrite.TokensForObject(optAttrs))
}

// hoistedInvokeTokens returns the value of the PCL local that holds the
// invoke of a hoisted data block. A ranged data block becomes a
// comprehension: a map keyed like for_each, or a list indexed like count.
func (ft *fileTransformer) hoistedInvokeTokens(
	hclType, dsName string, body *hclsyntax.Body, plan *dataPlan,
) hclwrite.Tokens {
	rangeAttr := rangeAttribute(body, isDataMeta)
	if rangeAttr == nil {
		return ft.invokeExprTokens(hclType, dsName)
	}
	saved := ft.rangeSubst
	defer func() { ft.rangeSubst = saved }()

	if rangeAttr.Name == "count" {
		coll := hclwrite.TokensForFunctionCall("range", ft.transformExpr(rangeAttr.Expr))
		ft.rangeSubst = plan.subst
		return forExprTokens("", plan.subst.index, coll, nil, ft.invokeExprTokens(hclType, dsName), nil, false)
	}
	coll := ft.transformForEachExpr(rangeAttr.Expr)
	ft.rangeSubst = plan.subst
	key := hclwrite.TokensForTraversal(hcl.Traversal{hcl.TraverseRoot{Name: plan.subst.key}})
	return forExprTokens(plan.subst.key, plan.subst.value, coll, key, ft.invokeExprTokens(hclType, dsName), nil, false)
}

// substRoot returns trav with its first two steps (e.g. each.key) replaced
// by the variable name.
func substRoot(trav hcl.Traversal, name string) hcl.Traversal {
	return append(hcl.Traversal{hcl.TraverseRoot{Name: name}}, trav[2:]...)
}

// emitPropertyBlocks writes the PCL form of blocks, the input property blocks
// of a resource or provider, to body. Each dynamic block becomes a for
// expression. The blocks of a `_` escaping block are input properties whatever
// their names, and the runtime still expands a dynamic block written there.
// props and m are the input properties and their bridge mapping (m may be nil).
func (ft *fileTransformer) emitPropertyBlocks(
	body *hclwrite.Body, blocks []*hclsyntax.Block, props []*schema.Property, m *bridge.BodyMapping,
) hcl.Diagnostics {
	var diags hcl.Diagnostics
	var objectBlocks []*hclsyntax.Block
	for _, block := range blocks {
		children := []*hclsyntax.Block{block}
		if block.Type == "_" {
			children = block.Body.Blocks
		}
		for _, child := range children {
			if child.Type == "dynamic" {
				diags = append(diags, ft.convertDynamicBlock(body, child, props, m)...)
			} else {
				objectBlocks = append(objectBlocks, child)
			}
		}
	}
	for _, objAttr := range ft.blocksToObjectAttrs(objectBlocks, props, m) {
		name := strings.TrimSpace(string(objAttr.Name.Bytes()))
		body.SetAttributeRaw(name, objAttr.Value)
	}
	return diags
}

// blocksToObjectAttrs converts HCL blocks (which represent array-of-object
// properties) back to PCL object attributes with array values. m is the bridge
// mapping of props, or nil.
func (ft *fileTransformer) blocksToObjectAttrs(
	blocks []*hclsyntax.Block, props []*schema.Property, m *bridge.BodyMapping,
) []hclwrite.ObjectAttrTokens {
	// Group blocks by their type name, preserving order of first occurrence.
	type blockGroup struct {
		name   string
		blocks []*hclsyntax.Block
	}
	var groups []blockGroup
	seen := map[string]int{}
	for _, block := range blocks {
		if idx, ok := seen[block.Type]; ok {
			groups[idx].blocks = append(groups[idx].blocks, block)
		} else {
			seen[block.Type] = len(groups)
			groups = append(groups, blockGroup{name: block.Type, blocks: []*hclsyntax.Block{block}})
		}
	}

	var result []hclwrite.ObjectAttrTokens
	for _, g := range groups {
		name, matched, elemMapping := lookupProperty(g.name, props, m)
		// A block is a list of objects, or one object when the bridge
		// flattens a MaxItems=1 block.
		elemProps := elementObjectProperties(matched)
		var elems []hclwrite.Tokens
		for _, block := range g.blocks {
			var objAttrs []hclwrite.ObjectAttrTokens
			for _, attr := range sortedAttributes(block.Body.Attributes) {
				attrName, prop, nested := lookupProperty(attr.Name, elemProps, elemMapping)
				objAttrs = append(objAttrs, hclwrite.ObjectAttrTokens{
					Name:  hclwrite.TokensForIdentifier(attrName),
					Value: ft.transformTypedExpr(attr.Expr, propertyType(prop), nested),
				})
			}
			objAttrs = append(objAttrs, ft.blocksToObjectAttrs(block.Body.Blocks, elemProps, elemMapping)...)
			elems = append(elems, hclwrite.TokensForObject(objAttrs))
		}
		value := hclwrite.TokensForTuple(elems)
		if isObjectProperty(matched) && len(elems) == 1 {
			value = elems[0]
		}
		result = append(result, hclwrite.ObjectAttrTokens{
			Name:  hclwrite.TokensForIdentifier(name),
			Value: value,
		})
	}
	return result
}

// convertDynamicBlock converts an HCL dynamic block back to a PCL for-expression attribute.
//
// dynamic "details" {
//
//	for_each = collection
//	content { key = details.value.key }
//
// }
//
// → details = [for __key, __value in collection : { key = __value.key }]
func (ft *fileTransformer) convertDynamicBlock(
	body *hclwrite.Body, block *hclsyntax.Block, props []*schema.Property, m *bridge.BodyMapping,
) hcl.Diagnostics {
	if len(block.Labels) == 0 {
		return hcl.Diagnostics{{
			Severity: hcl.DiagError,
			Summary:  "malformed dynamic block",
			Detail:   "dynamic block requires a label",
			Subject:  block.TypeRange.Ptr(),
		}}
	}
	propSnakeName := block.Labels[0]
	propName, matched, elemMapping := lookupProperty(propSnakeName, props, m)

	elemProps := elementObjectProperties(matched)

	forEachAttr, ok := block.Body.Attributes["for_each"]
	if !ok {
		return hcl.Diagnostics{{
			Severity: hcl.DiagError,
			Summary:  "missing for_each",
			Detail:   "dynamic block requires a for_each attribute",
			Subject:  block.TypeRange.Ptr(),
		}}
	}

	// Find the iterator name (defaults to block label).
	iteratorName := propSnakeName
	if iterAttr, ok := block.Body.Attributes["iterator"]; ok {
		if keyword := hcl.ExprAsKeyword(iterAttr.Expr); keyword != "" {
			iteratorName = keyword
		}
	}

	// Find the content block.
	var contentBlock *hclsyntax.Block
	for _, sub := range block.Body.Blocks {
		if sub.Type == "content" {
			contentBlock = sub
			break
		}
	}
	if contentBlock == nil {
		return hcl.Diagnostics{{
			Severity: hcl.DiagError,
			Summary:  "missing content block",
			Detail:   "dynamic block requires a content block",
			Subject:  block.TypeRange.Ptr(),
		}}
	}

	// Build the for-expression body: an object with each attribute from the content block.
	var objAttrs []hclwrite.ObjectAttrTokens
	for _, attr := range sortedAttributes(contentBlock.Body.Attributes) {
		attrName, prop, nested := lookupProperty(attr.Name, elemProps, elemMapping)
		// Transform the value expression, rewriting iterator references.
		valueTokens := ft.transformExprWithIterator(attr.Expr, iteratorName, propertyType(prop), nested)
		objAttrs = append(objAttrs, hclwrite.ObjectAttrTokens{
			Name:  hclwrite.TokensForIdentifier(attrName),
			Value: valueTokens,
		})
	}

	// Build: [for __key, __value in <collection> : { <attrs> }]
	collectionTokens := ft.transformTypedExpr(forEachAttr.Expr, forEachType, nil)
	objTokens := hclwrite.TokensForObject(objAttrs)

	forExprTokens := buildForExprTokens(collectionTokens, objTokens)
	body.SetAttributeRaw(propName, forExprTokens)
	return nil
}

// buildForExprTokens builds tokens for: [for __key, __value in <collection> : <value>]
func buildForExprTokens(collection, value hclwrite.Tokens) hclwrite.Tokens {
	tokens := hclwrite.Tokens{
		{Type: hclsyntax.TokenOBrack, Bytes: []byte("[")},
		{Type: hclsyntax.TokenIdent, Bytes: []byte("for"), SpacesBefore: 0},
		{Type: hclsyntax.TokenIdent, Bytes: []byte("__key"), SpacesBefore: 1},
		{Type: hclsyntax.TokenComma, Bytes: []byte(",")},
		{Type: hclsyntax.TokenIdent, Bytes: []byte("__value"), SpacesBefore: 1},
		{Type: hclsyntax.TokenIdent, Bytes: []byte("in"), SpacesBefore: 1},
	}
	if len(collection) > 0 {
		collection[0].SpacesBefore = 1
	}
	tokens = append(tokens, collection...)
	tokens = append(tokens, &hclwrite.Token{Type: hclsyntax.TokenColon, Bytes: []byte(":"), SpacesBefore: 1})
	if len(value) > 0 {
		value[0].SpacesBefore = 1
	}
	tokens = append(tokens, value...)
	tokens = append(tokens, &hclwrite.Token{Type: hclsyntax.TokenCBrack, Bytes: []byte("]")})
	return tokens
}

// transformExprWithIterator transforms an expression that is assigned to the schema type t,
// with the bridge mapping m, while rewriting references to the dynamic block iterator.
// <iteratorName>.value.x → __value.x, <iteratorName>.key → __key.
func (ft *fileTransformer) transformExprWithIterator(
	expr hclsyntax.Expression, iteratorName string, t schema.Type, m *bridge.BodyMapping,
) hclwrite.Tokens {
	if e, ok := expr.(*hclsyntax.ScopeTraversalExpr); ok && e.Traversal.RootName() == iteratorName {
		return ft.transformDynamicIteratorTraversal(e.Traversal, iteratorName)
	}
	// For non-traversal expressions, fall through to normal transform.
	// TODO: recursively handle nested expressions containing iterator references.
	return ft.transformTypedExpr(expr, t, m)
}

// transformDynamicIteratorTraversal rewrites a traversal rooted at the dynamic block iterator.
// details.value.key → __value.key
// details.key → __key
func (ft *fileTransformer) transformDynamicIteratorTraversal(trav hcl.Traversal, iteratorName string) hclwrite.Tokens {
	if len(trav) < 2 {
		return hclwrite.TokensForTraversal(trav)
	}
	secondStep, ok := trav[1].(hcl.TraverseAttr)
	if !ok {
		return hclwrite.TokensForTraversal(trav)
	}

	switch secondStep.Name {
	case "value":
		// details.value.x → __value.x
		rewritten := make(hcl.Traversal, 0, len(trav)-1)
		rewritten = append(rewritten, hcl.TraverseRoot{Name: "__value"})
		rewritten = append(rewritten, trav[2:]...)
		return hclwrite.TokensForTraversal(rewritten)
	case "key":
		// details.key → __key
		rewritten := make(hcl.Traversal, 0, len(trav)-1)
		rewritten = append(rewritten, hcl.TraverseRoot{Name: "__key"})
		rewritten = append(rewritten, trav[2:]...)
		return hclwrite.TokensForTraversal(rewritten)
	default:
		return hclwrite.TokensForTraversal(trav)
	}
}

// transformForExpr converts an HCL for-expression to PCL tokens, transforming
// sub-expressions (e.g., var.names → names).
func (ft *fileTransformer) transformForExpr(e *hclsyntax.ForExpr) hclwrite.Tokens {
	var keyTokens, condTokens hclwrite.Tokens
	if e.KeyExpr != nil {
		keyTokens = ft.transformExpr(e.KeyExpr)
	}
	if e.CondExpr != nil {
		condTokens = ft.transformExpr(e.CondExpr)
	}
	return forExprTokens(e.KeyVar, e.ValVar,
		ft.transformExpr(e.CollExpr), keyTokens, ft.transformExpr(e.ValExpr), condTokens, e.Group)
}

// forExprTokens builds the tokens of a PCL for-expression. It is a map
// comprehension when key is not nil. keyVar and cond can be empty.
func forExprTokens(keyVar, valVar string, coll, key, val, cond hclwrite.Tokens, group bool) hclwrite.Tokens {
	var open, close byte
	if key != nil {
		open, close = '{', '}'
	} else {
		open, close = '[', ']'
	}

	tokens := hclwrite.Tokens{
		{Type: hclsyntax.TokenTemplateControl, Bytes: []byte{open}},
		{Type: hclsyntax.TokenIdent, Bytes: []byte("for"), SpacesBefore: 0},
	}

	if keyVar != "" {
		tokens = append(tokens,
			&hclwrite.Token{Type: hclsyntax.TokenIdent, Bytes: []byte(keyVar), SpacesBefore: 1},
			&hclwrite.Token{Type: hclsyntax.TokenComma, Bytes: []byte(",")},
		)
	}

	tokens = append(tokens,
		&hclwrite.Token{Type: hclsyntax.TokenIdent, Bytes: []byte(valVar), SpacesBefore: 1},
		&hclwrite.Token{Type: hclsyntax.TokenIdent, Bytes: []byte("in"), SpacesBefore: 1},
	)

	if len(coll) > 0 {
		coll[0].SpacesBefore = 1
	}
	tokens = append(tokens, coll...)
	tokens = append(tokens, &hclwrite.Token{Type: hclsyntax.TokenColon, Bytes: []byte(":"), SpacesBefore: 1})

	if key != nil {
		if len(key) > 0 {
			key[0].SpacesBefore = 1
		}
		tokens = append(tokens, key...)
		tokens = append(tokens, &hclwrite.Token{Type: hclsyntax.TokenFatArrow, Bytes: []byte("=>"), SpacesBefore: 1})
	}

	if len(val) > 0 {
		val[0].SpacesBefore = 1
	}
	tokens = append(tokens, val...)

	if cond != nil {
		tokens = append(tokens, &hclwrite.Token{Type: hclsyntax.TokenIdent, Bytes: []byte("if"), SpacesBefore: 1})
		if len(cond) > 0 {
			cond[0].SpacesBefore = 1
		}
		tokens = append(tokens, cond...)
	}

	if group {
		tokens = append(tokens, &hclwrite.Token{Type: hclsyntax.TokenEllipsis, Bytes: []byte("..."), SpacesBefore: 0})
	}

	tokens = append(tokens, &hclwrite.Token{Type: hclsyntax.TokenTemplateSeqEnd, Bytes: []byte{close}})
	return tokens
}

// transformForEachExpr converts a for_each expression to PCL range tokens.
// The HCL codegen wraps list values as `{ for __key, __value in <list> : tostring(__key) => __value }`;
// this unwraps that pattern back to just `<list>`. Map expressions pass through unchanged.
func (ft *fileTransformer) transformForEachExpr(expr hclsyntax.Expression) hclwrite.Tokens {
	if forExpr, ok := expr.(*hclsyntax.ForExpr); ok && forExpr.KeyExpr != nil {
		return ft.transformExpr(forExpr.CollExpr)
	}
	return ft.transformTypedExpr(expr, forEachType, nil)
}

// forEachType is the schema type of a `for_each` argument. A key of the map is
// the value of `each.key`, so it is data and not the name of a property. The
// type of an element is unknown.
var forEachType schema.Type = &schema.MapType{}

// unwrapSingletonTupleExpr returns the transformed expression for the single
// element of a 1-element tuple, or the whole transformed tuple otherwise.
// Used to convert TF's list-shaped `replace_triggered_by = [x]` into PCL's
// scalar `replacementTrigger = x`.
func (ft *fileTransformer) unwrapSingletonTupleExpr(expr hclsyntax.Expression) hclwrite.Tokens {
	if tuple, ok := expr.(*hclsyntax.TupleConsExpr); ok && len(tuple.Exprs) == 1 {
		return ft.transformExpr(tuple.Exprs[0])
	}
	return ft.transformExpr(expr)
}

// transformPropertyPathList converts a tuple of property paths (as used in HCL
// for replace_on_changes, ignore_changes, etc.) to the PCL form, translating
// every segment of each path from TF snake_case to its Pulumi property name
// against props (the resource's input or output properties) and m, their
// bridge mapping.
func (ft *fileTransformer) transformPropertyPathList(
	expr hclsyntax.Expression, props []*schema.Property, m *bridge.BodyMapping,
) hclwrite.Tokens {
	tuple, ok := expr.(*hclsyntax.TupleConsExpr)
	if !ok {
		return ft.transformExpr(expr)
	}
	elems := make([]hclwrite.Tokens, 0, len(tuple.Exprs))
	for _, elem := range tuple.Exprs {
		elems = append(elems, ft.transformPropertyPath(elem, props, m))
	}
	return hclwrite.TokensForTuple(elems)
}

// transformPropertyPath converts a single property-path element — a bare
// traversal, or a legacy quoted string — to the Pulumi-cased traversal PCL
// expects.
func (ft *fileTransformer) transformPropertyPath(
	elem hclsyntax.Expression, props []*schema.Property, m *bridge.BodyMapping,
) hclwrite.Tokens {
	if val, diags := elem.Value(nil); !diags.HasErrors() && val.Type() == cty.String {
		trav, tdiags := hclsyntax.ParseTraversalAbs([]byte(val.AsString()), "", hcl.Pos{})
		if tdiags.HasErrors() {
			return hclwrite.TokensForIdentifier(val.AsString())
		}
		return hclwrite.TokensForTraversal(schemaAwarePropertyPath(trav, props, m))
	}
	if st, ok := elem.(*hclsyntax.ScopeTraversalExpr); ok {
		return hclwrite.TokensForTraversal(schemaAwarePropertyPath(st.Traversal, props, m))
	}
	return ft.transformExpr(elem)
}

// schemaAwarePropertyPath converts a property-path traversal from TF to PCL
// form, as [schemaAwareTraversalAttrs] does for a reference. Unlike a
// reference, whose root is a resource logical name, here the root is itself a
// property. m is the bridge mapping of props, or nil.
func schemaAwarePropertyPath(trav hcl.Traversal, props []*schema.Property, m *bridge.BodyMapping) hcl.Traversal {
	root, ok := trav[0].(hcl.TraverseRoot)
	if !ok {
		return trav
	}
	ref := append(hcl.Traversal{root, hcl.TraverseAttr{Name: root.Name, SrcRange: root.SrcRange}}, trav[1:]...)
	ref, _ = schemaAwareTraversalAttrs(ref, props, m)
	first := ref[1].(hcl.TraverseAttr)
	return append(hcl.Traversal{hcl.TraverseRoot{Name: first.Name, SrcRange: first.SrcRange}}, ref[2:]...)
}

// elementObjectProperties returns the object properties reachable from a matched
// property, unwrapping any list/map nesting, or nil when the property has no
// named fields.
func elementObjectProperties(p *schema.Property) []*schema.Property {
	if p == nil {
		return nil
	}
	t := codegen.UnwrapType(p.Type)
	for {
		switch tt := t.(type) {
		case *schema.ArrayType:
			t = codegen.UnwrapType(tt.ElementType)
		case *schema.MapType:
			t = codegen.UnwrapType(tt.ElementType)
		case *schema.ObjectType:
			return tt.Properties
		default:
			return nil
		}
	}
}

// isObjectProperty reports whether the schema types p as one object, as the
// bridge types a MaxItems=1 block.
func isObjectProperty(p *schema.Property) bool {
	if p == nil {
		return false
	}
	_, ok := codegen.UnwrapType(p.Type).(*schema.ObjectType)
	return ok
}

// isBoolLiteral reports whether expr is the literal boolean want.
func isBoolLiteral(expr hclsyntax.Expression, want bool) bool {
	lit, ok := expr.(*hclsyntax.LiteralValueExpr)
	if !ok || lit.Val.Type() != cty.Bool {
		return false
	}
	return lit.Val.True() == want
}

// invertTokens inverts a boolean token expression: if tokens is "!<expr>",
// returns "<expr>"; otherwise returns "!<tokens>".
func invertTokens(tokens hclwrite.Tokens) hclwrite.Tokens {
	if len(tokens) > 0 && tokens[0].Type == hclsyntax.TokenBang {
		rest := tokens[1:]
		if len(rest) > 0 {
			rest[0].SpacesBefore = tokens[0].SpacesBefore
		}
		return rest
	}
	return append(hclwrite.Tokens{{Type: hclsyntax.TokenBang, Bytes: []byte("!")}}, tokens...)
}

// emitPackageBlock writes a PCL "package" block from a workspace.PackageDescriptor.
// A replacement parameterization is labelled with the package alias; an
// extension keeps the base provider's namespace and so is written label-less,
// which is how PCL tells the two apart.
func emitPackageBlock(out *hclwrite.Body, alias string, desc workspace.PackageDescriptor) {
	param := desc.Parameterization
	labels := []string{alias}
	if desc.ExtensionParameterization != nil {
		param, labels = desc.ExtensionParameterization, nil
	}
	if param == nil {
		return
	}
	blk := out.AppendNewBlock("package", labels)
	blk.Body().SetAttributeValue("baseProviderName", cty.StringVal(desc.Name))
	if desc.Version != nil {
		blk.Body().SetAttributeValue("baseProviderVersion", cty.StringVal(desc.Version.String()))
	}
	paramBlk := blk.Body().AppendNewBlock("parameterization", nil)
	paramBlk.Body().SetAttributeValue("name", cty.StringVal(param.Name))
	paramBlk.Body().SetAttributeValue("version", cty.StringVal(param.Version.String()))
	paramBlk.Body().SetAttributeValue("value", cty.StringVal(base64.StdEncoding.EncodeToString(param.Value)))
	out.AppendNewline()
}

// readParameterizationInfos reads sdks/*/hcl.sdk.json files from dir
// and returns parameterized package descriptors keyed by alias.
func readParameterizationInfos(dir string) (map[string]workspace.PackageDescriptor, error) {
	sdksDir := filepath.Join(dir, "sdks")
	entries, err := os.ReadDir(sdksDir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	result := make(map[string]workspace.PackageDescriptor, len(entries))
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
		var d workspace.PackageDescriptor
		if err := json.Unmarshal(data, &d); err != nil {
			errs = append(errs, fmt.Errorf("%q: %w", path, err))
		} else {
			result[entry.Name()] = d
		}
	}
	return result, errors.Join(errs...)
}

// transformFunctionName maps HCL function names to their PCL equivalents.
// pclSupportedFunctions is the set of function names that PCL supports.
// Functions not in this set will be wrapped in notImplemented() during eject.
var pclSupportedFunctions = func() map[string]bool {
	m := map[string]bool{}
	for _, name := range []string{
		"can",
		"cwd",
		"element",
		"entries",
		"fileArchive",
		"fileAsset",
		"filebase64",
		"filebase64sha256",
		"fromBase64",
		"getOutput",
		"join",
		"length",
		"lookup",
		"mimeType",
		"notImplemented",
		"organization",
		"project",
		"pulumiResourceName",
		"pulumiResourceType",
		"range",
		"readDir",
		"readFile",
		"recover",
		"remoteArchive",
		"remoteAsset",
		"rootDirectory",
		"secret",
		"sha1",
		"singleOrNone",
		"split",
		"stack",
		"stringAsset",
		"assetArchive",
		"toBase64",
		"toJSON",
		"try",
		"unsecret",
	} {
		m[name] = true
	}
	return m
}()

func transformFunctionName(name string) string {
	switch name {
	case "base64encode":
		return "toBase64"
	case "base64decode":
		return "fromBase64"
	case "jsonencode":
		return "toJSON"
	case "sensitive":
		return "secret"
	case "one":
		return "singleOrNone"
	case "nonsensitive":
		return "unsecret"
	case "file":
		return "readFile"
	case "fileasset":
		return "fileAsset"
	case "filearchive":
		return "fileArchive"
	case "stringasset":
		return "stringAsset"
	case "assetarchive":
		return "assetArchive"
	case "remoteasset":
		return "remoteAsset"
	case "remotearchive":
		return "remoteArchive"
	case "pulumiresourcename":
		return "pulumiResourceName"
	case "pulumiresourcetype":
		return "pulumiResourceType"
	default:
		return name
	}
}
