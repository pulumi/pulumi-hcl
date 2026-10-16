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

package parser

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/blang/semver"
	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/ext/typeexpr"
	"github.com/hashicorp/hcl/v2/gohcl"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/pulumi/pulumi-hcl/pkg/hcl/ast"
	"github.com/pulumi/pulumi/sdk/v3/go/common/tokens"
	"github.com/pulumi/pulumi/sdk/v3/go/common/util/contract"
	"github.com/zclconf/go-cty/cty"
)

// Parser parses HCL files into an AST configuration.
type Parser struct {
	loader *Loader
}

// NewParser creates a new HCL parser.
func NewParser() *Parser {
	return &Parser{
		loader: NewLoader(),
	}
}

// ParseDirectory parses all HCL files in a directory into a configuration.
func (p *Parser) ParseDirectory(dir string) (*ast.Config, hcl.Diagnostics) {
	files, diags := p.loader.LoadDirectory(dir)
	if diags.HasErrors() {
		return nil, diags
	}

	primary, override := map[string]*hcl.File{}, map[string]*hcl.File{}
	for path, file := range files {
		if isOverrideFile(path) {
			override[path] = file
		} else {
			primary[path] = file
		}
	}

	config, d := p.parseFiles(primary, override)
	return config, diags.Extend(d)
}

// ParseFile parses a single HCL file into a configuration.
func (p *Parser) ParseFile(path string) (*ast.Config, hcl.Diagnostics) {
	file, diags := p.loader.LoadFile(path)
	if diags.HasErrors() {
		return nil, diags
	}

	config, d := p.parseFiles(map[string]*hcl.File{path: file}, nil)
	return config, diags.Extend(d)
}

// ParseSource parses HCL source code into a configuration.
func (p *Parser) ParseSource(filename string, src []byte) (*ast.Config, hcl.Diagnostics) {
	file, diags := p.loader.ParseFile(filename, src)
	if diags.HasErrors() {
		return nil, diags
	}

	config, d := p.parseFiles(map[string]*hcl.File{filename: file}, nil)
	return config, diags.Extend(d)
}

// parseFiles processes all loaded HCL files into a configuration. Blocks in
// the override files amend the blocks the primary files declare rather than
// declaring new ones; see applyOverrides.
func (p *Parser) parseFiles(primary, override map[string]*hcl.File) (*ast.Config, hcl.Diagnostics) {
	config := ast.NewConfig()
	config.Files = make(map[string]*hcl.File, len(primary)+len(override))
	maps.Copy(config.Files, primary)
	maps.Copy(config.Files, override)
	var diags hcl.Diagnostics

	calls := map[string]struct{}{}
	// Files are visited in path order so that the configuration a directory
	// parses to - and the diagnostics it produces - do not depend on map
	// iteration order.
	collect := func(files map[string]*hcl.File) []*hcl.Block {
		var blocks []*hcl.Block
		for _, path := range slices.Sorted(maps.Keys(files)) {
			file := files[path]
			fileCalls, scanned := ast.ProviderFunctionCallsInBody(file.Body)
			if !scanned {
				config.ProviderFunctionCallsIncomplete = true
			}
			for _, name := range fileCalls {
				calls[name] = struct{}{}
			}

			content, contentDiags := file.Body.Content(rootSchema)
			diags = append(diags, contentDiags...)
			if contentDiags.HasErrors() {
				continue
			}

			blocks = append(blocks, content.Blocks...)
		}
		return blocks
	}

	blocks, deferred, overrideDiags := applyOverrides(collect(primary), collect(override))
	diags = append(diags, overrideDiags...)

	for _, block := range blocks {
		diags = append(diags, p.parseBlock(config, block)...)
	}
	for _, block := range deferred {
		diags = append(diags, p.mergeParsedOverride(config, block)...)
	}
	config.ProviderFunctionCalls = slices.Sorted(maps.Keys(calls))

	return config, diags
}

// parseBlock parses a single top-level block.
func (p *Parser) parseBlock(config *ast.Config, block *hcl.Block) hcl.Diagnostics {
	switch block.Type {
	case "terraform":
		return p.parseTerraformBlock(config, block)
	case "language":
		return p.parseLanguageBlock(config, block)
	case "provider":
		return p.parseProviderBlock(config, block)
	case "variable":
		return p.parseVariableBlock(config, block)
	case "locals":
		return p.parseLocalsBlock(config, block)
	case "resource":
		return p.parseResourceBlock(config, block)
	case "data":
		return p.parseDataBlock(config, block)
	case "output":
		return p.parseOutputBlock(config, block)
	case "module":
		return p.parseModuleBlock(config, block)
	case "moved":
		return p.parseMovedBlock(config, block)
	case "removed":
		return p.parseRemovedBlock(config, block)
	case "import":
		return p.parseImportBlock(config, block)
	case "check":
		return p.parseCheckBlock(config, block)
	case "call":
		return p.parseCallBlock(config, block)
	default:
		// Body.Content(rootSchema) already rejected anything not listed in
		// rootSchema, so reaching here means a type was added to rootSchema
		// without a case in this switch.
		contract.Failf("block type %q allowed by rootSchema but not handled", block.Type)
		return nil
	}
}

// parseTerraformBlock parses a terraform block. Multiple terraform blocks
// merge into a single configuration, matching Terraform's behavior.
func (p *Parser) parseTerraformBlock(config *ast.Config, block *hcl.Block) hcl.Diagnostics {
	var diags hcl.Diagnostics

	content, contentDiags := block.Body.Content(terraformSchema)
	diags = append(diags, contentDiags...)

	if len(block.Labels) != 0 {
		diags = append(diags, &hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  "Invalid terraform block",
			Detail:   "The terraform block does not accept any labels.",
			Subject:  &block.DefRange,
		})
		return diags
	}

	tf := config.Terraform
	if tf == nil {
		tf = &ast.Terraform{
			RequiredProviders: make(map[string]*ast.RequiredProvider),
			DeclRange:         block.DefRange,
		}
	}

	if attr, ok := content.Attributes["required_version_range"]; ok {
		if tf.RequiredVersionRange != nil {
			diags = append(diags, &hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  "Duplicate required_version_range attribute",
				Detail:   "required_version_range is already set by a previous terraform block.",
				Subject:  attr.Range.Ptr(),
			})
		} else {
			tf.RequiredVersionRange = attr.Expr
		}
	}

	if attr, ok := content.Attributes["required_version"]; ok {
		diags = append(diags, &hcl.Diagnostic{
			Severity: hcl.DiagWarning,
			Summary:  "Unsupported attribute: required_version",
			Detail: "Pulumi HCL does not enforce required_version. " +
				"Use required_version_range to declare a supported Pulumi version range.",
			Subject: attr.Range.Ptr(),
		})
	}

	if attr, ok := content.Attributes["experiments"]; ok {
		diags = append(diags, &hcl.Diagnostic{
			Severity: hcl.DiagWarning,
			Summary:  "Ignoring terraform experiments argument",
			Detail: "Language experiments are a Terraform concept with no Pulumi HCL " +
				"equivalent; the `experiments` argument is accepted and ignored.",
			Subject: attr.Range.Ptr(),
		})
	}

	for _, subBlock := range content.Blocks {
		switch subBlock.Type {
		case "required_providers":
			providerDiags := p.parseRequiredProviders(tf, subBlock)
			diags = append(diags, providerDiags...)
		case "component":
			if tf.Component != nil {
				diags = append(diags, &hcl.Diagnostic{
					Severity: hcl.DiagError,
					Summary:  "Duplicate component block",
					Detail:   "Only one component block is allowed per terraform block.",
					Subject:  &subBlock.DefRange,
				})
				continue
			}
			comp, compDiags := p.parseTerraformComponentBlock(subBlock)
			diags = append(diags, compDiags...)
			tf.Component = comp
		case "package":
			if tf.Package != nil {
				diags = append(diags, &hcl.Diagnostic{
					Severity: hcl.DiagError,
					Summary:  "Duplicate package block",
					Detail:   "Only one package block is allowed per terraform block.",
					Subject:  &subBlock.DefRange,
				})
				continue
			}
			pkg, pkgDiags := p.parseTerraformPackageBlock(subBlock)
			diags = append(diags, pkgDiags...)
			tf.Package = pkg
		case "backend":
			diags = append(diags, &hcl.Diagnostic{
				Severity: hcl.DiagWarning,
				Summary:  "Ignoring terraform backend block",
				Detail: "Pulumi manages state through its own backend; the terraform `backend` " +
					"block is ignored. Configure the backend via `pulumi login`.",
				Subject: &subBlock.DefRange,
			})
		case "provider_meta":
			diags = append(diags, &hcl.Diagnostic{
				Severity: hcl.DiagWarning,
				Summary:  "Ignoring terraform provider_meta block",
				Detail: "Pulumi providers do not consume Terraform's `provider_meta` " +
					"side channel; the block is ignored.",
				Subject: &subBlock.DefRange,
			})
		}
	}

	config.Terraform = tf
	return diags
}

