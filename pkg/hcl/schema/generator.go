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

// Package schema generates Pulumi package schemas from HCL module definitions.
package schema

import (
	"context"
	"encoding/json"
	"fmt"
	gotoken "go/token"
	"maps"
	"slices"
	"sort"
	"strings"

	"github.com/blang/semver"
	"github.com/pulumi/pulumi-hcl/pkg/hcl/ast"
	"github.com/pulumi/pulumi-hcl/pkg/hcl/bridge"
	"github.com/pulumi/pulumi-hcl/pkg/hcl/eval"
	"github.com/pulumi/pulumi-hcl/pkg/hcl/packages"
	"github.com/pulumi/pulumi-hcl/pkg/hcl/transform"
	pulumischema "github.com/pulumi/pulumi/pkg/v3/codegen/schema"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource/urn"
	"github.com/pulumi/pulumi/sdk/v3/go/common/tokens"
	"github.com/pulumi/pulumi/sdk/v3/go/property"
	"github.com/zclconf/go-cty/cty"
	"github.com/zclconf/go-cty/cty/function"
)

// ResourceTypeResolver resolves a TF resource or data source type to its Pulumi
// schema and bridge mapping, so an output expression that references a resource
// or data source can be typed. It is satisfied by *packages.Resolver.
type ResourceTypeResolver interface {
	ResolveResource(ctx context.Context, tfType string) (*pulumischema.Resource, error)
	ResolveFunction(ctx context.Context, tfType string) (*pulumischema.Function, error)
	ResourceBodyMapping(ctx context.Context, tfType string) *bridge.BodyMapping
	DataSourceBodyMapping(ctx context.Context, tfType string) *bridge.BodyMapping
	ProviderFunctions(ctx context.Context, providerName string) (map[string]packages.ProviderFunction, error)
}

// ModuleLoader loads a child module's parsed configuration and resolved
// directory, so module.<name>.<output> references can be typed by recursively
// typing the child module's outputs.
type ModuleLoader interface {
	LoadModule(
		ctx context.Context, source, versionConstraint, callerDir string,
	) (config *ast.Config, dir string, err error)
}

// Binder supplies the external lookups used to type output expressions that
// reference resources, data sources, or child modules. A nil *Binder, or a nil
// field within it, leaves the corresponding references untyped (they fall back
// to "any"). Variables and locals are always typed, without a Binder.
type Binder struct {
	// Resources types resource and data source references from provider schemas.
	Resources ResourceTypeResolver
	// Modules loads child modules so their outputs can be typed.
	Modules ModuleLoader
	// ModuleDir is the directory child module sources are resolved relative to.
	ModuleDir string
}

func (b *Binder) child(dir string) *Binder {
	return &Binder{Resources: b.Resources, Modules: b.Modules, ModuleDir: dir}
}

// ModuleSchema represents a generated schema for an HCL module.
type ModuleSchema struct {
	// PackageName is the package name used in the schema and token prefix.
	PackageName string `json:"packageName"`

	// Version is the module version.
	Version string `json:"version,omitempty"`

	// ComponentName is the component name used in the token suffix.
	ComponentName string `json:"componentName"`

	// Module is the module segment used in the token middle.
	Module string `json:"module"`

	// Description is the module description.
	Description string `json:"description,omitempty"`

	// InputProperties are the module's input variables.
	InputProperties map[string]*PropertySpec `json:"inputProperties,omitempty"`

	// RequiredInputs lists required input property names.
	RequiredInputs []string `json:"requiredInputs,omitempty"`

	// OutputProperties are the module's outputs.
	OutputProperties map[string]*PropertySpec `json:"outputProperties,omitempty"`

	// RequiredOutputs lists output property names that are never null.
	RequiredOutputs []string `json:"requiredOutputs,omitempty"`
}

// PropertyType enumerates the types a schema property can have.
type PropertyType string

const (
	TypeString  PropertyType = "string"
	TypeNumber  PropertyType = "number"
	TypeBoolean PropertyType = "boolean"
	TypeArray   PropertyType = "array"
	TypeObject  PropertyType = "object"
	// TypeAny is a dynamic or unconstrained value.
	TypeAny PropertyType = "any"
)

