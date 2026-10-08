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

// Package packages handles Pulumi package schema loading and type mapping.
package packages

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/blang/semver"
	"github.com/hashicorp/hcl/v2"
	"github.com/pulumi/pulumi/pkg/v3/codegen/pcl"
	"github.com/pulumi/pulumi/pkg/v3/codegen/schema"
	"github.com/pulumi/pulumi/sdk/v3/go/common/util/contract"
	"github.com/pulumi/pulumi/sdk/v3/go/common/workspace"
)

var ErrNotFound = errors.New("not found")

// NotFoundError wraps ErrNotFound (via Unwrap) and carries an optional
// Suggestion: the closest HCL form by edit distance, when the package was
// loaded but the specific token was missing.
type NotFoundError struct {
	Token      string
	Suggestion string
}

func (e *NotFoundError) Error() string {
	if e.Suggestion != "" {
		return fmt.Sprintf("not found; did you mean %q?", e.Suggestion)
	}
	return "not found"
}

func (e *NotFoundError) Unwrap() error { return ErrNotFound }

type InvalidToken struct {
	token, reason string
}

func (err InvalidToken) Error() string {
	var b strings.Builder
	b.WriteString("invalid token")
	if err.token != "" {
		b.WriteRune(' ')
		b.WriteString(strconv.Quote(err.token))
	}
	if err.reason != "" {
		b.WriteRune(' ')
		b.WriteString(err.reason)
	}
	return b.String()
}

// PulumiResourceTokenToHCL converts a Pulumi resource token to its canonical HCL form.
func PulumiResourceTokenToHCL(pkg schema.PackageReference, token string) (string, hcl.Diagnostics) {
	return pulumiTokenToHCL(pkg, token, false)
}

// PulumiFunctionTokenToHCL converts a Pulumi function token to its canonical HCL form.
func PulumiFunctionTokenToHCL(pkg schema.PackageReference, token string) (string, hcl.Diagnostics) {
	return pulumiTokenToHCL(pkg, token, true)
}

// isPulumiProvidersToken matches the schema's own special case for
// "pulumi:providers:*" tokens, where TokenToModule returns "" regardless of
// any configured ModuleFormat. Callers that derive an HCL name from the module
// need the "providers" segment preserved to round-trip through the resolver.
func isPulumiProvidersToken(pkgName, mod string) bool {
	return pkgName == "pulumi" && (mod == "providers" || mod == "pulumi")
}

func pulumiTokenToHCL(pkg schema.PackageReference, token string, isFunction bool) (string, hcl.Diagnostics) {
	if token == "pulumi:pulumi:StackReference" {
		return "pulumi_stack_reference", nil
	}
	pkgName, mod, name, diag := pcl.DecomposeToken(token, hcl.Range{})
	if diag.HasErrors() {
		return "", diag
	}
	// Apply the schema's ModuleFormat regex when available. TokenToModule
	// collapses [pulumi:providers:*] tokens to the empty string by special-case,
	// but we want to preserve the "providers" segment so the resolver can route
	// the token back to a provider resource.
	if pkg != nil && !isPulumiProvidersToken(pkgName, mod) {
		mod = pkg.TokenToModule(token)
	}
	if isFunction && len(name) > 3 && strings.HasPrefix(name, "get") {
		r := rune(name[3])
		if r >= 'A' && r <= 'Z' {
			name = name[3:]
		}
	}
	hclToken := pkgName
	if mod != "index" && mod != "" {
		hclToken += "_" + camelToSnake(mod)
	}
	return hclToken + "_" + camelToSnake(name), nil
}

