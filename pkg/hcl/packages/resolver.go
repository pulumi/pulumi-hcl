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

package packages

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/pulumi/pulumi-hcl/pkg/hcl/ast"
	"github.com/pulumi/pulumi-hcl/pkg/hcl/bridge"
	"github.com/pulumi/pulumi-terraform-bridge/v3/pkg/tfbridge"
	shim "github.com/pulumi/pulumi-terraform-bridge/v3/pkg/tfshim"
	"github.com/pulumi/pulumi/pkg/v3/codegen/schema"
	"github.com/pulumi/pulumi/sdk/v3/go/common/util/logging"
	"github.com/pulumi/pulumi/sdk/v3/go/common/workspace"
)

// Resolver resolves TF resource and data source types to their Pulumi schemas
// and bridge body mappings. It consults the bridge mapping first (so a TF type
// maps directly to its Pulumi token regardless of how Pulumi organises modules)
// and falls back to the convention-based resolver. It holds exactly the
// dependencies the engine and schema generation share, so both paths resolve
// types identically.
type Resolver struct {
	loader             schema.ReferenceLoader
	providerInfoSource bridge.ProviderInfoSource
	packages           map[string]workspace.PackageDescriptor
}

// NewResolver builds a Resolver. providerInfoSource and packages may be nil/empty
// when no bridge mapper is available, in which case resolution uses only the
// convention-based fallback. knownProviders is ignored; it remains for
// existing callers.
func NewResolver(
	loader schema.ReferenceLoader,
	providerInfoSource bridge.ProviderInfoSource,
	pkgs map[string]workspace.PackageDescriptor,
	knownProviders []string,
) *Resolver {
	return &Resolver{
		loader:             loader,
		providerInfoSource: providerInfoSource,
		packages:           pkgs,
	}
}

// BoundResource is a resource block bound to the Pulumi schema of its type.
type BoundResource struct {
	// Package is the package of the provider that serves the block (see
	// [BlockPackage]).
	Package string
	Schema  *schema.Resource
	// Mapping is the bridge mapping of the type, or nil when it has none.
	Mapping *bridge.BodyMapping
}

// BoundDataSource is a data block bound to the Pulumi schema of its type.
type BoundDataSource struct {
	// Package is the package of the provider that serves the block (see
	// [BlockPackage]).
	Package string
	Schema  *schema.Function
	// Mapping is the bridge mapping of the type, or nil when it has none.
	Mapping *bridge.BodyMapping
}

// BindResource binds the resource block res, declared in the module whose
// `terraform` block is module, to the schema of its type in the package that
// serves it (see [BlockPackage]).
func (r *Resolver) BindResource(ctx context.Context, module *ast.Terraform, res *ast.Resource) (*BoundResource, error) {
	pkgName, err := BlockPackage(module, res.Provider, res.Type)
	if err != nil {
		return nil, err
	}
	resSchema, err := r.resolveResource(ctx, pkgName, res.Type)
	if err != nil {
		return nil, err
	}
	return &BoundResource{
		Package: pkgName,
		Schema:  resSchema,
		Mapping: r.ResourceBodyMapping(ctx, pkgName, res.Type),
	}, nil
}

// BindDataSource binds the data block ds, declared in the module whose
// `terraform` block is module, to the schema of its type in the package that
// serves it (see [BlockPackage]).
func (r *Resolver) BindDataSource(
	ctx context.Context, module *ast.Terraform, ds *ast.DataSource,
) (*BoundDataSource, error) {
	pkgName, err := BlockPackage(module, ds.Provider, ds.Type)
	if err != nil {
		return nil, err
	}
	fn, err := r.resolveFunction(ctx, pkgName, ds.Type)
	if err != nil {
		return nil, err
	}
	return &BoundDataSource{
		Package: pkgName,
		Schema:  fn,
		Mapping: r.DataSourceBodyMapping(ctx, pkgName, ds.Type),
	}, nil
}

// BlockPackage returns the Pulumi package of the provider that serves a
// resource or data source of type typ, declared in the module whose
// `terraform` block is tf. provider is the block's `provider` meta-argument,
// or nil when the block does not set it.
func BlockPackage(tf *ast.Terraform, provider hcl.Expression, typ string) (string, error) {
	local, err := ProviderLocalName(tf, provider, typ)
	if err != nil {
		return "", err
	}
	return ProviderPackage(tf, local), nil
}

