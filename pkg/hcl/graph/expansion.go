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
	"context"

	"github.com/pulumi/pulumi/pkg/v3/util/pdag"
	"github.com/pulumi/pulumi/sdk/v3/go/common/util/contract"
)

// BlockExpansion schedules one block's count/for_each instances independently
// of the block's graph node, so a consumer that needs a single instance can
// run before the block's other instances finish.
//
// Node structure (all created lazily during a walk except for static blocks):
//
//	deps ──▶ expand ──▶ instance… ──▶ complete
//	              │          │
//	              └──────────┴──────▶ gate[suffix] ──▶ (consumers)
//
// expand evaluates count/for_each and creates the instance nodes. complete
// becomes ready only after every instance finishes, so whole-block consumers
// bind to it. A gate stands for one instance identified by its key suffix
// (`[0]`, `["x"]`): consumers that reference only that instance bind to the
// gate instead of complete. A gate whose suffix matches no instance falls back
// to complete, so a bad index degrades to whole-block ordering and the
// consumer surfaces the evaluation error itself.
//
// Liveness holds by construction: complete and every gate are armed at
// creation with expand as their prerequisite, and the walker marks expand done
// even when its exec errors, so no dynamically created node can strand the
// walk. Instances are armed in finish, which runs via defer around the expand
// exec.
//
// A BlockExpansion is not safe for concurrent use. The contract is
// single-threaded by construction: creation and wiring happen in whichever
// callback materializes the skeleton (the graph build for static blocks, a
// module-init node for module blocks), and instance creation happens in the
// expand exec, which the walker cannot start until wiring armed it.
type BlockExpansion struct {
	g         *Graph
	key       NodeKey
	expand    pdag.Node
	armExpand pdag.Done
	complete  pdag.Node
	// gates holds the gates no instance has claimed yet; AddInstance removes
	// the gate it wires, and finish gives the leftovers their fallback edge.
	gates      map[string]pdag.Node
	armPending []pdag.Done
	expanded   bool
}

// NewBlockExpansion creates the expand/complete pair for one block. static
// marks a skeleton created before the walk starts: its expand node is interned
// under "<key>!expand" so InjectAfter sees it; skeletons created mid-walk must
// not touch the interning maps and stay anonymous. expandExec runs on the
// expand node; finish is deferred around it.
func (g *Graph) NewBlockExpansion(key NodeKey, static bool, expandExec func(context.Context) error) *BlockExpansion {
	b := &BlockExpansion{
		g:     g,
		key:   key,
		gates: make(map[string]pdag.Node),
	}
	exec := func(ctx context.Context) error {
		defer b.finish()
		return expandExec(ctx)
	}
	expandDesc := nodeDesc{block: key, aspect: aspectExpand}
	if static {
		b.expand, b.armExpand = g.internExecNode(NodeKey{Module: key.Module, ID: key.ID + "!expand"}, expandDesc, exec)
	} else {
		b.expand, b.armExpand = g.dag.NewNode(dagNode{desc: expandDesc, exec: exec})
	}

	complete, armComplete := g.dag.NewNode(dagNode{
		desc: nodeDesc{block: key, aspect: aspectComplete},
		exec: func(context.Context) error { return nil },
	})
	b.complete = complete
	contract.AssertNoErrorf(g.dag.NewEdge(b.expand, complete), "fresh nodes cannot form a cycle")
	armComplete()
	return b
}

// DependOn orders the expansion after dep: no instance is created (and so no
// instance work runs) until dep is done.
func (b *BlockExpansion) DependOn(dep pdag.Node) error {
	return cycleError(b.g.dag.NewEdge(dep, b.expand))
}

// Complete returns the node that is done once every instance has finished.
// Whole-block consumers bind to it.
func (b *BlockExpansion) Complete() pdag.Node { return b.complete }

// Gate returns the node standing for the instance with the given key suffix
// (`[0]`, `["x"]`), creating it on first use. Gates must be created before the
// expansion runs — consumers wire during skeleton materialization, which
// happens before Arm.
func (b *BlockExpansion) Gate(suffix string) pdag.Node {
	contract.Assertf(!b.expanded, "gate %q%s requested after expansion", b.key.String(), suffix)
	if gate, ok := b.gates[suffix]; ok {
		return gate
	}
	gate, arm := b.g.dag.NewNode(dagNode{
		desc: nodeDesc{block: b.key, aspect: aspectGate, index: suffix},
		exec: func(context.Context) error { return nil },
	})
	contract.AssertNoErrorf(b.g.dag.NewEdge(b.expand, gate), "fresh nodes cannot form a cycle")
	arm()
	b.gates[suffix] = gate
	return gate
}

// Arm makes the expand node runnable. Call once all deps and consumer gates
// are wired.
func (b *BlockExpansion) Arm() { b.armExpand() }

// CompleteBefore orders n after this block's completion, so a node outside
// the expansion (the block's own graph node, in particular) waits for every
// instance.
func (b *BlockExpansion) CompleteBefore(n pdag.Node) error {
	return cycleError(b.g.dag.NewEdge(b.complete, n))
}

// AddInstance creates the node that runs one instance's work, ordered before
// complete and standing behind the instance's gate if a consumer created one.
// Only call from the expand exec. Instances become runnable when the exec
// returns (finish arms them after all wiring).
func (b *BlockExpansion) AddInstance(suffix string, exec func(context.Context) error) error {
	inst, arm := b.g.dag.NewNode(dagNode{
		desc: nodeDesc{block: b.key, aspect: aspectInstance, index: suffix},
		exec: exec,
	})
	// Arm even on the error paths: a node left preparing stalls the walk
	// forever, while an armed node at worst runs before the walk aborts.
	b.armPending = append(b.armPending, arm)
	if err := cycleError(b.g.dag.NewEdge(inst, b.complete)); err != nil {
		return err
	}
	if gate, ok := b.gates[suffix]; ok {
		if err := cycleError(b.g.dag.NewEdge(inst, gate)); err != nil {
			return err
		}
		delete(b.gates, suffix)
	}
	return nil
}

// finish closes out an expansion: gates that matched no instance fall back to
// complete, and the created instances become runnable. Idempotent; deferred
// around the expand exec so an exec error cannot strand gates or instances.
func (b *BlockExpansion) finish() {
	if b.expanded {
		return
	}
	b.expanded = true
	for _, gate := range b.gates {
		// complete is armed but cannot be processing while expand still is,
		// so this edge is never reentrant.
		contract.AssertNoErrorf(b.g.dag.NewEdge(b.complete, gate),
			"gate fallback edges cannot form a cycle")
	}
	for _, arm := range b.armPending {
		arm()
	}
	b.armPending = nil
}