// PropertySpec describes a property in the schema.
type PropertySpec struct {
	// Type is the property type.
	Type PropertyType `json:"type,omitempty"`

	// Description is the property description.
	Description string `json:"description,omitempty"`

	// Default is the default value, if any.
	Default any `json:"default,omitempty"`

	// Secret indicates if the property is secret.
	Secret bool `json:"secret,omitempty"`

	// Items describes array element types.
	Items *PropertySpec `json:"items,omitempty"`

	// AdditionalProperties describes map value types.
	AdditionalProperties *PropertySpec `json:"additionalProperties,omitempty"`

	// Properties describes object properties.
	Properties map[string]*PropertySpec `json:"properties,omitempty"`

	// Required lists the names of object properties that are never null.
	Required []string `json:"required,omitempty"`

	// Ref is a reference to another type definition.
	Ref string `json:"$ref,omitempty"`

	// OneOf lists the alternatives of a union type: the value is of exactly one
	// of them.
	OneOf []*PropertySpec `json:"oneOf,omitempty"`
}

// GenerateModuleSchema generates a Pulumi schema from an HCL module
// configuration.
func GenerateModuleSchema(
	ctx context.Context, config *ast.Config, binder *Binder,
	token tokens.Type, version semver.Version,
) (*ModuleSchema, error) {
	schema := &ModuleSchema{
		PackageName:      token.Package().Name().String(),
		Version:          version.String(),
		ComponentName:    token.Name().String(),
		Module:           token.Module().Name().String(),
		InputProperties:  make(map[string]*PropertySpec),
		OutputProperties: make(map[string]*PropertySpec),
	}

	for _, v := range config.Variables {
		prop, err := variableToPropertySpec(v)
		if err != nil {
			return nil, fmt.Errorf("processing variable %q: %w", v.Name, err)
		}
		schema.InputProperties[v.Name] = prop

		// Track required inputs (no default and not nullable)
		if v.Default == nil && !v.Nullable {
			schema.RequiredInputs = append(schema.RequiredInputs, v.Name)
		}

		// Inputs are echoed as outputs, so a non-null input is a required output.
		if !v.Nullable {
			schema.RequiredOutputs = append(schema.RequiredOutputs, v.Name)
		}
	}

	scope, err := buildTypeScope(ctx, config, binder, map[string]bool{})
	if err != nil {
		return nil, err
	}
	evaluator := eval.NewEvaluator(scope)
	for _, o := range config.Outputs {
		val, err := inferOutputType(evaluator, o)
		if err != nil {
			return nil, fmt.Errorf("typing output %q: %w", o.Name, err)
		}
		prop, err := outputToPropertySpec(o, val)
		if err != nil {
			return nil, fmt.Errorf("processing output %q: %w", o.Name, err)
		}
		schema.OutputProperties[o.Name] = prop
		if !couldBeNull(val) {
			schema.RequiredOutputs = append(schema.RequiredOutputs, o.Name)
		}
	}

	sort.Strings(schema.RequiredInputs)
	sort.Strings(schema.RequiredOutputs)

	return schema, nil
}

// buildTypeScope returns an evaluation context in which variable, local,
// resource, data source, and module references resolve to unknown values of
// their types, so that an output expression can be evaluated for its type alone.
// Type information rides through cty evaluation, so typing an output is just
// evaluating it against typed unknowns and reading the result type.
//
// Every reference must resolve: a resource/data source/module that cannot be
// resolved, or a local that cannot be typed (an unresolvable reference or a
// cycle), is a hard error rather than a silent fallback to "any". path holds the
// module directories on the current recursion stack to detect module cycles.
func buildTypeScope(
	ctx context.Context, config *ast.Config, binder *Binder, path map[string]bool,
) (*eval.Context, error) {
	// Root the scope at the module's own directory so path.module-relative
	// file reads resolve against the module's files rather than the
	// provider's working directory.
	dir := "."
	if binder != nil && binder.ModuleDir != "" {
		dir = binder.ModuleDir
	}
	scope, err := eval.NewContext(dir, dir, dir, "", "", "")
	if err != nil {
		return nil, err
	}
	scope.UseTypeInferenceFunctions()

	for name, v := range config.Variables {
		t := v.TypeConstraint
		if t == cty.NilType {
			t = cty.DynamicPseudoType
		}
		scope.SetVariable(name, transform.RefinedUnknown(t, v.Nullable))
	}

	if binder != nil {
		if err := seedResourceTypes(ctx, scope, config, binder.Resources); err != nil {
			return nil, err
		}
		if err := seedModuleTypes(ctx, scope, config, binder, path); err != nil {
			return nil, err
		}
		if err := seedProviderFunctions(ctx, scope, config, binder.Resources); err != nil {
			return nil, err
		}
	}

	if err := seedLocalTypes(scope, config); err != nil {
		return nil, err
	}

	return scope, nil
}

