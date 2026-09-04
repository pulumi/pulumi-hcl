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

package schema

import (
	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/pulumi/pulumi-hcl/pkg/hcl/transform"
	"github.com/zclconf/go-cty/cty"
	"github.com/zclconf/go-cty/cty/convert"
)

// rangedRefMark marks the unknown list or map a counted or for_each resource,
// data source, or module reference is seeded as. An index into such a
// collection resolves to an instance or errors, so the instance is never null
// and its attributes carry the refinements of a direct reference. A collection
// from anywhere else, such as a variable, may hold null elements and is left
// alone.
type rangedRefMark struct{}

// markRangedRef marks val as a ranged reference when ranged is true.
func markRangedRef(val cty.Value, ranged bool) cty.Value {
	if ranged {
		return val.Mark(rangedRefMark{})
	}
	return val
}

// typeExpr evaluates expr against ctx for its type, the way HCL would, except
// that a conditional whose branches do not unify types as the union of its
// branches, traversals into a union type against each member, and traversals
// are applied one step at a time so an object indexed out of a ranged
// reference regains the null refinements of its type. Every other expression
// is delegated to HCL; a union that reaches such an expression is stripped from
// the result, since HCL evaluated it as a plain dynamic unknown.
func typeExpr(expr hcl.Expression, ctx *hcl.EvalContext) (cty.Value, hcl.Diagnostics) {
	switch e := expr.(type) {
	case *hclsyntax.ParenthesesExpr:
		return typeExpr(e.Expression, ctx)

	case *hclsyntax.ConditionalExpr:
		trueVal, trueDiags := typeExpr(e.TrueResult, ctx)
		falseVal, falseDiags := typeExpr(e.FalseResult, ctx)
		if trueDiags.HasErrors() || falseDiags.HasErrors() || !branchesDisagree(trueVal, falseVal) {
			break
		}
		return unionVal([]cty.Value{trueVal, falseVal}), nil

	case *hclsyntax.ScopeTraversalExpr:
		root, diags := e.Traversal[:1].TraverseAbs(ctx)
		if diags.HasErrors() {
			return root, diags
		}
		return traverse(root, e.Traversal[1:])

	case *hclsyntax.RelativeTraversalExpr:
		source, diags := typeExpr(e.Source, ctx)
		if diags.HasErrors() {
			return source, diags
		}
		return traverse(source, e.Traversal)

	case *hclsyntax.IndexExpr:
		collection, diags := typeExpr(e.Collection, ctx)
		if diags.HasErrors() {
			return collection, diags
		}
		key, keyDiags := e.Key.Value(ctx)
		if keyDiags.HasErrors() {
			return key, keyDiags
		}
		if _, isUnion := unionMembers(key); isUnion {
			break
		}
		return traverse(collection, hcl.Traversal{hcl.TraverseIndex{Key: key, SrcRange: e.SrcRange}})

	case *hclsyntax.TupleConsExpr:
		elems := make([]cty.Value, len(e.Exprs))
		for i, elemExpr := range e.Exprs {
			elem, diags := typeExpr(elemExpr, ctx)
			if diags.HasErrors() {
				return elem, diags
			}
			elems[i] = elem
		}
		return cty.TupleVal(elems), nil

	case *hclsyntax.ObjectConsExpr:
		attrs := make(map[string]cty.Value, len(e.Items))
		for _, item := range e.Items {
			key, diags := item.KeyExpr.Value(ctx)
			if diags.HasErrors() || key.IsMarked() || key.IsNull() || !key.IsKnown() {
				attrs = nil
				break
			}
			key, err := convert.Convert(key, cty.String)
			if err != nil {
				attrs = nil
				break
			}
			val, diags := typeExpr(item.ValueExpr, ctx)
			if diags.HasErrors() {
				return val, diags
			}
			attrs[key.AsString()] = val
		}
		if attrs != nil {
			return cty.ObjectVal(attrs), nil
		}
	}

	val, diags := expr.Value(ctx)
	return stripUnions(val), diags
}

// branchesDisagree reports whether HCL would reject a conditional over the two
// branch values: either is a union, or their types do not unify. A dynamic
// branch never disagrees, matching HCL, which adopts the other branch's type.
func branchesDisagree(trueVal, falseVal cty.Value) bool {
	if _, ok := unionMembers(trueVal); ok {
		return true
	}
	if _, ok := unionMembers(falseVal); ok {
		return true
	}
	trueTy, falseTy := trueVal.Type(), falseVal.Type()
	if trueTy == cty.DynamicPseudoType || falseTy == cty.DynamicPseudoType {
		return false
	}
	unified, _ := convert.UnifyUnsafe([]cty.Type{trueTy, falseTy})
	return unified == cty.NilType
}

// traverse applies traversal to val one step at a time. A step into a union
// applies to each member; members the step rejects are dropped, since the
// runtime rejects the step only when it selects such a member. When every
// member rejects the step, its diagnostics are reported.
func traverse(val cty.Value, traversal hcl.Traversal) (cty.Value, hcl.Diagnostics) {
	for _, step := range traversal {
		members, isUnion := unionMembers(val)
		if !isUnion {
			var diags hcl.Diagnostics
			val, diags = traverseStep(val, step)
			if diags.HasErrors() {
				return val, diags
			}
			continue
		}
		var survivors []cty.Value
		var firstDiags hcl.Diagnostics
		for _, m := range members {
			stepped, diags := traverseStep(m, step)
			if diags.HasErrors() {
				if firstDiags == nil {
					firstDiags = diags
				}
				continue
			}
			survivors = append(survivors, stepped)
		}
		if len(survivors) == 0 {
			return cty.DynamicVal, firstDiags
		}
		val = unionVal(survivors)
	}
	return val, nil
}

// traverseStep applies one traversal step to val. A ranged reference yields an
// unrefined unknown when indexed; when that instance is an object it is rebuilt
// with the refinements its type implies, as a direct reference to the same
// object carries, so attributes reached through the index keep their
// nullability. The instance sheds the ranged-reference mark, since it is no
// longer a collection of instances.
func traverseStep(val cty.Value, step hcl.Traverser) (cty.Value, hcl.Diagnostics) {
	stepped, diags := hcl.Traversal{step}.TraverseRel(val)
	if diags.HasErrors() {
		return stepped, diags
	}
	if _, isIndex := step.(hcl.TraverseIndex); isIndex && val.HasMark(rangedRefMark{}) && stepped.Type().IsObjectType() {
		_, marks := stepped.Unmark()
		delete(marks, rangedRefMark{})
		stepped = transform.RefinedUnknown(stepped.Type(), false).WithMarks(marks)
	}
	return stepped, nil
}