// PackageFromToken determines the package name from an HCL token and the list of
// known providers from required_providers.
//
// If exactly one known provider matches as a prefix of the token (or matches
// the token exactly, for single-segment tokens like `data "external"` whose
// provider and type names are the same word), that provider name is returned.
// If multiple known providers match, an error is returned to surface the
// ambiguity. If no known provider matches, the first underscore-delimited
// segment is used as the package name, or the whole token if it has no
// underscore.
func PackageFromToken(knownProviders []string, token string) (string, error) {
	var matches []string
	for _, p := range knownProviders {
		if strings.HasPrefix(token, p+"_") || token == p {
			matches = append(matches, p)
		}
	}
	switch len(matches) {
	case 0:
		if before, _, ok := strings.Cut(token, "_"); ok {
			return before, nil
		}
		return token, nil
	case 1:
		return matches[0], nil
	default:
		return "", fmt.Errorf("ambiguous token %q: matches multiple providers %v", token, matches)
	}
}

// ProviderAsResourceError is returned by ResolveResource when an HCL token
// resolves to a package's Provider schema. Providers must use a `provider`
// block, not a `resource` block.
type ProviderAsResourceError struct {
	Token string // resolved Pulumi token, e.g. "pulumi:providers:simple"
}

func (e *ProviderAsResourceError) Error() string {
	name := e.Token
	if i := strings.LastIndex(e.Token, ":"); i >= 0 {
		name = e.Token[i+1:]
	}
	return fmt.Sprintf(
		"%q is a provider type and cannot be declared with a resource block; "+
			"declare it with a `provider %q { ... }` block instead",
		e.Token, name)
}

// ResolvePackage resolves the package reference for an HCL resource or
// function token. Pair with pkg.Provider() when you need the Provider
// schema — ResolveResource rejects provider tokens.
func ResolvePackage(
	ctx context.Context, loader schema.ReferenceLoader, knownProviders []string, token string,
) (schema.PackageReference, error) {
	if token == "" {
		return nil, InvalidToken{token: token, reason: "Pulumi HCL tokens must be non-empty"}
	}
	if provider, ok := strings.CutPrefix(token, "pulumi_providers_"); ok {
		return resolvePackage(ctx, loader, &schema.PackageDescriptor{Name: provider})
	}
	if isStackReferenceToken(token) {
		return resolvePackage(ctx, loader, &schema.PackageDescriptor{Name: "pulumi"})
	}
	pkgName, err := PackageFromToken(knownProviders, token)
	if err != nil {
		return nil, err
	}
	return resolvePackage(ctx, loader, &schema.PackageDescriptor{Name: pkgName})
}

// isStackReferenceToken accepts both the underscore-split form and the legacy
// collapsed form so users don't have to write pulumi_pulumi_stack_reference.
func isStackReferenceToken(token string) bool {
	return token == "pulumi_stack_reference" || token == "pulumi_stackreference"
}

func ResolveResource(ctx context.Context, loader schema.ReferenceLoader, knownProviders []string, token string) (*schema.Resource, error) {
	pkg, err := resolvePackageForToken(ctx, loader, knownProviders, token)
	if err != nil {
		return nil, err
	}

	res, err := findResource(pkg, knownProviders, token)
	if errors.Is(err, ErrNotFound) {
		if extPkg, ok := extensionReference(ctx, loader, knownProviders, token); ok {
			if extRes, extErr := findResource(extPkg, knownProviders, token); !errors.Is(extErr, ErrNotFound) {
				res, err = extRes, extErr
			}
		}
	}
	if err != nil {
		return nil, err
	}
	if res != nil && res.IsProvider {
		return nil, &ProviderAsResourceError{Token: res.Token}
	}
	return res, nil
}

// extensionAwareLoader is implemented by loaders that can supply the schema an
// extension contributes to a base provider's namespace.
type extensionAwareLoader interface {
	LoadExtensionReference(ctx context.Context, baseName string) (schema.PackageReference, bool, error)
}

// extensionReference returns the extension schema for token's package, when the
// loader knows of an extension applied to that base provider. An extension's
// resources and functions share the base provider's namespace but are served
// only via the extension parameterization, so a token missing from the base
// schema may still be defined by an extension.
func extensionReference(
	ctx context.Context, loader schema.ReferenceLoader, knownProviders []string, token string,
) (schema.PackageReference, bool) {
	el, ok := loader.(extensionAwareLoader)
	if !ok {
		return nil, false
	}
	pkgName, err := PackageFromToken(knownProviders, token)
	if err != nil {
		return nil, false
	}
	ref, found, err := el.LoadExtensionReference(ctx, pkgName)
	if err != nil || !found {
		return nil, false
	}
	return ref, true
}