// parseLanguageBlock parses a language block, OpenTofu's extensible syntax
// for declaring implementation compatibility. Only the `pulumi` argument of a
// compatible_with block is interpreted; arguments addressed to other software
// (such as opentofu) are ignored without validation so the same module can
// declare compatibility with several implementations at once.
func (p *Parser) parseLanguageBlock(config *ast.Config, block *hcl.Block) hcl.Diagnostics {
	content, diags := block.Body.Content(languageSchema)

	if attr, ok := content.Attributes["edition"]; ok {
		const currentEdition = "tofu2024"
		switch kw := hcl.ExprAsKeyword(attr.Expr); {
		case kw == "":
			diags = append(diags, &hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  "Invalid language edition",
				Detail: fmt.Sprintf("The \"edition\" argument expects a bare language edition keyword. "+
					"Pulumi HCL supports only language edition %s, which is the default.", currentEdition),
				Subject: attr.Expr.Range().Ptr(),
			})
		case kw != currentEdition:
			diags = append(diags, &hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  "Unsupported language edition",
				Detail: fmt.Sprintf("Pulumi HCL does not support language edition %q; only %s is supported. "+
					"This module may be intended for use with other software.", kw, currentEdition),
				Subject: attr.Expr.Range().Ptr(),
			})
		}
	}

	if attr, ok := content.Attributes["experiments"]; ok {
		exprs, moreDiags := hcl.ExprList(attr.Expr)
		diags = append(diags, moreDiags...)
		for _, expr := range exprs {
			kw := hcl.ExprAsKeyword(expr)
			if kw == "" {
				diags = append(diags, &hcl.Diagnostic{
					Severity: hcl.DiagError,
					Summary:  "Invalid experiment keyword",
					Detail:   "Elements of \"experiments\" must all be keywords representing active experiments.",
					Subject:  expr.Range().Ptr(),
				})
				continue
			}
			diags = append(diags, &hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  "Unknown experiment keyword",
				Detail:   fmt.Sprintf("There is no current experiment with the keyword %q.", kw),
				Subject:  expr.Range().Ptr(),
			})
		}
	}

	for _, subBlock := range content.Blocks {
		sub, _, subDiags := subBlock.Body.PartialContent(languageCompatibleWithSchema)
		diags = append(diags, subDiags...)
		attr, ok := sub.Attributes["pulumi"]
		if !ok {
			continue
		}
		if config.Language != nil {
			diags = append(diags, &hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  "Duplicate compatible_with pulumi argument",
				Detail: fmt.Sprintf("The Pulumi version constraint was already declared at %s.",
					config.Language.DeclRange),
				Subject: attr.Range.Ptr(),
			})
			continue
		}
		config.Language = &ast.Language{
			CompatibleWithPulumi: attr.Expr,
			DeclRange:            attr.Range,
		}
	}

	return diags
}

// parseTerraformComponentBlock parses a component sub-block within terraform.
func (p *Parser) parseTerraformComponentBlock(block *hcl.Block) (*ast.ComponentBlock, hcl.Diagnostics) {
	content, diags := block.Body.Content(terraformComponentSchema)

	comp := &ast.ComponentBlock{
		Module:    "index",
		DeclRange: block.DefRange,
	}

	if attr, ok := content.Attributes["name"]; ok {
		valDiags := gohcl.DecodeExpression(attr.Expr, nil, &comp.Name)
		diags = append(diags, valDiags...)
		if !valDiags.HasErrors() && !tokens.IsName(comp.Name) {
			diags = append(diags, &hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  "Invalid component name",
				Detail:   fmt.Sprintf("%q is not a valid Pulumi name.", comp.Name),
				Subject:  attr.Expr.Range().Ptr(),
			})
		}
	}

	if attr, ok := content.Attributes["module"]; ok {
		valDiags := gohcl.DecodeExpression(attr.Expr, nil, &comp.Module)
		diags = append(diags, valDiags...)
		if !valDiags.HasErrors() && !tokens.IsName(comp.Module) {
			diags = append(diags, &hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  "Invalid component module",
				Detail:   fmt.Sprintf("%q is not a valid Pulumi name.", comp.Module),
				Subject:  attr.Expr.Range().Ptr(),
			})
		}
	}

	return comp, diags
}

// parseTerraformPackageBlock parses a package sub-block within terraform.
func (p *Parser) parseTerraformPackageBlock(block *hcl.Block) (*ast.PackageBlock, hcl.Diagnostics) {
	content, diags := block.Body.Content(terraformPackageSchema)

	pkg := &ast.PackageBlock{
		Version:   "0.0.0-dev",
		DeclRange: block.DefRange,
	}

	if attr, ok := content.Attributes["name"]; ok {
		valDiags := gohcl.DecodeExpression(attr.Expr, nil, &pkg.Name)
		diags = append(diags, valDiags...)
		if !valDiags.HasErrors() && !tokens.IsName(pkg.Name) {
			diags = append(diags, &hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  "Invalid package name",
				Detail:   fmt.Sprintf("%q is not a valid Pulumi name.", pkg.Name),
				Subject:  attr.Expr.Range().Ptr(),
			})
		}
	}

	if attr, ok := content.Attributes["version"]; ok {
		valDiags := gohcl.DecodeExpression(attr.Expr, nil, &pkg.Version)
		diags = append(diags, valDiags...)
		if !valDiags.HasErrors() {
			if _, err := semver.Parse(pkg.Version); err != nil {
				diags = append(diags, &hcl.Diagnostic{
					Severity: hcl.DiagError,
					Summary:  "Invalid package version",
					Detail:   fmt.Sprintf("%q is not a valid semver version: %s", pkg.Version, err),
					Subject:  attr.Expr.Range().Ptr(),
				})
			}
		}
	}

	return pkg, diags
}

