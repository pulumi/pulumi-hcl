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

// Package transform handles conversion between cty values and Pulumi property values.
package transform

import (
	"errors"
	"fmt"
	"maps"
	"math/big"
	"os"
	"slices"
	"strconv"
	"strings"
	"unicode"

	"github.com/hashicorp/hcl/v2"
	"github.com/pulumi/pulumi-hcl/pkg/hcl/bridge"
	"github.com/pulumi/pulumi-hcl/pkg/hcl/eval"
	"github.com/pulumi/pulumi/pkg/v3/codegen"
	"github.com/pulumi/pulumi/pkg/v3/codegen/cgstrings"
	"github.com/pulumi/pulumi/pkg/v3/codegen/schema"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource/archive"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource/asset"
	"github.com/pulumi/pulumi/sdk/v3/go/common/util/cmdutil"
	"github.com/pulumi/pulumi/sdk/v3/go/property"
	"github.com/zclconf/go-cty/cty"
	"github.com/zclconf/go-cty/cty/convert"
)

// EvalFunc evaluates an HCL expression.
//
// When extraVars is non-nil, the expression should be evaluated with these
// additional variables in scope (e.g. iterator variables for dynamic blocks).
type EvalFunc = func(propKey resource.PropertyKey, expr hcl.Expression, extraVars map[string]cty.Value) (cty.Value, hcl.Diagnostics)

// EvalFunctionWithSchema evaluates a function-call body against the Pulumi
// function schema. When mapping is non-nil, TF names that are blocks (per the
// TF schema) are accepted in block form even if the Pulumi schema flattens
// them, and non-conventional Pulumi renames are honored.
func EvalFunctionWithSchema(config hcl.Body, r *schema.Function, mapping *bridge.BodyMapping, eval EvalFunc) (property.Map, hcl.Diagnostics) {
	var props []*schema.Property
	if r.Inputs != nil {
		props = r.Inputs.Properties
	}
	functionInputs, attrExprs, diags := evalBlockWithSchema(config, props, mapping, eval)
	if diags.HasErrors() {
		return property.Map{}, diags
	}

	m, err := ctyToFunctionInputs(functionInputs, r, attrExprs, mapping)
	if err != nil {
		return property.Map{}, append(diags,
			conversionDiagnostic(err, "failed to convert HCL function inputs to Pulumi inputs"))
	}
	return m, diags
}

// EvalResourceWithSchema evaluates a resource body against the Pulumi resource
// schema. See EvalFunctionWithSchema for the role of mapping. The second
// result lists the paths within the evaluated inputs that carry an ephemeral
// mark. Paths are in the assembled inputs' namespace — snake-cased Pulumi
// names with MaxItemsOne blocks already flattened — and sorted.
func EvalResourceWithSchema(config hcl.Body, r *schema.Resource, mapping *bridge.BodyMapping, eval EvalFunc) (property.Map, []cty.Path, hcl.Diagnostics) {
	resourceInputs, attrExprs, diags := evalBlockWithSchema(config, r.InputProperties, mapping, eval)
	if diags.HasErrors() {
		return property.Map{}, nil, diags
	}

	m, err := ctyToResourceInputs(resourceInputs, r, attrExprs, mapping)
	if err != nil {
		return property.Map{}, nil, append(diags,
			conversionDiagnostic(err, "failed to convert HCL resource inputs to Pulumi inputs"))
	}
	return m, ephemeralInputPaths(resourceInputs), diags
}

// ephemeralInputPaths returns the paths within inputs whose value carries
// eval.EphemeralMark, sorted for determinism.
func ephemeralInputPaths(inputs cty.Value) []cty.Path {
	_, pathMarks := inputs.UnmarkDeepWithPaths()
	var out []cty.Path
	for _, pm := range pathMarks {
		if _, ok := pm.Marks[eval.EphemeralMark]; ok {
			out = append(out, pm.Path)
		}
	}
	slices.SortFunc(out, func(a, b cty.Path) int {
		return strings.Compare(fmt.Sprintf("%v", a), fmt.Sprintf("%v", b))
	})
	return out
}

func conversionDiagnostic(err error, fallbackSummary string) *hcl.Diagnostic {
	var re *rangedDiagError
	if errors.As(err, &re) {
		rng := re.rng
		return &hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  re.summary,
			Detail:   re.detail,
			Subject:  &rng,
		}
	}
	return &hcl.Diagnostic{
		Severity: hcl.DiagError,
		Summary:  fallbackSummary,
		Detail:   err.Error(),
	}
}

// rangedDiagError carries the HCL source range to blame, so a deep
// conversion failure can anchor its top-level diagnostic at the
// originating sub-expression.
type rangedDiagError struct {
	summary string
	detail  string
	rng     hcl.Range
}

func (e *rangedDiagError) Error() string {
	if e.detail == "" {
		return e.summary
	}
	return e.summary + "; " + e.detail
}

// evalBlockWithSchema also returns each top-level attribute's source
// expression so the conversion path can anchor error diagnostics. The
// optional mapping overrides which TF names are blocks vs attributes and
// supplies non-conventional TF→Pulumi name renames.
func evalBlockWithSchema(config hcl.Body, props []*schema.Property, mapping *bridge.BodyMapping, eval EvalFunc) (cty.Value, map[string]hcl.Expression, hcl.Diagnostics) {
	body, diags := config.Content(inputBodyFromProperties(props, mapping))
	if diags.HasErrors() {
		return cty.Value{}, nil, diags
	}

	if len(props) == 0 {
		return cty.EmptyObjectVal, nil, diags
	}

	// storageKey is the cty/attrExprs key for a TF attribute; using the
	// snake-form of the Pulumi name lets the downstream ctyToObject pipeline
	// match the property via its existing camelCaseFromSnakeCase lookup, even
	// when the bridge applied a non-default rename.
	storageKey := func(tfName string, prop *schema.Property) string {
		if prop != nil {
			return snakeCaseFromCamelCase(prop.Name)
		}
		return tfName
	}

	attrExprs := make(map[string]hcl.Expression, len(body.Attributes))
	resourceInputs := make(map[string]cty.Value, len(body.Attributes)+len(body.Blocks))
	for name, attr := range body.Attributes {
		puName, prop := resolvePulumiProperty(name, props, mapping)
		if prop == nil {
			return cty.Value{}, nil, append(diags, unmappedProperty(name, puName, props, attr.Range))
		}

		out, attrDiag := eval(resource.PropertyKey(prop.Name), attr.Expr, nil)
		diags = diags.Extend(attrDiag)
		if attrDiag.HasErrors() {
			return cty.Value{}, nil, diags
		}
		key := storageKey(name, prop)
		attrExprs[key] = attr.Expr
		resourceInputs[key] = conformCtyToType(out, ctyTypeFromType(prop.Type, nil))
	}

	// Blocks are walked in source order so that static and dynamic blocks
	// targeting the same order-significant list interleave the way the
	// author wrote them.
	for _, block := range body.Blocks {
		name := block.Type
		if name == "dynamic" {
			d := evalDynamicBlock(block, props, resourceInputs, mapping, eval)
			diags = diags.Extend(d)
			if d.HasErrors() {
				return cty.Value{}, nil, diags
			}
			continue
		}

		puName, prop := resolvePulumiProperty(name, props, mapping)
		if prop == nil {
			return cty.Value{}, nil, append(diags, unmappedProperty(name, puName, props, block.DefRange))
		}

		blockProps, isList := blockPropertiesOf(prop.Type)
		var nestedMapping *bridge.BodyMapping
		if mapping != nil {
			if fm := mapping.Lookup(name); fm != nil {
				nestedMapping = fm.Nested
			}
		}
		evaluated, _, d := evalBlockWithSchema(block.Body, blockProps, nestedMapping, eval)
		diags = diags.Extend(d)
		if d.HasErrors() {
			return cty.Value{}, nil, diags
		}
		key := storageKey(name, prop)
		if isList {
			appendBlockListValues(resourceInputs, key, []cty.Value{evaluated})
		} else {
			if existing, ok := resourceInputs[key]; ok {
				// An earlier dynamic block with an unknown for_each owns the
				// value; its expansion cannot be counted.
				if unmarked, _ := existing.Unmark(); !unmarked.IsKnown() {
					continue
				}
				diags = append(diags, tooManySingularBlocks(name, block.DefRange))
				return cty.Value{}, nil, diags
			}
			// TF block flattened to a single Pulumi object (MaxItemsOne).
			resourceInputs[key] = evaluated
		}
	}

	return cty.ObjectVal(resourceInputs), attrExprs, diags
}

// blockPropertiesOf returns the inner-block property list and whether the
// outer container is a list-shaped block (Array<Object>). For Object-typed
// Pulumi props (the bridge's MaxItemsOne flattening), the inner Properties
// are still the block's contents and isList is false.
func blockPropertiesOf(typ schema.Type) (props []*schema.Property, isList bool) {
	if obj, ok := AsHCLBlockType(typ); ok {
		return obj.Properties, true
	}
	if obj, ok := codegen.UnwrapType(typ).(*schema.ObjectType); ok {
		return obj.Properties, false
	}
	return nil, false
}

// dynamicBlockSchema is the body schema for the inside of a dynamic block.
var dynamicBlockSchema = &hcl.BodySchema{
	Attributes: []hcl.AttributeSchema{
		{Name: "for_each", Required: true},
		{Name: "iterator"},
	},
	Blocks: []hcl.BlockHeaderSchema{
		{Type: "content"},
	},
}

