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

package eval

import (
	"fmt"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource/urn"
	"github.com/pulumi/pulumi/sdk/v3/go/common/util/contract"
	"github.com/zclconf/go-cty/cty"
	"github.com/zclconf/go-cty/cty/convert"
)

// Evaluator handles expression evaluation for HCL configurations.
type Evaluator struct {
	ctx *Context
}

// NewEvaluator creates a new evaluator with the given context.
func NewEvaluator(ctx *Context) *Evaluator {
	return &Evaluator{ctx: ctx}
}

// Context returns the evaluation context.
func (e *Evaluator) Context() *Context {
	return e.ctx
}

// Evaluate evaluates an HCL expression and returns the result.
func (e *Evaluator) Evaluate(expr hcl.Expression) (cty.Value, hcl.Diagnostics) {
	return expr.Value(e.ctx.HCLContext())
}

// SensitiveMark is the cty value mark applied to sensitive values.
const SensitiveMark = "sensitive"

// EphemeralMark is the cty value mark applied to ephemeral values.
const EphemeralMark = "ephemeral"

// SyntheticMark is the cty value mark applied to attributes that pulumi-hcl
// injects onto a resource-reference object but that have no OpenTofu
// equivalent.
//
// In general, they are not user visable.
const SyntheticMark = "pulumi-synthetic"

// DepMark records the URN of a Pulumi resource a value transitively depends
// on. Marks ride along with values through cty expression evaluation, so the
// per-property dep set is just the union of DepMarks on the converted value
// — no parallel static analysis of expressions is needed.
type DepMark urn.URN

// stripSyntheticAttributes recursively removes object attributes whose value
// carries SyntheticMark.
func stripSyntheticAttributes(val cty.Value) cty.Value {
	out, err := cty.Transform(val, func(_ cty.Path, val cty.Value) (cty.Value, error) {
		if !val.IsKnown() || val.IsNull() || !val.Type().IsObjectType() {
			return val, nil
		}
		val, marks := val.Unmark()
		attrs := make(map[string]cty.Value, val.LengthInt())
		for k := range val.Type().AttributeTypes() {
			v := val.GetAttr(k)
			if v.HasMark(SyntheticMark) {
				continue
			}
			attrs[k] = v
		}
		return cty.ObjectVal(attrs).WithMarks(marks), nil
	})
	contract.AssertNoErrorf(err, "an error was never returned from cty.Transform")
	return out
}

// CollectDepURNs returns every distinct URN carried by a DepMark anywhere
// in val's value tree.
func CollectDepURNs(val cty.Value) []string {
	var urns []string
	seen := make(map[string]bool)
	_, marks := val.UnmarkDeep()
	for m := range marks {
		dm, ok := m.(DepMark)
		if !ok {
			continue
		}
		s := string(dm)
		if !seen[s] {
			seen[s] = true
			urns = append(urns, s)
		}
	}
	return urns
}

// SensitiveArgumentDiagnostic returns the diagnostic Terraform emits when
// a sensitive value is supplied as a meta-argument such as `for_each`.
// argName is the meta-argument's name. The shape of the diagnostic
// intentionally mirrors Terraform's so users coming from Terraform see a
// familiar message.
func SensitiveArgumentDiagnostic(argName string, expr hcl.Expression) *hcl.Diagnostic {
	return &hcl.Diagnostic{
		Severity: hcl.DiagError,
		Summary:  fmt.Sprintf("Invalid %s argument", argName),
		Detail: fmt.Sprintf(
			"Sensitive values, or values derived from sensitive values, "+
				"cannot be used as %s arguments. If used, the sensitive value "+
				"could be exposed as a resource instance key. If you want to use "+
				"the value anyway, wrap it in nonsensitive(...).",
			argName),
		Subject: expr.Range().Ptr(),
	}
}