// parseRequiredProviders parses the required_providers block.
func (p *Parser) parseRequiredProviders(tf *ast.Terraform, block *hcl.Block) hcl.Diagnostics {
	var diags hcl.Diagnostics

	attrs, attrDiags := block.Body.JustAttributes()
	diags = append(diags, attrDiags...)

	for name, attr := range attrs {
		if _, exists := tf.RequiredProviders[name]; exists {
			diags = append(diags, &hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  "Duplicate required_providers entry",
				Detail: fmt.Sprintf(
					"required provider %q is already declared by a previous required_providers block.",
					name),
				Subject: attr.Range.Ptr(),
			})
			continue
		}

		provider := &ast.RequiredProvider{
			Name:      name,
			DeclRange: attr.Range,
		}

		// The value can be a string (version only) or an object with
		// `source`, `version`, and/or `configuration_aliases`. We walk
		// the object item-by-item rather than evaluating it whole because
		// `configuration_aliases` holds traversals (e.g. `[simple.foo]`)
		// that can't resolve in the empty parse-time context. pulumi-hcl
		// doesn't act on `configuration_aliases` — the matching provider
		// arrives via the caller's `providers = {...}` block at runtime
		// — so we accept and discard it.
		if pairs, mapDiags := hcl.ExprMap(attr.Expr); !mapDiags.HasErrors() {
			for _, pair := range pairs {
				kw := hcl.ExprAsKeyword(pair.Key)
				switch kw {
				case "source":
					diags = append(diags, gohcl.DecodeExpression(pair.Value, nil, &provider.Source)...)
				case "version":
					diags = append(diags, gohcl.DecodeExpression(pair.Value, nil, &provider.Version)...)
				case "configuration_aliases":
					// Accept and ignore — see comment above.
				}
			}
		} else {
			diags = append(diags, gohcl.DecodeExpression(attr.Expr, nil, &provider.Version)...)
		}

		// Pulumi providers don't go through TF-style constraint resolution, so
		// any version given must be a concrete semver.
		if provider.Version != "" && provider.IsPulumi() {
			if _, err := semver.ParseTolerant(provider.Version); err != nil {
				diags = append(diags, &hcl.Diagnostic{
					Severity: hcl.DiagError,
					Summary:  "Invalid provider version",
					Detail: fmt.Sprintf(
						"%q is not a valid semver version for Pulumi provider %q: %s",
						provider.Version, name, err),
					Subject: attr.Expr.Range().Ptr(),
				})
			}
		}

		tf.RequiredProviders[name] = provider
	}

	return diags
}

// parseProviderBlock parses a provider block.
func (p *Parser) parseProviderBlock(config *ast.Config, block *hcl.Block) hcl.Diagnostics {
	var diags hcl.Diagnostics

	// `call` is reserved for the method-call namespace populated by
	// `call "<res>" "<method>" { ... }` blocks.
	if block.Labels[0] == "call" {
		return hcl.Diagnostics{{
			Severity: hcl.DiagError,
			Summary:  "Reserved provider name",
			Detail: `"call" is reserved as the namespace for method calls on resources ` +
				`(declared via call blocks) and cannot be used as a provider package name.`,
			Subject: block.LabelRanges[0].Ptr(),
		}}
	}

	content, remain, contentDiags := block.Body.PartialContent(providerSchema)
	diags = append(diags, contentDiags...)

	providerConfig, escDiags := mergeEscapeBlock(remain, content.Blocks, "provider-specific", "provider")
	diags = append(diags, escDiags...)

	provider := &ast.Provider{
		Name:      block.Labels[0],
		Config:    providerConfig,
		DeclRange: block.DefRange,
	}

	if attr, ok := content.Attributes["alias"]; ok {
		diags = append(diags, gohcl.DecodeExpression(attr.Expr, nil, &provider.Alias)...)
	}
	if attr, ok := content.Attributes["for_each"]; ok {
		provider.ForEach = attr.Expr
	}
	if provider.ForEach != nil && provider.Alias == "" {
		diags = append(diags, &hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  `Alias required when using "for_each"`,
			Detail:   "The for_each argument is allowed only for provider configurations with an alias.",
			Subject:  provider.ForEach.Range().Ptr(),
		})
	}
	for _, subBlock := range content.Blocks {
		if subBlock.Type == "pulumi" {
			diags = append(diags, p.parsePulumiProviderOptions(subBlock, provider)...)
		}
	}

	key := provider.Key()
	if _, exists := config.Providers[key]; exists {
		diags = append(diags, &hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  "Duplicate provider configuration",
			Detail:   fmt.Sprintf("A provider configuration for %q already exists.", key),
			Subject:  &block.DefRange,
		})
		return diags
	}

	config.Providers[key] = provider
	return diags
}

// parsePulumiProviderOptions parses a provider block's nested `pulumi { }`
// block, extracting Pulumi-specific options onto provider. These options live
// in their own block so they cannot collide with the provider's own
// configuration attributes.
func (p *Parser) parsePulumiProviderOptions(block *hcl.Block, provider *ast.Provider) hcl.Diagnostics {
	content, diags := block.Body.Content(pulumiProviderOptionsSchema)

	if attr, ok := content.Attributes["env_var_mappings"]; ok {
		provider.EnvVarMappings = attr.Expr
	}
	if attr, ok := content.Attributes["plugin_download_url"]; ok {
		provider.PluginDownloadURL = attr.Expr
	}
	if attr, ok := content.Attributes["additional_secret_outputs"]; ok {
		provider.AdditionalSecretOutputs = attr.Expr
	}
	if attr, ok := content.Attributes["version"]; ok {
		provider.Version = attr.Expr
	}

	return diags
}

// parseVariableBlock parses a variable block.
func (p *Parser) parseVariableBlock(config *ast.Config, block *hcl.Block) hcl.Diagnostics {
	var diags hcl.Diagnostics

	name := block.Labels[0]
	if _, exists := config.Variables[name]; exists {
		diags = append(diags, &hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  "Duplicate variable",
			Detail:   fmt.Sprintf("A variable named %q was already declared.", name),
			Subject:  &block.DefRange,
		})
		return diags
	}

	content, contentDiags := block.Body.Content(variableSchema)
	diags = append(diags, contentDiags...)

	variable := &ast.Variable{
		Name:      name,
		Nullable:  true, // Default
		DeclRange: block.DefRange,
	}

	if attr, ok := content.Attributes["type"]; ok {
		variable.Type = attr.Expr
		// The bare keywords `list` and `map` are shorthand forms that the
		// HCL-level type expression parser doesn't include, equivalent to
		// list(any) and map(any).
		switch hcl.ExprAsKeyword(attr.Expr) {
		case "list":
			variable.TypeConstraint = cty.List(cty.DynamicPseudoType)
		case "map":
			variable.TypeConstraint = cty.Map(cty.DynamicPseudoType)
		default:
			ty, defs, typeDiags := typeexpr.TypeConstraintWithDefaults(attr.Expr)
			diags = append(diags, typeDiags...)
			if !typeDiags.HasErrors() {
				variable.TypeConstraint = ty
				variable.TypeDefaults = defs
			}
		}
	}

	if attr, ok := content.Attributes["default"]; ok {
		variable.Default = attr.Expr
	}

	if attr, ok := content.Attributes["description"]; ok {
		diags = append(diags, gohcl.DecodeExpression(attr.Expr, nil, &variable.Description)...)
	}

	if attr, ok := content.Attributes["sensitive"]; ok {
		diags = append(diags, gohcl.DecodeExpression(attr.Expr, nil, &variable.Sensitive)...)
	}

	if attr, ok := content.Attributes["ephemeral"]; ok {
		diags = append(diags, gohcl.DecodeExpression(attr.Expr, nil, &variable.Ephemeral)...)
	}

	if attr, ok := content.Attributes["nullable"]; ok {
		diags = append(diags, gohcl.DecodeExpression(attr.Expr, nil, &variable.Nullable)...)
	}

	for _, subBlock := range content.Blocks {
		if subBlock.Type == "validation" {
			validation, valDiags := p.parseValidationBlock(subBlock)
			diags = append(diags, valDiags...)
			if validation != nil {
				variable.Validations = append(variable.Validations, validation)
			}
		}
	}

	config.Variables[name] = variable
	return diags
}

// parseValidationBlock parses a validation block within a variable.
func (p *Parser) parseValidationBlock(block *hcl.Block) (*ast.Validation, hcl.Diagnostics) {
	content, diags := block.Body.Content(validationSchema)

	validation := &ast.Validation{
		DeclRange: block.DefRange,
	}

	if attr, ok := content.Attributes["condition"]; ok {
		validation.Condition = attr.Expr
	}

	if attr, ok := content.Attributes["error_message"]; ok {
		validation.ErrorMessage = attr.Expr
	}

	return validation, diags
}