// evalDynamicBlock expands a dynamic block and merges its results into resourceInputs.
//
// A dynamic block has the form:
//
//	dynamic "prop_name" {
//	  for_each = <collection>
//	  content {
//	    <attrs evaluated with iterator bound>
//	  }
//	}
func evalDynamicBlock(
	block *hcl.Block, props []*schema.Property,
	resourceInputs map[string]cty.Value, mapping *bridge.BodyMapping, eval EvalFunc,
) hcl.Diagnostics {
	var diags hcl.Diagnostics
	propName := block.Labels[0]

	var prop *schema.Property
	if mapping != nil {
		if fm := mapping.Lookup(propName); fm != nil {
			for _, p := range props {
				if p.Name == fm.PulumiName {
					prop = p
					break
				}
			}
		}
	}
	if prop == nil {
		for _, p := range props {
			if snakeCaseFromCamelCase(p.Name) == propName {
				prop = p
				break
			}
		}
	}
	if prop == nil {
		diags = append(diags, &hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  "invalid dynamic block type",
			Detail:   fmt.Sprintf("no property %q found in schema", propName),
			Subject:  block.DefRange.Ptr(),
		})
		return diags
	}

	blockProps, isList := blockPropertiesOf(prop.Type)
	if blockProps == nil {
		diags = append(diags, &hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  "invalid dynamic block type",
			Detail:   fmt.Sprintf("property %q is not a block type", propName),
			Subject:  block.DefRange.Ptr(),
		})
		return diags
	}

	content, d := block.Body.Content(dynamicBlockSchema)
	diags = diags.Extend(d)
	if d.HasErrors() {
		return diags
	}

	forEachVal, d := eval("", content.Attributes["for_each"].Expr, nil)
	diags = diags.Extend(d)
	if d.HasErrors() {
		return diags
	}
	// ElementIterator panics on marked containers; per-element marks are
	// re-applied below so collection-level sensitivity / DepMarks reach
	// every attribute derived from each.value.
	var forEachMarks cty.ValueMarks
	forEachVal, forEachMarks = forEachVal.Unmark()

	if !forEachVal.CanIterateElements() && forEachVal.Type() != cty.DynamicPseudoType {
		diags = append(diags, &hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  "Invalid dynamic for_each value",
			Detail: fmt.Sprintf("Cannot use a %s value in for_each. An iterable collection is required.",
				forEachVal.Type().FriendlyName()),
			Subject: content.Attributes["for_each"].Expr.Range().Ptr(),
		})
		return diags
	}
	if forEachVal.IsNull() {
		diags = append(diags, &hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  "Invalid dynamic for_each value",
			Detail:   "Cannot use a null value in for_each.",
			Subject:  content.Attributes["for_each"].Expr.Range().Ptr(),
		})
		return diags
	}

	// Determine the iterator variable name (defaults to block label).
	// `iterator = <ident>` takes a bare identifier as the new iterator
	// name, not an expression to evaluate — extract it directly from
	// the traversal instead of running it through eval.
	iteratorName := propName
	if iterAttr, ok := content.Attributes["iterator"]; ok {
		traversals := iterAttr.Expr.Variables()
		if len(traversals) == 1 && len(traversals[0]) == 1 {
			iteratorName = traversals[0].RootName()
		} else {
			diags = append(diags, &hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  "Invalid iterator value",
				Detail:   "The iterator argument must be a single identifier.",
				Subject:  iterAttr.Range.Ptr(),
			})
			return diags
		}
	}

	// Find the content block.
	var contentBody hcl.Body
	for _, b := range content.Blocks {
		if b.Type == "content" {
			contentBody = b.Body
			break
		}
	}
	if contentBody == nil {
		diags = append(diags, &hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  "missing content block",
			Detail:   "dynamic block requires a content block",
			Subject:  block.DefRange.Ptr(),
		})
		return diags
	}

	var nestedMapping *bridge.BodyMapping
	if mapping != nil {
		if fm := mapping.Lookup(propName); fm != nil {
			nestedMapping = fm.Nested
		}
	}

	key := snakeCaseFromCamelCase(prop.Name)

	// An unknown for_each means the set of expanded blocks cannot be
	// enumerated yet, so the whole property becomes unknown. Content
	// expressions are not evaluated; the content body is still checked
	// against the schema, with every attribute standing in as unknown.
	if !forEachVal.IsKnown() {
		unknownEval := func(resource.PropertyKey, hcl.Expression, map[string]cty.Value) (cty.Value, hcl.Diagnostics) {
			return cty.DynamicVal.WithMarks(forEachMarks), nil
		}
		_, _, d := evalBlockWithSchema(contentBody, blockProps, nestedMapping, unknownEval)
		diags = diags.Extend(d)
		if d.HasErrors() {
			return diags
		}
		resourceInputs[key] = cty.DynamicVal.WithMarks(forEachMarks)
		return diags
	}

	// Evaluate the content block for each element.
	var values []cty.Value
	it := forEachVal.ElementIterator()
	for it.Next() {
		key, value := it.Element()
		if len(forEachMarks) > 0 {
			key = key.WithMarks(forEachMarks)
			value = value.WithMarks(forEachMarks)
		}

		iterVars := map[string]cty.Value{
			iteratorName: cty.ObjectVal(map[string]cty.Value{
				"key":   key,
				"value": value,
			}),
		}

		dynamicEval := func(propKey resource.PropertyKey, expr hcl.Expression, extraVars map[string]cty.Value) (cty.Value, hcl.Diagnostics) {
			merged := make(map[string]cty.Value, len(iterVars)+len(extraVars))
			maps.Copy(merged, iterVars)
			maps.Copy(merged, extraVars)
			return eval(propKey, expr, merged)
		}

		evaluated, _, d := evalBlockWithSchema(contentBody, blockProps, nestedMapping, dynamicEval)
		diags = diags.Extend(d)
		if d.HasErrors() {
			return diags
		}
		values = append(values, evaluated)
	}

	if len(values) > 0 {
		switch {
		case isList:
			appendBlockListValues(resourceInputs, key, values)
		default:
			// Singular block (MaxItemsOne): only an expansion of length 1
			// fills it, and only if nothing else already has.
			if len(values) > 1 {
				diags = append(diags, tooManySingularBlocks(propName, block.DefRange))
				return diags
			}
			if existing, ok := resourceInputs[key]; ok {
				// An earlier unknown expansion owns the value; its element
				// count cannot be checked.
				if unmarked, _ := existing.Unmark(); !unmarked.IsKnown() {
					return diags
				}
				diags = append(diags, tooManySingularBlocks(propName, block.DefRange))
				return diags
			}
			resourceInputs[key] = values[0]
		}
	}
	return diags
}

// appendBlockListValues appends values to the block list already accumulated
// at resourceInputs[key] (earlier static or dynamic blocks), preserving the
// source order of an order-significant list. If an earlier dynamic block with
// an unknown for_each already made the whole property unknown, it stays
// unknown, since the expanded set of blocks cannot be enumerated.
func appendBlockListValues(resourceInputs map[string]cty.Value, key string, values []cty.Value) {
	if existing, ok := resourceInputs[key]; ok {
		if unmarked, _ := existing.Unmark(); !unmarked.IsKnown() {
			return
		}
		prior := make([]cty.Value, 0, len(values))
		for it := existing.ElementIterator(); it.Next(); {
			_, v := it.Element()
			prior = append(prior, v)
		}
		values = append(prior, values...)
	}
	// Per-element object types can diverge when content sets disjoint subsets
	// of optional fields (e.g. `lookup(v, "k", null)` produces null-of-Dynamic
	// for absent keys), so blockListValue falls back to a tuple rather than
	// cty.ListVal, which would panic.
	resourceInputs[key] = blockListValue(values)
}

// unmappedProperty reports a TF name the provider's mapping admitted into the
// body but its Pulumi schema has no property for — a self-inconsistent
// provider package (e.g. a bridged nested type whose name two resources share
// with different shapes), which the engine cannot evaluate against.
func unmappedProperty(tfName, puName string, props []*schema.Property, rng hcl.Range) *hcl.Diagnostic {
	names := make([]string, len(props))
	for i, p := range props {
		names[i] = p.Name
	}
	return &hcl.Diagnostic{
		Severity: hcl.DiagError,
		Summary:  fmt.Sprintf("Provider schema has no property for %q", tfName),
		Detail: fmt.Sprintf("The provider's mapping names %q as %q, but its schema defines only: %s. "+
			"The provider's schema and mapping disagree; this is a provider bug.",
			tfName, puName, strings.Join(names, ", ")),
		Subject: rng.Ptr(),
	}
}

// tooManySingularBlocks reports more than one block (static or dynamically
// expanded) for a MaxItems=1 field.
func tooManySingularBlocks(name string, rng hcl.Range) *hcl.Diagnostic {
	return &hcl.Diagnostic{
		Severity: hcl.DiagError,
		Summary:  fmt.Sprintf("Too many %s blocks", name),
		Detail:   fmt.Sprintf("No more than 1 %q blocks are allowed", name),
		Subject:  rng.Ptr(),
	}
}

// unwrapSingular converts a MaxItemsOne field's TF-shaped collection to its
// flattened value: empty becomes null, a single element becomes the element
// (container marks carried over). More than one element is an error, matching
// the field's one-item limit. Values that are not known collections pass
// through untouched.
func unwrapSingular(path string, val cty.Value) (cty.Value, error) {
	unmarked, marks := val.Unmark()
	if ty := unmarked.Type(); (!ty.IsTupleType() && !ty.IsListType() && !ty.IsSetType()) ||
		unmarked.IsNull() || !unmarked.IsKnown() {
		return val, nil
	}
	switch n := unmarked.LengthInt(); n {
	case 0:
		return cty.NullVal(cty.DynamicPseudoType).WithMarks(marks), nil
	case 1:
		it := unmarked.ElementIterator()
		it.Next()
		_, elem := it.Element()
		return elem.WithMarks(marks), nil
	default:
		return cty.Value{}, fmt.Errorf(
			"%q: too many list items: attribute supports 1 item maximum, but config has %d declared", path, n)
	}
}

// schemaToCtyPrimitive returns the cty primitive type corresponding to a
// Pulumi schema type, if applicable.
func schemaToCtyPrimitive(typ schema.Type) (cty.Type, bool) {
	switch codegen.UnwrapType(typ) {
	case schema.BoolType:
		return cty.Bool, true
	case schema.IntType, schema.NumberType:
		return cty.Number, true
	case schema.StringType:
		return cty.String, true
	default:
		return cty.NilType, false
	}
}

func conformCtyToType(val cty.Value, typ cty.Type) cty.Value {
	// LengthInt and friends panic on marked values, so unmark for inspection
	// and re-apply afterward to keep DepMarks propagating.
	val, marks := val.Unmark()
	out := conformUnmarkedCtyToType(val, typ)
	if len(marks) > 0 {
		out = out.WithMarks(marks)
	}
	return out
}

func conformUnmarkedCtyToType(val cty.Value, typ cty.Type) cty.Value {
	if val.Type().Equals(typ) || !val.IsKnown() || val.IsNull() {
		return val
	}

	if val.Type().IsObjectType() && typ.IsMapType() {
		elemType := typ.ElementType()
		m := make(map[string]cty.Value, val.LengthInt())
		uniform := true
		for attrs := val.ElementIterator(); attrs.Next(); {
			k, v := attrs.Element()
			v = conformCtyToType(v, elemType)
			// Coerce elements to the target type so that a literal like
			// {fromBool = true, fromString = "true"} becomes a uniform map<bool>,
			// and so that {a = [x, y], b = [z]} becomes a uniform map<list<...>>
			// instead of two differently sized tuples. Without this, cty.MapVal
			// panics on mixed-type values.
			if !v.Type().Equals(elemType) {
				if converted, err := convert.Convert(v, elemType); err == nil {
					v = converted
				} else {
					uniform = false
				}
			}
			m[k.AsString()] = v
		}
		if len(m) == 0 {
			return cty.MapValEmpty(elemType)
		}
		// An element that cannot be converted (a null of an unrelated type, say)
		// would make cty.MapVal panic, so keep the object shape instead.
		if !uniform {
			return cty.ObjectVal(m)
		}
		return cty.MapVal(m)
	}

	return val
}