// ProviderLocalName returns the local name of the provider that serves a
// resource or data source of type typ, declared in the module whose
// `terraform` block is tf. When provider (the block's `provider`
// meta-argument) is a provider configuration reference, it names the
// provider. Otherwise, the provider is the one that typ implies.
func ProviderLocalName(tf *ast.Terraform, provider hcl.Expression, typ string) (string, error) {
	if name, ok := providerConfigName(provider); ok {
		return name, nil
	}
	return PackageFromToken(KnownProviders(tf), typ)
}

// providerConfigName returns the provider local name that expr references,
// when expr has the form of a provider configuration reference: name,
// name.alias, or name.alias[key]. Any other `provider` value, such as a
// provider that a method call returns (call.<resource>.<method>), is a value
// that carries its own provider; no configuration names its package.
func providerConfigName(expr hcl.Expression) (string, bool) {
	if expr == nil {
		return "", false
	}
	maxSteps := 3
	if index, ok := expr.(*hclsyntax.IndexExpr); ok {
		expr, maxSteps = index.Collection, 2
	}
	traversal, diags := hcl.AbsTraversalForExpr(expr)
	if diags.HasErrors() || len(traversal) > maxSteps {
		return "", false
	}
	if len(traversal) > 1 {
		if _, ok := traversal[1].(hcl.TraverseAttr); !ok {
			return "", false
		}
	}
	if len(traversal) > 2 {
		if _, ok := traversal[2].(hcl.TraverseIndex); !ok {
			return "", false
		}
	}
	return traversal.RootName(), true
}

// ProviderPackage returns the Pulumi package name of the provider with the
// local name local, in the module whose `terraform` block is tf: the last
// segment of its required_providers source ("hashicorp/simple" → "simple"),
// or local when the module does not declare a source for it.
func ProviderPackage(tf *ast.Terraform, local string) string {
	if tf != nil {
		if req, ok := tf.RequiredProviders[local]; ok && req.Source != "" {
			parts := strings.Split(req.Source, "/")
			return parts[len(parts)-1]
		}
	}
	return local
}

// KnownProviders returns the local names that the required_providers of the
// `terraform` block tf declares.
func KnownProviders(tf *ast.Terraform) []string {
	if tf == nil {
		return nil
	}
	return slices.Collect(maps.Keys(tf.RequiredProviders))
}

// resolveResource resolves a TF resource type to a Pulumi schema.Resource
// that the provider package pkgName serves: from pkgName itself, else from an
// extension of it. The synthetic terraform_data type resolves to its builtin
// schema.
func (r *Resolver) resolveResource(ctx context.Context, pkgName, tfType string) (*schema.Resource, error) {
	if tfType == TerraformDataType {
		return TerraformDataSchema(), nil
	}
	res, err := r.resolveResourceIn(ctx, pkgName, tfType)
	for _, ext := range r.extensionsOf(pkgName) {
		if !errors.Is(err, ErrNotFound) {
			break
		}
		// The base package's not-found error carries its suggestion.
		if extRes, extErr := r.resolveResourceIn(ctx, ext, tfType); !errors.Is(extErr, ErrNotFound) {
			res, err = extRes, extErr
		}
	}
	return res, err
}

// resolveResourceIn resolves a TF resource type in the package pkgName,
// preferring the bridge mapping and falling back to the convention-based
// resolver.
func (r *Resolver) resolveResourceIn(ctx context.Context, pkgName, tfType string) (*schema.Resource, error) {
	if info := r.providerInfoForType(ctx, pkgName, tfType); info != nil {
		if res, ok := info.Resources[tfType]; ok && res != nil && string(res.Tok) != "" {
			schemaRes, err := r.loadResourceByToken(ctx, info.Name, string(res.Tok))
			if err == nil {
				return schemaRes, nil
			}
			logging.V(5).Infof("bridge resource token %q (from %q) not loadable: %v", res.Tok, tfType, err)
		}
	}
	return ResolveResource(ctx, r.loader, pkgName, tfType)
}

