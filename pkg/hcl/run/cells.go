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

package run

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/hashicorp/hcl/v2"

	"github.com/pulumi/pulumi-hcl/pkg/hcl/eval"
	"github.com/pulumi/pulumi-hcl/pkg/hcl/graph"
	"github.com/pulumi/pulumi-hcl/pkg/hcl/modulepath"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource/urn"
	"github.com/zclconf/go-cty/cty"
)

// A cell is one (resource/data block × module instance) unit of expansion.
// Each cell owns a graph.BlockExpansion: its expand node evaluates
// count/for_each in the cell's evaluation context and creates one node per
// instance, so instances of different cells — and of the same cell — run
// concurrently, and a consumer that references a single instance waits only
// for that instance's gate rather than the whole block.
//
// Root cells materialize before the walk starts; a module's cells materialize
// when its init node runs and the instance set is known. Either way the order
// is: create every cell of the scope, wire dependencies and gates, then arm —
// so a gate is always created before its target's expansion runs.
type expansionCell struct {
	block graph.NodeKey
	mi    modulepath.Path // module instance path
}

func cellKey(node *graph.Node, mi *moduleInstance) expansionCell {
	c := expansionCell{block: node.Key}
	if mi != nil {
		c.mi = mi.Path
	}
	return c
}

// cell returns the expansion cell for a block within one module instance,
// erroring when materialization never created it.
func (e *Engine) cell(block graph.NodeKey, mi modulepath.Path) (*graph.BlockExpansion, error) {
	b, ok := e.expansions.Get(expansionCell{block: block, mi: mi})
	if !ok {
		return nil, fmt.Errorf("no expansion cell for %q in module instance %q", block.String(), mi.String())
	}
	return b, nil
}

// urnRegistry accumulates the URNs of the resource instances each cell has
// registered so far, so dependency metadata can be recorded at resource
// granularity: destroy ordering is resource-wide — an instance-keyed
// reference or depends_on still makes every instance of the target wait for
// the consumer — so a URN collected from one instance widens to every
// registered sibling. Instances that register later cannot be recorded (their
// URNs do not exist yet), so a consumer that raced ahead of a delayed sibling
// leaves that sibling free to destroy independently.
type urnRegistry struct {
	mu   sync.Mutex
	m    map[expansionCell][]string
	cell map[string]expansionCell // URN → owning cell
}

func newURNRegistry() *urnRegistry {
	return &urnRegistry{
		m:    make(map[expansionCell][]string),
		cell: make(map[string]expansionCell),
	}
}

func (r *urnRegistry) add(c expansionCell, urn string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.m[c] = append(r.m[c], urn)
	r.cell[urn] = c
}

func (r *urnRegistry) get(c expansionCell) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.m[c])
}