// resolvePackageForToken collapses package-load failures to ErrNotFound;
// structural errors (InvalidToken, ambiguous matches) propagate.
func resolvePackageForToken(
	ctx context.Context, loader schema.ReferenceLoader, knownProviders []string, token string,
) (schema.PackageReference, error) {
	if token == "" {
		return nil, InvalidToken{token: token, reason: "Pulumi HCL tokens must be non-empty"}
	}
	if provider, ok := strings.CutPrefix(token, "pulumi_providers_"); ok {
		pkg, err := resolvePackage(ctx, loader, &schema.PackageDescriptor{Name: provider})
		if err != nil {
			return nil, ErrNotFound
		}
		return pkg, nil
	}
	if isStackReferenceToken(token) {
		pkg, err := resolvePackage(ctx, loader, &schema.PackageDescriptor{Name: "pulumi"})
		if err != nil {
			return nil, ErrNotFound
		}
		return pkg, nil
	}
	pkgName, err := PackageFromToken(knownProviders, token)
	if err != nil {
		return nil, err
	}
	pkg, err := resolvePackage(ctx, loader, &schema.PackageDescriptor{Name: pkgName})
	if err != nil {
		return nil, ErrNotFound
	}
	return pkg, nil
}

func findResource(pkg schema.PackageReference, knownProviders []string, token string) (*schema.Resource, error) {
	if strings.HasPrefix(token, "pulumi_providers_") {
		return pkg.Provider()
	}
	if isStackReferenceToken(token) {
		r, ok, err := pkg.Resources().Get("pulumi:pulumi:StackReference")
		contract.Assertf(ok, "stack references are there")
		return r, err
	}
	pkgName, err := PackageFromToken(knownProviders, token)
	if err != nil {
		return nil, err
	}
	key := hclSearchKey(pkgName, token)
	var matches []string
	for iter := pkg.Resources().Range(); iter.Next(); {
		if tokenSearchKey(pkg, iter.Token()) == key {
			matches = append(matches, iter.Token())
		}
	}
	switch len(matches) {
	case 0:
		return nil, notFoundWithSuggestion(pkg, token, false)
	case 1:
		res, _, err := pkg.Resources().Get(matches[0])
		return res, err
	default:
		return nil, ambiguousMatchError(token, "resources", matches)
	}
}

// hclSearchKey returns the normalized member-name key used to match an HCL
// token against tokenSearchKey-derived keys. For multi-segment tokens it's the
// suffix after the provider prefix; for single-segment tokens (where the type
// name equals the provider name) the whole token is the member name.
func hclSearchKey(pkgName, token string) string {
	if token != pkgName {
		token = token[len(pkgName)+1:]
	}
	return searchKeyReplacer.Replace(token)
}

// ambiguousMatchError reports an HCL token whose search key matches more than
// one schema member, so neither can be picked without guessing.
func ambiguousMatchError(token, kind string, matches []string) error {
	slices.Sort(matches)
	return fmt.Errorf("ambiguous token %q: matches multiple %s %v", token, kind, matches)
}

func notFoundWithSuggestion(pkg schema.PackageReference, hclToken string, isFunction bool) error {
	return &NotFoundError{
		Token:      hclToken,
		Suggestion: nearestHCLToken(pkg, hclToken, isFunction),
	}
}

// searchKeyReplacer strips the separators that search keys ignore. "." is
// among them because an HCL identifier cannot contain one, so a module such as
// "helm.sh/v3" is written "helm_sh_v3".
var searchKeyReplacer = strings.NewReplacer("/", "", "_", "", ".", "")

