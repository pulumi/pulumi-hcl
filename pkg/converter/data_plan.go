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
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
)

// dataPlan records how the converter emits one data block.
//
// A data block becomes an invoke. The invoke is inlined at its reference only
// when the inlined invoke runs exactly as often as Terraform reads the data
// block. In all other cases, the invoke is hoisted to a PCL local:
//
//   - an unranged data block becomes `name = invoke(...)`.
//   - a for_each data block becomes `name = {for k, v in coll : k => invoke(...)}`.
//   - a count data block becomes `name = [for i in range(count) : invoke(...)]`.
type dataPlan struct {
	// local is the name of the PCL local that holds the hoisted invoke, or ""
	// when the invoke is inlined at its one reference.
	local string
	// ranged is true when the data block has count or for_each.
	ranged bool
	// subst names the PCL variables that replace each.* and count.* in the
	// body of the invoke. It is nil when the range variables convert to
	// range.*.
	subst *rangeSubst
}

// rangeSubst names the PCL variables that replace each.key, each.value and
// count.index.
type rangeSubst struct {
	key, value, index string
}

// iterScope describes how often an expression is evaluated.
type iterScope struct {
	// ranged is true when the expression can be evaluated more than once.
	ranged bool
	// rangeAttr is the count or for_each attribute of the ranged block whose
	// body directly holds the expression.
	rangeAttr *hclsyntax.Attribute
	// forExpr is the for expression whose body directly holds the
	// expression, when that for expression itself is evaluated once.
	forExpr *hclsyntax.ForExpr
}

// dataRef is one reference to a data block.
type dataRef struct {
	scope iterScope
	// key is the index expression of data.<type>.<name>[key], or nil.
	key hclsyntax.Expression
	// repeated is true when the converted reference reads the invoke more than
	// once. This is the case for a reference to a whole MaxItems=1 field (see
	// [blockListTokens]).
	repeated bool
}

// dataPlanner counts the references to each data block in a project.
type dataPlanner struct {
	ft   *fileTransformer
	refs map[dataReference][]dataRef
	// activeCalls holds the call blocks whose bodies are being walked, to
	// stop on a cycle.
	activeCalls map[callReference]bool
}

// planDataBlocks decides, for each data block in bodies, if its invoke is
// inlined or hoisted to a local. It fills ft.dataPlans.
func (ft *fileTransformer) planDataBlocks(bodies []*hclsyntax.Body) {
	p := &dataPlanner{
		ft:          ft,
		refs:        make(map[dataReference][]dataRef),
		activeCalls: make(map[callReference]bool),
	}
	for _, body := range bodies {
		p.walkTopLevel(body)
	}

	pclNames := ft.pclNames(bodies)
	bound := boundNames(ft.dataBlocks)

	// The data blocks in source order, so that the local names are
	// deterministic.
	var order []dataReference
	for _, body := range bodies {
		for _, block := range body.Blocks {
			if block.Type == "data" && len(block.Labels) == 2 {
				order = append(order, dataReference{block.Labels[0], block.Labels[1]})
			}
		}
	}

	hoist := make(map[dataReference]bool, len(order))
	for _, ref := range order {
		rangeAttr := rangeAttribute(ft.dataBlocks[ref], isDataMeta)
		plan := &dataPlan{ranged: rangeAttr != nil}
		ft.dataPlans[ref] = plan
		refs := p.refs[ref]
		switch {
		case len(refs) != 1 || refs[0].repeated:
			hoist[ref] = true
		case !plan.ranged:
			hoist[ref] = refs[0].scope.ranged
		default:
			subst, aligned := ft.alignedSubst(rangeAttr, refs[0])
			plan.subst = subst
			hoist[ref] = !aligned
		}
	}

	// Name the hoisted locals. An inlined invoke whose each.* moves to the
	// variables of a for expression must not capture a PCL name, so it is
	// hoisted when it would. Each new local can capture another name, so
	// repeat until no plan changes.
	for changed := true; changed; {
		for _, ref := range order {
			if plan := ft.dataPlans[ref]; hoist[ref] && plan.local == "" {
				plan.local = uniqueName(ref.name, pclNames)
			}
		}
		changed = false
		for _, ref := range order {
			plan := ft.dataPlans[ref]
			if hoist[ref] || plan.subst == nil {
				continue
			}
			if pclNames[plan.subst.key] || bound[plan.subst.key] ||
				pclNames[plan.subst.value] || bound[plan.subst.value] {
				hoist[ref], plan.subst, changed = true, nil, true
			}
		}
	}

	// Name the loop variables of the hoisted ranged locals.
	avoid := func(name string) string {
		for pclNames[name] || bound[name] {
			name += "_"
		}
		return name
	}
	for _, ref := range order {
		if plan := ft.dataPlans[ref]; hoist[ref] && plan.ranged {
			plan.subst = &rangeSubst{key: avoid("key"), value: avoid("value"), index: avoid("index")}
		}
	}
}

