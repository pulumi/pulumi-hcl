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

package graph

import (
	"fmt"
	"iter"
	"maps"
	"slices"
	"strings"

	"github.com/agext/levenshtein"

	"github.com/pulumi/pulumi-hcl/pkg/hcl/modulepath"
)

// undeclaredReference describes a reference to key, a node that nothing
// declares. ok is false when the kind of the referenced object is not known
// from key.
func (g *Graph) undeclaredReference(key NodeKey) (summary, detail string, ok bool) {
	if g.providerRefs[key] {
		return "", "", false
	}
	if path, call, ok := g.undeclaredModuleCall(key); ok {
		scope, ok := g.scopes[path]
		if !ok {
			return "", "", false
		}
		return "Reference to undeclared module",
			fmt.Sprintf("No module call named %q is declared in %s.%s",
				call, moduleDisplayAddr(path), suggestName(call, maps.Keys(scope.config.Modules))),
			true
	}
	scope, ok := g.scopes[key.Module]
	if !ok {
		return "", "", false
	}
	config := scope.config
	namespace, name, ok := strings.Cut(key.ID, ".")
	if !ok {
		return "", "", false
	}
	switch namespace {
	case "var":
		suggestion := suggestName(name, maps.Keys(config.Variables))
		if suggestion == "" {
			suggestion = fmt.Sprintf(" This variable can be declared with a variable %q {} block.", name)
		}
		return "Reference to undeclared input variable",
			fmt.Sprintf("An input variable with the name %q has not been declared.%s", name, suggestion), true
	case "local":
		return "Reference to undeclared local value",
			fmt.Sprintf("A local value with the name %q has not been declared.%s",
				name, suggestName(name, maps.Keys(config.Locals))),
			true
	case "data":
		typ, resName, ok := strings.Cut(name, ".")
		if !ok {
			return "", "", false
		}
		var hint string
		if _, ok := config.Resources[name]; ok {
			hint = fmt.Sprintf("\n\nDid you mean the resource resource %s?", name)
		}
		return "Reference to undeclared resource",
			fmt.Sprintf("There is no data resource %q %q definition in %s.%s",
				typ, resName, moduleDisplayAddr(key.Module), hint),
			true
	case "output":
		// The module call is declared, so key names an output it lacks.
		return "Unsupported attribute", fmt.Sprintf("This object does not have an attribute named %q.", name), true
	case "call", "resource", "ephemeral":
		// A Pulumi method call has no Terraform equivalent, and a key from an
		// explicit "resource." or "ephemeral." prefix drops the resource name.
		return "", "", false
	}
	var hint string
	if _, ok := config.DataSources[key.ID]; ok {
		hint = fmt.Sprintf("\n\nDid you mean the data resource data.%s?", key.ID)
	}
	return "Reference to undeclared resource",
		fmt.Sprintf("There is no managed resource %q %q definition in %s.%s",
			namespace, name, moduleDisplayAddr(key.Module), hint),
		true
}

// suggestName returns a " Did you mean ...?" sentence for the first candidate,
// in sorted order, that is close to given, or "" when none is close.
func suggestName(given string, candidates iter.Seq[string]) string {
	for _, candidate := range slices.Sorted(candidates) {
		if levenshtein.Distance(given, candidate, nil) < 3 {
			return fmt.Sprintf(" Did you mean %q?", candidate)
		}
	}
	return ""
}

// undeclaredModuleCall reports whether key refers through a module call that
// is not declared. It returns the path of the module that the reference is
// written in and the name of the call.
func (g *Graph) undeclaredModuleCall(key NodeKey) (modulepath.Path, string, bool) {
	if name, ok := strings.CutPrefix(key.ID, "module."); ok {
		return key.Module, name, true
	}
	if !strings.HasPrefix(key.ID, "output.") {
		return modulepath.Path{}, "", false
	}
	if _, declared := g.loaded[key.Module]; declared {
		return modulepath.Path{}, "", false
	}
	parent, call, ok := key.Module.Parent()
	return parent, call.Name(), ok
}

// moduleDisplayAddr names the module at path for a diagnostic.
func moduleDisplayAddr(path modulepath.Path) string {
	if path.IsRoot() {
		return "the root module"
	}
	return ModuleAddress(path)
}