// AsHCLBlockType reports whether typ is a List<Object> type that should be
// rendered as repeated HCL blocks rather than an attribute. If so it returns
// the inner ObjectType.
func AsHCLBlockType(typ schema.Type) (*schema.ObjectType, bool) {
	arr, ok := codegen.UnwrapType(typ).(*schema.ArrayType)
	if !ok {
		return nil, false
	}
	obj, ok := codegen.UnwrapType(arr.ElementType).(*schema.ObjectType)
	return obj, ok
}

func inputBodyFromProperties(r []*schema.Property, mapping *bridge.BodyMapping) *hcl.BodySchema {
	body := new(hcl.BodySchema)

	// pulumiNameForTF resolves a TF (snake_case) name to the matching Pulumi
	// property when the mapping provides an explicit rename.
	pulumiNameForTF := map[string]string{}
	tfBlockNames := map[string]bool{}
	if mapping != nil {
		for tfName, fm := range mapping.Fields {
			pulumiNameForTF[tfName] = fm.PulumiName
			if fm.TFBlock {
				tfBlockNames[tfName] = true
			}
		}
	}

	// addedBlocks tracks block names we've emitted so we don't double-add when
	// a TF block also matches the convention-based block path.
	addedBlocks := map[string]bool{}
	addedAttrs := map[string]bool{}

	var hasBlockTypes bool
	for _, p := range r {
		typeName := snakeCaseFromCamelCase(p.Name)
		// If the mapping reverses to a different TF name, prefer that.
		for tfName, puName := range pulumiNameForTF {
			if puName == p.Name {
				typeName = tfName
				break
			}
		}

		if tfBlockNames[typeName] {
			body.Blocks = append(body.Blocks, hcl.BlockHeaderSchema{Type: typeName})
			addedBlocks[typeName] = true
			hasBlockTypes = true
			continue
		}
		if _, ok := AsHCLBlockType(p.Type); ok {
			body.Blocks = append(body.Blocks, hcl.BlockHeaderSchema{Type: typeName})
			addedBlocks[typeName] = true
			hasBlockTypes = true
			continue
		}
		body.Attributes = append(body.Attributes, hcl.AttributeSchema{
			Name: typeName,
			// A property with a schema-pinned const value is auto-filled
			// by ctyToObject and is therefore not required in the HCL body.
			Required: p.IsRequired() && p.ConstValue == nil,
		})
		addedAttrs[typeName] = true
	}

	// For any TF block declared by the mapping that didn't map back to a
	// Pulumi property (rare; e.g. provider-side filters), surface it as a
	// block so the parser doesn't reject it outright.
	for tfName := range tfBlockNames {
		if !addedBlocks[tfName] && !addedAttrs[tfName] {
			body.Blocks = append(body.Blocks, hcl.BlockHeaderSchema{Type: tfName})
			hasBlockTypes = true
		}
	}

	if hasBlockTypes {
		body.Blocks = append(body.Blocks, hcl.BlockHeaderSchema{
			Type:       "dynamic",
			LabelNames: []string{"type"},
		})
	}
	return body
}

func ctyToResourceInputs(val cty.Value, r *schema.Resource, attrExprs map[string]hcl.Expression, mapping *bridge.BodyMapping) (property.Map, error) {
	return ctyToObject(r.Token, val, r.InputProperties, attrExprs, false /* already in a secret */, mapping)
}

// CtyToResourceOutputs converts a TF-shaped attributes object (e.g. decoded
// state attributes) into the resource's Pulumi output property map. State
// attributes with no schema property are dropped: `timeouts` always (SDKv2
// bookkeeping), `id` unless the schema models it (see IDProperty).
func CtyToResourceOutputs(val cty.Value, r *schema.Resource, mapping *bridge.BodyMapping) (property.Map, error) {
	if ty := val.Type(); ty.IsObjectType() && (ty.HasAttribute("id") || ty.HasAttribute("timeouts")) {
		attrs := val.AsValueMap()
		if IDProperty(r, mapping) == nil {
			delete(attrs, "id")
		}
		delete(attrs, "timeouts")
		val = cty.ObjectVal(attrs)
	}
	return ctyToObject(r.Token, val, r.Properties, nil, false /* already in a secret */, mapping)
}

// IDProperty returns the Pulumi property that models the TF `id` attribute —
// the dynamic bridge's renamed <resource>Id — or nil when the schema does not
// model id. Always resolve via the mapping: fixID's naming rule varies with
// the schema's other fields.
func IDProperty(r *schema.Resource, mapping *bridge.BodyMapping) *schema.Property {
	_, p := resolvePulumiProperty("id", r.Properties, mapping)
	return p
}

func ctyToFunctionInputs(val cty.Value, r *schema.Function, attrExprs map[string]hcl.Expression, mapping *bridge.BodyMapping) (property.Map, error) {
	var inputs []*schema.Property
	if r.Inputs != nil {
		inputs = r.Inputs.Properties
	}
	return ctyToObject(r.Token, val, inputs, attrExprs, false /* already in a secret */, mapping)
}

// resolvePulumiProperty picks the Pulumi property for a TF (snake_case) name,
// going through the mapping when available (for renames the snake↔camel
// convention cannot recover, e.g. pluralized list fields), falling back to
// the convention otherwise.
func resolvePulumiProperty(tfName string, props []*schema.Property, mapping *bridge.BodyMapping) (string, *schema.Property) {
	if fm := mapping.Lookup(tfName); fm != nil {
		for _, p := range props {
			if p.Name == fm.PulumiName {
				return p.Name, p
			}
		}
	}
	return camelCaseFromSnakeCase(tfName, props)
}

// ctyToObject's attrExprs maps each attribute's HCL (snake_case) name to
// its source expression; passing nil disables source-range error
// reporting for this object's children.
func ctyToObject(path string, val cty.Value, properties []*schema.Property, attrExprs map[string]hcl.Expression, alreadyInSecret bool, mapping *bridge.BodyMapping) (property.Map, error) {
	seen := make(map[string]struct{})
	result := map[string]property.Value{}
	for it := val.ElementIterator(); it.Next(); {
		k, v := it.Element()
		keyStr := k.AsString()
		puField, prop := resolvePulumiProperty(keyStr, properties, mapping)
		if prop == nil {
			// We have not found the correct field in the property list, so we should put together an error
			// message.
			paths := make([]string, len(properties))
			for i, p := range properties {
				paths[i] = p.Name
			}
			return property.Map{}, fmt.Errorf("could not find %q (translated from %q) in %q: %s",
				puField, keyStr, path, strings.Join(paths, ", "))
		}
		seen[puField] = struct{}{}
		// A MaxItemsOne field still in TF shape (attribute-syntax assignment,
		// state attributes) is a single-element collection; unwrap it to the
		// flattened value. An explicit empty collection becomes a present-null
		// property — the encoding the bridge maps back to an empty TF list —
		// where an unset field stays absent entirely.
		var err error
		if fieldIsSingular(mapping, puField) {
			if v, err = unwrapSingular(keyStr, v); err != nil {
				return property.Map{}, err
			}
		}
		result[puField], err = ctyToResourceProperty(keyStr, v, prop.Type, attrExprs[keyStr],
			prop.Secret || alreadyInSecret, nestedMappingFor(prop.Name, mapping))
		if err != nil {
			return property.Map{}, err
		}
		if prop.Secret && !alreadyInSecret {
			result[puField] = result[puField].WithSecret(true)
		}
	}

	for _, prop := range properties {
		_, ok := seen[prop.Name]
		if ok {
			continue
		}
		if c := prop.ConstValue; c != nil {
			v, err := schemaLiteralToProperty(c)
			if err != nil {
				return property.Map{}, fmt.Errorf("%q: const value %#v: %w", prop.Name, c, err)
			}
			result[prop.Name] = v.WithSecret(prop.Secret)
			continue
		}
		if d := prop.DefaultValue; d != nil {
			v, err := getDefault(snakeCaseFromCamelCase(prop.Name), d, prop.Type)
			if err != nil {
				return property.Map{}, err
			}
			result[prop.Name] = v.WithSecret(prop.Secret)
			if prop.Secret {
				result[prop.Name] = result[prop.Name].WithSecret(true)
			}
		}
	}
	return property.NewMap(result), nil
}

// schemaLiteralToProperty converts a schema-bound literal (a default or const
// value) to a property value. Schema binding stores integers as int or int32,
// which property.Any rejects, so they are widened to float64 first.
func schemaLiteralToProperty(v any) (property.Value, error) {
	switch i := v.(type) {
	case int:
		v = float64(i)
	case int32:
		v = float64(i)
	}
	return property.Any(v)
}

func getDefault(path string, d *schema.DefaultValue, typ schema.Type) (property.Value, error) {
	for _, env := range d.Environment {
		if v := os.Getenv(env); v != "" {
			switch typ {
			case schema.BoolType:
				return property.New(cmdutil.IsTruthy(v)), nil
			case schema.NumberType:
				n, err := strconv.ParseFloat(v, 64)
				if err != nil {
					return property.Value{}, fmt.Errorf("%q: unable to parse %q into a number: %w",
						path, v, err)
				}
				return property.New(n), nil
			default:
				return property.New(v), nil
			}
		}
	}
	if v := d.Value; v != nil {
		v, err := schemaLiteralToProperty(v)
		if err != nil {
			// This indicates an invalid schema
			return property.Value{}, fmt.Errorf("%q: reading default value from %#v: %w",
				path, v, err)
		}
		return v, nil
	}
	return property.New(property.Null), nil
}