// seedProviderFunctions installs type-only projections of the provider-defined
// functions the module's expressions call, so those calls type-check and
// evaluate to unknown values of their declared return types. Modules whose
// files the parser could not scan for calls (JSON syntax) seed every declared
// provider's functions instead, tolerating providers that fail to resolve.
func seedProviderFunctions(
	ctx context.Context, scope *eval.Context, config *ast.Config, resolver ResourceTypeResolver,
) error {
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
		return nil
	}
	if resolver == nil {
		return fmt.Errorf("cannot type provider-defined function calls: no resource resolver provided")
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
		fns, err := resolver.ProviderFunctions(ctx, providerName)
		if err != nil {
			if lenient {
				continue
			}
			return fmt.Errorf("resolving provider functions for %q: %w", providerName, err)
		}
		for tfName, fnSchema := range fns {
			f, err := transform.ProviderFunction(fnSchema.Function, fnSchema.Variadic, false, nil)
			if err != nil {
				return fmt.Errorf("projecting function provider::%s::%s: %w", providerName, tfName, err)
			}
			table[ast.ProviderFunctionName(providerName, tfName)] = f
		}
	}
	scope.SetProviderFunctions(table)
	return nil
}

// seedLocalTypes types each local against the (already seeded) scope and binds
// it. Locals may reference variables, resources, modules, and one another, so it
// evaluates to a fixpoint: each pass types any local whose references already
// resolve. If a pass makes no progress while locals remain, those locals cannot
// be typed (an unresolvable reference or a local cycle) and the first such
// failure is returned. Each local binds the value it evaluated to, preserving
// the nullability refinements references through the local read later.
func seedLocalTypes(scope *eval.Context, config *ast.Config) error {
	remaining := maps.Clone(config.Locals)
	for len(remaining) > 0 {
		progress := false
		for name, l := range remaining {
			val, diags := typeExpr(l.Value, scope.HCLContext())
			if diags.HasErrors() {
				continue
			}
			scope.SetLocal(name, val)
			delete(remaining, name)
			progress = true
		}
		if !progress {
			for _, name := range slices.Sorted(maps.Keys(remaining)) {
				_, diags := typeExpr(remaining[name].Value, scope.HCLContext())
				return fmt.Errorf("typing local %q: %s", name, diags.Error())
			}
		}
	}
	return nil
}

// rangedType wraps an element reference type to match how a count or for_each
// reference is shaped: count yields a list of instances, for_each a map of them.
func rangedType(elem cty.Type, count, forEach bool) cty.Type {
	switch {
	case count:
		return cty.List(elem)
	case forEach:
		return cty.Map(elem)
	default:
		return elem
	}
}

// seedResourceTypes binds each resource and data source to an unknown value of
// its reference type, resolved from its provider schema. Ranged (count/for_each)
// references are bound as a list/map of the element type. A type that cannot be
// resolved is an error.
func seedResourceTypes(
	ctx context.Context, scope *eval.Context, config *ast.Config, resolver ResourceTypeResolver,
) error {
	if resolver == nil {
		if len(config.Resources) > 0 || len(config.DataSources) > 0 {
			return fmt.Errorf("cannot type resource references: no resource resolver provided")
		}
		return nil
	}
	for _, key := range slices.Sorted(maps.Keys(config.Resources)) {
		res := config.Resources[key]
		schemaRes, err := resolver.ResolveResource(ctx, res.Type)
		if err != nil {
			return fmt.Errorf("resolving resource %q: %w", key, err)
		}
		if schemaRes == nil {
			return fmt.Errorf("resolving resource %q: no schema for type %q", key, res.Type)
		}
		mapping := resolver.ResourceBodyMapping(ctx, res.Type)
		ranged := res.Count != nil || res.ForEach != nil
		ty := rangedType(transform.ResourceReferenceType(schemaRes, mapping), res.Count != nil, res.ForEach != nil)
		scope.SetResource(key, urn.URN(""), markRangedRef(transform.RefinedUnknown(ty, false), ranged))
	}

	for _, key := range slices.Sorted(maps.Keys(config.DataSources)) {
		ds := config.DataSources[key]
		fn, err := resolver.ResolveFunction(ctx, ds.Type)
		if err != nil {
			return fmt.Errorf("resolving data source %q: %w", key, err)
		}
		if fn == nil {
			return fmt.Errorf("resolving data source %q: no schema for type %q", key, ds.Type)
		}
		mapping := resolver.DataSourceBodyMapping(ctx, ds.Type)
		ranged := ds.Count != nil || ds.ForEach != nil
		ty := rangedType(transform.DataSourceReferenceType(fn, mapping), ds.Count != nil, ds.ForEach != nil)
		scope.SetDataSource(key, markRangedRef(transform.RefinedUnknown(ty, false), ranged))
	}
	return nil
}