// resolveFunction mirrors resolveResource for TF data sources. The synthetic
// terraform_remote_state type resolves to its builtin schema.
func (r *Resolver) resolveFunction(ctx context.Context, pkgName, tfType string) (*schema.Function, error) {
	if tfType == RemoteStateType {
		return TerraformRemoteStateSchema(), nil
	}
	fn, err := r.resolveFunctionIn(ctx, pkgName, tfType)
	for _, ext := range r.extensionsOf(pkgName) {
		if !errors.Is(err, ErrNotFound) {
			break
		}
		if extFn, extErr := r.resolveFunctionIn(ctx, ext, tfType); !errors.Is(extErr, ErrNotFound) {
			fn, err = extFn, extErr
		}
	}
	return fn, err
}

// resolveFunctionIn mirrors resolveResourceIn for TF data sources.
func (r *Resolver) resolveFunctionIn(ctx context.Context, pkgName, tfType string) (*schema.Function, error) {
	if info := r.providerInfoForType(ctx, pkgName, tfType); info != nil {
		if d, ok := info.DataSources[tfType]; ok && d != nil && string(d.Tok) != "" {
			fn, err := r.loadFunctionByToken(ctx, info.Name, string(d.Tok))
			if err == nil {
				return fn, nil
			}
			logging.V(5).Infof("bridge data source token %q (from %q) not loadable: %v", d.Tok, tfType, err)
		}
	}
	return ResolveFunction(ctx, r.loader, pkgName, tfType)
}

// ResourceBodyMapping returns the bridge BodyMapping for a TF resource type
// that the package pkgName serves, or nil.
func (r *Resolver) ResourceBodyMapping(ctx context.Context, pkgName, tfResourceType string) *bridge.BodyMapping {
	info := r.providerInfoForType(ctx, pkgName, tfResourceType)
	if info == nil {
		return nil
	}
	return bridge.ResourceBodyMapping(info, tfResourceType)
}

// DataSourceBodyMapping mirrors ResourceBodyMapping for a TF data source.
func (r *Resolver) DataSourceBodyMapping(ctx context.Context, pkgName, tfType string) *bridge.BodyMapping {
	info := r.providerInfoForType(ctx, pkgName, tfType)
	if info == nil {
		return nil
	}
	return bridge.DataSourceBodyMapping(info, tfType)
}

// ProviderConfigBodyMapping returns the BodyMapping for a provider block by TF
// provider local name (e.g. "aws").
func (r *Resolver) ProviderConfigBodyMapping(ctx context.Context, providerName string) *bridge.BodyMapping {
	info := r.providerInfoFor(ctx, providerName, providerName)
	if info == nil {
		return nil
	}
	return bridge.ProviderConfigBodyMapping(info)
}

// extensionsOf returns, sorted, the extension packages whose base is the
// package pkgName. The base provider serves the resources and functions of
// its extensions.
func (r *Resolver) extensionsOf(pkgName string) []string {
	var exts []string
	for name, desc := range r.packages {
		if desc.ExtensionParameterization != nil && desc.Name == pkgName {
			exts = append(exts, name)
		}
	}
	slices.Sort(exts)
	return exts
}

// providerInfoForType fetches the bridge info that the package pkgName serves
// for a type, under the type's prefix.
func (r *Resolver) providerInfoForType(ctx context.Context, pkgName, tfType string) *tfbridge.ProviderInfo {
	return r.providerInfoFor(ctx, pkgName, bridge.TypeProvider(tfType))
}

// providerInfoFor fetches the bridge ProviderInfo that the package pkgName
// serves under the TF provider name tfProvider, or nil.
func (r *Resolver) providerInfoFor(ctx context.Context, pkgName, tfProvider string) *tfbridge.ProviderInfo {
	if r.providerInfoSource == nil {
		return nil
	}
	desc, ok := r.packages[pkgName]
	if !ok {
		desc = workspace.PackageDescriptor{PluginDescriptor: workspace.PluginDescriptor{Name: pkgName}}
	}
	info, err := r.providerInfoSource.GetProviderInfo(ctx, tfProvider, &desc)
	if err != nil {
		logging.V(5).Infof("bridge mapping of %q unavailable for %q: %v", tfProvider, pkgName, err)
		return nil
	}
	return info
}

