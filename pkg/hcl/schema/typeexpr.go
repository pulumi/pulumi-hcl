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
	ctyconvert "github.com/zclconf/go-cty/cty/convert"
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

// union marks the unknown value of DynamicPseudoType that a conditional
// yields when its branch types do not unify. Its members are the branch values.
// A traversal step applies to every member that supports it and unifies the
// results, so an attribute the members share types as that attribute. A union
// that reaches an output types as any. A union that reaches an expression HCL evaluates (a
// function call, a for expression, ...) degrades to a plain DynamicVal.
type union struct {
	members []cty.Value
}

// asUnion returns the union val carries, if any.
func asUnion(val cty.Value) (*union, bool) {
	if val.Type() != cty.DynamicPseudoType {
		return nil, false
	}
	for m := range val.Marks() {
		if u, ok := m.(*union); ok {
			return u, true
		}
	}
	return nil, false
}

// dropUnions strips union marks from val and every value nested in it. HCL
// re-applies the marks of an argument to the value it computes from it, so a
// union mark on such a value no longer describes it.
func dropUnions(val cty.Value) cty.Value {
	unmarked, pvm := val.UnmarkDeepWithPaths()
	for _, p := range pvm {
		for m := range p.Marks {
			if _, ok := m.(*union); ok {
				delete(p.Marks, m)
			}
		}
	}
	return unmarked.MarkWithPaths(pvm)
}

// unify returns the value that stands for a value that is one of members.
// Members of one type unify to an unknown of that type, not null when no
// member can be null; members of one object type unify attribute by attribute,
// so each attribute keeps the refinements of its members. A null of unknown
// type only makes the result nullable. Members of different types form a
// union, unless convert is set and UnifyUnsafe finds a type every member
// converts to, which is what HCL does for the branches of a conditional. Only
// marks every member carries survive.
func unify(members []cty.Value, convert bool) cty.Value {
	var flat []cty.Value
	for _, m := range members {
		if u, ok := asUnion(m); ok {
			flat = append(flat, u.members...)
		} else {
			flat = append(flat, m)
		}
	}

	nullable := false
	var typed []cty.Value
	marks := cty.NewValueMarks()
	for i, m := range flat {
		unmarked, ms := m.Unmark()
		if i == 0 {
			marks = ms
		} else {
			for mark := range marks {
				if _, ok := ms[mark]; !ok {
					delete(marks, mark)
				}
			}
		}
		if unmarked.Range().CouldBeNull() {
			nullable = true
		}
		if unmarked.IsNull() && unmarked.Type() == cty.DynamicPseudoType {
			continue
		}
		typed = append(typed, unmarked)
	}
	if len(typed) == 0 {
		return cty.NullVal(cty.DynamicPseudoType)
	}

	ty := typed[0].Type()
	same := true
	types := make([]cty.Type, len(typed))
	for i, m := range typed {
		types[i] = m.Type()
		same = same && ty.Equals(types[i])
	}
	switch {
	case same && ty.IsObjectType() && !nullable:
		attrs := make(map[string]cty.Value, len(ty.AttributeTypes()))
		for name := range ty.AttributeTypes() {
			attr := make([]cty.Value, len(typed))
			for i, m := range typed {
				attr[i] = m.GetAttr(name)
			}
			attrs[name] = unify(attr, convert)
		}
		return cty.ObjectVal(attrs).WithMarks(marks)
	case same:
	case convert:
		if ty, _ = ctyconvert.UnifyUnsafe(types); ty != cty.NilType {
			break
		}
		fallthrough
	default:
		return cty.DynamicVal.Mark(&union{members: flat})
	}
	val := cty.UnknownVal(ty)
	if !nullable {
		val = val.RefineNotNull()
	}
	return val.WithMarks(marks)
}

// outputRequired reports whether the value inferred for an output is never
// null: for a union, whether no member can be null.
func outputRequired(val cty.Value) bool {
	if u, ok := asUnion(val); ok {
		for _, m := range u.members {
			m, _ := m.Unmark()
			if m.Range().CouldBeNull() {
				return false
			}
		}
		return true
	}
	val, _ = val.UnmarkDeep()
	return !val.Range().CouldBeNull()
}