// seedModuleTypes binds each module call to an unknown value of its output
// object type, computed by recursively typing the child module's outputs.
// Ranged calls are bound as a list/map of that object. A module that cannot be
// loaded, or whose source is already on the recursion path (a module cycle), is
// an error.
func seedModuleTypes(
	ctx context.Context, scope *eval.Context, config *ast.Config, binder *Binder, path map[string]bool,
) error {
	if binder.Modules == nil {
		if len(config.Modules) > 0 {
			return fmt.Errorf("cannot type module references: no module loader provided")
		}
		return nil
	}
	for _, name := range slices.Sorted(maps.Keys(config.Modules)) {
		call := config.Modules[name]
		childConfig, dir, err := binder.Modules.LoadModule(ctx, call.Source, call.Version, binder.ModuleDir)
		if err != nil {
			return fmt.Errorf("loading module %q: %w", name, err)
		}
		if path[dir] {
			return fmt.Errorf("module cycle through %q (%s)", name, dir)
		}

		path[dir] = true
		childScope, err := buildTypeScope(ctx, childConfig, binder.child(dir), path)
		delete(path, dir)
		if err != nil {
			return fmt.Errorf("typing module %q: %w", name, err)
		}

		childEval := eval.NewEvaluator(childScope)
		attrs := make(map[string]cty.Value, len(childConfig.Outputs))
		for _, o := range childConfig.Outputs {
			val, err := inferOutputType(childEval, o)
			if err != nil {
				return fmt.Errorf("typing module %q output %q: %w", name, o.Name, err)
			}
			attrs[o.Name] = val
		}
		// A direct reference resolves attribute by attribute, so the object
		// value carries each output's nullability. A ranged reference is indexed
		// first, dropping that refinement, so seed a non-null list/map instead.
		obj := cty.ObjectVal(attrs)
		var val cty.Value
		switch {
		case call.Count != nil:
			val = cty.UnknownVal(cty.List(obj.Type())).RefineNotNull().Mark(rangedRefMark{})
		case call.ForEach != nil:
			val = cty.UnknownVal(cty.Map(obj.Type())).RefineNotNull().Mark(rangedRefMark{})
		default:
			val = obj
		}
		scope.SetModule(name, val)
	}
	return nil
}

// inferOutputType evaluates an output's value expression against the type scope
// and returns the resulting unknown value, whose type and nullability describe
// the output. An output with no value, or one that cannot be evaluated against
// the scope, is an error. The value keeps only its union marks, so its range
// can be read.
func inferOutputType(evaluator *eval.Evaluator, o *ast.Output) (cty.Value, error) {
	if o.Value == nil {
		return cty.NilVal, fmt.Errorf("output has no value expression")
	}
	val, diags := typeExpr(o.Value, evaluator.Context().HCLContext())
	if diags.HasErrors() {
		return cty.NilVal, fmt.Errorf("%s", diags.Error())
	}
	return keepUnions(val), nil
}

// variableToPropertySpec converts an HCL variable to a PropertySpec.
func variableToPropertySpec(v *ast.Variable) (*PropertySpec, error) {
	prop := &PropertySpec{
		Description: v.Description,
		Secret:      v.Sensitive || v.Ephemeral,
	}

	// Convert type constraint to schema type
	if v.TypeConstraint != cty.NilType {
		typeSpec, err := ctyTypeToPropertySpec(v.TypeConstraint)
		if err != nil {
			return nil, err
		}
		prop.Type = typeSpec.Type
		prop.Items = typeSpec.Items
		prop.AdditionalProperties = typeSpec.AdditionalProperties
		prop.Properties = typeSpec.Properties
	} else {
		// Default to any type if no constraint specified
		prop.Type = TypeAny
	}

	// A variable default is a constant expression (it cannot reference other
	// values), so a nil evaluation context is sufficient. The Pulumi schema
	// only permits constant defaults on primitive properties; a default on a
	// collection or object is conveyed by the property being optional rather
	// than by an explicit default value.
	if v.Default != nil {
		val, diags := v.Default.Value(nil)
		if diags.HasErrors() {
			return nil, fmt.Errorf("evaluating default: %s", diags.Error())
		}
		if d, ok := ctyToConstant(val); ok {
			prop.Default = d
		}
	}

	return prop, nil
}

