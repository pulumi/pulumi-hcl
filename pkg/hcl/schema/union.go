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
	"slices"

	"github.com/zclconf/go-cty/cty"
)

// unionMark marks a dynamic unknown that stands for one of several typed
// values. cty has no union type, so a conditional whose branch types do not
// unify is typed as the union of its branches: at runtime only the selected
// branch is ever evaluated, so the result is one of the branch values, not a
// unification of them. Members are never unions themselves and no member is
// dynamic (a dynamic member makes the whole value dynamic).
type unionMark struct {
	members []cty.Value
}

// unionMembers returns the members of a union value, and false for any other
// value.
func unionMembers(v cty.Value) ([]cty.Value, bool) {
	_, marks := v.Unmark()
	for m := range marks {
		if u, ok := m.(*unionMark); ok {
			return u.members, true
		}
	}
	return nil, false
}

// unionVal returns the value standing for any of members: the member itself
// when they collapse to one distinct type, a dynamic unknown when any member is
// dynamic, and otherwise a union of the distinct member types. Nested unions
// are flattened. Members keep only their union marks, since the members ride
// inside a mark where a later unmark cannot reach them.
func unionVal(members []cty.Value) cty.Value {
	var flat []cty.Value
	for _, m := range members {
		if nested, ok := unionMembers(m); ok {
			flat = append(flat, nested...)
			continue
		}
		if m.Type() == cty.DynamicPseudoType {
			return cty.DynamicVal
		}
		m = keepUnions(m)
		if !containsType(flat, m.Type()) {
			flat = append(flat, m)
		}
	}
	if len(flat) == 1 {
		return flat[0]
	}
	return cty.DynamicVal.Mark(&unionMark{members: flat})
}

func containsType(vals []cty.Value, t cty.Type) bool {
	return slices.ContainsFunc(vals, func(v cty.Value) bool { return v.Type().Equals(t) })
}

// couldBeNull reports whether v, or any member of a union v, could be null.
func couldBeNull(v cty.Value) bool {
	if members, ok := unionMembers(v); ok {
		return slices.ContainsFunc(members, couldBeNull)
	}
	return v.Range().CouldBeNull()
}

// stripUnions removes every union mark from v's value tree. A union that flowed
// through an operation the typing walker does not model carries stale members,
// so the value degrades to what HCL computed for it (a dynamic unknown).
func stripUnions(v cty.Value) cty.Value {
	unmarked, pvms := v.UnmarkDeepWithPaths()
	return unmarked.MarkWithPaths(filterMarks(pvms, func(m any) bool {
		_, isUnion := m.(*unionMark)
		return !isUnion
	}))
}

// keepUnions removes every mark from v's value tree except union marks.
func keepUnions(v cty.Value) cty.Value {
	unmarked, pvms := v.UnmarkDeepWithPaths()
	return unmarked.MarkWithPaths(filterMarks(pvms, func(m any) bool {
		_, isUnion := m.(*unionMark)
		return isUnion
	}))
}

func filterMarks(pvms []cty.PathValueMarks, keep func(any) bool) []cty.PathValueMarks {
	var out []cty.PathValueMarks
	for _, pvm := range pvms {
		kept := cty.ValueMarks{}
		for m := range pvm.Marks {
			if keep(m) {
				kept[m] = struct{}{}
			}
		}
		if len(kept) > 0 {
			out = append(out, cty.PathValueMarks{Path: pvm.Path, Marks: kept})
		}
	}
	return out
}