// ctyToResourceProperty's expr, when non-nil, lets union-discrimination
// failures attach an HCL source range to the returned error.
func ctyToResourceProperty(path string, val cty.Value, prop schema.Type, expr hcl.Expression, alreadyInSecret bool, mapping *bridge.BodyMapping) (property.Value, error) {
	// A whole-resource reference (e.g. `handler = aws_lambda_function.fn`) is
	// identified by its resourceMark; capture the URN before the unmark below
	// strips it, so the *schema.ResourceType case can build a reference from it.
	resRefURN, isResRef := eval.ResourceReferenceURN(val)

	if val.IsMarked() {
		var marks cty.ValueMarks
		val, marks = val.Unmark()
		_, isSensitive := marks[eval.SensitiveMark]
		// Ephemeral values are persisted as secrets: encrypted state addresses
		// the persistence concern that ephemerality exists to solve.
		_, isEphemeral := marks[eval.EphemeralMark]
		if (isSensitive || isEphemeral) && !alreadyInSecret {
			v, err := ctyToResourceProperty(path, val, prop, expr, true, mapping)
			return v.WithSecret(true), err
		}
	}

	// Strip unneeded signifiers
	prop = codegen.UnwrapType(prop)

	// Handle null & unknown

	switch {
	case val.IsNull():
		return property.Value{}, nil
	case !val.IsKnown():
		return property.New(property.Computed), nil
	}

	// Coerce the value to match the expected schema type when possible.
	// This handles cases like boolean = "true" where the HCL literal is a
	// string but the schema expects a boolean.
	if targetCtyType, ok := schemaToCtyPrimitive(prop); ok && !val.Type().Equals(targetCtyType) {
		if converted, err := convert.Convert(val, targetCtyType); err == nil {
			val = converted
		}
	}

	switch {
	case val.Type().Equals(cty.String):
		return property.New(val.AsString()), nil
	case val.Type().Equals(cty.Bool):
		return property.New(val.True()), nil
	case val.Type().Equals(cty.Number):
		f, _ := val.AsBigFloat().Float64()
		return property.New(f), nil
	}

	// We don't have any type info, so do a direct conversion.
	if prop == schema.AnyType {
		return ctyToPropertyValue(val)
	}

	// Handle asset and archive types.
	if prop == schema.AssetType || prop == schema.ArchiveType {
		if !val.Type().IsCapsuleType() {
			return property.Value{}, fmt.Errorf("%q: expected asset or archive, got %s", path, val.Type().FriendlyName())
		}
		switch inner := val.EncapsulatedValue().(type) {
		case *asset.Asset:
			return property.New(inner), nil
		case *archive.Archive:
			return property.New(inner), nil
		default:
			return property.Value{}, fmt.Errorf("%q: unexpected capsule value type %T", path, inner)
		}
	}

	// Handle complex types
	switch prop := prop.(type) {
	case *schema.ResourceType:
		if !val.Type().IsObjectType() {
			return property.Value{}, fmt.Errorf("expected object at %q for resource reference, found %#v", path, val.Type())
		}
		// A resource reference arrives as the referenced resource's outputs
		// object: its identity comes from the resourceMark (captured above), its
		// ID from the resource's id attribute.
		if isResRef {
			return property.New(resourceReferenceFromOutputs(resRefURN, val.AsValueMap())), nil
		}
		return property.New(property.Computed), nil
	case *schema.ObjectType:
		if !val.Type().IsObjectType() {
			return property.Value{}, fmt.Errorf("expected object at %q, found %#v", path, val.Type())
		}
		m, err := ctyToObject(path, val, prop.Properties, attrExprsByKey(expr), alreadyInSecret, mapping)
		return property.New(m), err
	case *schema.ArrayType:
		if !val.Type().IsListType() && !val.Type().IsSetType() && !val.Type().IsTupleType() {
			return property.Value{}, fmt.Errorf("expected list or set at %q, found %#v", path, val.Type())
		}
		elemExprs := exprListElements(expr)
		arr := make([]property.Value, 0, val.LengthInt())
		for it := val.ElementIterator(); it.Next(); {
			_, elem := it.Element()
			i := len(arr)
			var sub hcl.Expression
			if i < len(elemExprs) {
				sub = elemExprs[i]
			}
			convertedElem, err := ctyToResourceProperty(fmt.Sprintf("%s[%d]", path, i), elem, prop.ElementType, sub, alreadyInSecret, mapping)
			if err != nil {
				return property.Value{}, err
			}
			arr = append(arr, convertedElem)
		}
		return property.New(arr), nil
	case *schema.MapType:
		if !val.Type().IsMapType() && !val.Type().IsObjectType() {
			return property.Value{}, fmt.Errorf("expected map at %q, found %#v", path, val.Type())
		}
		elemExprs := attrExprsByKey(expr)
		m := make(map[string]property.Value, val.LengthInt())
		for it := val.ElementIterator(); it.Next(); {
			k, elem := it.Element()
			key := k.AsString()
			convertedElem, err := ctyToResourceProperty(fmt.Sprintf("%s[%q]", path, key), elem, prop.ElementType, elemExprs[key], alreadyInSecret, mapping)
			if err != nil {
				return property.Value{}, err
			}
			m[key] = convertedElem
		}
		return property.New(m), nil
	case *schema.UnionType:
		if chosen, err := SelectUnionMemberByConst(val, prop); err != nil {
			summary := fmt.Sprintf("cannot determine union variant for %q", path)
			if expr != nil {
				return property.Value{}, &rangedDiagError{
					summary: summary,
					detail:  err.Error(),
					rng:     expr.Range(),
				}
			}
			return property.Value{}, fmt.Errorf("%s: %w", summary, err)
		} else if chosen != nil {
			return ctyToResourceProperty(path, val, chosen, expr, alreadyInSecret, mapping)
		}
		return ctyToPropertyValue(val)
	default:
		return property.Value{}, fmt.Errorf("%q: incorrect value type: cannot convert %s to %s",
			path, val.Type().FriendlyName(), prop)
	}
}

// resourceReferenceFromOutputs builds a reference to a whole resource. The URN
// comes from the resourceMark; the ID from the resource's id output attribute,
// which is unknown during preview (mapped to a computed ID) and null for a
// component, mirroring the cases a Pulumi resource reference distinguishes.
func resourceReferenceFromOutputs(u resource.URN, attrs map[string]cty.Value) property.ResourceReference {
	ref := property.ResourceReference{URN: u}
	idAttr, ok := attrs["id"]
	if !ok {
		ref.ID = property.New(property.Null)
		return ref
	}
	idAttr, _ = idAttr.Unmark()
	switch {
	case !idAttr.IsKnown():
		ref.ID = property.New(property.Computed)
	case idAttr.Type() == cty.String && !idAttr.IsNull():
		ref.ID = property.New(idAttr.AsString())
	default:
		ref.ID = property.New(property.Null)
	}
	return ref
}

func resourceReferenceCty(outputs map[string]cty.Value, id property.Value, u resource.URN) cty.Value {
	attrs := make(map[string]cty.Value, len(outputs)+1)
	maps.Copy(attrs, outputs)
	attrs["id"] = resourceRefIDCty(id)
	return eval.MarkResourceReference(cty.ObjectVal(attrs), u)
}

func resourceRefIDCty(id property.Value) cty.Value {
	switch {
	case id.IsComputed():
		return cty.UnknownVal(cty.String)
	case id.IsString():
		return cty.StringVal(id.AsString())
	default:
		return cty.NullVal(cty.String)
	}
}

func camelCaseFromSnakeCase(s string, props []*schema.Property) (string, *schema.Property) {
	for _, p := range props {
		if snakeCaseFromCamelCase(p.Name) == s {
			return p.Name, p
		}
	}
	return cgstrings.ModifyStringAroundDelimeter(s, "_", cgstrings.UppercaseFirst), nil
}

func SnakeCaseFromPulumiCase(s string) string {
	return snakeCaseFromCamelCase(s)
}

func PulumiCaseFromSnakeCase(s string, props []*schema.Property) (string, *schema.Property) {
	return camelCaseFromSnakeCase(s, props)
}

// Convert from camelCase to snake_case.
//
// This function looses information, since we cannot distinguish between SCREAM & scream.
func snakeCaseFromCamelCase(s string) string {
	var b strings.Builder
	runes := []rune(s)
	var fromCapital bool

	for i, c := range runes {
		switch {
		case unicode.IsUpper(c):
			if b.Len() == 0 {
				b.WriteRune(unicode.ToLower(c))
				fromCapital = true
				continue
			}

			// If the previous letter was capitalized, and the next letter is capitalized, continue the
			// word.
			if fromCapital && (len(runes) <= i+1 || unicode.IsUpper(runes[i+1]) || unicode.IsNumber(runes[i+1])) {
				b.WriteRune(unicode.ToLower(c))
				continue
			}

			// Otherwise start a new word
			b.WriteRune('_')
			b.WriteRune(unicode.ToLower(c))
			fromCapital = true
		default:
			fromCapital = false
			b.WriteRune(c)
		}
	}
	return b.String()
}

// CtyToPropertyValue converts a cty.Value to a Pulumi PropertyValue.
//
// Because this conversion is untyped, it should be avoided when type information is available.
func CtyToPropertyValue(val cty.Value) (property.Value, error) {
	return ctyToPropertyValue(val)
}

func ctyToPropertyValue(val cty.Value) (property.Value, error) {
	// Handle sensitive- and ephemeral-marked values by unwrapping, converting,
	// and wrapping as secret.
	if val.IsMarked() {
		unmarked, marks := val.Unmark()
		_, isSensitive := marks[eval.SensitiveMark]
		_, isEphemeral := marks[eval.EphemeralMark]
		pv, err := ctyToPropertyValue(unmarked)
		return pv.WithSecret(isSensitive || isEphemeral), err
	}

	if val.IsNull() {
		return property.New(property.Null), nil
	}

	if !val.IsKnown() {
		// Unknown values are represented as computed in Pulumi
		return property.New(property.Computed), nil
	}

	typ := val.Type()

	switch {
	case typ == cty.Bool:
		return property.New(val.True()), nil

	case typ == cty.String:
		return property.New(val.AsString()), nil

	case typ == cty.Number:
		f64, _ := val.AsBigFloat().Float64()
		return property.New(f64), nil

	case typ.IsListType() || typ.IsTupleType():
		return ctyListToPropertyValue(val)

	case typ.IsSetType():
		return ctySetToPropertyValue(val)

	case typ.IsMapType() || typ.IsObjectType():
		return ctyObjectToPropertyValue(val)

	case typ == cty.DynamicPseudoType:
		// Dynamic type - try to infer the type from the underlying value
		return property.New(property.Null), nil

	case typ.IsCapsuleType():
		switch inner := val.EncapsulatedValue().(type) {
		case *asset.Asset:
			return property.New(inner), nil
		case *archive.Archive:
			return property.New(inner), nil
		default:
			return property.Value{}, fmt.Errorf("unknown capsule type %T", inner)
		}

	default:
		return property.Value{}, fmt.Errorf("unknown type %v", typ)
	}
}

// ctyListToPropertyValue converts a cty list/tuple to a Pulumi array.
func ctyListToPropertyValue(val cty.Value) (property.Value, error) {
	arr := make([]property.Value, 0, val.LengthInt())
	for it := val.ElementIterator(); it.Next(); {
		_, elemVal := it.Element()
		pv, err := ctyToPropertyValue(elemVal)
		if err != nil {
			return property.Value{}, err
		}
		arr = append(arr, pv)
	}
	return property.New(arr), nil
}