// ctyToConstant converts a primitive cty.Value to a Go value usable as a Pulumi
// schema default. Only booleans, numbers, and strings are valid schema
// constants, so null and non-primitive values return ok=false.
func ctyToConstant(val cty.Value) (any, bool) {
	if val.IsNull() {
		return nil, false
	}
	switch val.Type() {
	case cty.String:
		return val.AsString(), true
	case cty.Number:
		f, _ := val.AsBigFloat().Float64()
		return f, true
	case cty.Bool:
		return val.True(), true
	default:
		return nil, false
	}
}

// outputToPropertySpec converts an HCL output to a PropertySpec. val is the
// unknown value inferred from the output's value expression; its type gives the
// property shape and its per-attribute nullability gives nested required fields.
// A DynamicPseudoType maps to the any type.
func outputToPropertySpec(o *ast.Output, val cty.Value) (*PropertySpec, error) {
	prop, err := ctyValueToPropertySpec(val)
	if err != nil {
		return nil, err
	}
	prop.Description = o.Description
	prop.Secret = o.Sensitive || o.Ephemeral
	return prop, nil
}

// ctyValueToPropertySpec converts an inferred unknown value to a PropertySpec.
// A union value becomes a OneOf over its members. For object types it reads
// each attribute's nullability from the value's refinements to populate
// Required; collections fall back to ctyTypeToPropertySpec, where nested object
// fields' requiredness rides on optional-attribute metadata.
func ctyValueToPropertySpec(v cty.Value) (*PropertySpec, error) {
	if members, ok := unionMembers(v); ok {
		oneOf := make([]*PropertySpec, len(members))
		for i, m := range members {
			spec, err := ctyValueToPropertySpec(m)
			if err != nil {
				return nil, err
			}
			oneOf[i] = spec
		}
		return &PropertySpec{OneOf: oneOf}, nil
	}
	t := v.Type()
	if !t.IsObjectType() {
		return ctyTypeToPropertySpec(t)
	}
	props := make(map[string]*PropertySpec, len(t.AttributeTypes()))
	var required []string
	for name := range t.AttributeTypes() {
		attr := v.GetAttr(name)
		propSpec, err := ctyValueToPropertySpec(attr)
		if err != nil {
			return nil, err
		}
		props[name] = propSpec
		if !couldBeNull(attr) {
			required = append(required, name)
		}
	}
	sort.Strings(required)
	return &PropertySpec{Type: TypeObject, Properties: props, Required: required}, nil
}

// ctyTypeToPropertySpec converts a cty.Type to a PropertySpec.
func ctyTypeToPropertySpec(t cty.Type) (*PropertySpec, error) {
	switch {
	case t == cty.String:
		return &PropertySpec{Type: TypeString}, nil

	case t == cty.Number:
		return &PropertySpec{Type: TypeNumber}, nil

	case t == cty.Bool:
		return &PropertySpec{Type: TypeBoolean}, nil

	case t == cty.DynamicPseudoType:
		return &PropertySpec{Type: TypeAny}, nil

	case t.IsListType():
		elemSpec, err := ctyTypeToPropertySpec(t.ElementType())
		if err != nil {
			return nil, err
		}
		return &PropertySpec{
			Type:  TypeArray,
			Items: elemSpec,
		}, nil

	case t.IsSetType():
		// Sets are represented as arrays in JSON Schema
		elemSpec, err := ctyTypeToPropertySpec(t.ElementType())
		if err != nil {
			return nil, err
		}
		return &PropertySpec{
			Type:  TypeArray,
			Items: elemSpec,
		}, nil

	case t.IsMapType():
		elemSpec, err := ctyTypeToPropertySpec(t.ElementType())
		if err != nil {
			return nil, err
		}
		return &PropertySpec{
			Type:                 TypeObject,
			AdditionalProperties: elemSpec,
		}, nil

	case t.IsTupleType():
		// Tuples are represented as arrays with the first element type. An empty
		// tuple (e.g. the literal `[]`) has no element type; treat it as an array
		// of any, since the Pulumi schema requires an items type on every array.
		elemTypes := t.TupleElementTypes()
		elem := cty.DynamicPseudoType
		if len(elemTypes) > 0 {
			elem = elemTypes[0]
		}
		elemSpec, err := ctyTypeToPropertySpec(elem)
		if err != nil {
			return nil, err
		}
		return &PropertySpec{
			Type:  TypeArray,
			Items: elemSpec,
		}, nil

	case t.IsObjectType():
		optional := t.OptionalAttributes()
		props := make(map[string]*PropertySpec)
		var required []string
		for name, attrType := range t.AttributeTypes() {
			propSpec, err := ctyTypeToPropertySpec(attrType)
			if err != nil {
				return nil, err
			}
			props[name] = propSpec
			if _, isOptional := optional[name]; !isOptional {
				required = append(required, name)
			}
		}
		sort.Strings(required)
		return &PropertySpec{
			Type:       TypeObject,
			Properties: props,
			Required:   required,
		}, nil

	default:
		// Fall back to the any type for unknown types
		return &PropertySpec{Type: TypeAny}, nil
	}
}

