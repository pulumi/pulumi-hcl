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
	"fmt"
	"strings"

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
	knownProviders     []string
}

// NewResolver builds a Resolver. providerInfoSource and packages may be nil/empty
// when no bridge mapper is available, in which case resolution uses only the
// convention-based fallback.
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
		knownProviders:     knownProviders,
	}
}

// ResolveResource resolves a TF resource type to a Pulumi schema.Resource,
// preferring the bridge mapping and falling back to the convention-based
// resolver. The synthetic terraform_data type resolves to its builtin schema.
func (r *Resolver) ResolveResource(ctx context.Context, tfType string) (*schema.Resource, error) {
	if tfType == TerraformDataType {
		return TerraformDataSchema(), nil
	}
	if info := r.providerInfoForType(ctx, tfType); info != nil {
		if res, ok := info.Resources[tfType]; ok && res != nil && string(res.Tok) != "" {
			schemaRes, err := r.loadResourceByToken(ctx, info.Name, string(res.Tok))
			if err == nil {
				return schemaRes, nil
			}
			logging.V(5).Infof("bridge resource token %q (from %q) not loadable: %v", res.Tok, tfType, err)
		}
	}
	return ResolveResource(ctx, r.loader, r.knownProviders, tfType)
}

// ResolveFunction mirrors ResolveResource for TF data sources and functions. The
// synthetic terraform_remote_state type resolves to its builtin schema.
func (r *Resolver) ResolveFunction(ctx context.Context, tfType string) (*schema.Function, error) {
	if tfType == RemoteStateType {
		return TerraformRemoteStateSchema(), nil
	}
	if info := r.providerInfoForType(ctx, tfType); info != nil {
		if d, ok := info.DataSources[tfType]; ok && d != nil && string(d.Tok) != "" {
			fn, err := r.loadFunctionByToken(ctx, info.Name, string(d.Tok))
			if err == nil {
				return fn, nil
			}
			logging.V(5).Infof("bridge data source token %q (from %q) not loadable: %v", d.Tok, tfType, err)
		}
	}
	return ResolveFunction(ctx, r.loader, r.knownProviders, tfType)
}

// ResourceBodyMapping returns the bridge BodyMapping for a TF resource type, or
// nil when the mapper is unavailable, the provider isn't known, or the type
// isn't in its schema.
func (r *Resolver) ResourceBodyMapping(ctx context.Context, tfResourceType string) *bridge.BodyMapping {
	info := r.providerInfoForType(ctx, tfResourceType)
	if info == nil {
		return nil
	}
	return bridge.ResourceBodyMapping(info, tfResourceType)
}

// DataSourceBodyMapping mirrors ResourceBodyMapping for a TF data source.
func (r *Resolver) DataSourceBodyMapping(ctx context.Context, tfType string) *bridge.BodyMapping {
	info := r.providerInfoForType(ctx, tfType)
	if info == nil {
		return nil
	}
	return bridge.DataSourceBodyMapping(info, tfType)
}

// ProviderConfigBodyMapping returns the BodyMapping for a provider block by TF
// provider local name (e.g. "aws").
func (r *Resolver) ProviderConfigBodyMapping(ctx context.Context, providerName string) *bridge.BodyMapping {
	info := r.providerInfoFor(ctx, providerName)
	if info == nil {
		return nil
	}
	return bridge.ProviderConfigBodyMapping(info)
}

// providerInfoForType resolves the TF provider that owns a resource or data
// source token (split on first underscore) and fetches its bridge info. For
// single-segment tokens — providers like hashicorp/external whose type name
// equals the provider name — the whole token is the provider name.
func (r *Resolver) providerInfoForType(ctx context.Context, tfType string) *tfbridge.ProviderInfo {
	if idx := strings.IndexByte(tfType, '_'); idx > 0 {
		return r.providerInfoFor(ctx, tfType[:idx])
	}
	if tfType == "" {
		return nil
	}
	return r.providerInfoFor(ctx, tfType)
}

// providerInfoFor fetches the bridge ProviderInfo for a TF provider local
// name, threading the SDK-on-disk descriptor (when present) through to the
// mapper so dynamically bridged providers route via the correct
// parameterization. Returns nil on miss.
func (r *Resolver) providerInfoFor(ctx context.Context, tfProvider string) *tfbridge.ProviderInfo {
	if r.providerInfoSource == nil {
		return nil
	}
	var desc *workspace.PackageDescriptor
	if d, ok := r.packages[tfProvider]; ok {
		desc = &d
	}
	info, err := r.providerInfoSource.GetProviderInfo(ctx, tfProvider, desc)
	if err != nil {
		logging.V(5).Infof("bridge mapping unavailable for %q: %v", tfProvider, err)
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
	if info := r.providerInfoFor(ctx, providerName); info != nil {
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