// alignedSubst reports whether the one reference r to a data block with the
// range attribute rangeAttr can take the inlined invoke. This is true when r
// indexes the data block with the iteration key of an enclosing range over
// the same collection, so the inlined invoke runs once for each element, as
// Terraform reads the data block.
//
// The returned substitution is nil when each.* and count.* keep their normal
// conversion to range.*.
func (ft *fileTransformer) alignedSubst(rangeAttr *hclsyntax.Attribute, r dataRef) (*rangeSubst, bool) {
	key, ok := r.key.(*hclsyntax.ScopeTraversalExpr)
	if !ok {
		return nil, false
	}
	if outer := r.scope.rangeAttr; outer != nil {
		if outer.Name != rangeAttr.Name || !ft.sameTokens(outer.Expr, rangeAttr.Expr) {
			return nil, false
		}
		want := "each.key"
		if rangeAttr.Name == "count" {
			want = "count.index"
		}
		return nil, traversalString(key.Traversal) == want
	}
	if fe := r.scope.forExpr; fe != nil && rangeAttr.Name == "for_each" {
		if fe.KeyVar == "" || len(key.Traversal) != 1 || key.Traversal.RootName() != fe.KeyVar ||
			!ft.sameTokens(fe.CollExpr, rangeAttr.Expr) {
			return nil, false
		}
		return &rangeSubst{key: fe.KeyVar, value: fe.ValVar}, true
	}
	return nil, false
}

// traversalString returns trav as dotted attribute names, or "" when trav
// has a step that is not an attribute.
func traversalString(trav hcl.Traversal) string {
	names := []string{trav.RootName()}
	for _, step := range trav[1:] {
		attr, ok := step.(hcl.TraverseAttr)
		if !ok {
			return ""
		}
		names = append(names, attr.Name)
	}
	return strings.Join(names, ".")
}

// sameTokens reports whether a and b are the same sequence of tokens, without
// regard to newlines and comments.
func (ft *fileTransformer) sameTokens(a, b hclsyntax.Expression) bool {
	ta, tb := ft.exprTokens(a), ft.exprTokens(b)
	if ta == nil || tb == nil || len(ta) != len(tb) {
		return false
	}
	for i := range ta {
		if ta[i].Type != tb[i].Type || string(ta[i].Bytes) != string(tb[i].Bytes) {
			return false
		}
	}
	return true
}

// exprTokens lexes the source of expr, without newlines and comments. It
// returns nil when the source is not available.
func (ft *fileTransformer) exprTokens(expr hclsyntax.Expression) hclsyntax.Tokens {
	r := expr.Range()
	src := ft.srcBytes(r)
	if src == nil {
		return nil
	}
	tokens, diags := hclsyntax.LexExpression(src, r.Filename, r.Start)
	if diags.HasErrors() {
		return nil
	}
	var result hclsyntax.Tokens
	for _, t := range tokens {
		switch t.Type {
		case hclsyntax.TokenNewline, hclsyntax.TokenComment, hclsyntax.TokenEOF:
		default:
			result = append(result, t)
		}
	}
	return result
}