// typeExpr evaluates expr against ctx for its type, the way HCL would, except
// that traversals are applied one step at a time so an object indexed out of
// a ranged reference regains the null refinements of its type, and that a
// conditional whose branch types do not unify yields a union instead of an
// error. Every other expression is delegated to HCL, which degrades a union
// among its inputs to a plain DynamicVal.
func typeExpr(expr hcl.Expression, ctx *hcl.EvalContext) (cty.Value, hcl.Diagnostics) {
	switch e := expr.(type) {
	case *hclsyntax.ParenthesesExpr:
		return typeExpr(e.Expression, ctx)

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
		return traverse(collection, hcl.Traversal{hcl.TraverseIndex{Key: key, SrcRange: e.SrcRange}})

	case *hclsyntax.ConditionalExpr:
		return typeConditional(e, ctx)
	}

	val, diags := expr.Value(ctx)
	return dropUnions(val), diags
}

// typeConditional types a conditional. A known condition selects its branch. An
// unknown condition unifies both branches. A condition HCL would reject (null,
// or not convertible to bool) is delegated to HCL for its diagnostic.
func typeConditional(e *hclsyntax.ConditionalExpr, ctx *hcl.EvalContext) (cty.Value, hcl.Diagnostics) {
	cond, diags := typeExpr(e.Condition, ctx)
	if diags.HasErrors() {
		return cond, diags
	}
	cond, _ = cond.Unmark()
	if cond.IsKnown() {
		b, err := ctyconvert.Convert(cond, cty.Bool)
		if err != nil || b.IsNull() {
			return e.Value(ctx)
		}
		if b.True() {
			return typeExpr(e.TrueResult, ctx)
		}
		return typeExpr(e.FalseResult, ctx)
	}
	trueVal, diags := typeExpr(e.TrueResult, ctx)
	if diags.HasErrors() {
		return trueVal, diags
	}
	falseVal, diags := typeExpr(e.FalseResult, ctx)
	if diags.HasErrors() {
		return falseVal, diags
	}
	return unify([]cty.Value{trueVal, falseVal}, true), nil
}

// traverse applies traversal to val one step at a time.
func traverse(val cty.Value, traversal hcl.Traversal) (cty.Value, hcl.Diagnostics) {
	for _, step := range traversal {
		var diags hcl.Diagnostics
		val, diags = traverseStep(val, step)
		if diags.HasErrors() {
			return val, diags
		}
	}
	return val, nil
}

// traverseStep applies one traversal step to val. A step into a union applies
// to every member and the results unify. A null member is kept as it is. A
// member the step does not apply to is pruned, since a type-correct program
// only takes the step on members that support it; the step is an error only
// when no member supports it. A ranged reference yields an unrefined unknown when indexed; when that
// instance is an object it is rebuilt with the refinements its type implies,
// as a direct reference to the same object carries, so attributes reached
// through the index keep their nullability. The instance sheds the
// ranged-reference mark, since it is no longer a collection of instances.
func traverseStep(val cty.Value, step hcl.Traverser) (cty.Value, hcl.Diagnostics) {
	if u, ok := asUnion(val); ok {
		var results []cty.Value
		var failed hcl.Diagnostics
		for _, m := range u.members {
			if m.IsNull() {
				results = append(results, m)
				continue
			}
			next, diags := traverseStep(m, step)
			if diags.HasErrors() {
				failed = append(failed, diags...)
				continue
			}
			results = append(results, next)
		}
		if len(results) == 0 {
			return cty.DynamicVal, failed
		}
		return unify(results, false), nil
	}
	next, diags := hcl.Traversal{step}.TraverseRel(val)
	if diags.HasErrors() {
		return next, diags
	}
	if _, isIndex := step.(hcl.TraverseIndex); isIndex && val.HasMark(rangedRefMark{}) && next.Type().IsObjectType() {
		_, marks := next.Unmark()
		delete(marks, rangedRefMark{})
		next = transform.RefinedUnknown(next.Type(), false).WithMarks(marks)
	}
	return next, nil
}