// ctySetToPropertyValue converts a cty set to a Pulumi array. The slice is
// allocated non-nil so an empty set becomes an empty array rather than null.
func ctySetToPropertyValue(val cty.Value) (property.Value, error) {
	arr := make([]property.Value, 0, val.LengthInt())
	for it := val.ElementIterator(); it.Next(); {
		_, elemVal := it.Element()
		pv, err := ctyToPropertyValue(elemVal)
		if err != nil {
			return property.Value{}, err
		}
		arr = append(arr, pv)
	}
	return property.New(arr), nil
}

// ctyObjectToPropertyValue converts a cty map/object to a Pulumi object.
func ctyObjectToPropertyValue(val cty.Value) (property.Value, error) {
	obj := make(map[string]property.Value, val.LengthInt())
	for it := val.ElementIterator(); it.Next(); {
		keyVal, elemVal := it.Element()
		key := keyVal.AsString()
		pv, err := ctyToPropertyValue(elemVal)
		if err != nil {
			return property.Value{}, err
		}
		obj[key] = pv
	}
	return property.New(obj), nil
}

// ResourceOutputToCty projects Pulumi resource outputs into a cty value.
// When mapping is non-nil the cty keys are TF attribute names, so HCL
// traversals like `aws_lambda_function.fn.function_name` resolve even when
// the bridge has renamed the property on the Pulumi side.
func ResourceOutputToCty(
	pv property.Map, r *schema.Resource, mapping *bridge.BodyMapping, dryRun bool,
) (map[string]cty.Value, error) {
	properties := r.Properties
	// version + pluginDownloadURL aren't in the schema Properties but
	// flow through as outputs via the Pulumi resource-options machinery.
	if r.IsProvider {
		properties = append(slices.Clone(r.Properties),
			&schema.Property{Name: "version", Type: schema.StringType},
			&schema.Property{Name: "pluginDownloadURL", Type: schema.StringType},
		)
	}
	return propertyObjectToCtyMap("", pv, properties, mapping, dryRun)
}

// ResourceReferenceType returns the cty type of a reference to r — the value
// bound for e.g. `aws_s3_bucket.b`. Its attributes are r's output properties
// under their TF names plus the synthetic `id`, mirroring the value
// ResourceOutputToCty puts in scope at runtime, so an expression referencing the
// resource can be typed against it.
func ResourceReferenceType(r *schema.Resource, mapping *bridge.BodyMapping) cty.Type {
	properties := r.Properties
	if r.IsProvider {
		properties = append(slices.Clone(r.Properties),
			&schema.Property{Name: "version", Type: schema.StringType},
			&schema.Property{Name: "pluginDownloadURL", Type: schema.StringType},
		)
	}
	seed := map[string]cty.Type{"id": cty.String}
	if t, ok := TimeoutsType(mapping); ok {
		seed["timeouts"] = t
	}
	return ctyObjectType(properties, seed, mapping)
}

// TimeoutsType returns the type of the `timeouts` block implied for a resource
// or data source — one optional string per declared operation — and false when
// its schema declares none.
func TimeoutsType(mapping *bridge.BodyMapping) (cty.Type, bool) {
	if mapping == nil || mapping.Timeouts == nil {
		return cty.NilType, false
	}
	attrs := make(map[string]cty.Type, len(mapping.Timeouts))
	for _, op := range mapping.Timeouts {
		attrs[op] = cty.String
	}
	return cty.Object(attrs), true
}

// DataSourceReferenceType returns the cty type of a `data.<type>.<name>`
// reference: the object type of the data source's return value, or
// DynamicPseudoType when the return type is not an object.
func DataSourceReferenceType(fn *schema.Function, mapping *bridge.BodyMapping) cty.Type {
	obj, ok := fn.ReturnType.(*schema.ObjectType)
	if !ok {
		return cty.DynamicPseudoType
	}
	var seed map[string]cty.Type
	if t, ok := TimeoutsType(mapping); ok {
		seed = map[string]cty.Type{"timeouts": t}
	}
	return ctyObjectType(obj.Properties, seed, mapping)
}

// FunctionOutputToCty mirrors ResourceOutputToCty for invoke return values.
func FunctionOutputToCty(pv property.Map, r *schema.Function, mapping *bridge.BodyMapping, dryRun bool) (cty.Value, error) {
	if obj, ok := r.ReturnType.(*schema.ObjectType); ok {
		o, err := propertyObjectToCtyMap("", pv, obj.Properties, mapping, dryRun)
		return cty.ObjectVal(o), err
	}
	for k, scalarPV := range pv.AsMap() {
		return propertyValueToCty(k, scalarPV, r.ReturnType, dryRun)
	}
	if dryRun {
		return cty.UnknownVal(ctyTypeFromType(r.ReturnType, mapping)), nil
	}
	return cty.Value{}, fmt.Errorf("invoke %q: provider returned empty scalar result", r.Token)
}

// tfNameForPulumi returns the TF-side attribute name for a Pulumi property
// name when the mapping carries an explicit rename or non-default casing;
// otherwise the convention-based snake_case form is used.
func tfNameForPulumi(pulumiName string, mapping *bridge.BodyMapping) string {
	if mapping != nil {
		for tfName, fm := range mapping.Fields {
			if fm.PulumiName == pulumiName {
				return tfName
			}
		}
	}
	return snakeCaseFromCamelCase(pulumiName)
}

// nestedMappingFor returns the nested BodyMapping for a property by Pulumi
// name, when mapping has it; nil otherwise.
func nestedMappingFor(pulumiName string, mapping *bridge.BodyMapping) *bridge.BodyMapping {
	if mapping == nil {
		return nil
	}
	for _, fm := range mapping.Fields {
		if fm.PulumiName == pulumiName {
			return fm.Nested
		}
	}
	return nil
}

func propertyObjectToCtyMap(
	path string, m property.Map, properties []*schema.Property,
	mapping *bridge.BodyMapping, dryRun bool,
) (map[string]cty.Value, error) {
	result := make(map[string]cty.Value, m.Len())
	for _, p := range properties {
		hclName := tfNameForPulumi(p.Name, mapping)
		nested := nestedMappingFor(p.Name, mapping)
		// In TF, a MaxItems=1 field — block or list/set attribute — is still
		// list-shaped. The bridge flattens it to a single Pulumi value; we
		// re-wrap it as a singleton list on output so HCL source like
		// `r.settings[0].x` resolves the same way as in TF.
		singular := fieldIsSingular(mapping, p.Name)
		// A TF TypeSet field (set block or set attribute) materializes as a
		// cty set, so set semantics — content-based, order-independent
		// equality — match TF. The Pulumi wire encoding carries it as an
		// ordered array; re-type it on the way back out.
		setField := fieldIsSet(mapping, p.Name)
		v, ok := m.GetOk(p.Name)
		// During preview, required properties that are null are treated as unknown.
		// This is because resource.Computed{} (the SDK's representation of an unknown value)
		// has a null inner element, which gets serialized to a null proto value by gRPC
		// marshaling, losing the "unknown" signal. Since a required property should never
		// legitimately be null, we safely treat null-during-preview as unknown.
		if !ok || (dryRun && v.IsNull() && p.IsRequired()) {
			t := ctyTypeFromType(p.Type, nested)
			if singular {
				t = cty.List(t)
			}
			if setField && t.IsListType() {
				t = cty.Set(t.ElementType())
			}
			if dryRun {
				result[hclName] = cty.UnknownVal(t)
			} else {
				result[hclName] = cty.NullVal(t)
			}
			// A property the schema declares secret stays secret even when the
			// provider elided its (unknown) value, matching the secret marking a
			// known value carries in through propertyValueToCtyWithMapping.
			if p.Secret {
				result[hclName] = result[hclName].Mark(eval.SensitiveMark)
			}
			continue
		}
		var vPath string
		if path == "" {
			vPath = hclName
		} else {
			vPath = path + "." + hclName
		}
		convertedV, err := propertyValueToCtyWithMapping(vPath, v, p.Type, nested, dryRun)
		if err != nil {
			return nil, err
		}
		if singular {
			if convertedV.IsNull() {
				switch {
				case !fieldIsBlock(mapping, p.Name):
					// A flattened attribute's null output stands for both an
					// unset field and an explicitly-empty list — the wire
					// encoding collapses them
					// (pulumi/pulumi-terraform-bridge#3558) — so it renders
					// as null, the unset shape.
					convertedV = cty.NullVal(cty.List(convertedV.Type()))
				case fieldIsComputed(mapping, p.Name) && !setField:
					// An unset Computed list block is provider-controlled:
					// absence means null, so `r.block == null` holds. Unset
					// set blocks materialize as empty either way.
					convertedV = cty.NullVal(cty.List(convertedV.Type()))
				default:
					// An omitted MaxItems=1 block is an empty list of blocks, not a
					// single-element list holding null, so `length(r.block)` is 0.
					convertedV = cty.ListValEmpty(convertedV.Type())
				}
			} else {
				convertedV = cty.TupleVal([]cty.Value{convertedV})
			}
		}
		if setField {
			convertedV = ctySetFromSequence(convertedV)
		}
		result[hclName] = convertedV
	}

	return result, nil
}

func fieldIsQuery[T any](mapping *bridge.BodyMapping, pulumiName string, query func(*bridge.FieldMapping) T) T {
	if mapping != nil {
		for _, fm := range mapping.Fields {
			if fm.PulumiName == pulumiName {
				return query(fm)
			}
		}
	}

	var t T
	return t
}

// fieldIsSingular reports whether the Pulumi property is a MaxItemsOne
// flattening of a TF list/set field per the bridge mapping.
func fieldIsSingular(mapping *bridge.BodyMapping, pulumiName string) bool {
	return fieldIsQuery(mapping, pulumiName, func(fm *bridge.FieldMapping) bool {
		return fm.MaxItemsOne
	})
}

// fieldIsBlock reports whether the Pulumi property is a TF block per the
// bridge mapping.
func fieldIsBlock(mapping *bridge.BodyMapping, pulumiName string) bool {
	return fieldIsQuery(mapping, pulumiName, func(fm *bridge.FieldMapping) bool {
		return fm.TFBlock
	})
}

// fieldIsComputed reports whether the Pulumi property is Computed in the TF
// schema per the bridge mapping.
func fieldIsComputed(mapping *bridge.BodyMapping, pulumiName string) bool {
	return fieldIsQuery(mapping, pulumiName, func(fm *bridge.FieldMapping) bool {
		return fm.TFComputed
	})
}

// fieldIsSet reports whether the Pulumi property is a TF TypeSet field per
// the bridge mapping.
func fieldIsSet(mapping *bridge.BodyMapping, pulumiName string) bool {
	return fieldIsQuery(mapping, pulumiName, func(fm *bridge.FieldMapping) bool {
		return fm.TFSet
	})
}