// rangeAttribute returns the count or for_each attribute of body, or nil.
// isMeta reports whether an attribute name is a meta-argument of the block
// type, so that an input property named count is not a range.
func rangeAttribute(body *hclsyntax.Body, isMeta func(name string) bool) *hclsyntax.Attribute {
	for _, name := range []string{"for_each", "count"} {
		if attr, ok := body.Attributes[name]; ok && isMeta(name) {
			return attr
		}
	}
	return nil
}

// walkTopLevel records the data references in the top-level body of a file.
func (p *dataPlanner) walkTopLevel(body *hclsyntax.Body) {
	for _, attr := range body.Attributes {
		p.walkExpr(attr.Expr, iterScope{})
	}
	for _, block := range body.Blocks {
		switch block.Type {
		case "call":
			// A call body is emitted at each reference to the call.
		case "data":
			p.walkRangedBody(block.Body, isDataMeta)
		case "resource":
			p.walkRangedBody(block.Body, isResourceMeta)
		case "module":
			p.walkRangedBody(block.Body, isModuleMeta)
		case "provider":
			p.walkRangedBody(block.Body, isProviderMeta)
		default:
			p.walkBody(block.Body, iterScope{}, nil)
		}
	}
}

// walkRangedBody records the data references in the body of a top-level
// block that can have count or for_each.
func (p *dataPlanner) walkRangedBody(body *hclsyntax.Body, isMeta func(name string) bool) {
	rangeAttr := rangeAttribute(body, isMeta)
	if rangeAttr == nil {
		p.walkBody(body, iterScope{}, nil)
		return
	}
	// The range expression is evaluated once, outside of the range.
	p.walkExpr(rangeAttr.Expr, iterScope{})
	p.walkBody(body, iterScope{ranged: true, rangeAttr: rangeAttr}, rangeAttr)
}

// walkBody records the data references in body, which is evaluated in
// scope. It does not walk the attribute skip.
func (p *dataPlanner) walkBody(body *hclsyntax.Body, scope iterScope, skip *hclsyntax.Attribute) {
	for _, attr := range body.Attributes {
		if attr != skip {
			p.walkExpr(attr.Expr, scope)
		}
	}
	for _, block := range body.Blocks {
		if block.Type != "dynamic" {
			p.walkBody(block.Body, scope, nil)
			continue
		}
		// The content of a dynamic block is evaluated once for each element
		// of its for_each.
		for _, attr := range block.Body.Attributes {
			p.walkExpr(attr.Expr, scope)
		}
		for _, content := range block.Body.Blocks {
			p.walkBody(content.Body, iterScope{ranged: true}, nil)
		}
	}
}

// walkExpr records the data references in expr, which is evaluated in scope.
func (p *dataPlanner) walkExpr(expr hclsyntax.Expression, scope iterScope) {
	w := &refWalker{p: p, frames: []walkFrame{{scope: scope}}}
	// refWalker never returns diagnostics.
	_ = hclsyntax.Walk(expr, w)
}

type walkFrame struct {
	node  hclsyntax.Node
	scope iterScope
}

// refWalker is an hclsyntax.Walker that tracks the iteration scope of each
// node.
type refWalker struct {
	p      *dataPlanner
	frames []walkFrame
}

func (w *refWalker) Enter(n hclsyntax.Node) hcl.Diagnostics {
	parent := w.frames[len(w.frames)-1]
	scope := parent.scope
	if fe, ok := parent.node.(*hclsyntax.ForExpr); ok && n != hclsyntax.Node(fe.CollExpr) {
		scope = iterScope{ranged: true}
		if !parent.scope.ranged {
			scope.forExpr = fe
		}
	}
	w.frames = append(w.frames, walkFrame{node: n, scope: scope})

	trav, ok := n.(*hclsyntax.ScopeTraversalExpr)
	if !ok || len(trav.Traversal) < 3 {
		return nil
	}
	typeAttr, ok1 := trav.Traversal[1].(hcl.TraverseAttr)
	nameAttr, ok2 := trav.Traversal[2].(hcl.TraverseAttr)
	if !ok1 || !ok2 {
		return nil
	}
	switch trav.Traversal.RootName() {
	case "data":
		ref := dataReference{typeAttr.Name, nameAttr.Name}
		if _, ok := w.p.ft.dataBlocks[ref]; !ok {
			return nil
		}
		var key hclsyntax.Expression
		if idx, ok := parent.node.(*hclsyntax.IndexExpr); ok &&
			hclsyntax.Node(idx.Collection) == n && len(trav.Traversal) == 3 {
			key = idx.Key
		}
		w.p.refs[ref] = append(w.p.refs[ref], dataRef{
			scope: scope, key: key, repeated: w.wrapped(ref, trav),
		})
	case "call":
		ref := callReference{typeAttr.Name, nameAttr.Name}
		body, ok := w.p.ft.callBlocks[ref]
		if !ok || w.p.activeCalls[ref] {
			return nil
		}
		w.p.activeCalls[ref] = true
		w.p.walkBody(body, iterScope{ranged: scope.ranged}, nil)
		delete(w.p.activeCalls, ref)
	}
	return nil
}