// tokenSearchKey produces a normalized lookup key for a Pulumi token by
// concatenating the schema-declared module and member name and stripping
// "_", "/" and "." separators after lowercasing. The schema's ModuleFormat
// regex (applied by TokenToModule) is responsible for separating the module
// from the member name and for collapsing the implicit "index" root module to
// the empty string; bridged-style tokens like "aws:iam/getRole:getRole"
// resolve correctly only when the schema sets that regex.
func tokenSearchKey(pkg schema.PackageReference, tok string) string {
	mod := pkg.TokenToModule(tok)
	name := strings.Split(tok, ":")[2]
	return searchKeyReplacer.Replace(strings.ToLower(mod + name))
}

// getlessSearchKey drops the member's "get" prefix when it is followed by an
// uppercase letter, matching PulumiFunctionTokenToHCL's data source naming.
func getlessSearchKey(pkg schema.PackageReference, tok string) string {
	name := strings.Split(tok, ":")[2]
	if len(name) <= 3 || !strings.HasPrefix(name, "get") || name[3] < 'A' || name[3] > 'Z' {
		return tokenSearchKey(pkg, tok)
	}
	mod := pkg.TokenToModule(tok)
	return searchKeyReplacer.Replace(strings.ToLower(mod + name[3:]))
}

func resolvePackage(ctx context.Context, loader schema.ReferenceLoader, descriptor *schema.PackageDescriptor) (schema.PackageReference, error) {
	if descriptor.Name == "pulumi" {
		return schema.DefaultPulumiPackage.Reference(), nil
	}

	pkg, err := loader.LoadPackageReferenceV2(ctx, descriptor)
	if err != nil {
		return nil, fmt.Errorf("unable to load schema from %s: %w", descriptor, err)
	}
	return pkg, nil
}

// ParameterizationAwareLoader wraps a schema.ReferenceLoader and enriches load
// requests for packages with a local SDK descriptor: the base provider name and
// parameterization, or for plain packages the version and download URL.
type ParameterizationAwareLoader struct {
	inner   schema.ReferenceLoader
	aliases map[string]workspace.PackageDescriptor
}

func NewParameterizationAwareLoader(
	inner schema.ReferenceLoader,
	aliases map[string]workspace.PackageDescriptor,
) *ParameterizationAwareLoader {
	return &ParameterizationAwareLoader{inner: inner, aliases: aliases}
}

func (l *ParameterizationAwareLoader) enrich(descriptor *schema.PackageDescriptor) *schema.PackageDescriptor {
	if descriptor.Parameterization != nil {
		return descriptor
	}
	desc, ok := l.aliases[descriptor.Name]
	if !ok {
		return descriptor
	}
	// Base version may be nil for auto-derived TF-style entries (we let the
	// plugin loader pick the installed terraform-provider version); file-based
	// descriptors from `pulumi package add` carry an explicit version.
	var baseVersion *semver.Version
	if desc.Version != nil {
		v := *desc.Version
		baseVersion = &v
	}
	if desc.Parameterization == nil {
		if desc.ExtensionParameterization != nil {
			return extensionDescriptor(desc)
		}
		// A git-sourced plugin is found only by its download URL and version.
		enriched := *descriptor
		if enriched.Version == nil {
			enriched.Version = baseVersion
		}
		if enriched.DownloadURL == "" {
			enriched.DownloadURL = desc.PluginDownloadURL
		}
		return &enriched
	}
	pkgName := desc.Name
	if pkgName == "" {
		pkgName = descriptor.Name
	}
	return &schema.PackageDescriptor{
		Name:    pkgName,
		Version: baseVersion,
		Parameterization: &schema.ParameterizationDescriptor{
			Name:    desc.Parameterization.Name,
			Version: desc.Parameterization.Version,
			Value:   desc.Parameterization.Value,
		},
	}
}