// ToJSON serializes the schema to JSON.
func (s *ModuleSchema) ToJSON() ([]byte, error) {
	return json.MarshalIndent(s, "", "  ")
}

// pulumiCase converts a snake_case HCL identifier (a variable, output, or object
// field name) to the camelCase name Pulumi conventionally uses for the schema
// property. props is nil because an MLC module has no provider property list to
// resolve against — the convention is the definition of the camelCase name.
//
// This conversion is only ever applied in the snake→camel direction, and the
// snake_case name is always retained as the source of truth (the boundary
// converters below iterate the snake-keyed spec rather than inverting a
// camelCase name), so the lossy camel→snake inverse is never relied upon.
func pulumiCase(snake string) string {
	name, _ := transform.PulumiCaseFromSnakeCase(snake, nil)
	return name
}

// InputsToHCL maps component inputs — keyed by their camelCase schema property
// names — to the snake_case names the HCL module declares, recursively renaming
// object fields at every depth. Map keys are user data and are left unchanged.
// It iterates the snake-keyed input spec and looks up each input by its derived
// camelCase name, so the mapping never depends on inverting a camelCase name.
func (s *ModuleSchema) InputsToHCL(inputs property.Map) property.Map {
	out := make(map[string]property.Value, inputs.Len())
	for snake, spec := range s.InputProperties {
		if v, ok := inputs.GetOk(pulumiCase(snake)); ok {
			out[snake] = convertPropertyNames(v, spec, false)
		}
	}
	return property.NewMap(out)
}

// OutputsToPulumi maps the module's outputs — keyed by their snake_case HCL
// names — to the camelCase schema property names, recursively renaming object
// fields at every depth. Map keys are left unchanged.
func (s *ModuleSchema) OutputsToPulumi(outputs property.Map) property.Map {
	out := make(map[string]property.Value, outputs.Len())
	for snake, v := range outputs.All {
		out[pulumiCase(snake)] = convertPropertyNames(v, s.OutputProperties[snake], true)
	}
	return property.NewMap(out)
}

// convertPropertyNames recursively renames object-field names in v according to
// spec, which describes v's type. Object fields (spec.Properties, snake_case)
// are renamed between snake_case and camelCase; the dynamic keys of a map
// (spec.AdditionalProperties) are user data and left unchanged. toPulumi selects
// the direction: snake_case→camelCase when true, the reverse when false. A
// value's secret flag and dependencies are preserved across the rename.
func convertPropertyNames(v property.Value, spec *PropertySpec, toPulumi bool) property.Value {
	if spec == nil || v.IsNull() || v.IsComputed() {
		return v
	}
	if len(spec.OneOf) > 0 {
		spec = mergeSpecs(spec.OneOf)
	}
	switch {
	case len(spec.Properties) > 0 && v.IsMap():
		obj := v.AsMap()
		out := make(map[string]property.Value, obj.Len())
		for snake, fieldSpec := range spec.Properties {
			src, dst := pulumiCase(snake), snake
			if toPulumi {
				src, dst = dst, src
			}
			if fv, ok := obj.GetOk(src); ok {
				out[dst] = convertPropertyNames(fv, fieldSpec, toPulumi)
			}
		}
		return property.WithGoValue(v, out)
	case spec.AdditionalProperties != nil && v.IsMap():
		obj := v.AsMap()
		out := make(map[string]property.Value, obj.Len())
		for k, mv := range obj.All {
			out[k] = convertPropertyNames(mv, spec.AdditionalProperties, toPulumi)
		}
		return property.WithGoValue(v, out)
	case spec.Items != nil && v.IsArray():
		arr := v.AsArray()
		out := make([]property.Value, arr.Len())
		for i, e := range arr.All {
			out[i] = convertPropertyNames(e, spec.Items, toPulumi)
		}
		return property.WithGoValue(v, out)
	default:
		return v
	}
}