// wrapped reports whether the converter writes the data reference trav as the
// list that HCL reads for a MaxItems=1 field. trav is the node that Enter
// visits now. The converter maps the steps of trav itself (see
// [fileTransformer.dataSteps]), and the steps of a relative traversal over
// trav or over trav[key] (see [fileTransformer.relativeTraversalSourceProps]).
func (w *refWalker) wrapped(ref dataReference, trav *hclsyntax.ScopeTraversalExpr) bool {
	ft := w.p.ft
	_, steps := ft.dataSteps(ref, trav.Traversal)
	props, mapping := ft.dataResultProps(ref.hclType)
	if _, block := mapSteps(steps, props, mapping); block != nil {
		return true
	}
	// The last frame is trav itself.
	i := len(w.frames) - 2
	var source hclsyntax.Expression = trav
	if idx, ok := w.frames[i].node.(*hclsyntax.IndexExpr); ok && idx.Collection == source {
		source = idx
		i--
	}
	rel, ok := w.frames[i].node.(*hclsyntax.RelativeTraversalExpr)
	if !ok || rel.Source != source {
		return false
	}
	props, mapping = ft.relativeTraversalSourceProps(rel.Source)
	_, block := mapSteps(rel.Traversal, props, mapping)
	return block != nil
}

func (w *refWalker) Exit(hclsyntax.Node) hcl.Diagnostics {
	w.frames = w.frames[:len(w.frames)-1]
	return nil
}

// pclNames returns the names that the PCL program declares at the top level.
func (ft *fileTransformer) pclNames(bodies []*hclsyntax.Body) map[string]bool {
	names := make(map[string]bool)
	add := func(name string) {
		if rewritten, ok := ft.nameRewrites[name]; ok {
			name = rewritten
		}
		names[name] = true
	}
	for _, body := range bodies {
		for name := range body.Attributes {
			add(name)
		}
		for _, block := range body.Blocks {
			switch block.Type {
			case "locals":
				for name := range block.Body.Attributes {
					add(name)
				}
			case "provider":
				if len(block.Labels) >= 1 {
					add(providerResourceName(block))
				}
			default:
				if name := blockLogicalName(block); name != "" {
					add(name)
				}
			}
		}
	}
	return names
}

// boundNames returns the names that a for expression or a dynamic block
// binds in any data block body. A loop variable with one of these names can
// be shadowed in the PCL form of a data block body.
func boundNames(dataBlocks map[dataReference]*hclsyntax.Body) map[string]bool {
	names := map[string]bool{"__key": true, "__value": true}
	for _, body := range dataBlocks {
		_ = hclsyntax.VisitAll(body, func(n hclsyntax.Node) hcl.Diagnostics {
			if fe, ok := n.(*hclsyntax.ForExpr); ok {
				names[fe.KeyVar] = true
				names[fe.ValVar] = true
			}
			return nil
		})
	}
	delete(names, "")
	return names
}

// uniqueName returns a valid identifier for name that is not in used, and
// adds it to used.
func uniqueName(name string, used map[string]bool) string {
	if !hclsyntax.ValidIdentifier(name) {
		name = sanitizeIdentifier(name)
	}
	for used[name] {
		name += "_"
	}
	used[name] = true
	return name
}