// parseLocalsBlock parses a locals block.
func (p *Parser) parseLocalsBlock(config *ast.Config, block *hcl.Block) hcl.Diagnostics {
	var diags hcl.Diagnostics

	attrs, attrDiags := block.Body.JustAttributes()
	diags = append(diags, attrDiags...)

	for name, attr := range attrs {
		if _, exists := config.Locals[name]; exists {
			diags = append(diags, &hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  "Duplicate local value",
				Detail:   fmt.Sprintf("A local value named %q was already declared.", name),
				Subject:  &attr.Range,
			})
			continue
		}

		config.Locals[name] = &ast.Local{
			Name:      name,
			Value:     attr.Expr,
			DeclRange: attr.Range,
		}
	}

	return diags
}

// countForEachConflict returns a diagnostic when a block sets both count and
// for_each, which are mutually exclusive; it returns nil when at most one is
// set. The diagnostic is anchored at the for_each expression.
func countForEachConflict(count, forEach hcl.Expression) *hcl.Diagnostic {
	if count == nil || forEach == nil {
		return nil
	}
	return &hcl.Diagnostic{
		Severity: hcl.DiagError,
		Summary:  `Invalid combination of "count" and "for_each"`,
		Detail: `The "count" and "for_each" meta-arguments are mutually-exclusive. ` +
			`Only one may be used to be explicit about the number of resources to be created.`,
		Subject: forEach.Range().Ptr(),
	}
}

// parseResourceBlock parses a resource block and records it in the config's
// Resources map.
func (p *Parser) parseResourceBlock(config *ast.Config, block *hcl.Block) hcl.Diagnostics {
	resourceType := block.Labels[0]
	name := block.Labels[1]
	key := ast.ResourceKey(resourceType, name)

	if _, exists := config.Resources[key]; exists {
		return hcl.Diagnostics{{
			Severity: hcl.DiagError,
			Summary:  "Duplicate resource",
			Detail:   fmt.Sprintf("A resource %q %q was already declared.", resourceType, name),
			Subject:  &block.DefRange,
		}}
	}

	resource, diags := p.decodeResourceBlock(block)
	config.Resources[key] = resource
	return append(diags, typeLabelDiags("resource", block)...)
}

// parseDataBlock parses a data block and records it in the config's
// DataSources map.
func (p *Parser) parseDataBlock(config *ast.Config, block *hcl.Block) hcl.Diagnostics {
	dataType := block.Labels[0]
	name := block.Labels[1]
	key := ast.ResourceKey(dataType, name)

	if _, exists := config.DataSources[key]; exists {
		return hcl.Diagnostics{{
			Severity: hcl.DiagError,
			Summary:  "Duplicate data source",
			Detail:   fmt.Sprintf("A data source %q %q was already declared.", dataType, name),
			Subject:  &block.DefRange,
		}}
	}

	ds, diags := p.decodeDataBlock(block)
	config.DataSources[key] = ds
	return append(diags, typeLabelDiags("data source", block)...)
}

// typeLabelDiags warns when the type label of block is not an HCL identifier.
// A reference splits at each ".", so nothing can reference such a block.
func typeLabelDiags(kind string, block *hcl.Block) hcl.Diagnostics {
	label := block.Labels[0]
	if hclsyntax.ValidIdentifier(label) {
		return nil
	}
	detail := fmt.Sprintf("The %s type %q is not a valid identifier, so references to this %s cannot work.",
		kind, label, kind)
	if valid, err := MakeValidIdentifier(label); err == nil {
		detail += fmt.Sprintf(" Use %q instead.", valid)
	}
	return hcl.Diagnostics{{
		Severity: hcl.DiagWarning,
		Summary:  "Type name is not a valid identifier",
		Detail:   detail,
		Subject:  block.LabelRanges[0].Ptr(),
	}}
}

// decodeDataBlock decodes a data block body into a DataSource without
// recording it in the config, so callers that scope the block elsewhere
// (e.g. a check's data source) can reuse the same decoding. A data source is
// a provider read, so the managed-resource surface is rejected: lifecycle
// arguments with a dedicated diagnostic, everything else by schema.
func (p *Parser) decodeDataBlock(block *hcl.Block) (*ast.DataSource, hcl.Diagnostics) {
	content, remain, diags := block.Body.PartialContent(dataBlockSchema)

	config, escDiags := mergeEscapeBlock(remain, content.Blocks, "resource-type-specific", "data")
	diags = append(diags, escDiags...)

	ds := &ast.DataSource{
		Type:      block.Labels[0],
		Name:      block.Labels[1],
		Config:    config,
		DeclRange: block.DefRange,
		TypeRange: block.LabelRanges[0],
	}

	if attr, ok := content.Attributes["count"]; ok {
		ds.Count = attr.Expr
	}

	if attr, ok := content.Attributes["for_each"]; ok {
		ds.ForEach = attr.Expr
	}

	if d := countForEachConflict(ds.Count, ds.ForEach); d != nil {
		diags = append(diags, d)
	}

	if attr, ok := content.Attributes["depends_on"]; ok {
		deps, depsDiags := decodeDependsOn(attr)
		diags = append(diags, depsDiags...)
		ds.DependsOn = append(ds.DependsOn, deps...)
	}

	if attr, ok := content.Attributes["provider"]; ok {
		ds.Provider = attr.Expr
	}

	for _, subBlock := range content.Blocks {
		switch subBlock.Type {
		case "pulumi":
			optContent, optDiags := subBlock.Body.Content(pulumiDataOptionsSchema)
			diags = append(diags, optDiags...)
			if attr, ok := optContent.Attributes["parent"]; ok {
				traversal, travDiags := hcl.AbsTraversalForExpr(attr.Expr)
				diags = append(diags, travDiags...)
				if traversal != nil {
					ds.ResourceParent = traversal
				}
			}
			if attr, ok := optContent.Attributes["version"]; ok {
				ds.Version = attr.Expr
			}
			if attr, ok := optContent.Attributes["plugin_download_url"]; ok {
				ds.PluginDownloadURL = attr.Expr
			}
		case "lifecycle":
			lcResult, lcDiags := p.parseLifecycleBlock(subBlock)
			diags = append(diags, lcDiags...)
			diags = append(diags, dataLifecycleArgDiags(lcResult.Lifecycle, subBlock)...)
			ds.Preconditions = append(ds.Preconditions, lcResult.Preconditions...)
			ds.Postconditions = append(ds.Postconditions, lcResult.Postconditions...)
		case "timeouts":
			timeouts, timeoutsDiags := p.parseTimeoutsBlock(subBlock)
			diags = append(diags, timeoutsDiags...)
			ds.Timeouts = timeouts
		case "connection", "provisioner":
			diags = append(diags, &hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  "Unsupported block type",
				Detail:   fmt.Sprintf("Blocks of type %q are not valid for data resources.", subBlock.Type),
				Subject:  &subBlock.DefRange,
			})
		}
	}

	return ds, diags
}

// dataLifecycleArgDiags rejects every lifecycle argument on a data block;
// only precondition/postcondition blocks apply to data resources.
func dataLifecycleArgDiags(lc *ast.Lifecycle, block *hcl.Block) hcl.Diagnostics {
	if lc == nil {
		return nil
	}
	var diags hcl.Diagnostics
	for _, arg := range []struct {
		name string
		set  bool
	}{
		{"create_before_destroy", lc.CreateBeforeDestroy != nil},
		{"prevent_destroy", lc.PreventDestroy != nil},
		{"ignore_changes", len(lc.IgnoreChanges) > 0 || lc.IgnoreAllChanges},
		{"replace_triggered_by", len(lc.ReplaceTriggeredBy) > 0},
	} {
		if arg.set {
			diags = append(diags, &hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  "Invalid data resource lifecycle argument",
				Detail: fmt.Sprintf("The lifecycle argument %q is defined only for managed resources "+
					"(\"resource\" blocks), and is not valid for data resources.", arg.name),
				Subject: block.DefRange.Ptr(),
			})
		}
	}
	return diags
}