// ctySetFromSequence re-types a re-expanded TF TypeSet field from the ordered
// list/tuple the Pulumi wire encoding carries to the cty set TF materializes.
// A tuple whose element types cannot unify keeps its original type. Sets
// cannot carry per-element marks, so element marks are lifted to the set.
func ctySetFromSequence(v cty.Value) cty.Value {
	v, marks := v.Unmark()
	t := v.Type()
	switch {
	case t.IsListType() && !v.IsKnown():
		v = cty.UnknownVal(cty.Set(t.ElementType()))
	case t.IsListType() && v.IsNull():
		v = cty.NullVal(cty.Set(t.ElementType()))
	case t.IsListType():
		if converted, err := convert.Convert(v, cty.Set(t.ElementType())); err == nil {
			v = converted
		}
	case t.IsTupleType():
		// A tuple has no single element type; a dynamic-element set asks
		// convert to unify the per-element types.
		if converted, err := convert.Convert(v, cty.Set(cty.DynamicPseudoType)); err == nil {
			v = converted
		}
	}
	return v.WithMarks(marks)
}

func convertToSchemaCtyType(path string, val cty.Value, typ schema.Type) (cty.Value, error) {
	target := ctyTypeFromType(typ, nil)
	if val.Type().Equals(target) {
		return val, nil
	}
	converted, err := convert.Convert(val, target)
	if err != nil {
		return cty.Value{}, fmt.Errorf("%s: %w", path, err)
	}
	return converted, nil
}

// ctyTypeFromType converts a Pulumi schema type to its cty type. Nested
// object/resource attributes are named via the bridge mapping's TF names so
// that the type of an unknown or null nested block — e.g. aws_eks_cluster's
// computed `identity[].oidc[]`, which the bridge renames to `oidcs` — still
// type-checks against the TF-convention `identity[0].oidc[0]` traversal written
// in HCL. A nil mapping falls back to the snake_case-of-Pulumi-name convention.
func ctyTypeFromType(typ schema.Type, mapping *bridge.BodyMapping) cty.Type {
	return ctyTypeFromTypeRec(typ, mapping, map[any]bool{})
}

// ctyTypeFromTypeRec is the cycle-guarded core of ctyTypeFromType. seen holds the
// object and resource types on the current recursion path. A cyclic schema type
// has no finite cty representation, so revisiting a type already on the path
// yields DynamicPseudoType; every level reached before the cycle keeps its type.
func ctyTypeFromTypeRec(typ schema.Type, mapping *bridge.BodyMapping, seen map[any]bool) cty.Type {
	typ = codegen.UnwrapType(typ)

	switch typ {
	case schema.StringType:
		return cty.String
	case schema.BoolType:
		return cty.Bool
	case schema.NumberType, schema.IntType:
		return cty.Number
	case schema.AnyType, schema.JSONType, schema.AnyResourceType:
		return cty.DynamicPseudoType
	case schema.AssetType:
		return eval.AssetCapsuleType
	case schema.ArchiveType:
		return eval.ArchiveCapsuleType
	}

	switch typ := typ.(type) {
	case *schema.ArrayType:
		et := ctyTypeFromTypeRec(typ.ElementType, mapping, seen)
		if ctyTypeContainsDynamic(et) {
			return cty.DynamicPseudoType
		}
		return cty.List(et)
	case *schema.MapType:
		et := ctyTypeFromTypeRec(typ.ElementType, mapping, seen)
		if ctyTypeContainsDynamic(et) {
			return cty.DynamicPseudoType
		}
		return cty.Map(et)
	case *schema.EnumType:
		return ctyTypeFromTypeRec(typ.ElementType, mapping, seen)
	case *schema.ObjectType:
		if seen[typ] {
			return cty.DynamicPseudoType
		}
		seen[typ] = true
		defer delete(seen, typ)
		return ctyObjectTypeRec(typ.Properties, nil, mapping, seen)
	case *schema.ResourceType:
		if typ.Resource == nil {
			return cty.DynamicPseudoType
		}
		if seen[typ.Resource] {
			return cty.DynamicPseudoType
		}
		seen[typ.Resource] = true
		defer delete(seen, typ.Resource)
		return ctyObjectTypeRec(typ.Resource.Properties,
			map[string]cty.Type{"id": cty.String}, mapping, seen)
	case *schema.InvalidType:
		return cty.DynamicPseudoType
	case *schema.TokenType:
		if typ.UnderlyingType != nil {
			return ctyTypeFromTypeRec(typ.UnderlyingType, mapping, seen)
		}
		return cty.DynamicPseudoType
	case *schema.UnionType:
		// If all element types resolve to the same cty type, use it.
		// Otherwise fall back to DynamicPseudoType so any union member is
		// accepted without forcing conversion to one arbitrary member's type.
		var common cty.Type
		found := false
		consider := func(t schema.Type) bool {
			ct := ctyTypeFromTypeRec(t, mapping, seen)
			if ct == cty.NilType {
				return true
			}
			if !found {
				common = ct
				found = true
				return true
			}
			return common.Equals(ct)
		}
		if typ.DefaultType != nil && !consider(typ.DefaultType) {
			return cty.DynamicPseudoType
		}
		for _, t := range typ.ElementTypes {
			if !consider(t) {
				return cty.DynamicPseudoType
			}
		}
		if !found {
			return cty.NilType
		}
		return common
	default:
		return cty.NilType
	}
}

// ctyObjectType builds an object type whose attributes are named by the
// mapping's TF names (snake_case-of-Pulumi-name when mapping is nil). seed
// pre-populates always-optional synthetic attributes (e.g. a resource
// reference's `id`).
func ctyObjectType(
	properties []*schema.Property, seed map[string]cty.Type, mapping *bridge.BodyMapping,
) cty.Type {
	return ctyObjectTypeRec(properties, seed, mapping, map[any]bool{})
}

// ctyObjectTypeRec is the cycle-guarded core of ctyObjectType; see
// ctyTypeFromTypeRec for how seen bounds recursion through a cyclic type graph.
func ctyObjectTypeRec(
	properties []*schema.Property, seed map[string]cty.Type, mapping *bridge.BodyMapping, seen map[any]bool,
) cty.Type {
	attrs := make(map[string]cty.Type, len(properties)+len(seed))
	var optional []string
	for k, v := range seed {
		attrs[k] = v
		optional = append(optional, k)
	}
	for _, p := range properties {
		key := tfNameForPulumi(p.Name, mapping)
		if !p.IsRequired() {
			optional = append(optional, key)
		}
		t := ctyTypeFromTypeRec(p.Type, nestedMappingFor(p.Name, mapping), seen)
		// The bridge flattens a MaxItems=1 field to a single value, but TF
		// models it as a list; re-wrap it as a list so a reference like
		// `r.settings[0].x` types the same way it resolves at runtime (see
		// propertyObjectToCtyMap).
		if fieldIsSingular(mapping, p.Name) {
			t = cty.List(t)
		}
		// A TF TypeSet field is set-typed; see propertyObjectToCtyMap.
		if fieldIsSet(mapping, p.Name) && t.IsListType() {
			t = cty.Set(t.ElementType())
		}
		attrs[key] = t
	}
	return cty.ObjectWithOptionalAttrs(attrs, optional)
}

// RefinedUnknown returns an unknown value of type t that refines required
// (non-optional) object attributes not-null and leaves optional ones nullable.
// It recurses into nested objects so attribute access preserves the refinement;
// collections are leaves, since indexing drops refinements and their element
// types already carry optional-attribute metadata.
func RefinedUnknown(t cty.Type, nullable bool) cty.Value {
	if t.IsObjectType() && !nullable {
		optional := t.OptionalAttributes()
		attrs := make(map[string]cty.Value, len(t.AttributeTypes()))
		for name, attrType := range t.AttributeTypes() {
			_, isOptional := optional[name]
			attrs[name] = RefinedUnknown(attrType, isOptional)
		}
		return cty.ObjectVal(attrs)
	}
	u := cty.UnknownVal(t)
	if nullable {
		return u
	}
	return u.RefineNotNull()
}

// ctyTypeContainsDynamic reports whether the given cty type embeds
// [cty.DynamicPseudoType].
func ctyTypeContainsDynamic(t cty.Type) bool {
	switch {
	case t == cty.DynamicPseudoType:
		return true
	case t.IsListType(), t.IsMapType(), t.IsSetType():
		return ctyTypeContainsDynamic(t.ElementType())
	case t.IsTupleType():
		return slices.ContainsFunc(t.TupleElementTypes(), ctyTypeContainsDynamic)
	case t.IsObjectType():
		return slices.ContainsFunc(slices.Collect(maps.Values(t.AttributeTypes())), ctyTypeContainsDynamic)
	default:
		return false
	}
}

// unifyOrObject collapses a heterogeneous map of cty values into either a
// homogeneous cty.MapVal (if the element types unify cleanly) or a
// cty.ObjectVal (which permits per-attribute types). cty does not have a
// union type, so these are the two ways to carry heterogeneous-element data.
func unifyOrObject(m map[string]cty.Value) cty.Value {
	keys := make([]string, 0, len(m))
	types := make([]cty.Type, 0, len(m))
	for k, v := range m {
		keys = append(keys, k)
		types = append(types, v.Type())
	}
	unified, conversions := convert.UnifyUnsafe(types)
	if unified == cty.NilType || isLossyPrimitiveUnification(unified, types) {
		return cty.ObjectVal(m)
	}
	out := make(map[string]cty.Value, len(m))
	for i, k := range keys {
		v := m[k]
		if conv := conversions[i]; conv != nil {
			cv, err := conv(v)
			if err != nil {
				return cty.ObjectVal(m)
			}
			v = cv
		}
		out[k] = v
	}
	// cty.MapVal panics on inconsistent element types. Unification can
	// pick a DynamicPseudoType supertype yet leave concrete elements
	// untouched; verify post-conversion homogeneity before committing.
	first := out[keys[0]].Type()
	for _, k := range keys[1:] {
		if !out[k].Type().Equals(first) {
			return cty.ObjectVal(m)
		}
	}
	return cty.MapVal(out)
}

// isLossyPrimitiveUnification reports whether unifying to unified would coerce
// elements between primitive types at any depth — e.g. collapsing [number,
// string, bool] to string, or [list(number), list(string)] to list(string).
// Such a unification erases an element's own type, so a heterogeneous value
// must stay a tuple/object rather than be flattened to a list/map. A
// structural unification (e.g. object -> map) whose primitive leaves are
// untouched is left to proceed.
func isLossyPrimitiveUnification(unified cty.Type, types []cty.Type) bool {
	for _, t := range types {
		if isLossyConversion(t, unified) {
			return true
		}
	}
	return false
}