// widen replaces each URN owned by a known cell with that cell's full
// registered instance set, passing unknown URNs through, and returns the
// deduplicated, sorted result.
func (r *urnRegistry) widen(urns []string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, u := range urns {
		if c, ok := r.cell[u]; ok {
			out = append(out, r.m[c]...)
		} else {
			out = append(out, u)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// materializeRootCells creates, wires, and arms the cells for every top-level
// resource/data block, plus the plan barrier separating plan-time data reads
// from resource creation. Runs after graph validation and before the walk.
func (e *Engine) materializeRootCells(g *graph.Graph) error {
	e.planBarrier = g.NewJoinNode("!plan-reads", "plan barrier")

	var blocks, locals []*graph.Node
	for _, node := range g.ExpandableNodes() {
		if node.ModuleInfo == nil {
			if node.Type == graph.NodeTypeLocal {
				locals = append(locals, node)
			} else {
				blocks = append(blocks, node)
			}
			continue
		}
		// A module holding a plan-time data source keeps the barrier open
		// until its init runs: init wires the data cells' completions to the
		// barrier before the barrier's init prerequisite is met. (Such an
		// init never transitively depends on a resource — that would have
		// classified the data source as deferred — so this cannot cycle with
		// the barrier gating resource expansion.)
		if node.Type == graph.NodeTypeDataSource && node.PlanTimeRead {
			init, ok := g.KeyNode(graph.NodeKey{Module: node.ModuleInfo.Path, ID: "__init__"})
			if !ok {
				return fmt.Errorf("no init node for module %q", graph.ModuleAddress(node.ModuleInfo.Path))
			}
			if err := g.Order(init, e.planBarrier); err != nil {
				return err
			}
		}
	}
	if err := e.materializeCells(g, blocks, []*moduleInstance{nil}, true); err != nil {
		return err
	}
	return e.wireRootLocals(g, locals)
}

// wireRootLocals gives each classified root local its instance-granular
// dependency edges: whole-block references bind to the target cell's
// completion, literal-indexed references to that instance's gate. The local's
// static deps were wired at graph build; block-level edges were deliberately
// omitted there so narrowness flows through the local to its consumers.
// Although the cells are already armed, nothing executes until the walk
// starts, so creating gates here still precedes every expansion.
func (e *Engine) wireRootLocals(g *graph.Graph, locals []*graph.Node) error {
	for _, node := range locals {
		ln, ok := g.KeyNode(node.Key)
		if !ok {
			return fmt.Errorf("no graph node for %q", node.Key.String())
		}
		for _, whole := range node.Deps.Whole {
			target, err := e.cell(whole, modulepath.Root())
			if err != nil {
				return err
			}
			if err := g.Order(target.Complete(), ln); err != nil {
				return err
			}
		}
		for _, narrow := range node.Deps.Narrow {
			target, err := e.cell(narrow.Node, modulepath.Root())
			if err != nil {
				return err
			}
			if err := g.Order(target.Gate(narrow.Suffix), ln); err != nil {
				return err
			}
		}
	}
	return nil
}

// materializeModuleCells creates, wires, and arms the cells for one module's
// blocks across its just-created instances. Called from the module's init
// node.
func (e *Engine) materializeModuleCells(modInfo *graph.ModuleInfo, instances []*moduleInstance) error {
	var blocks []*graph.Node
	for _, node := range e.graph.ExpandableNodes() {
		if node.ModuleInfo != nil && node.ModuleInfo.Path == modInfo.Path {
			blocks = append(blocks, node)
		}
	}
	return e.materializeCells(e.graph, blocks, instances, false)
}

func (e *Engine) materializeCells(g *graph.Graph, blocks []*graph.Node, mis []*moduleInstance, static bool) error {
	for _, node := range blocks {
		for _, mi := range mis {
			e.newCell(g, node, mi, static)
		}
	}
	for _, node := range blocks {
		for _, mi := range mis {
			if err := e.wireCell(g, node, mi); err != nil {
				return err
			}
		}
	}
	for _, node := range blocks {
		for _, mi := range mis {
			cell, ok := e.expansions.Get(cellKey(node, mi))
			if !ok {
				return fmt.Errorf("no expansion cell for %v", cellKey(node, mi))
			}
			cell.Arm()
		}
	}
	return nil
}

func (e *Engine) newCell(g *graph.Graph, node *graph.Node, mi *moduleInstance, static bool) {
	var b *graph.BlockExpansion
	b = g.NewBlockExpansion(node.Key, static, func(ctx context.Context) error {
		if node.Type == graph.NodeTypeDataSource {
			return e.expandDataCell(ctx, node, b, mi)
		}
		return e.expandResourceCell(ctx, node, b, mi)
	})
	e.expansions.Set(cellKey(node, mi), b)
}

// wireCell wires one cell's classified dependencies: static graph nodes
// directly, whole-block references to the target cell's completion, and
// instance references to the target cell's gate — targets resolved within the
// same module instance. The block's own graph node is ordered after the
// cell's completion, so whole-module aggregation still waits for every
// instance.
func (e *Engine) wireCell(g *graph.Graph, node *graph.Node, mi *moduleInstance) error {
	key := cellKey(node, mi)
	b, ok := e.expansions.Get(key)
	if !ok {
		return fmt.Errorf("no expansion cell for %v", key)
	}
	for _, dep := range node.Deps.Static {
		if err := b.DependOn(dep); err != nil {
			return err
		}
	}
	for _, whole := range node.Deps.Whole {
		target, err := e.cell(whole, key.mi)
		if err != nil {
			return err
		}
		if err := b.DependOn(target.Complete()); err != nil {
			return err
		}
	}
	for _, narrow := range node.Deps.Narrow {
		target, err := e.cell(narrow.Node, key.mi)
		if err != nil {
			return err
		}
		if err := b.DependOn(target.Gate(narrow.Suffix)); err != nil {
			return err
		}
	}

	// The plan/apply phase boundary: plan-time reads complete before the
	// barrier, resource expansion waits for it. Deferred reads participate in
	// neither — they transitively wait on a resource, so they land after the
	// barrier on their own.
	switch {
	case node.Type == graph.NodeTypeResource:
		if err := b.DependOn(e.planBarrier); err != nil {
			return err
		}
	case node.PlanTimeRead:
		if err := b.CompleteBefore(e.planBarrier); err != nil {
			return err
		}
	}

	blockNode, ok := g.KeyNode(node.Key)
	if !ok {
		return fmt.Errorf("no graph node for %q", node.Key.String())
	}
	return b.CompleteBefore(blockNode)
}

// cellEvalState resolves the evaluation context, parent URN, and module
// instance for a cell.
func cellEvalState(e *Engine, mi *moduleInstance) (*eval.Context, urn.URN, *moduleInstance) {
	if mi != nil {
		return mi.EvalCtx, mi.URN, mi
	}
	return e.evaluator.Context(), e.stackURN, nil
}

// metaArgs is the result of evaluating a block's count/for_each into an
// expander: the meta-argument dependency keys (references there establish
// dependencies that govern destroy ordering even when the body never uses the
// target) and which argument if any was unknown.
type metaArgs struct {
	deps       []string
	unknownArg string
}

func evalMetaArgs(
	evalCtx *eval.Context, node *graph.Node, expander *graph.ResourceExpander,
	countExpr, forEachExpr hcl.Expression,
) (metaArgs, error) {
	tempEvaluator := eval.NewEvaluator(evalCtx)
	var m metaArgs
	if countExpr != nil {
		count, isBool, unknown, deps, diags := tempEvaluator.EvaluateCount(countExpr)
		if diags.HasErrors() {
			return m, fmt.Errorf("evaluating count: %s", diags.Error())
		}
		m.deps = append(m.deps, deps...)
		switch {
		case unknown:
			m.unknownArg = "count"
		case isBool:
			expander.SetBoolCount(node.Key, count)
		default:
			expander.SetCount(node.Key, count)
		}
	}
	if forEachExpr != nil {
		forEach, unknown, deps, diags := tempEvaluator.EvaluateForEach(forEachExpr)
		if diags.HasErrors() {
			return m, fmt.Errorf("evaluating for_each: %s", diags.Error())
		}
		m.deps = append(m.deps, deps...)
		if unknown {
			m.unknownArg = "for_each"
		} else {
			expander.SetForEach(node.Key, forEach)
		}
	}
	return m, nil
}

// expandResourceCell evaluates a resource cell's count/for_each and creates
// one instance node per expanded instance.
func (e *Engine) expandResourceCell(
	ctx context.Context, node *graph.Node, b *graph.BlockExpansion, mi *moduleInstance,
) error {
	res := node.Resource
	if res == nil {
		return fmt.Errorf("resource node missing Resource field")
	}
	evalCtx, parentURN, modInst := cellEvalState(e, mi)

	resSchema, err := e.resolver.ResolveResource(ctx, res.Type)
	if err != nil {
		if diag := unknownTokenDiag("resource", res.TypeRange, err); diag != err {
			return diag
		}
		return fmt.Errorf("resolving resource type %s: %w", res.Type, err)
	}

	expander := graph.NewResourceExpander()
	meta, err := evalMetaArgs(evalCtx, node, expander, res.Count, res.ForEach)
	if err != nil {
		return err
	}

	baseKey := node.Key.ID

	// A count/for_each that reads values this operation has not yet produced
	// cannot be expanded. During preview, register no instances and bind the
	// resource address to unknown so downstream references resolve to unknown
	// rather than an empty collection.
	if meta.unknownArg != "" {
		if !e.dryRun {
			return fmt.Errorf("%s: the %s value depends on values that are not yet known", node.Key, meta.unknownArg)
		}
		evalCtx.SetResource(baseKey, "", cty.UnknownVal(cty.DynamicPseudoType))
		return nil
	}

	// Orphaned instances need the block's destroy provisioners even when the
	// block expands to zero instances this run.
	if !e.hasFailedDependency(res) {
		e.recordBlockEntry(ctx, res, resSchema, evalCtx, parentURN, modInst)
	}

	result := expander.Expand(node)

	// Empty count/for_each still needs the resource address bound so
	// downstream references see an empty collection rather than "no such
	// attribute".
	if len(result.Instances) == 0 {
		if res.Count != nil || res.ForEach != nil {
			empty := cty.EmptyObjectVal
			if res.Count != nil {
				empty = cty.EmptyTupleVal
			}
			evalCtx.SetResource(baseKey, "", empty)
		}
		return nil
	}

	for _, instance := range result.Instances {
		inst := instance
		err := b.AddInstance(inst.Key.Suffix, func(ctx context.Context) error {
			if e.hasFailedDependency(res) {
				e.failedNodes.Set(inst.Key, fmt.Errorf("skipped: dependency failed"))
				return nil
			}
			if err := e.registerResourceInstanceInContext(
				ctx, node, res, resSchema, inst, evalCtx, parentURN, modInst, meta.deps,
			); err != nil {
				return fmt.Errorf("registering %s: %w", inst.Key, err)
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// expandDataCell evaluates a data cell's count/for_each and creates one
// instance node per read.
func (e *Engine) expandDataCell(
	ctx context.Context, node *graph.Node, b *graph.BlockExpansion, mi *moduleInstance,
) error {
	ds := node.DataSource
	if ds == nil {
		return fmt.Errorf("data source node missing DataSource field")
	}
	evalCtx, _, _ := cellEvalState(e, mi)

	funcSchema, err := e.resolver.ResolveFunction(ctx, ds.Type)
	if err != nil {
		if diag := unknownTokenDiag("data source", ds.TypeRange, err); diag != err {
			return diag
		}
		return fmt.Errorf("resolving data source type %s: %w", ds.Type, err)
	}

	dsKey := strings.TrimPrefix(node.Key.ID, "data.")

	if ds.Count == nil && ds.ForEach == nil {
		return b.AddInstance("", func(ctx context.Context) error {
			ctyOutputs, err := e.invokeDataSourceOnce(ctx, node, ds, funcSchema, evalCtx, mi)
			if err != nil {
				return err
			}
			evalCtx.SetDataSource(dsKey, ctyOutputs)
			return nil
		})
	}

	expander := graph.NewResourceExpander()
	meta, err := evalMetaArgs(evalCtx, node, expander, ds.Count, ds.ForEach)
	if err != nil {
		return err
	}

	// See the matching case in expandResourceCell: unexpandable during
	// preview means no invokes and an unknown aggregate value.
	if meta.unknownArg != "" {
		if !e.dryRun {
			return fmt.Errorf("%s: the %s value depends on values that are not yet known", node.Key, meta.unknownArg)
		}
		evalCtx.SetDataSource(dsKey, cty.UnknownVal(cty.DynamicPseudoType))
		return nil
	}

	result := expander.Expand(node)

	if len(result.Instances) == 0 {
		empty := cty.EmptyObjectVal
		if ds.Count != nil {
			empty = cty.EmptyTupleVal
		}
		evalCtx.SetDataSource(dsKey, empty)
		return nil
	}

	for _, instance := range result.Instances {
		inst := instance
		err := b.AddInstance(inst.Key.Suffix, func(ctx context.Context) error {
			instCtx := evalCtx.WithIteration(inst.Index, inst.EachKey, inst.EachValue)
			ctyOut, err := e.invokeDataSourceOnce(ctx, node, ds, funcSchema, instCtx, mi)
			if err != nil {
				return err
			}
			switch {
			case inst.EachKey != nil:
				evalCtx.SetEachData(dsKey, inst.EachKey.AsString(), ctyOut)
			case inst.Index != nil:
				evalCtx.SetCountData(dsKey, *inst.Index, ctyOut)
			default:
				// A bool-derived count expands to a single unsuffixed
				// instance whose aggregate value is a one-element tuple.
				evalCtx.SetDataSource(dsKey, cty.TupleVal([]cty.Value{ctyOut}))
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	return nil
}