// decodeResourceBlock decodes a resource block body into a Resource without
// recording it in the config.
func (p *Parser) decodeResourceBlock(block *hcl.Block) (*ast.Resource, hcl.Diagnostics) {
	var diags hcl.Diagnostics

	resourceType := block.Labels[0]
	name := block.Labels[1]

	content, remain, contentDiags := block.Body.PartialContent(resourceSchema)
	diags = append(diags, contentDiags...)

	config, escDiags := mergeEscapeBlock(remain, content.Blocks, "resource-type-specific", "resource")
	diags = append(diags, escDiags...)

	resource := &ast.Resource{
		Type:      resourceType,
		Name:      name,
		Config:    config,
		DeclRange: block.DefRange,
		TypeRange: block.LabelRanges[0],
	}

	// Parse meta-arguments
	if attr, ok := content.Attributes["count"]; ok {
		resource.Count = attr.Expr
	}

	if attr, ok := content.Attributes["for_each"]; ok {
		resource.ForEach = attr.Expr
	}

	if d := countForEachConflict(resource.Count, resource.ForEach); d != nil {
		diags = append(diags, d)
	}

	if attr, ok := content.Attributes["depends_on"]; ok {
		deps, depsDiags := decodeDependsOn(attr)
		diags = append(diags, depsDiags...)
		resource.DependsOn = append(resource.DependsOn, deps...)
	}

	if attr, ok := content.Attributes["provider"]; ok {
		resource.Provider = attr.Expr
	}

	if attr, ok := content.Attributes["providers"]; ok {
		exprs, exprDiags := hcl.ExprList(attr.Expr)
		diags = append(diags, exprDiags...)
		for _, expr := range exprs {
			traversal, travDiags := hcl.AbsTraversalForExpr(expr)
			diags = append(diags, travDiags...)
			if traversal != nil {
				resource.Providers = append(resource.Providers, traversal)
			}
		}
	}

	// Parse nested blocks
	for _, subBlock := range content.Blocks {
		switch subBlock.Type {
		case "pulumi":
			diags = append(diags, p.parsePulumiResourceOptions(subBlock, resource)...)
		case "lifecycle":
			lcResult, lcDiags := p.parseLifecycleBlock(subBlock)
			diags = append(diags, lcDiags...)
			resource.Lifecycle = lcResult.Lifecycle
			resource.Preconditions = append(resource.Preconditions, lcResult.Preconditions...)
			resource.Postconditions = append(resource.Postconditions, lcResult.Postconditions...)
		case "connection":
			conn, connDiags := p.parseConnectionBlock(subBlock)
			diags = append(diags, connDiags...)
			resource.Connection = conn
		case "provisioner":
			prov, provDiags := p.parseProvisionerBlock(subBlock)
			diags = append(diags, provDiags...)
			if prov != nil {
				resource.Provisioners = append(resource.Provisioners, prov)
			}
		case "timeouts":
			timeouts, timeoutsDiags := p.parseTimeoutsBlock(subBlock)
			diags = append(diags, timeoutsDiags...)
			resource.Timeouts = timeouts
		}
	}

	return resource, diags
}

// parsePulumiResourceOptions parses a resource or data block's nested
// `pulumi { }` block, extracting Pulumi-specific options onto resource.
func (p *Parser) parsePulumiResourceOptions(block *hcl.Block, resource *ast.Resource) hcl.Diagnostics {
	content, diags := block.Body.Content(pulumiResourceOptionsSchema)

	if attr, ok := content.Attributes["name"]; ok {
		resource.PulumiName = attr.Expr
	}

	if attr, ok := content.Attributes["parent"]; ok {
		traversal, travDiags := hcl.AbsTraversalForExpr(attr.Expr)
		diags = append(diags, travDiags...)
		if traversal != nil {
			resource.ResourceParent = traversal
		}
	}

	if attr, ok := content.Attributes["additional_secret_outputs"]; ok {
		exprs, exprDiags := hcl.ExprList(attr.Expr)
		diags = append(diags, exprDiags...)
		for _, expr := range exprs {
			traversal, travDiags := hcl.RelTraversalForExpr(expr)
			diags = append(diags, travDiags...)
			if traversal != nil {
				resource.AdditionalSecretOutputs = append(resource.AdditionalSecretOutputs, traversal)
			}
		}
	}

	if attr, ok := content.Attributes["protect"]; ok {
		resource.Protect = attr.Expr
	}

	if attr, ok := content.Attributes["retain_on_delete"]; ok {
		resource.RetainOnDelete = attr.Expr
	}

	if attr, ok := content.Attributes["deleted_with"]; ok {
		traversal, travDiags := hcl.AbsTraversalForExpr(attr.Expr)
		diags = append(diags, travDiags...)
		if traversal != nil {
			resource.DeletedWith = traversal
		}
	}

	if attr, ok := content.Attributes["replace_with"]; ok {
		exprs, exprDiags := hcl.ExprList(attr.Expr)
		diags = append(diags, exprDiags...)
		for _, expr := range exprs {
			traversal, travDiags := hcl.AbsTraversalForExpr(expr)
			diags = append(diags, travDiags...)
			if traversal != nil {
				resource.ReplaceWith = append(resource.ReplaceWith, traversal)
			}
		}
	}

	if attr, ok := content.Attributes["hide_diffs"]; ok {
		exprs, exprDiags := hcl.ExprList(attr.Expr)
		diags = append(diags, exprDiags...)
		for _, expr := range exprs {
			traversal, travDiags := hcl.RelTraversalForExpr(expr)
			diags = append(diags, travDiags...)
			if traversal != nil {
				resource.HideDiff = append(resource.HideDiff, traversal)
			}
		}
	}

	if attr, ok := content.Attributes["replace_on_changes"]; ok {
		exprs, exprDiags := hcl.ExprList(attr.Expr)
		diags = append(diags, exprDiags...)
		for _, expr := range exprs {
			traversal, travDiags := hcl.RelTraversalForExpr(expr)
			diags = append(diags, travDiags...)
			if traversal != nil {
				resource.ReplaceOnChanges = append(resource.ReplaceOnChanges, traversal)
			}
		}
	}

	if attr, ok := content.Attributes["import_id"]; ok {
		diags = append(diags, gohcl.DecodeExpression(attr.Expr, nil, &resource.ImportID)...)
	}

	if attr, ok := content.Attributes["env_var_mappings"]; ok {
		resource.EnvVarMappings = attr.Expr
	}

	if attr, ok := content.Attributes["version"]; ok {
		resource.Version = attr.Expr
	}

	if attr, ok := content.Attributes["plugin_download_url"]; ok {
		resource.PluginDownloadURL = attr.Expr
	}

	if attr, ok := content.Attributes["aliases"]; ok {
		resource.Aliases = attr.Expr
	}

	return diags
}

// lifecycleResult contains the parsed lifecycle block plus any preconditions/postconditions.
type lifecycleResult struct {
	Lifecycle      *ast.Lifecycle
	Preconditions  []*ast.CheckRule
	Postconditions []*ast.CheckRule
}