// isLossyConversion reports whether converting from to to would coerce a
// primitive leaf to a different primitive type.
func isLossyConversion(from, to cty.Type) bool {
	if from.Equals(to) || from == cty.DynamicPseudoType || to == cty.DynamicPseudoType {
		return false
	}
	switch {
	case to.IsPrimitiveType():
		return true
	case to.IsListType(), to.IsSetType():
		elem := to.ElementType()
		switch {
		case from.IsListType(), from.IsSetType():
			return isLossyConversion(from.ElementType(), elem)
		case from.IsTupleType():
			return slices.ContainsFunc(from.TupleElementTypes(), func(t cty.Type) bool {
				return isLossyConversion(t, elem)
			})
		}
	case to.IsMapType():
		elem := to.ElementType()
		switch {
		case from.IsMapType():
			return isLossyConversion(from.ElementType(), elem)
		case from.IsObjectType():
			return slices.ContainsFunc(slices.Collect(maps.Values(from.AttributeTypes())), func(t cty.Type) bool {
				return isLossyConversion(t, elem)
			})
		}
	case to.IsObjectType() && from.IsObjectType():
		for name, at := range from.AttributeTypes() {
			if ot, ok := to.AttributeTypes()[name]; ok && isLossyConversion(at, ot) {
				return true
			}
		}
	case to.IsTupleType() && from.IsTupleType():
		fromElems, toElems := from.TupleElementTypes(), to.TupleElementTypes()
		if len(fromElems) != len(toElems) {
			return false
		}
		for i, ft := range fromElems {
			if isLossyConversion(ft, toElems[i]) {
				return true
			}
		}
	}
	return false
}

// blockListValue assembles the elements of a repeated TF block (each an object)
// into a cty list when they share a type, or a tuple when an optional attribute
// is set in some blocks and omitted in others so their object types differ.
// Unlike unifyOrTuple it never collapses the objects into a map, which would
// stringify attributes and destroy the object structure the resource schema
// expects.
func blockListValue(values []cty.Value) cty.Value {
	first := values[0].Type()
	for _, v := range values[1:] {
		if !v.Type().Equals(first) {
			return cty.TupleVal(values)
		}
	}
	return cty.ListVal(values)
}

// unifyOrTuple is the slice counterpart to unifyOrObject: a homogeneous
// cty.ListVal when element types unify cleanly, otherwise cty.TupleVal.
func unifyOrTuple(arr []cty.Value) cty.Value {
	types := make([]cty.Type, len(arr))
	for i, v := range arr {
		types[i] = v.Type()
	}
	unified, conversions := convert.UnifyUnsafe(types)
	if unified == cty.NilType || isLossyPrimitiveUnification(unified, types) {
		return cty.TupleVal(arr)
	}
	out := make([]cty.Value, len(arr))
	for i, v := range arr {
		if conv := conversions[i]; conv != nil {
			cv, err := conv(v)
			if err != nil {
				return cty.TupleVal(arr)
			}
			v = cv
		}
		out[i] = v
	}
	first := out[0].Type()
	for _, v := range out[1:] {
		if !v.Type().Equals(first) {
			return cty.TupleVal(arr)
		}
	}
	return cty.ListVal(out)
}

func propertyValueToCty(path string, v property.Value, typ schema.Type, dryRun bool) (cty.Value, error) {
	return propertyValueToCtyWithMapping(path, v, typ, nil, dryRun)
}

// propertyValueToCtyWithMapping is the mapping-aware variant of
// propertyValueToCty. Mapping is consulted for object/resource conversions so
// nested fields land under their TF names.
func propertyValueToCtyWithMapping(path string, v property.Value, typ schema.Type, mapping *bridge.BodyMapping, dryRun bool) (cty.Value, error) {
	typ = codegen.UnwrapType(typ)
	if v.Secret() {
		computedV, err := propertyValueToCtyWithMapping(path, v.WithSecret(false), typ, mapping, dryRun)
		return computedV.Mark(eval.SensitiveMark), err
	}

	switch {

	// Primitive types

	case v.IsComputed():
		return cty.UnknownVal(ctyTypeFromType(typ, mapping)), nil
	case v.IsString():
		return cty.StringVal(v.AsString()), nil
	case v.IsBool():
		return cty.BoolVal(v.AsBool()), nil
	case v.IsNumber():
		return cty.NumberFloatVal(v.AsNumber()), nil
	case v.IsNull():
		return cty.NullVal(ctyTypeFromType(typ, mapping)), nil

	// Collection types

	case v.IsMap():
		if u, ok := typ.(*schema.UnionType); ok {
			if chosen := selectUnionMember(v, u); chosen != nil {
				return propertyValueToCtyWithMapping(path, v, chosen, mapping, dryRun)
			}
		}
		elemType := schema.AnyType
		switch typ := typ.(type) {
		case *schema.ResourceType:
			if typ.Resource == nil {
				break
			}
			result, err := propertyObjectToCtyMap(path, v.AsMap(), typ.Resource.Properties, mapping, dryRun)
			if err != nil {
				return cty.Value{}, err
			}
			// resolveResourceRefsInOutputs carries a resource reference and any
			// fetched state through the map's __ref key; the rest of the map is
			// the referenced resource's outputs.
			var ref *property.ResourceReference
			if refVal, ok := v.AsMap().GetOk("__ref"); ok && refVal.IsResourceReference() {
				r := refVal.AsResourceReference()
				ref = &r
				result["id"] = resourceRefIDCty(r.ID)
			}
			obj := cty.ObjectVal(result)
			if mapping == nil {
				// Mapping-aware values keep TF names the schema type rejects, so
				// only convert when there is no mapping.
				if obj, err = convertToSchemaCtyType(path, obj, typ); err != nil {
					return cty.Value{}, err
				}
			}
			if ref != nil {
				obj = eval.MarkResourceReference(obj, ref.URN)
			}
			return obj, nil
		case *schema.ObjectType:
			m, err := propertyObjectToCtyMap(path, v.AsMap(), typ.Properties, mapping, dryRun)
			if err != nil {
				return cty.Value{}, err
			}
			if mapping != nil {
				return cty.ObjectVal(m), nil
			}
			return convertToSchemaCtyType(path, cty.ObjectVal(m), typ)
		case *schema.MapType:
			elemType = typ.ElementType
		}
		m := make(map[string]cty.Value, v.AsMap().Len())
		for k, v := range v.AsMap().All {
			convertedV, err := propertyValueToCtyWithMapping(fmt.Sprintf("%s[%q]", path, k), v, elemType, mapping, dryRun)
			if err != nil {
				return cty.Value{}, err
			}
			m[k] = convertedV
		}
		if len(m) == 0 {
			return cty.MapValEmpty(ctyTypeFromType(elemType, mapping)), nil
		}

		// When elements have differing cty types — e.g. a Map<Any|JSON> whose
		// values pick String, Number, Object, ... — try to unify them so we
		// can still emit a cty.MapVal. If unification fails (truly
		// incompatible shapes), fall back to cty.ObjectVal, which cty allows
		// to have heterogeneous attribute values. See the pulumi_stack
		// example, where a non-homogeneous block is typed dynamically:
		//
		//	 resource "pulumi_stash" "myStash" {
		//	   input = {
		//	     "key" = ["value", "s"]
		//	     ""    = false
		//	   }
		//	 }
		return unifyOrObject(m), nil

	case v.IsArray():
		if u, ok := typ.(*schema.UnionType); ok {
			if chosen := selectUnionMember(v, u); chosen != nil {
				return propertyValueToCtyWithMapping(path, v, chosen, mapping, dryRun)
			}
		}
		elemType := schema.AnyType
		if arr, ok := typ.(*schema.ArrayType); ok {
			elemType = arr.ElementType
		}
		arr := make([]cty.Value, v.AsArray().Len())
		for i, v := range v.AsArray().All {
			convertedV, err := propertyValueToCtyWithMapping(fmt.Sprintf("%s[%d]", path, i), v, elemType, mapping, dryRun)
			if err != nil {
				return cty.Value{}, err
			}
			arr[i] = convertedV
		}
		if len(arr) == 0 {
			return cty.ListValEmpty(ctyTypeFromType(elemType, mapping)), nil
		}
		// Same story as the map case: if elements ended up with different cty
		// types (dynamic schema element), try to unify into a common type
		// for cty.ListVal; otherwise fall back to cty.TupleVal.
		return unifyOrTuple(arr), nil

	case v.IsResourceReference():
		ref := v.AsResourceReference()
		resType, ok := typ.(*schema.ResourceType)
		if !ok || resType.Resource == nil {
			return cty.NullVal(ctyTypeFromType(typ, mapping)), nil
		}
		result, err := propertyObjectToCtyMap(path, property.Map{}, resType.Resource.Properties, mapping, dryRun)
		if err != nil {
			return cty.Value{}, err
		}
		return resourceReferenceCty(result, ref.ID, ref.URN), nil

	case v.IsAsset():
		return cty.CapsuleVal(eval.AssetCapsuleType, v.AsAsset()), nil

	case v.IsArchive():
		return cty.CapsuleVal(eval.ArchiveCapsuleType, v.AsArchive()), nil

	default:
		return cty.Value{}, fmt.Errorf("%s: unhandled property %s", path, v.GoString())
	}
}

// PropertyValueToCty converts a Pulumi PropertyValue to a cty.Value.
func PropertyValueToCty(pv property.Value) cty.Value {
	if pv.Secret() {
		return PropertyValueToCty(pv.WithSecret(false)).
			WithMarks(cty.NewValueMarks(eval.SensitiveMark))
	}

	if pv.IsNull() {
		return cty.NullVal(cty.DynamicPseudoType)
	}

	if pv.IsComputed() {
		return cty.UnknownVal(cty.DynamicPseudoType)
	}

	switch {
	case pv.IsBool():
		return cty.BoolVal(pv.AsBool())

	case pv.IsString():
		return cty.StringVal(pv.AsString())

	case pv.IsNumber():
		return cty.NumberFloatVal(pv.AsNumber())

	case pv.IsArray():
		arr := pv.AsArray()
		if arr.Len() == 0 {
			return cty.ListValEmpty(cty.DynamicPseudoType)
		}
		vals := make([]cty.Value, arr.Len())
		for i, elem := range arr.All {
			vals[i] = PropertyValueToCty(elem)
		}
		return cty.TupleVal(vals)

	case pv.IsMap():
		obj := pv.AsMap()
		if obj.Len() == 0 {
			return cty.EmptyObjectVal
		}
		vals := make(map[string]cty.Value)
		for k, v := range obj.All {
			vals[string(k)] = PropertyValueToCty(v)
		}
		return cty.ObjectVal(vals)

	case pv.IsAsset():
		return cty.CapsuleVal(eval.AssetCapsuleType, pv.AsAsset())

	case pv.IsArchive():
		return cty.CapsuleVal(eval.ArchiveCapsuleType, pv.AsArchive())

	case pv.IsResourceReference():
		// No schema here to type the referenced resource's outputs, so the value
		// carries only the id attribute and the identifying resourceMark.
		ref := pv.AsResourceReference()
		return resourceReferenceCty(nil, ref.ID, ref.URN)

	default:
		return cty.NullVal(cty.DynamicPseudoType)
	}
}

