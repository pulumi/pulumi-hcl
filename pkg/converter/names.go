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

package converter

import (
	"maps"
	"path/filepath"
	"slices"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/pulumi/pulumi/pkg/v3/codegen/pcl"

	"github.com/pulumi/pulumi-hcl/pkg/hcl/parser"
)

// symbolKind is the HCL namespace of a top-level declaration. In one module,
// two declarations can have the same name when their kinds or types are
// different. PCL has one namespace for all top-level declarations except
// outputs, which have a namespace of their own.
type symbolKind int

const (
	symVariable symbolKind = iota
	symLocal
	symModule
	symResource
	symProvider
)

// symbol identifies a top-level HCL declaration that the PCL program declares.
type symbol struct {
	// dir is the directory of the module that declares the symbol.
	dir  string
	kind symbolKind
	// typ is the type of a resource, or the package of a provider.
	typ string
	// name is the HCL name. For a provider, it is the PCL logical name (see
	// [providerResourceName]).
	name string
}

// declaredSymbols returns the symbols that block declares, in source order.
func declaredSymbols(block *hclsyntax.Block) []symbol {
	dir := filepath.Dir(block.Range().Filename)
	sym := func(kind symbolKind, typ, name string) []symbol {
		return []symbol{{dir: dir, kind: kind, typ: typ, name: name}}
	}
	switch block.Type {
	case "variable":
		if len(block.Labels) >= 1 {
			return sym(symVariable, "", block.Labels[0])
		}
	case "module":
		if len(block.Labels) >= 1 {
			return sym(symModule, "", block.Labels[0])
		}
	case "resource":
		if len(block.Labels) >= 2 {
			return sym(symResource, block.Labels[0], block.Labels[1])
		}
	case "provider":
		if len(block.Labels) >= 1 {
			return sym(symProvider, block.Labels[0], providerResourceName(block))
		}
	case "locals":
		var syms []symbol
		for _, attr := range sortedAttributes(block.Body.Attributes) {
			syms = append(syms, sym(symLocal, "", attr.Name)...)
		}
		return syms
	}
	return nil
}

// nameSymbols gives each top-level declaration in bodies a PCL name that is
// unique in its module. A declaration keeps its HCL name when that name is a
// valid identifier and no declaration before it in source order has the name.
// The other declarations get a new name from [uniqueName].
func (ft *fileTransformer) nameSymbols(bodies []*hclsyntax.Body) {
	var syms []symbol
	for _, body := range bodies {
		dir := filepath.Dir(body.SrcRange.Filename)
		if ft.usedNames[dir] == nil {
			ft.usedNames[dir] = make(map[string]struct{})
		}
		for _, block := range body.Blocks {
			// The PCL form of a terraform block with attributes is a pulumi
			// block, which has a fixed name.
			if block.Type == "terraform" && len(block.Body.Attributes) > 0 {
				ft.usedNames[dir][pcl.PulumiBlockName] = struct{}{}
			}
			syms = append(syms, declaredSymbols(block)...)
		}
	}
	for _, s := range syms {
		used := ft.usedNames[s.dir]
		if _, taken := used[s.name]; parser.ValidIdentifier(s.name) && !taken {
			used[s.name] = struct{}{}
			ft.symbolNames[s] = s.name
		}
	}
	for _, s := range syms {
		if _, ok := ft.symbolNames[s]; !ok {
			ft.symbolNames[s] = uniqueName(s.name, ft.usedNames[s.dir])
		}
	}
}

// pclName returns the PCL name of the declaration of kind, typ and name in
// the module of r. If the module does not declare it, pclName returns name.
func (ft *fileTransformer) pclName(r hcl.Range, kind symbolKind, typ, name string) string {
	if pclName, ok := ft.symbolNames[symbol{filepath.Dir(r.Filename), kind, typ, name}]; ok {
		return pclName
	}
	return name
}

// callTargetName returns the PCL name of the resource or provider that a call
// block for name in the module of r targets. It finds the target as the
// runtime does: first a resource with the name, then a provider with the
// name.
func (ft *fileTransformer) callTargetName(r hcl.Range, name string) string {
	config := ft.modules[filepath.Dir(r.Filename)]
	if config == nil {
		return name
	}
	for _, key := range slices.Sorted(maps.Keys(config.Resources)) {
		if res := config.Resources[key]; res.Name == name {
			return ft.pclName(r, symResource, res.Type, name)
		}
	}
	for _, key := range slices.Sorted(maps.Keys(config.Providers)) {
		if p := config.Providers[key]; p.Alias == name || (p.Alias == "" && p.Name == name) {
			return ft.pclName(r, symProvider, p.Name, name)
		}
	}
	return name
}