// parseLifecycleBlock parses a lifecycle block.
func (p *Parser) parseLifecycleBlock(block *hcl.Block) (*lifecycleResult, hcl.Diagnostics) {
	content, diags := block.Body.Content(lifecycleSchema)

	lifecycle := &ast.Lifecycle{
		DeclRange: block.DefRange,
	}

	if attr, ok := content.Attributes["create_before_destroy"]; ok {
		diags = append(diags, gohcl.DecodeExpression(attr.Expr, nil, &lifecycle.CreateBeforeDestroy)...)
	}

	if attr, ok := content.Attributes["prevent_destroy"]; ok {
		lifecycle.PreventDestroy = attr.Expr
	}

	if attr, ok := content.Attributes["ignore_changes"]; ok {
		// Check for "all" keyword
		kw := hcl.ExprAsKeyword(attr.Expr)
		if kw == "all" {
			lifecycle.IgnoreAllChanges = true
		} else {
			// Parse as list of traversals
			exprs, exprDiags := hcl.ExprList(attr.Expr)
			diags = append(diags, exprDiags...)
			for _, expr := range exprs {
				expr, shimDiags := shimTraversalInString(expr)
				diags = append(diags, shimDiags...)

				traversal, travDiags := hcl.RelTraversalForExpr(expr)
				diags = append(diags, travDiags...)
				if traversal != nil {
					lifecycle.IgnoreChanges = append(lifecycle.IgnoreChanges, traversal)
				}
			}
		}
	}

	if attr, ok := content.Attributes["replace_triggered_by"]; ok {
		exprs, exprDiags := hcl.ExprList(attr.Expr)
		diags = append(diags, exprDiags...)
		lifecycle.ReplaceTriggeredBy = exprs
	}

	result := &lifecycleResult{
		Lifecycle: lifecycle,
	}

	// Parse preconditions and postconditions
	for _, subBlock := range content.Blocks {
		switch subBlock.Type {
		case "precondition":
			rule, ruleDiags := p.parseCheckRule(subBlock)
			diags = append(diags, ruleDiags...)
			if rule != nil {
				result.Preconditions = append(result.Preconditions, rule)
			}
		case "postcondition":
			rule, ruleDiags := p.parseCheckRule(subBlock)
			diags = append(diags, ruleDiags...)
			if rule != nil {
				result.Postconditions = append(result.Postconditions, rule)
			}
		}
	}

	return result, diags
}

// parseConnectionBlock parses a connection block. The block body is stored
// verbatim on the Connection so the runtime can re-evaluate every attribute
// (including ones the parser inspected like `type`) against the live HCL
// eval context — PartialContent strips recognized attrs from `remain`, so
// using it here would hide `host`, `user`, etc. from the runtime.
func (p *Parser) parseConnectionBlock(block *hcl.Block) (*ast.Connection, hcl.Diagnostics) {
	content, _, diags := block.Body.PartialContent(connectionSchema)

	conn := &ast.Connection{
		Config:    block.Body,
		DeclRange: block.DefRange,
	}

	if attr, ok := content.Attributes["type"]; ok {
		diags = append(diags, gohcl.DecodeExpression(attr.Expr, nil, &conn.Type)...)
	}

	if conn.Type == "" {
		conn.Type = "ssh" // Default
	}

	return conn, diags
}

// exprAsKeywordOrString returns the value of expr if it's either a bare
// keyword (e.g. `continue`) or a string literal (`"continue"`). TF accepts
// both forms for `when` and `on_failure`; we match that.
func exprAsKeywordOrString(expr hcl.Expression) string {
	if kw := hcl.ExprAsKeyword(expr); kw != "" {
		return kw
	}
	val, diags := expr.Value(nil)
	if diags.HasErrors() || !val.IsKnown() || val.IsNull() {
		return ""
	}
	if val.Type() != cty.String {
		return ""
	}
	return val.AsString()
}

// mergeEscapeBlock merges the special `_` escaping block (if any in blocks)
// into config, so arguments whose names collide with meta-arguments can still
// be written unambiguously. what and blockKind fill the duplicate-block error
// message: arguments in the escaping block are interpreted as <what>, and each
// <blockKind> block allows only one escaping block.
func mergeEscapeBlock(config hcl.Body, blocks hcl.Blocks, what, blockKind string) (hcl.Body, hcl.Diagnostics) {
	var diags hcl.Diagnostics
	var seen *hcl.Block
	for _, block := range blocks {
		if block.Type != "_" {
			continue
		}
		if seen != nil {
			diags = append(diags, &hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  "Duplicate escaping block",
				Detail: fmt.Sprintf(
					"The special block type \"_\" can be used to force particular arguments to be interpreted as %s rather than as meta-arguments, but each %s block can have only one such block. The first escaping block was at %s.",
					what, blockKind, seen.DefRange,
				),
				Subject: &block.DefRange,
			})
			continue
		}
		seen = block
		config = &ast.EscapedBody{
			Body:   hcl.MergeBodies([]hcl.Body{config, block.Body}),
			Base:   config,
			Escape: block.Body,
		}
	}
	return config, diags
}

// parseProvisionerBlock parses a provisioner block.
func (p *Parser) parseProvisionerBlock(block *hcl.Block) (*ast.Provisioner, hcl.Diagnostics) {
	content, remain, diags := block.Body.PartialContent(provisionerSchema)

	config, escDiags := mergeEscapeBlock(remain, content.Blocks, "provisioner-type-specific", "provisioner")
	diags = append(diags, escDiags...)

	provisioner := &ast.Provisioner{
		Type:      block.Labels[0],
		Config:    config,
		When:      "create", // Default
		OnFailure: "fail",   // Default
		DeclRange: block.DefRange,
	}

	if attr, ok := content.Attributes["when"]; ok {
		if kw := exprAsKeywordOrString(attr.Expr); kw != "" {
			if kw != "create" && kw != "destroy" {
				diags = append(diags, &hcl.Diagnostic{
					Severity: hcl.DiagError,
					Summary:  "Invalid \"when\" value",
					Detail:   fmt.Sprintf("Expected \"create\" or \"destroy\", got %q.", kw),
					Subject:  attr.Expr.Range().Ptr(),
				})
			} else {
				provisioner.When = kw
			}
		}
	}

	if attr, ok := content.Attributes["on_failure"]; ok {
		if kw := exprAsKeywordOrString(attr.Expr); kw != "" {
			if kw != "fail" && kw != "continue" {
				diags = append(diags, &hcl.Diagnostic{
					Severity: hcl.DiagError,
					Summary:  "Invalid \"on_failure\" value",
					Detail:   fmt.Sprintf("Expected \"fail\" or \"continue\", got %q.", kw),
					Subject:  attr.Expr.Range().Ptr(),
				})
			} else {
				provisioner.OnFailure = kw
			}
		}
	}

	// Parse connection override
	for _, subBlock := range content.Blocks {
		if subBlock.Type == "connection" {
			conn, connDiags := p.parseConnectionBlock(subBlock)
			diags = append(diags, connDiags...)
			provisioner.Connection = conn
		}
	}

	return provisioner, diags
}

// parseTimeoutsBlock parses a timeouts block.
func (p *Parser) parseTimeoutsBlock(block *hcl.Block) (*ast.Timeouts, hcl.Diagnostics) {
	content, diags := block.Body.Content(timeoutsSchema)

	timeouts := &ast.Timeouts{
		DeclRange: block.DefRange,
	}

	if attr, ok := content.Attributes["create"]; ok {
		timeouts.Create = attr.Expr
	}

	if attr, ok := content.Attributes["read"]; ok {
		timeouts.Read = attr.Expr
	}

	if attr, ok := content.Attributes["update"]; ok {
		timeouts.Update = attr.Expr
	}

	if attr, ok := content.Attributes["delete"]; ok {
		timeouts.Delete = attr.Expr
	}

	if attr, ok := content.Attributes["default"]; ok {
		timeouts.Default = attr.Expr
	}

	return timeouts, diags
}