// mergeSpecs folds the alternatives of a union into one spec that renames every
// field any alternative declares. A field's Pulumi name depends only on its HCL
// name, so renaming against the merged spec renames a value of any alternative
// correctly; the merged spec describes no single alternative's type.
func mergeSpecs(specs []*PropertySpec) *PropertySpec {
	merged := &PropertySpec{}
	byName := map[string][]*PropertySpec{}
	var items, additional []*PropertySpec
	for _, spec := range specs {
		if len(spec.OneOf) > 0 {
			spec = mergeSpecs(spec.OneOf)
		}
		for name, field := range spec.Properties {
			byName[name] = append(byName[name], field)
		}
		if spec.Items != nil {
			items = append(items, spec.Items)
		}
		if spec.AdditionalProperties != nil {
			additional = append(additional, spec.AdditionalProperties)
		}
	}
	if len(byName) > 0 {
		merged.Properties = make(map[string]*PropertySpec, len(byName))
		for name, fields := range byName {
			merged.Properties[name] = mergeSpecs(fields)
		}
	}
	if len(items) > 0 {
		merged.Items = mergeSpecs(items)
	}
	if len(additional) > 0 {
		merged.AdditionalProperties = mergeSpecs(additional)
	}
	return merged
}

// ToPulumiPackageSchema converts the module schema to a full Pulumi package
// schema serving this single component.
func (s *ModuleSchema) ToPulumiPackageSchema() pulumischema.PackageSpec {
	spec, err := PackageSchema(s, []*ModuleSchema{s})
	if err != nil {
		panic(err)
	}
	return spec
}

// PackageSchema builds the package schema serving components, one component
// resource each. Package identity comes from the first component; the package
// description comes from root, which is nil for a package whose root directory
// holds no component.
func PackageSchema(root *ModuleSchema, components []*ModuleSchema) (pulumischema.PackageSpec, error) {
	if len(components) == 0 {
		return pulumischema.PackageSpec{}, fmt.Errorf("no components to combine")
	}
	spec := pulumischema.PackageSpec{
		Name:      components[0].PackageName,
		Version:   components[0].Version,
		Types:     map[string]pulumischema.ComplexTypeSpec{},
		Resources: map[string]pulumischema.ResourceSpec{},
	}
	if root != nil {
		spec.Description = root.Description
	}
	for _, c := range components {
		token, res, types := c.resourceSpec()
		if _, ok := spec.Resources[token]; ok {
			return pulumischema.PackageSpec{}, fmt.Errorf("component token %q is defined twice", token)
		}
		spec.Resources[token] = res
		for t, typ := range types {
			if _, ok := spec.Types[t]; ok {
				return pulumischema.PackageSpec{}, fmt.Errorf("type token %q is defined twice", t)
			}
			spec.Types[t] = typ
		}
	}
	overrides, err := goModuleOverrides(components)
	if err != nil {
		return pulumischema.PackageSpec{}, err
	}
	if len(overrides) > 0 {
		info, err := json.Marshal(map[string]any{
			"moduleToPackage":      overrides,
			"respectSchemaVersion": true,
		})
		if err != nil {
			return pulumischema.PackageSpec{}, err
		}
		spec.Language = map[string]pulumischema.RawMessage{"go": info}
	}
	return spec, nil
}

// goModuleOverrides maps module segments that would render as invalid Go
// package names to valid ones, so the generated Go SDK parses.
func goModuleOverrides(components []*ModuleSchema) (map[string]string, error) {
	overrides := map[string]string{}
	seen := map[string]string{}
	for _, c := range components {
		pkg := strings.ReplaceAll(c.Module, "-", "")
		if !gotoken.IsIdentifier(pkg) {
			pkg = "_" + pkg
		}
		if prev, ok := seen[pkg]; ok && prev != c.Module {
			return nil, fmt.Errorf("modules %q and %q both map to Go package %q", prev, c.Module, pkg)
		}
		seen[pkg] = c.Module
		if pkg != c.Module {
			overrides[c.Module] = pkg
		}
	}
	return overrides, nil
}