// PropertyMapToCty converts a Pulumi PropertyMap to a cty.Value object.
func PropertyMapToCty(pm property.Map) cty.Value {
	if pm.Len() == 0 {
		return cty.EmptyObjectVal
	}

	vals := make(map[string]cty.Value)
	for k, v := range pm.All {
		vals[string(k)] = PropertyValueToCty(v)
	}
	return cty.ObjectVal(vals)
}

// CtyToPropertyMap converts a cty.Value object to a Pulumi PropertyMap.
func CtyToPropertyMap(val cty.Value) (property.Map, error) {
	if val.IsNull() || !val.IsKnown() {
		return property.Map{}, nil
	}

	if !val.Type().IsObjectType() && !val.Type().IsMapType() {
		return property.Map{}, fmt.Errorf("expected object or map type, got %s", val.Type().FriendlyName())
	}

	pm := make(map[string]property.Value)
	for it := val.ElementIterator(); it.Next(); {
		keyVal, elemVal := it.Element()
		key := keyVal.AsString()
		pv, err := CtyToPropertyValue(elemVal)
		if err != nil {
			return property.Map{}, fmt.Errorf("converting property %q: %w", key, err)
		}
		pm[key] = pv
	}
	return property.NewMap(pm), nil
}

// missingDiscriminatorError: the value lacks the discriminator property.
type missingDiscriminatorError struct {
	Discriminator string   // snake_case name for HCL display
	Allowed       []string // quoted const values: `"a"`, `"b"`, ...
}

func (e *missingDiscriminatorError) Error() string {
	return fmt.Sprintf("missing discriminator %q (expected one of %s)",
		e.Discriminator, strings.Join(e.Allowed, ", "))
}

// unrecognizedDiscriminatorError: the discriminator value matches no variant's Const.
type unrecognizedDiscriminatorError struct {
	Discriminator string
	Allowed       []string
	Actual        string // pre-formatted (quoted for strings, GoString otherwise)
}

func (e *unrecognizedDiscriminatorError) Error() string {
	return fmt.Sprintf("discriminator %q is %s, expected one of %s",
		e.Discriminator, e.Actual, strings.Join(e.Allowed, ", "))
}

// nonObjectDiscriminatorError: the value is not an object/map and so cannot carry a discriminator.
type nonObjectDiscriminatorError struct {
	Discriminator string
	Allowed       []string
	GotType       string
}

func (e *nonObjectDiscriminatorError) Error() string {
	return fmt.Sprintf("expected an object with discriminator %q (one of %s), got %s",
		e.Discriminator, strings.Join(e.Allowed, ", "), e.GotType)
}

// SelectUnionMemberByConst picks the union member whose const-pinned
// discriminator matches val. Returns (nil, nil) if no member is
// const-discriminated (caller falls back to shape-based matching);
// otherwise either a chosen type or one of *missingDiscriminatorError,
// *unrecognizedDiscriminatorError, *nonObjectDiscriminatorError.
func SelectUnionMemberByConst(val cty.Value, u *schema.UnionType) (schema.Type, error) {
	candidates := slices.Clone(u.ElementTypes)
	if u.DefaultType != nil {
		candidates = append([]schema.Type{u.DefaultType}, candidates...)
	}

	type cand struct {
		typ  schema.Type
		disc *schema.Property
	}
	var withConst []cand
	for _, t := range candidates {
		obj, ok := codegen.UnwrapType(t).(*schema.ObjectType)
		if !ok {
			continue
		}
		var disc *schema.Property
		for _, p := range obj.Properties {
			if p.ConstValue != nil {
				disc = p
				break
			}
		}
		if disc == nil {
			continue
		}
		withConst = append(withConst, cand{typ: t, disc: disc})
	}

	if len(withConst) == 0 {
		return nil, nil
	}
	discName := withConst[0].disc.Name
	for _, c := range withConst[1:] {
		if c.disc.Name != discName {
			return nil, nil
		}
	}

	hclName := snakeCaseFromCamelCase(discName)
	allowed := make([]string, 0, len(withConst))
	for _, c := range withConst {
		allowed = append(allowed, formatConstValue(c.disc.ConstValue))
	}

	if !val.Type().IsObjectType() && !val.Type().IsMapType() {
		return nil, &nonObjectDiscriminatorError{
			Discriminator: hclName,
			Allowed:       allowed,
			GotType:       val.Type().FriendlyName(),
		}
	}

	attrs := val.AsValueMap()
	discVal, ok := attrs[hclName]
	if ok {
		discVal, _ = discVal.Unmark()
	}
	if !ok || discVal.IsNull() {
		return nil, &missingDiscriminatorError{
			Discriminator: hclName,
			Allowed:       allowed,
		}
	}
	if !discVal.IsKnown() {
		// The discriminator is a computed value not yet known, so the variant
		// cannot be chosen; a generic object conversion is used instead.
		return nil, nil
	}

	for _, c := range withConst {
		if CtyEqualsConst(discVal, c.disc.ConstValue) {
			return c.typ, nil
		}
	}

	return nil, &unrecognizedDiscriminatorError{
		Discriminator: hclName,
		Allowed:       allowed,
		Actual:        formatCtyDiscriminator(discVal),
	}
}

// formatConstValue renders a schema const for display in an error
// message. Strings are quoted; other scalars (bool/int32/float64) use
// their natural Go representation.
func formatConstValue(v any) string {
	if s, ok := v.(string); ok {
		return fmt.Sprintf("%q", s)
	}
	return fmt.Sprintf("%v", v)
}

// formatCtyDiscriminator renders an observed discriminator value for
// inclusion in an error; the format mirrors formatConstValue.
func formatCtyDiscriminator(v cty.Value) string {
	switch v.Type() {
	case cty.String:
		return fmt.Sprintf("%q", v.AsString())
	case cty.Bool:
		return fmt.Sprintf("%v", v.True())
	case cty.Number:
		f, _ := v.AsBigFloat().Float64()
		return strconv.FormatFloat(f, 'g', -1, 64)
	}
	return v.GoString()
}

// CtyEqualsConst reports whether a cty value matches a schema-bound
// const value. bindConstValue normalises consts to one of string, bool,
// int32, or float64, so those are the cases we accept.
func CtyEqualsConst(v cty.Value, constVal any) bool {
	switch c := constVal.(type) {
	case string:
		return v.Type() == cty.String && v.AsString() == c
	case bool:
		return v.Type() == cty.Bool && v.True() == c
	case int32:
		if v.Type() != cty.Number {
			return false
		}
		n, acc := v.AsBigFloat().Int64()
		return acc == big.Exact && n == int64(c)
	case float64:
		if v.Type() != cty.Number {
			return false
		}
		f, _ := v.AsBigFloat().Float64()
		return f == c
	}
	return false
}

// attrExprsByKey returns the per-key value expressions of an object/map
// construction, or nil if expr is nil or not statically destructurable.
func attrExprsByKey(expr hcl.Expression) map[string]hcl.Expression {
	if expr == nil {
		return nil
	}
	pairs, diags := hcl.ExprMap(expr)
	if diags.HasErrors() {
		return nil
	}
	out := make(map[string]hcl.Expression, len(pairs))
	for _, kv := range pairs {
		k, kd := kv.Key.Value(nil)
		if kd.HasErrors() || k.Type() != cty.String {
			continue
		}
		out[k.AsString()] = kv.Value
	}
	return out
}

// exprListElements returns the element expressions of a list/tuple
// construction, or nil if expr is nil or not statically destructurable.
func exprListElements(expr hcl.Expression) []hcl.Expression {
	if expr == nil {
		return nil
	}
	elems, diags := hcl.ExprList(expr)
	if diags.HasErrors() {
		return nil
	}
	return elems
}

// selectUnionMember picks the union member that v conforms to by recursively
// matching the value's shape against each candidate type. ObjectType members
// are preferred over MapType members when both could fit, because objects are
// the more specific shape.
func selectUnionMember(v property.Value, typ *schema.UnionType) schema.Type {
	candidates := slices.Clone(typ.ElementTypes)
	if typ.DefaultType != nil {
		candidates = append([]schema.Type{typ.DefaultType}, candidates...)
	}

	isObject := func(t schema.Type) bool {
		_, ok := codegen.UnwrapType(t).(*schema.ObjectType)
		return ok
	}

	for _, t := range candidates {
		if !isObject(t) {
			continue
		}
		if valueMatchesType(v, t) {
			return t
		}
	}
	for _, t := range candidates {
		if !isObject(t) && valueMatchesType(v, t) {
			return t
		}
	}
	return nil
}

// valueMatchesType reports whether v could be produced from a schema of type
// typ. It traverses recursively so a union nested inside arrays/maps/objects
// is resolved by inspecting the corresponding sub-values.
func valueMatchesType(v property.Value, typ schema.Type) bool {
	typ = codegen.UnwrapType(typ)
	if typ == schema.AnyType {
		return true
	}
	if v.IsNull() || v.IsComputed() {
		return true
	}
	if u, ok := typ.(*schema.UnionType); ok {
		return selectUnionMember(v, u) != nil
	}
	switch {
	case v.IsString():
		return typ == schema.StringType
	case v.IsBool():
		return typ == schema.BoolType
	case v.IsNumber():
		return typ == schema.NumberType || typ == schema.IntType
	case v.IsArray():
		arr, ok := typ.(*schema.ArrayType)
		if !ok {
			return false
		}
		for _, e := range v.AsArray().All {
			if !valueMatchesType(e, arr.ElementType) {
				return false
			}
		}
		return true
	case v.IsMap():
		m := v.AsMap()
		switch typ := typ.(type) {
		case *schema.ObjectType:
			props := make(map[string]*schema.Property, len(typ.Properties))
			for _, p := range typ.Properties {
				props[p.Name] = p
			}
			for k, e := range m.All {
				p, ok := props[string(k)]
				if !ok {
					return false
				}
				if !valueMatchesType(e, p.Type) {
					return false
				}
			}
			for _, p := range typ.Properties {
				if !p.IsRequired() {
					continue
				}
				v, ok := m.GetOk(p.Name)
				if !ok || v.IsNull() {
					return false
				}
			}
			return true
		case *schema.MapType:
			for _, e := range m.All {
				if !valueMatchesType(e, typ.ElementType) {
					return false
				}
			}
			return true
		}
	}
	return false
}