// parseOutputBlock parses an output block.
func (p *Parser) parseOutputBlock(config *ast.Config, block *hcl.Block) hcl.Diagnostics {
	var diags hcl.Diagnostics

	name := block.Labels[0]
	if _, exists := config.Outputs[name]; exists {
		diags = append(diags, &hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  "Duplicate output",
			Detail:   fmt.Sprintf("An output named %q was already declared.", name),
			Subject:  &block.DefRange,
		})
		return diags
	}

	content, contentDiags := block.Body.Content(outputSchema)
	diags = append(diags, contentDiags...)

	output := &ast.Output{
		Name:      name,
		DeclRange: block.DefRange,
	}

	if attr, ok := content.Attributes["value"]; ok {
		output.Value = attr.Expr
	}

	if attr, ok := content.Attributes["description"]; ok {
		diags = append(diags, gohcl.DecodeExpression(attr.Expr, nil, &output.Description)...)
	}

	if attr, ok := content.Attributes["sensitive"]; ok {
		diags = append(diags, gohcl.DecodeExpression(attr.Expr, nil, &output.Sensitive)...)
	}

	if attr, ok := content.Attributes["ephemeral"]; ok {
		diags = append(diags, gohcl.DecodeExpression(attr.Expr, nil, &output.Ephemeral)...)
	}

	if attr, ok := content.Attributes["depends_on"]; ok {
		deps, depsDiags := decodeDependsOn(attr)
		diags = append(diags, depsDiags...)
		output.DependsOn = append(output.DependsOn, deps...)
	}

	// Parse preconditions
	for _, subBlock := range content.Blocks {
		if subBlock.Type == "precondition" {
			rule, ruleDiags := p.parseCheckRule(subBlock)
			diags = append(diags, ruleDiags...)
			if rule != nil {
				output.Preconditions = append(output.Preconditions, rule)
			}
		}
	}

	config.Outputs[name] = output
	return diags
}

// parseCheckBlock parses a top-level check block. Each assert block reuses the
// condition/error_message schema shared with preconditions. A check may declare
// at most one scoped data source, which is decoded onto the check and read, at
// evaluation time, into a context visible only to the check's assertions.
func (p *Parser) parseCheckBlock(config *ast.Config, block *hcl.Block) hcl.Diagnostics {
	var diags hcl.Diagnostics

	name := block.Labels[0]
	if _, exists := config.Checks[name]; exists {
		diags = append(diags, &hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  "Duplicate check",
			Detail:   fmt.Sprintf("A check named %q was already declared.", name),
			Subject:  &block.DefRange,
		})
		return diags
	}

	content, contentDiags := block.Body.Content(checkSchema)
	diags = append(diags, contentDiags...)

	check := &ast.Check{
		Name:      name,
		DeclRange: block.DefRange,
	}

	for _, subBlock := range content.Blocks {
		switch subBlock.Type {
		case "assert":
			rule, ruleDiags := p.parseCheckRule(subBlock)
			diags = append(diags, ruleDiags...)
			if rule != nil {
				check.Asserts = append(check.Asserts, rule)
			}
		case "data":
			if check.DataResource != nil {
				diags = append(diags, &hcl.Diagnostic{
					Severity: hcl.DiagError,
					Summary:  "Multiple data resource blocks",
					Detail: fmt.Sprintf("This check block already has a data resource defined at %s.",
						check.DataResource.DeclRange),
					Subject: &subBlock.DefRange,
				})
				continue
			}
			ds, dsDiags := p.decodeDataBlock(subBlock)
			diags = append(diags, dsDiags...)
			check.DataResource = ds
		}
	}

	if len(check.Asserts) == 0 {
		diags = append(diags, &hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  "Missing assert block",
			Detail:   "A check block must contain at least one assert block.",
			Subject:  &block.DefRange,
		})
		return diags
	}

	config.Checks[name] = check
	return diags
}

// parseCheckRule parses a precondition/postcondition block.
func (p *Parser) parseCheckRule(block *hcl.Block) (*ast.CheckRule, hcl.Diagnostics) {
	content, diags := block.Body.Content(preconditionSchema)

	rule := &ast.CheckRule{
		DeclRange: block.DefRange,
	}

	if attr, ok := content.Attributes["condition"]; ok {
		rule.Condition = attr.Expr
	}

	if attr, ok := content.Attributes["error_message"]; ok {
		rule.ErrorMessage = attr.Expr
	}

	return rule, diags
}

// parseModuleBlock parses a module block.
func (p *Parser) parseModuleBlock(config *ast.Config, block *hcl.Block) hcl.Diagnostics {
	var diags hcl.Diagnostics

	name := block.Labels[0]
	if _, exists := config.Modules[name]; exists {
		diags = append(diags, &hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  "Duplicate module",
			Detail:   fmt.Sprintf("A module named %q was already declared.", name),
			Subject:  &block.DefRange,
		})
		return diags
	}

	content, remain, contentDiags := block.Body.PartialContent(moduleSchema)
	diags = append(diags, contentDiags...)

	moduleConfig, escDiags := mergeEscapeBlock(remain, content.Blocks, "module input variables", "module")
	diags = append(diags, escDiags...)

	module := &ast.Module{
		Name:      name,
		Config:    moduleConfig,
		Providers: make(map[string]hcl.Expression),
		DeclRange: block.DefRange,
	}

	if attr, ok := content.Attributes["source"]; ok {
		diags = append(diags, gohcl.DecodeExpression(attr.Expr, nil, &module.Source)...)
	}

	if attr, ok := content.Attributes["version"]; ok {
		diags = append(diags, gohcl.DecodeExpression(attr.Expr, nil, &module.Version)...)
	}

	if attr, ok := content.Attributes["count"]; ok {
		module.Count = attr.Expr
	}

	if attr, ok := content.Attributes["for_each"]; ok {
		module.ForEach = attr.Expr
	}

	if d := countForEachConflict(module.Count, module.ForEach); d != nil {
		diags = append(diags, d)
	}

	if attr, ok := content.Attributes["depends_on"]; ok {
		deps, depsDiags := decodeDependsOn(attr)
		diags = append(diags, depsDiags...)
		module.DependsOn = append(module.DependsOn, deps...)
	}

	// Parse providers map. Keys may be bare provider names ("simple"),
	// dotted local aliases ("simple.foo"), or quoted strings. HCL flags a
	// bare dotted form as "Ambiguous attribute key" at Value() time, so
	// resolve via AbsTraversalForExpr first (it goes through the
	// ObjectConsKeyExpr wrapper without triggering that diagnostic).
	if attr, ok := content.Attributes["providers"]; ok {
		pairs, pairDiags := hcl.ExprMap(attr.Expr)
		diags = append(diags, pairDiags...)
		for _, pair := range pairs {
			key, keyDiags := providersMapKey(pair.Key)
			diags = append(diags, keyDiags...)
			if key == "" {
				continue
			}
			module.Providers[key] = pair.Value
		}
	}

	for _, subBlock := range content.Blocks {
		if subBlock.Type == "pulumi" {
			optContent, optDiags := subBlock.Body.Content(pulumiModuleOptionsSchema)
			diags = append(diags, optDiags...)
			if attr, ok := optContent.Attributes["name"]; ok {
				module.PulumiName = attr.Expr
			}
			if attr, ok := optContent.Attributes["protect"]; ok {
				module.Protect = attr.Expr
			}
		}
	}

	config.Modules[name] = module
	return diags
}

// providersMapKey resolves a key from a module-call's `providers = {...}`
// map to its canonical "name" or "name.alias" string form. Reports a
// diagnostic when the key isn't a traversal (bare or dotted) or a static
// string.
func providersMapKey(key hcl.Expression) (string, hcl.Diagnostics) {
	if trav, td := hcl.AbsTraversalForExpr(key); !td.HasErrors() && len(trav) > 0 {
		var name strings.Builder
		name.WriteString(trav.RootName())
		for i := 1; i < len(trav); i++ {
			attr, ok := trav[i].(hcl.TraverseAttr)
			if !ok {
				return "", hcl.Diagnostics{&hcl.Diagnostic{
					Severity: hcl.DiagError,
					Summary:  "Invalid provider map key",
					Detail:   "Each key in a module's `providers = {...}` map must be a local provider name, optionally followed by a single alias step (e.g. `simple` or `simple.foo`).",
					Subject:  key.Range().Ptr(),
				}}
			}
			name.WriteString("." + attr.Name)
		}
		return name.String(), nil
	}
	v, vd := key.Value(nil)
	if vd.HasErrors() {
		return "", vd
	}
	if v.Type() != cty.String || v.IsNull() {
		return "", hcl.Diagnostics{&hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  "Invalid provider map key",
			Detail:   "Each key in a module's `providers = {...}` map must be a local provider name, optionally followed by a single alias step (e.g. `simple` or `simple.foo`), or a string literal of that form.",
			Subject:  key.Range().Ptr(),
		}}
	}
	return v.AsString(), nil
}