// EvaluateCount evaluates a count expression and returns
// (count, isBool, unknown, deps, diags). isBool is true when the expression
// evaluated to a boolean (true→1, false→0), which suppresses the numeric
// index suffix on resource names. unknown is true when the expression reads
// values the current operation has not yet produced, so the number of
// instances cannot be determined. deps holds the URNs of the resources the
// expression references — even though count itself is not applied, a
// reference in it establishes a dependency, as in TF.
// Returns (1, false, false, nil, nil) if expr is nil (no count specified).
func (e *Evaluator) EvaluateCount(expr hcl.Expression) (count int, isBool bool, unknown bool, deps []string, diags hcl.Diagnostics) {
	if expr == nil {
		return 1, false, false, nil, nil
	}

	val, diags := e.Evaluate(expr)
	if diags.HasErrors() {
		return 0, false, false, nil, diags
	}
	deps = CollectDepURNs(val)

	// Count never flows into inputs, so drop marks; AsBigFloat / True panic on
	// them. Sensitive values are allowed: unlike a for_each key, a count does
	// not appear in instance keys.
	val, _ = val.Unmark()

	if !val.IsKnown() {
		return 0, false, true, deps, nil
	}

	if val.IsNull() {
		return 0, false, false, deps, hcl.Diagnostics{{
			Severity: hcl.DiagError,
			Summary:  "Invalid count value",
			Detail:   "Count cannot be null.",
			Subject:  expr.Range().Ptr(),
		}}
	}

	if val.Type() == cty.Bool {
		if val.True() {
			return 1, true, false, deps, nil
		}
		return 0, true, false, deps, nil
	}

	converted, err := convert.Convert(val, cty.Number)
	if err != nil {
		return 0, false, false, deps, hcl.Diagnostics{{
			Severity: hcl.DiagError,
			Summary:  "Invalid count value",
			Detail:   fmt.Sprintf("Count must be a number or boolean, got %s.", val.Type().FriendlyName()),
			Subject:  expr.Range().Ptr(),
		}}
	}
	bf := converted.AsBigFloat()
	i64, _ := bf.Int64()
	count = int(i64)

	if count < 0 {
		return 0, false, false, deps, hcl.Diagnostics{{
			Severity: hcl.DiagError,
			Summary:  "Invalid count value",
			Detail:   "Count must be a non-negative integer.",
			Subject:  expr.Range().Ptr(),
		}}
	}

	return count, false, false, deps, nil
}

// EvaluateForEach evaluates a for_each expression and returns
// (result, unknown, deps, diags). unknown is true when the collection — or, for
// a set, any of its elements, since those become instance keys — reads values
// the current operation has not yet produced, so the instances cannot be
// enumerated. Map and object values may be unknown as long as their keys are
// known. deps holds the URNs of the resources the expression references, so a
// reference in for_each establishes a dependency, as in TF.
// Returns a nil map if expr is nil (no for_each specified).
func (e *Evaluator) EvaluateForEach(expr hcl.Expression) (result map[string]cty.Value, unknown bool, deps []string, diags hcl.Diagnostics) {
	if expr == nil {
		return nil, false, nil, nil
	}

	val, diags := e.Evaluate(expr)
	if diags.HasErrors() {
		return nil, false, nil, diags
	}
	deps = CollectDepURNs(val)

	if val.HasMark(SensitiveMark) {
		return nil, false, deps, hcl.Diagnostics{SensitiveArgumentDiagnostic("for_each", expr)}
	}

	// Unmark only the container — per-element DepMarks must survive on the
	// values stored in result so each.value carries deps into the body.
	val, _ = val.Unmark()

	if !val.IsKnown() {
		return nil, true, deps, nil
	}

	if val.IsNull() {
		diags = append(diags, &hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  "Invalid for_each value",
			Detail:   "for_each cannot be null.",
			Subject:  expr.Range().Ptr(),
		})
		return nil, false, deps, diags
	}

	ty := val.Type()
	result = make(map[string]cty.Value)

	switch {
	case ty.IsMapType() || ty.IsObjectType():
		for it := val.ElementIterator(); it.Next(); {
			k, v := it.Element()
			k, _ = k.Unmark()
			result[k.AsString()] = v
		}
	case ty.IsSetType():
		// An empty set is always a no-op, even if its element type is
		// `dynamic`.
		if val.LengthInt() > 0 && ty.ElementType() != cty.String {
			diags = append(diags, &hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  "Invalid for_each value",
				Detail:   "for_each set must contain strings.",
				Subject:  expr.Range().Ptr(),
			})
			return nil, false, deps, diags
		}
		if !val.IsWhollyKnown() {
			return nil, true, deps, nil
		}
		for it := val.ElementIterator(); it.Next(); {
			_, v := it.Element()
			unmarkedV, _ := v.Unmark()
			if unmarkedV.IsNull() {
				diags = append(diags, &hcl.Diagnostic{
					Severity: hcl.DiagError,
					Summary:  "Invalid for_each value",
					Detail:   "for_each set must not contain null values.",
					Subject:  expr.Range().Ptr(),
				})
				return nil, false, deps, diags
			}
			result[unmarkedV.AsString()] = v
		}
	default:
		diags = append(diags, &hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  "Invalid for_each value",
			Detail:   fmt.Sprintf("for_each must be a map or set, not %s.", ty.FriendlyName()),
			Subject:  expr.Range().Ptr(),
		})
		return nil, false, deps, diags
	}

	return result, false, deps, diags
}