// LoadExtensionReference loads the schema an extension contributes to baseName's
// namespace, or (nil, false) when no extension applies. An extension's resource
// tokens live in the base provider's namespace but are served only when the base
// provider is parameterized with the extension, so the extension schema is loaded
// by treating the extension as a parameterization of the base provider.
func (l *ParameterizationAwareLoader) LoadExtensionReference(
	ctx context.Context, baseName string,
) (schema.PackageReference, bool, error) {
	for _, desc := range l.aliases {
		if desc.ExtensionParameterization == nil || desc.Name != baseName {
			continue
		}
		ref, err := l.inner.LoadPackageReferenceV2(ctx, extensionDescriptor(desc))
		if err != nil {
			return nil, false, err
		}
		return ref, true, nil
	}
	return nil, false, nil
}

func extensionDescriptor(desc workspace.PackageDescriptor) *schema.PackageDescriptor {
	return &schema.PackageDescriptor{
		Name:    desc.Name,
		Version: desc.Version,
		Parameterization: &schema.ParameterizationDescriptor{
			Name:    desc.ExtensionParameterization.Name,
			Version: desc.ExtensionParameterization.Version,
			Value:   desc.ExtensionParameterization.Value,
		},
	}
}

func (l *ParameterizationAwareLoader) LoadPackage(pkg string, version *semver.Version) (*schema.Package, error) {
	return l.LoadPackageV2(context.TODO(), &schema.PackageDescriptor{Name: pkg, Version: version})
}

func (l *ParameterizationAwareLoader) LoadPackageV2(ctx context.Context, descriptor *schema.PackageDescriptor) (*schema.Package, error) {
	ref, err := l.LoadPackageReferenceV2(ctx, descriptor)
	if err != nil {
		return nil, err
	}
	return ref.Definition()
}

func (l *ParameterizationAwareLoader) LoadPackageReference(pkg string, version *semver.Version) (schema.PackageReference, error) {
	return l.LoadPackageReferenceV2(context.TODO(), &schema.PackageDescriptor{Name: pkg, Version: version})
}

func (l *ParameterizationAwareLoader) LoadPackageReferenceV2(ctx context.Context, descriptor *schema.PackageDescriptor) (schema.PackageReference, error) {
	return l.inner.LoadPackageReferenceV2(ctx, l.enrich(descriptor))
}

var _ schema.ReferenceLoader = (*ParameterizationAwareLoader)(nil)

func ResolveFunction(ctx context.Context, loader schema.ReferenceLoader, knownProviders []string, token string) (*schema.Function, error) {
	if token == "" {
		return nil, InvalidToken{token: token, reason: "Pulumi HCL tokens must be non-empty"}
	}

	pkgName, err := PackageFromToken(knownProviders, token)
	if err != nil {
		return nil, err
	}

	pkg, err := resolvePackage(ctx, loader, &schema.PackageDescriptor{Name: pkgName})
	if err != nil {
		return nil, ErrNotFound
	}

	fn, err := findFunction(pkg, pkgName, token)
	if errors.Is(err, ErrNotFound) {
		if extPkg, ok := extensionReference(ctx, loader, knownProviders, token); ok {
			if extFn, extErr := findFunction(extPkg, pkgName, token); !errors.Is(extErr, ErrNotFound) {
				fn, err = extFn, extErr
			}
		}
	}
	return fn, err
}

// findFunction matches token against pkg's functions by the full member name
// first, then with the member's "get" prefix omitted, as data sources may.
func findFunction(pkg schema.PackageReference, pkgName, token string) (*schema.Function, error) {
	key := hclSearchKey(pkgName, token)
	for _, searchKey := range []func(schema.PackageReference, string) string{tokenSearchKey, getlessSearchKey} {
		var matches []string
		for iter := pkg.Functions().Range(); iter.Next(); {
			if searchKey(pkg, iter.Token()) == key {
				matches = append(matches, iter.Token())
			}
		}
		switch len(matches) {
		case 0:
			continue
		case 1:
			fn, _, err := pkg.Functions().Get(matches[0])
			return fn, err
		default:
			return nil, ambiguousMatchError(token, "functions", matches)
		}
	}
	return nil, notFoundWithSuggestion(pkg, token, true)
}