// parseMovedBlock parses a moved block.
func (p *Parser) parseMovedBlock(config *ast.Config, block *hcl.Block) hcl.Diagnostics {
	content, diags := block.Body.Content(movedSchema)

	moved := &ast.Moved{
		DeclRange: block.DefRange,
	}

	if attr, ok := content.Attributes["from"]; ok {
		traversal, travDiags := hcl.AbsTraversalForExpr(attr.Expr)
		diags = append(diags, travDiags...)
		if traversal != nil {
			moved.From = traversal
		}
	}

	if attr, ok := content.Attributes["to"]; ok {
		traversal, travDiags := hcl.AbsTraversalForExpr(attr.Expr)
		diags = append(diags, travDiags...)
		if traversal != nil {
			moved.To = traversal
		}
	}

	config.Moved = append(config.Moved, moved)
	return diags
}

// parseRemovedBlock parses a removed block. Only destroy = true is supported:
// forgetting a resource (destroy = false, or an omitted lifecycle block) means
// dropping it from state without destroying it, which has no Pulumi engine
// mapping, so those forms are rejected rather than silently destroying or
// retaining the resource.
func (p *Parser) parseRemovedBlock(config *ast.Config, block *hcl.Block) hcl.Diagnostics {
	content, diags := block.Body.Content(removedSchema)

	removed := &ast.Removed{
		DeclRange: block.DefRange,
	}

	if attr, ok := content.Attributes["from"]; ok {
		traversal, travDiags := hcl.AbsTraversalForExpr(attr.Expr)
		diags = append(diags, travDiags...)
		for _, step := range traversal {
			if _, isIndex := step.(hcl.TraverseIndex); isIndex {
				diags = append(diags, &hcl.Diagnostic{
					Severity: hcl.DiagError,
					Summary:  "Resource instance keys not allowed",
					Detail:   "A removed block's \"from\" address must not include instance keys; the block applies to every instance of the resource.",
					Subject:  attr.Expr.Range().Ptr(),
				})
				return diags
			}
		}
		if len(traversal) > 0 {
			if traversal.RootName() == "data" {
				diags = append(diags, &hcl.Diagnostic{
					Severity: hcl.DiagError,
					Summary:  "Invalid \"from\" address",
					Detail:   "Data sources are not tracked in state, so they cannot be used in a removed block; delete the data block instead.",
					Subject:  attr.Expr.Range().Ptr(),
				})
				return diags
			}
			addr, ok := ast.ParseTargetAddr(traversal)
			if !ok {
				diags = append(diags, &hcl.Diagnostic{
					Severity: hcl.DiagError,
					Summary:  "Invalid \"from\" address",
					Detail:   "A removed block's \"from\" address must be a resource address (type.name) or a module address (module.name).",
					Subject:  attr.Expr.Range().Ptr(),
				})
				return diags
			}
			removed.From = addr
		}
	}

	destroy := false
	var seenLifecycle *hcl.Block
	for _, sub := range content.Blocks {
		switch sub.Type {
		case "lifecycle":
			if seenLifecycle != nil {
				diags = append(diags, &hcl.Diagnostic{
					Severity: hcl.DiagError,
					Summary:  "Duplicate lifecycle block",
					Detail:   fmt.Sprintf("The removed block already has a lifecycle block at %s.", seenLifecycle.DefRange),
					Subject:  &sub.DefRange,
				})
				continue
			}
			seenLifecycle = sub

			lcContent, lcDiags := sub.Body.Content(removedLifecycleSchema)
			diags = append(diags, lcDiags...)
			if attr, ok := lcContent.Attributes["destroy"]; ok {
				diags = append(diags, gohcl.DecodeExpression(attr.Expr, nil, &destroy)...)
			}
		case "provisioner":
			prov, provDiags := p.parseProvisionerBlock(sub)
			diags = append(diags, provDiags...)
			if prov == nil {
				continue
			}
			if prov.When != "destroy" {
				diags = append(diags, &hcl.Diagnostic{
					Severity: hcl.DiagError,
					Summary:  "Invalid \"removed.provisioner\" block",
					Detail:   "Removed blocks can only contain destroy provisioners; \"when = destroy\" is required.",
					Subject:  &sub.DefRange,
				})
				continue
			}
			removed.Provisioners = append(removed.Provisioners, prov)
		}
	}

	if diags.HasErrors() {
		return diags
	}

	removed.Destroy = destroy
	if !removed.Destroy {
		diags = append(diags, &hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  "Unsupported removed block",
			Detail: "Removing a resource from state without destroying it is not supported; " +
				"set `lifecycle { destroy = true }` to destroy the resource, or run " +
				"`pulumi state delete <urn>` to remove it from state.",
			Subject: &block.DefRange,
		})
	}

	if len(removed.Provisioners) > 0 && removed.From.Type == "" {
		diags = append(diags, &hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  "Invalid removed block",
			Detail:   "Removed blocks containing provisioners can only target resources, not modules.",
			Subject:  &block.DefRange,
		})
		return diags
	}

	config.Removed = append(config.Removed, removed)
	return diags
}

// parseCallBlock parses a call block.
func (p *Parser) parseCallBlock(config *ast.Config, block *hcl.Block) hcl.Diagnostics {
	var diags hcl.Diagnostics

	resourceName := block.Labels[0]
	methodName := block.Labels[1]
	key := ast.CallKey(resourceName, methodName)

	if _, exists := config.Calls[key]; exists {
		diags = append(diags, &hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  "Duplicate call block",
			Detail:   fmt.Sprintf("A call block for %q.%q was already declared.", resourceName, methodName),
			Subject:  &block.DefRange,
		})
		return diags
	}

	config.Calls[key] = &ast.Call{
		ResourceName: resourceName,
		MethodName:   methodName,
		Config:       block.Body,
		DeclRange:    block.DefRange,
	}
	return diags
}

// parseImportBlock parses an import block.
func (p *Parser) parseImportBlock(config *ast.Config, block *hcl.Block) hcl.Diagnostics {
	content, diags := block.Body.Content(importSchema)

	imp := &ast.Import{
		DeclRange: block.DefRange,
	}

	if attr, ok := content.Attributes["to"]; ok {
		diags = append(diags, validateImportToExpr(attr.Expr)...)
		imp.To = attr.Expr
	}

	if attr, ok := content.Attributes["id"]; ok {
		imp.Id = attr.Expr
	}

	if attr, ok := content.Attributes["provider"]; ok {
		imp.Provider = attr.Expr
	}

	if attr, ok := content.Attributes["for_each"]; ok {
		imp.ForEach = attr.Expr
	}

	config.Imports = append(config.Imports, imp)
	return diags
}

// validateImportToExpr checks that an import block's `to` expression has the
// shape of a resource address. Traversal parts must be static, but index keys
// may be arbitrary expressions (e.g. each.key), so they are not inspected
// here; they are evaluated at runtime.
func validateImportToExpr(expr hcl.Expression) hcl.Diagnostics {
	if _, diags := hcl.AbsTraversalForExpr(expr); !diags.HasErrors() {
		return nil
	}
	switch e := expr.(type) {
	case *hclsyntax.IndexExpr:
		return validateImportToExpr(e.Collection)
	case *hclsyntax.RelativeTraversalExpr:
		return validateImportToExpr(e.Source)
	default:
		return hcl.Diagnostics{{
			Severity: hcl.DiagError,
			Summary:  "Invalid import address expression",
			Detail:   "Import address must be a reference to a resource's address, and only allows for indexing with dynamic keys.",
			Subject:  expr.Range().Ptr(),
		}}
	}
}