// ParseTraversal parses a traversal into its components.
func ParseTraversal(traversal hcl.Traversal) (namespace string, parts []string) {
	if len(traversal) == 0 {
		return "", nil
	}

	namespace = traversal.RootName()

	for i := 1; i < len(traversal); i++ {
		switch step := traversal[i].(type) {
		case hcl.TraverseAttr:
			parts = append(parts, step.Name)
		case hcl.TraverseIndex:
			// For index traversals, convert to string representation
			idx := step.Key
			if idx.Type() == cty.String {
				parts = append(parts, idx.AsString())
			} else if idx.Type() == cty.Number {
				bf := idx.AsBigFloat()
				i64, _ := bf.Int64()
				parts = append(parts, fmt.Sprintf("%d", i64))
			}
		}
	}

	return namespace, parts
}

// ExtractDependencies extracts all resource/data/module dependencies from an expression.
func ExtractDependencies(expr hcl.Expression) []string {
	var deps []string
	seen := make(map[string]bool)
	for _, traversal := range expr.Variables() {
		if dep := traversalToDep(traversal); dep != "" && !seen[dep] {
			deps = append(deps, dep)
			seen[dep] = true
		}
	}
	return deps
}

// NonRecoverableDependencies is ExtractDependencies restricted to references
// that are not the recoverable value argument of a recover(value, recovery)
// call. A dependency reached only through such an argument does not gate the
// referencing node: if the dependency fails, recover() substitutes the recovery
// value rather than failing the node.
func NonRecoverableDependencies(expr hcl.Expression) []string {
	recoverArgRanges := recoverValueArgRanges(expr)
	var deps []string
	seen := make(map[string]bool)
	for _, traversal := range expr.Variables() {
		if within(traversal.SourceRange(), recoverArgRanges) {
			continue
		}
		if dep := traversalToDep(traversal); dep != "" && !seen[dep] {
			deps = append(deps, dep)
			seen[dep] = true
		}
	}
	return deps
}

// recoverValueArgRanges returns the source ranges of the first (value) argument
// of every recover(...) call in expr.
func recoverValueArgRanges(expr hcl.Expression) []hcl.Range {
	node, ok := expr.(hclsyntax.Node)
	if !ok {
		return nil
	}
	var ranges []hcl.Range
	_ = hclsyntax.VisitAll(node, func(n hclsyntax.Node) hcl.Diagnostics {
		if call, ok := n.(*hclsyntax.FunctionCallExpr); ok && call.Name == "recover" && len(call.Args) >= 1 {
			ranges = append(ranges, call.Args[0].Range())
		}
		return nil
	})
	return ranges
}

func within(r hcl.Range, ranges []hcl.Range) bool {
	for _, outer := range ranges {
		if outer.ContainsOffset(r.Start.Byte) {
			return true
		}
	}
	return false
}

// traversalToDep converts a traversal to its resource/data/module dependency
// key, or "" when the traversal is not a dependency (var, local, count, ...).
func traversalToDep(traversal hcl.Traversal) string {
	namespace, parts := ParseTraversal(traversal)
	switch namespace {
	case "var", "local", "path", "terraform", "count", "each", "self":
		return ""
	case "data":
		if len(parts) >= 2 {
			return fmt.Sprintf("data.%s.%s", parts[0], parts[1])
		}
	case "module":
		if len(parts) >= 1 {
			return fmt.Sprintf("module.%s", parts[0])
		}
	default:
		if len(parts) >= 1 {
			return fmt.Sprintf("%s.%s", namespace, parts[0])
		}
	}
	return ""
}

// IsKnown returns true if the value is fully known (not unknown).
func IsKnown(val cty.Value) bool {
	if !val.IsKnown() {
		return false
	}
	if val.Type().IsCollectionType() || val.Type().IsObjectType() || val.Type().IsTupleType() {
		for it := val.ElementIterator(); it.Next(); {
			_, v := it.Element()
			if !IsKnown(v) {
				return false
			}
		}
	}
	return true
}

// EvaluateExpression evaluates an HCL expression.
func (e *Evaluator) EvaluateExpression(expr hcl.Expression) (cty.Value, hcl.Diagnostics) {
	return expr.Value(e.ctx.HCLContext())
}