// resourceSpec renders the component's resource spec and the named types it
// references. Variable and output names, which are snake_case in HCL, are
// exposed under their camelCase Pulumi property names. Object types are emitted
// as named types and referenced via `$ref`, so a consumer binds them as proper
// object types (a property's inline type spec cannot carry `properties`).
func (s *ModuleSchema) resourceSpec() (string, pulumischema.ResourceSpec, map[string]pulumischema.ComplexTypeSpec) {
	componentToken := fmt.Sprintf("%s:%s:%s", s.PackageName, s.Module, s.ComponentName)
	types := make(map[string]pulumischema.ComplexTypeSpec)

	inputProps := make(map[string]pulumischema.PropertySpec)
	for name, prop := range s.InputProperties {
		inputProps[pulumiCase(name)] = s.schemaProperty(prop, pascalCase(pulumiCase(name)), types)
	}

	// Output properties are the inputs plus the declared outputs.
	outputProps := make(map[string]pulumischema.PropertySpec)
	for name, prop := range s.InputProperties {
		outputProps[pulumiCase(name)] = s.schemaProperty(prop, pascalCase(pulumiCase(name)), types)
	}
	for name, prop := range s.OutputProperties {
		outputProps[pulumiCase(name)] = s.schemaProperty(prop, pascalCase(pulumiCase(name)), types)
	}

	requiredInputs := make([]string, 0, len(s.RequiredInputs))
	for _, name := range s.RequiredInputs {
		requiredInputs = append(requiredInputs, pulumiCase(name))
	}
	sort.Strings(requiredInputs)

	// Dedup in case an input and an output share a name, collapsing to one
	// output property.
	requiredSet := make(map[string]struct{}, len(s.RequiredOutputs))
	for _, name := range s.RequiredOutputs {
		requiredSet[pulumiCase(name)] = struct{}{}
	}
	requiredOutputs := make([]string, 0, len(requiredSet))
	for name := range requiredSet {
		requiredOutputs = append(requiredOutputs, name)
	}
	sort.Strings(requiredOutputs)

	return componentToken, pulumischema.ResourceSpec{
		ObjectTypeSpec: pulumischema.ObjectTypeSpec{
			Type:        "object",
			Description: s.Description,
			Properties:  outputProps,
			Required:    requiredOutputs,
		},
		IsComponent:     true,
		InputProperties: inputProps,
		RequiredInputs:  requiredInputs,
	}, types
}

// pascalCase upper-cases the first letter of a camelCase name, for use in a type
// token.
func pascalCase(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// schemaProperty converts a PropertySpec to a Pulumi PropertySpec, carrying the
// property-level description, secret flag, and default over the underlying type.
func (s *ModuleSchema) schemaProperty(
	prop *PropertySpec, typeName string, types map[string]pulumischema.ComplexTypeSpec,
) pulumischema.PropertySpec {
	return pulumischema.PropertySpec{
		TypeSpec:    s.schemaType(prop, typeName, types),
		Description: prop.Description,
		Secret:      prop.Secret,
		Default:     prop.Default,
	}
}

// schemaType converts a PropertySpec to a Pulumi TypeSpec. An object type is
// registered as a named type (keyed by a token derived from typeName) in types
// and referenced via `$ref`; the dynamic keys of a map are not named, so its
// value type recurses without registering field names.
func (s *ModuleSchema) schemaType(
	prop *PropertySpec, typeName string, types map[string]pulumischema.ComplexTypeSpec,
) pulumischema.TypeSpec {
	switch {
	case len(prop.OneOf) > 0:
		oneOf := make([]pulumischema.TypeSpec, len(prop.OneOf))
		for i, member := range prop.OneOf {
			oneOf[i] = s.schemaType(member, fmt.Sprintf("%s%d", typeName, i), types)
		}
		return pulumischema.TypeSpec{OneOf: oneOf}
	case prop.Properties != nil:
		token := fmt.Sprintf("%s:%s:%s", s.PackageName, s.Module, typeName)
		fields := make(map[string]pulumischema.PropertySpec, len(prop.Properties))
		for name, field := range prop.Properties {
			camel := pulumiCase(name)
			fields[camel] = s.schemaProperty(field, typeName+pascalCase(camel), types)
		}
		required := make([]string, 0, len(prop.Required))
		for _, name := range prop.Required {
			required = append(required, pulumiCase(name))
		}
		sort.Strings(required)
		types[token] = pulumischema.ComplexTypeSpec{
			ObjectTypeSpec: pulumischema.ObjectTypeSpec{Type: "object", Properties: fields, Required: required},
		}
		return pulumischema.TypeSpec{Ref: "#/types/" + token}
	case prop.AdditionalProperties != nil:
		elem := s.schemaType(prop.AdditionalProperties, typeName+"Value", types)
		return pulumischema.TypeSpec{Type: "object", AdditionalProperties: &elem}
	case prop.Items != nil:
		elem := s.schemaType(prop.Items, typeName+"Item", types)
		return pulumischema.TypeSpec{Type: "array", Items: &elem}
	case prop.Type == TypeAny:
		return pulumischema.TypeSpec{Ref: "pulumi.json#/Any"}
	default:
		return pulumischema.TypeSpec{Type: string(prop.Type)}
	}
}