// ProviderFunction pairs a resolved Pulumi function with the TF-side facts
// its schema cannot carry.
type ProviderFunction struct {
	*schema.Function

	// Variadic is true when the function's final parameter is variadic. It
	// comes from the TF signature in the bridge mapping's provider schema;
	// a function resolved by convention alone is never variadic.
	Variadic bool
}

// ProviderFunctions returns the provider-defined functions exposed by the
// provider registered under the given TF local name, keyed by TF function
// name (e.g. "arn_parse"). A Pulumi function projects as a provider-defined
// function when its schema declares multi-argument inputs; single-bag invokes
// are data sources. TF names come from the bridge mapping when available and
// otherwise derive from the token name via the bridge naming convention. When
// two tokens derive the same TF name, the index-module token wins and the
// rest are dropped.
func (r *Resolver) ProviderFunctions(ctx context.Context, providerName string) (map[string]ProviderFunction, error) {
	out := map[string]ProviderFunction{}

	pkgName := providerName
	if info := r.providerInfoFor(ctx, providerName, providerName); info != nil {
		if info.Name != "" {
			pkgName = info.Name
		}
		var signatures map[string]shim.Function
		if info.P != nil {
			signatures = info.P.Functions()
		}
		for tfName, f := range info.Functions {
			if f == nil || string(f.Tok) == "" {
				continue
			}
			fn, err := r.loadFunctionByToken(ctx, pkgName, string(f.Tok))
			if err != nil {
				logging.V(5).Infof("bridge function token %q (for %q) not loadable: %v", f.Tok, tfName, err)
				continue
			}
			if fn.MultiArgumentInputs {
				out[tfName] = ProviderFunction{
					Function: fn,
					Variadic: signatures[tfName].VariadicParameter != nil,
				}
			}
		}
	}

	pkg, err := resolvePackage(ctx, r.loader, &schema.PackageDescriptor{Name: pkgName})
	if err != nil {
		if len(out) > 0 {
			return out, nil
		}
		return nil, err
	}
	chosenToken := map[string]string{}
	for iter := pkg.Functions().Range(); iter.Next(); {
		tok := iter.Token()
		name := tok[strings.LastIndexByte(tok, ':')+1:]
		tfName := tfbridge.PulumiToTerraformName(name, nil, nil)
		if _, mapped := out[tfName]; mapped && chosenToken[tfName] == "" {
			continue
		}
		if prev, ok := chosenToken[tfName]; ok && !preferFunctionToken(pkg, tok, prev) {
			continue
		}
		fn, err := iter.Function()
		if err != nil {
			logging.V(5).Infof("function token %q not loadable: %v", tok, err)
			continue
		}
		if !fn.MultiArgumentInputs {
			continue
		}
		chosenToken[tfName] = tok
		out[tfName] = ProviderFunction{Function: fn}
	}
	return out, nil
}

// preferFunctionToken reports whether tok should replace prev as the source
// for a TF function name: the index module wins, then the lexicographically
// smaller token, for determinism. TokenToModule normalizes the index module
// to the empty string.
func preferFunctionToken(pkg schema.PackageReference, tok, prev string) bool {
	tokIndex := pkg.TokenToModule(tok) == ""
	prevIndex := pkg.TokenToModule(prev) == ""
	if tokIndex != prevIndex {
		return tokIndex
	}
	return tok < prev
}

// loadResourceByToken loads the Pulumi schema for an exact Pulumi token.
func (r *Resolver) loadResourceByToken(ctx context.Context, pkgName, tok string) (*schema.Resource, error) {
	pkg, err := r.loader.LoadPackageReferenceV2(ctx, &schema.PackageDescriptor{Name: pkgName})
	if err != nil {
		return nil, err
	}
	res, ok, err := pkg.Resources().Get(tok)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("%w: %s is not in the %s schema", ErrNotFound, tok, pkgName)
	}
	return res, nil
}

// loadFunctionByToken loads the Pulumi schema for an exact Pulumi function token.
func (r *Resolver) loadFunctionByToken(ctx context.Context, pkgName, tok string) (*schema.Function, error) {
	pkg, err := r.loader.LoadPackageReferenceV2(ctx, &schema.PackageDescriptor{Name: pkgName})
	if err != nil {
		return nil, err
	}
	fn, ok, err := pkg.Functions().Get(tok)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("%w: %s is not in the %s schema", ErrNotFound, tok, pkgName)
	}
	return fn, nil
}
