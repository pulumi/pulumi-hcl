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

// Package graph implements dependency graph construction and topological sorting
// for HCL configuration execution ordering.
package graph

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"math/big"
	"slices"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/pulumi/pulumi-hcl/pkg/hcl/ast"
	"github.com/pulumi/pulumi-hcl/pkg/hcl/eval"
	"github.com/pulumi/pulumi-hcl/pkg/hcl/modulepath"
	"github.com/pulumi/pulumi/pkg/v3/util/pdag"
	"github.com/pulumi/pulumi/sdk/v3/go/common/util/contract"
	"github.com/zclconf/go-cty/cty"
)

// ModuleInfo holds metadata for nodes that are part of an inlined module.
type ModuleInfo struct {
	// Path identifies this module call within the nesting tree.
	// Its leaf step is bare (no count/for_each disambiguator) — runtime
	// instances of count/for_each-expanded calls live separately.
	Path modulepath.Path

	Module     *ast.Module // the module block from the parent config
	SourcePath string      // resolved source path (for component type name)

	// Terraform is the module's own `terraform` block (nil when it has
	// none). Its `required_providers` names the providers this module uses,
	// which the runtime needs to re-key provider references crossing the
	// module boundary — see [LocalProviderName].
	Terraform *ast.Terraform

	// ParentSourcePath is the resolved source directory of the parent module
	// (or the root program dir for top-level calls). It's the dir that
	// Module.Source is relative to, so the runtime can re-resolve it when
	// loading the child module on demand.
	ParentSourcePath string
}

// ModuleName returns the block label of this module call (e.g. "first").
func (m *ModuleInfo) ModuleName() string {
	_, last, ok := m.Path.Parent()
	if !ok {
		return ""
	}
	return last.Name()
}

// ModuleAddress renders path in Terraform address syntax —
// "module.<name>[key].module.<name>" — for diagnostics. The root renders "".
func ModuleAddress(path modulepath.Path) string {
	var b strings.Builder
	for s := range path.Steps {
		if b.Len() > 0 {
			b.WriteByte('.')
		}
		b.WriteString("module.")
		b.WriteString(s.Name())
		if idx, ok := s.Index(); ok {
			fmt.Fprintf(&b, "[%d]", idx)
		} else if key, ok := s.Key(); ok {
			fmt.Fprintf(&b, "[%q]", key)
		}
	}
	return b.String()
}

// ParentPath returns the path of the enclosing module, or modulepath.Root() if
// this module is at the root of the configuration.
func (m *ModuleInfo) ParentPath() modulepath.Path {
	parent, _, ok := m.Path.Parent()
	if !ok {
		return modulepath.Root()
	}
	return parent
}

// LoadedModule represents a loaded and parsed module (used by ModuleLoader).
type LoadedModule struct {
	Config     *ast.Config
	SourcePath string
}

// ModuleLoader loads module configurations from source paths. version is
// the `version` attribute on the module block (only meaningful for registry
// sources); workDir is the directory the source is relative to.
type ModuleLoader interface {
	LoadModule(source, version, workDir string) (*LoadedModule, error)
}

// NodeKey identifies a node: the static config path of its containing module
// (always keyless) plus the element id in scope-relative reference syntax
// (e.g. "var.x", "data.aws_ami.a", "module.<name>" for a completion node,
// which lives in the parent module's scope).
type NodeKey struct {
	Module modulepath.Path
	ID     string
}

func (k NodeKey) String() string {
	if k.Module.IsRoot() {
		return k.ID
	}
	return ModuleAddress(k.Module) + "." + k.ID
}

// TraversalKey resolves a reference traversal in the module at scope to its
// node key; ok is false for non-referencing traversals. A `module.<name>`
// reference names the call's completion node (which lives in scope itself);
// `module.<name>.<output>` resolves into the child module.
func TraversalKey(scope modulepath.Path, traversal hcl.Traversal) (NodeKey, bool) {
	namespace, parts := eval.ParseTraversal(traversal)
	switch namespace {
	case "", "path", "terraform", "count", "each", "self", "pulumi":
	case "data":
		if len(parts) >= 2 {
			return NodeKey{Module: scope, ID: "data." + parts[0] + "." + parts[1]}, true
		}
	case "module":
		if len(parts) >= 1 {
			if out := moduleOutputName(traversal); out != "" {
				return NodeKey{Module: scope.Append(modulepath.NewStep(parts[0])), ID: "output." + out}, true
			}
			return NodeKey{Module: scope, ID: "module." + parts[0]}, true
		}
	case "call":
		if len(parts) >= 2 {
			return NodeKey{Module: scope, ID: "call." + parts[0] + "." + parts[1]}, true
		}
	default: // var, local, and resource references share one shape.
		if len(parts) >= 1 {
			return NodeKey{Module: scope, ID: namespace + "." + parts[0]}, true
		}
	}
	return NodeKey{}, false
}

// Node represents a node in the dependency graph.
type Node struct {
	// Key is the unique identifier for this node
	Key NodeKey

	// Type indicates what kind of node this is
	Type NodeType

	// Resource is set for resource nodes
	Resource *ast.Resource

	// DataSource is set for data-source nodes
	DataSource *ast.DataSource

	// Local is set for local value nodes
	Local *ast.Local

	// Variable is set for variable nodes
	Variable *ast.Variable

	// Output is set for output nodes
	Output *ast.Output

	// Module is set for module nodes
	Module *ast.Module

	// Provider is set for provider nodes
	Provider *ast.Provider

	// Call is set for call nodes
	Call *ast.Call

	// ModuleInfo is set for nodes that belong to an inlined module.
	ModuleInfo *ModuleInfo

	// Deps is set for resource/data nodes: the same dependencies that back the
	// node's graph edges, kept in classified form so the engine's expansion
	// layer can wire them per module instance at instance granularity.
	Deps *BlockDeps

	// PlanTimeRead is set on data-source nodes whose transitive prerequisites
	// contain no resource: the read needs nothing a plan cannot know, so it
	// completes before any resource instance is created (the plan/apply phase
	// boundary). Deferred reads — those reaching a resource, directly or
	// through their module's expansion — are false.
	PlanTimeRead bool
}

// BlockDeps classifies a resource/data block's dependencies for the expansion
// layer. Static deps are graph nodes outside the block's own scope's
// resource/data blocks (variables, locals, providers, module init, …) and are
// wired as-is. Whole and Narrow name same-scope resource/data blocks by node
// key; the engine resolves them against the consumer's own module instance —
// Whole binds to the target's completion, Narrow to the gate of one instance.
type BlockDeps struct {
	Static []pdag.Node
	Whole  []NodeKey
	Narrow []InstanceKey
}

// NodeType indicates what type of configuration element a node represents.
type NodeType int

const (
	NodeTypeUnknown NodeType = iota
	NodeTypeVariable
	NodeTypeLocal
	NodeTypeResource
	NodeTypeDataSource
	NodeTypeModule
	NodeTypeOutput
	NodeTypeProvider
	NodeTypeBuiltin
	NodeTypeCall
	NodeTypeModuleInit
	NodeTypeVariableValidation
)

func (t NodeType) String() string {
	switch t {
	case NodeTypeVariable:
		return "variable"
	case NodeTypeLocal:
		return "local"
	case NodeTypeResource:
		return "resource"
	case NodeTypeDataSource:
		return "data"
	case NodeTypeModule:
		return "module"
	case NodeTypeOutput:
		return "output"
	case NodeTypeProvider:
		return "provider"
	case NodeTypeBuiltin:
		return "builtin"
	case NodeTypeCall:
		return "call"
	case NodeTypeModuleInit:
		return "module_init"
	case NodeTypeVariableValidation:
		return "variable_validation"
	default:
		return "unknown"
	}
}

// Graph represents a dependency graph of configuration elements.
type Graph struct {
	seen map[NodeKey]internedNode
	dag  *pdag.DAG[dagNode]

	// references records the source location(s) at which each node key was
	// referenced from a user-written traversal. Used by Validate to anchor
	// errors about unknown nodes back at the offending source.
	references map[NodeKey][]hcl.Range

	// keyByDagNode lets AddNode resolve each dependency's key so we can
	// track dependent counts. pdag only exposes node indices, not the
	// metadata we attached at creation time.
	keyByDagNode map[pdag.Node]NodeKey

	// dependents counts how many other nodes list a given key in their
	// dependency list. Read by HasDependents at Walk time.
	dependents map[NodeKey]int
	// initReads are the module-init edges deferred to the end of the build.
	initReads []initEdges

	// moved holds the moved blocks of each module keyed by that module's path.
	// A moved block's from/to addresses are relative to the module it is written
	// in, so resolving a rename needs the blocks scoped to the resource's own
	// module.
	moved map[modulepath.Path][]*ast.Moved

	// removed holds every removed block in the module tree, child-declared
	// addresses rewritten to be root-relative during inlining.
	removed []*ast.Removed

	// scopes maps a module's path to its scope, so expression-level dependency
	// extraction can resolve provider-defined function calls to the provider
	// block they use.
	scopes map[modulepath.Path]*moduleScope

	// missingProviders collects, keyed by provider address, the diagnostics
	// for module-call `providers` entries that name a provider configuration
	// the parent scope does not have. Reported by Validate.
	missingProviders map[string]*hcl.Diagnostic
}

// aspect is the scheduling role a dag node plays for its block: the block's
// own graph node, or one of the expansion-skeleton nodes serving it.
type aspect int

const (
	aspectBlock aspect = iota
	aspectExpand
	aspectComplete
	aspectGate
	aspectInstance
	aspectSync
)

// nodeDesc says what a dag node stands for, for diagnostics only — identity
// lives in the graph's interning maps. The zero value describes an anonymous
// node and renders empty.
type nodeDesc struct {
	block  NodeKey // the config block the node serves; zero for named synthetics
	aspect aspect
	index  string // instance suffix for gates/instances: `[0]`, `["x"]`
	name   string // for named synchronization points ("plan barrier")
}

func (d nodeDesc) String() string {
	if d.name != "" {
		return d.name
	}
	addr := d.block.String()
	if addr == "" {
		return ""
	}
	switch d.aspect {
	case aspectExpand:
		return addr + " (expand)"
	case aspectComplete:
		return addr + " (completion)"
	case aspectGate:
		return addr + d.index + " (gate)"
	case aspectInstance:
		return addr + d.index
	default:
		return addr
	}
}

type dagNode struct {
	desc nodeDesc
	exec func(context.Context) error
}

type internedNode struct {
	i pdag.Node
	n *Node
}

// NewGraph creates a new empty graph.
func NewGraph() *Graph {
	return &Graph{
		seen:         make(map[NodeKey]internedNode),
		dag:          pdag.New[dagNode](),
		references:   make(map[NodeKey][]hcl.Range),
		keyByDagNode: make(map[pdag.Node]NodeKey),
		dependents:   make(map[NodeKey]int),
		moved:        make(map[modulepath.Path][]*ast.Moved),
		scopes:       make(map[modulepath.Path]*moduleScope),

		missingProviders: make(map[string]*hcl.Diagnostic),
	}
}

// MovedBlocks returns the moved blocks declared in the module at path (the root
// module is modulepath.Root()). Their from/to addresses are relative to that
// module.
func (g *Graph) MovedBlocks(path modulepath.Path) []*ast.Moved {
	return g.moved[path]
}

// recordRef records that key was referenced from the given source range.
// Multiple references to the same key accumulate; this is used by Validate
// to anchor errors about unknown nodes back to the offending source.
func (g *Graph) recordRef(key NodeKey, rng hcl.Range) {
	g.references[key] = append(g.references[key], rng)
}

func (g *Graph) Walk(ctx context.Context, apply func(context.Context, *Node) error, parallel int) error {
	err := g.dag.Walk(ctx, func(ctx context.Context, n dagNode) error {
		if n.exec != nil {
			return n.exec(ctx)
		}
		node, ok := g.seen[n.desc.block]
		contract.Assertf(ok, "invalid graph - key not interned")
		return apply(ctx, node.n)
	}, pdag.MaxProcs(parallel))
	return dropSpuriousCancel(err)
}

// dropSpuriousCancel removes a top-level context.Canceled that the parallel
// walker joins onto a genuine failure. When a node fails, pdag.Walk cancels the
// walk's context and — depending on whether the drain loop is still mid-iteration
// when cancellation fires — non-deterministically joins the resulting
// context.Canceled onto the real error. That trailing cancellation is a
// scheduling artifact, not a distinct failure, so it is dropped whenever a
// genuine error remains; a walk that fails solely because its context was
// canceled still surfaces that.
func dropSpuriousCancel(err error) error {
	joined, ok := err.(interface{ Unwrap() []error })
	if !ok {
		return err
	}
	kept := make([]error, 0, len(joined.Unwrap()))
	for _, e := range joined.Unwrap() {
		if errors.Is(e, context.Canceled) {
			continue
		}
		kept = append(kept, e)
	}
	if len(kept) == 0 {
		return err
	}
	return errors.Join(kept...)
}

// InjectAfter injects a step to run after all nodes matching the predicate, and before any
// other node. This creates an inflection point in the graph.
func (g *Graph) InjectAfter(f func(context.Context) error, match func(*Node) bool) error {
	n, done := g.dag.NewNode(dagNode{exec: f})
	done()
	for _, node := range g.seen {
		var err error
		if match(node.n) {
			err = g.dag.NewEdge(node.i, n)
		} else {
			err = g.dag.NewEdge(n, node.i)
		}
		if err != nil {
			return cycleError(err)
		}
	}
	return nil
}

func (g *Graph) newNode(key NodeKey) (*Node, pdag.Node) {
	if n, ok := g.seen[key]; ok {
		contract.Assertf(n.n.Key == key, "key should not be changed")
		return n.n, n.i
	}
	i, done := g.dag.NewNode(dagNode{desc: nodeDesc{block: key}})
	n := &Node{Key: key}
	done() // We don't execute the graph as we build - so this is always safe
	g.seen[key] = internedNode{
		i: i,
		n: n,
	}
	g.keyByDagNode[i] = key
	return n, i
}

// AddNode adds a node to the graph.
func (g *Graph) AddNode(node *Node, deps []pdag.Node) error {
	n, i := g.newNode(node.Key)
	*n = *node
	for _, dep := range deps {
		err := cycleError(g.dag.NewEdge(dep, i))
		if err != nil {
			return err
		}
		if key, ok := g.keyByDagNode[dep]; ok {
			g.dependents[key]++
		}
	}
	return nil
}

// ForcedCreateBeforeDestroy returns the set of resource node keys that must be
// created before their prior instance is destroyed. A resource is included when
// it declares create_before_destroy, or when a resource that transitively
// depends on it does: the behaviour propagates to a resource's dependencies so
// that every create in a replacement chain runs before any delete.
func (g *Graph) ForcedCreateBeforeDestroy() map[NodeKey]bool {
	forced := make(map[NodeKey]bool)
	visited := make(map[pdag.Node]bool)

	var mark func(node pdag.Node)
	mark = func(node pdag.Node) {
		if visited[node] {
			return
		}
		visited[node] = true
		var n *Node
		if key, ok := g.keyByDagNode[node]; ok {
			if in, ok := g.seen[key]; ok {
				n = in.n
				if n.Type == NodeTypeResource {
					forced[key] = true
				}
			}
		}
		// Recurse into dependencies through any node type, so a dependency
		// reached via a local or other intermediary is still forced.
		for dep := range g.dag.Predecessors(node) {
			mark(dep)
		}
		// A node whose classified deps omit block-level edges (root locals)
		// still forces the blocks it reads.
		if n != nil && n.Deps != nil {
			for _, whole := range n.Deps.Whole {
				if in, ok := g.seen[whole]; ok {
					mark(in.i)
				}
			}
			for _, narrow := range n.Deps.Narrow {
				if in, ok := g.seen[narrow.Node]; ok {
					mark(in.i)
				}
			}
		}
	}

	for _, n := range g.seen {
		if declaresCreateBeforeDestroy(n.n) {
			mark(n.i)
		}
	}
	return forced
}

// declaresCreateBeforeDestroy reports whether a resource node sets
// create_before_destroy = true in its lifecycle block.
func declaresCreateBeforeDestroy(n *Node) bool {
	return n.Type == NodeTypeResource && n.Resource != nil &&
		n.Resource.Lifecycle != nil &&
		n.Resource.Lifecycle.CreateBeforeDestroy != nil &&
		*n.Resource.Lifecycle.CreateBeforeDestroy
}

// HasDependents reports whether any other node in the graph lists `key` in
// its dependency list. Used by the engine to skip work for nodes whose
// output nothing consumes (e.g. unused `provider` blocks).
func (g *Graph) HasDependents(key NodeKey) bool {
	return g.dependents[key] > 0
}

// KeyNode returns the dag node interned under key.
func (g *Graph) KeyNode(key NodeKey) (pdag.Node, bool) {
	n, ok := g.seen[key]
	return n.i, ok
}

// ExpandableNodes returns the nodes carrying classified deps — resource/data
// blocks the engine schedules through BlockExpansion cells, plus root locals
// it wires at instance granularity — sorted by key for deterministic
// materialization.
func (g *Graph) ExpandableNodes() []*Node {
	var out []*Node
	for _, n := range g.seen {
		if n.n.Deps != nil {
			out = append(out, n.n)
		}
	}
	slices.SortFunc(out, func(a, b *Node) int { return cmp.Compare(a.Key.String(), b.Key.String()) })
	return out
}

// initEdges is one module init node and the nodes its component registration
// reads, pending the cycle check that only a finished graph can answer.
type initEdges struct {
	init  pdag.Node
	reads []pdag.Node
}

// BuildFromConfig builds a dependency graph from an HCL configuration.
// moduleLoader is required when config contains modules.
func BuildFromConfig(config *ast.Config, moduleLoader ModuleLoader, workDir string) (*Graph, error) {
	g := NewGraph()
	root := modulepath.Root()
	g.moved[root] = config.Moved
	g.removed = slices.Clone(config.Removed)
	if err := checkRemovedStillExists(g.removed, root, config); err != nil {
		return nil, err
	}
	rootScope := &moduleScope{config: config}
	g.scopes[root] = rootScope

	contract.AssertNoErrorf(errors.Join(
		g.AddNode(&Node{
			Key:  NodeKey{ID: "pulumi.stack"},
			Type: NodeTypeBuiltin,
		}, nil),
		g.AddNode(&Node{
			Key:  NodeKey{ID: "pulumi.project"},
			Type: NodeTypeBuiltin,
		}, nil),
		g.AddNode(&Node{
			Key:  NodeKey{ID: "pulumi.organization"},
			Type: NodeTypeBuiltin,
		}, nil),
	), "nodes without dependencies cannot error")

	// Variable values come from outside, so a variable depends only on whatever
	// its validation rules reference (e.g. another variable).
	for name, v := range config.Variables {
		if err := g.addVariableNodes(NodeKey{ID: "var." + name}, v, nil, nil, root); err != nil {
			return nil, err
		}
	}

	// Add local value nodes. Root locals carry classified deps and no
	// block-level edges: the engine wires their resource/data dependencies at
	// instance granularity, so narrowness flows through them. (Module locals
	// keep block-level edges — references through them widen across module
	// instances.)
	for name, local := range config.Locals {
		bd, deps := g.localDeps(local, root)
		err := g.AddNode(&Node{
			Key:   NodeKey{ID: "local." + name},
			Type:  NodeTypeLocal,
			Local: local,
			Deps:  bd,
		}, deps)
		if err != nil {
			return nil, err
		}
	}

	// Add provider nodes (must come before resources since resources can reference them)
	for key, provider := range config.Providers {
		nodeKey := NodeKey{ID: key}
		deps := g.providerDeps(provider, nodeKey, root)
		err := g.AddNode(&Node{
			Key:      nodeKey,
			Type:     NodeTypeProvider,
			Provider: provider,
		}, deps)
		if err != nil {
			return nil, err
		}
	}

	// Add resource nodes
	for key, resource := range config.Resources {
		bd, deps := g.resourceDeps(resource, root)
		provDeps := g.defaultProviderDeps(resource.Provider, resource.Type, config, root)
		bd.Static = append(bd.Static, provDeps...)
		deps = append(deps, provDeps...)
		err := g.AddNode(&Node{
			Key:      NodeKey{ID: key},
			Type:     NodeTypeResource,
			Resource: resource,
			Deps:     bd,
		}, deps)
		if err != nil {
			return nil, err
		}
	}

	// Add data source nodes
	for key, dataSource := range config.DataSources {
		bd, deps := g.dataSourceDeps(dataSource, root)
		provDeps := g.defaultProviderDeps(dataSource.Provider, dataSource.Type, config, root)
		bd.Static = append(bd.Static, provDeps...)
		deps = append(deps, provDeps...)
		err := g.AddNode(&Node{
			Key:        NodeKey{ID: "data." + key},
			Type:       NodeTypeDataSource,
			DataSource: dataSource,
			Deps:       bd,
		}, deps)
		if err != nil {
			return nil, err
		}
	}

	// Inline module contents into the graph for fine-grained dependency tracking.
	for name, module := range config.Modules {
		if err := g.inlineModule(name, module, root, moduleLoader, workDir, rootScope); err != nil {
			return nil, fmt.Errorf("inlining module %s: %w", name, err)
		}
	}

	if err := checkDuplicateRemovedProvisioners(g.removed); err != nil {
		return nil, err
	}

	if err := g.addCallNodes(config, root, nil); err != nil {
		return nil, err
	}

	// Add output nodes
	for name, output := range config.Outputs {
		deps := g.outputDeps(output, root)
		err := g.AddNode(&Node{
			Key:    NodeKey{ID: "output." + name},
			Type:   NodeTypeOutput,
			Output: output,
		}, deps)
		if err != nil {
			return nil, err
		}
	}

	g.addInitReadEdges()
	g.classifyPlanTimeReads()

	return g, nil
}

// addInitReadEdges orders each module init after what its component
// registration reads, skipping any edge that would close a cycle. Mutually
// dependent modules are legal — the dependency runs from one module's outputs
// to the other's variables, not between the calls — and there the argument
// cannot be known that early, so init reports it unset.
func (g *Graph) addInitReadEdges() {
	// Two init edges can be individually acyclic and jointly cyclic, so which
	// one is refused depends on the order they are tried; modules are inlined
	// in map order, so sort before draining or the choice varies per run.
	slices.SortFunc(g.initReads, func(a, b initEdges) int {
		aN, bN := g.keyByDagNode[a.init], g.keyByDagNode[b.init]
		if mCmp := modulepath.Compare(aN.Module, bN.Module); mCmp != 0 {
			return mCmp
		}
		return cmp.Compare(aN.ID, bN.ID)
	})
	for _, e := range g.initReads {
		for _, dep := range e.reads {
			if err := g.dag.NewEdge(dep, e.init); err != nil {
				continue
			}
			if key, ok := g.keyByDagNode[dep]; ok {
				g.dependents[key]++
			}
		}
	}
}

// classifyPlanTimeReads marks each data-source node whose transitive
// prerequisites contain no resource node. Reachability runs over the
// block-level graph, so a data source inside a module whose expansion depends
// on a resource (through the module's init node) classifies as deferred.
func (g *Graph) classifyPlanTimeReads() {
	memo := make(map[pdag.Node]bool)
	var reachesResource func(n pdag.Node) bool
	reachesResource = func(n pdag.Node) bool {
		if v, ok := memo[n]; ok {
			return v
		}
		memo[n] = false
		var node *Node
		if in, ok := g.seen[g.keyByDagNode[n]]; ok {
			node = in.n
			if node.Type == NodeTypeResource {
				memo[n] = true
				return true
			}
		}
		for p := range g.dag.Predecessors(n) {
			if reachesResource(p) {
				memo[n] = true
				return true
			}
		}
		// A node whose classified deps omit block-level edges (root locals)
		// still reads the blocks those deps name.
		if node != nil && node.Deps != nil {
			for _, whole := range node.Deps.Whole {
				if in, ok := g.seen[whole]; ok && reachesResource(in.i) {
					memo[n] = true
					return true
				}
			}
			for _, narrow := range node.Deps.Narrow {
				if in, ok := g.seen[narrow.Node]; ok && reachesResource(in.i) {
					memo[n] = true
					return true
				}
			}
		}
		return false
	}
	for _, in := range g.seen {
		if in.n.Type == NodeTypeDataSource {
			in.n.PlanTimeRead = !reachesResource(in.i)
		}
	}
}

// internExecNode creates an exec node interned under key (as a Builtin, so
// pre-walk passes see it but Validate accepts it), leaving arming to the
// caller.
func (g *Graph) internExecNode(key NodeKey, desc nodeDesc, exec func(context.Context) error) (pdag.Node, pdag.Done) {
	i, done := g.dag.NewNode(dagNode{desc: desc, exec: exec})
	g.seen[key] = internedNode{i: i, n: &Node{Key: key, Type: NodeTypeBuiltin}}
	g.keyByDagNode[i] = key
	return i, done
}

// NewJoinNode creates an armed no-op node interned under key, for the engine
// to use as a synchronization point (edges added via Order). name is how the
// node reads in diagnostics.
func (g *Graph) NewJoinNode(key, name string) pdag.Node {
	i, done := g.internExecNode(NodeKey{ID: key}, nodeDesc{aspect: aspectSync, name: name},
		func(context.Context) error { return nil })
	done()
	return i
}

// Order adds the edge from → to.
func (g *Graph) Order(from, to pdag.Node) error {
	return cycleError(g.dag.NewEdge(from, to))
}

// cycleError rewraps a pdag cycle error to name the participating nodes in
// dependency order, closing the loop back on the first node. Anonymous nodes
// are dropped from the report; other errors pass through unchanged.
func cycleError(err error) error {
	var c pdag.ErrorCycle[dagNode]
	if !errors.As(err, &c) {
		return err
	}
	var parts []string
	for _, n := range c.Cycle {
		if s := n.desc.String(); s != "" {
			parts = append(parts, s)
		}
	}
	if len(parts) == 0 {
		return err
	}
	parts = append(parts, parts[0])
	return fmt.Errorf("dependency cycle: %s", strings.Join(parts, " -> "))
}

// defaultProviderDeps returns an implicit dependency on the un-aliased
// `provider "<pkg>" {}` block, if one exists, for resources/data sources that
// don't set `provider` explicitly. Without this the engine could process the
// resource before the default provider finishes registering — the provider
// block's config would then never make it into the resource.
func (g *Graph) defaultProviderDeps(provider hcl.Expression, typ string, config *ast.Config, path modulepath.Path) []pdag.Node {
	if provider != nil {
		return nil
	}
	pkgName := packageNameFromResourceType(typ)
	if pkgName == "" {
		return nil
	}
	if _, ok := config.Providers[pkgName]; !ok {
		return nil
	}
	_, idx := g.newNode(NodeKey{Module: path, ID: pkgName})
	return []pdag.Node{idx}
}

// moduleScope is an ancestor module's path and config, linked toward the root
// via parent. Scopes are immutable once built and shared by reference. mod and
// parentPath describe the module call that instantiated this scope (nil/root
// for the root), so pass-through provider references can resolve into the
// parent scope.
type moduleScope struct {
	path       modulepath.Path
	config     *ast.Config
	parent     *moduleScope
	mod        *ast.Module
	parentPath modulepath.Path
}

// inheritedProviderDeps returns an edge to the nearest ancestor's default
// `<pkg>` configuration (see defaultProviderNode) for an in-module
// resource/data source with no `provider`, no own-module block, and no
// pass-through, forcing that configuration to register before the resource.
func (g *Graph) inheritedProviderDeps(provider hcl.Expression, typ string, parent *moduleScope) []pdag.Node {
	if provider != nil {
		return nil
	}
	pkgName := packageNameFromResourceType(typ)
	if pkgName == "" {
		return nil
	}
	return g.defaultProviderNode(pkgName, parent)
}

// passThroughProviderDeps returns an edge from an in-module resource to the
// parent-scope provider that its module call's `providers = { ... }` argument
// passes in for the resource's package. scope is the resource's own module
// scope. Returns nil when no pass-through entry applies.
//
// The reference resolves strictly against the parent scope — provider
// configurations are never inherited into a `providers` argument: the parent
// must have a matching `provider` block, have received the configuration
// through its own module call, or (for an un-aliased reference on the root)
// declare the provider in `required_providers`, which stands for the implicit
// empty default configuration. Anything else is a missing provider, reported
// by Validate.
func (g *Graph) passThroughProviderDeps(provider hcl.Expression, typ string, scope *moduleScope) []pdag.Node {
	mod := scope.mod
	if mod == nil || len(mod.Providers) == 0 {
		return nil
	}
	// Look up by the key the resource would use in the child scope:
	//   explicit `provider = simple.foo` → "simple.foo"
	//   implicit default                  → "simple"
	var key string
	if provider != nil {
		key = providerExprKey(provider)
	} else {
		key = packageNameFromResourceType(typ)
	}
	if key == "" {
		return nil
	}
	passExpr, ok := mod.Providers[key]
	if !ok {
		return nil
	}
	return g.resolvePassedProvider(scope.config, key, passExpr, scope.parent)
}

// resolvePassedProvider returns an edge to the parent-scope provider
// configuration that a module call's `providers = { <childKey> = <passExpr> }`
// entry names, resolving strictly against parent: its `provider` block, the
// pass-through entry of its own module call (whose shadow node stands in), or
// — for an un-aliased reference on the root — its `required_providers`
// declaration, which stands for the implicit empty default configuration
// (nothing registers it, so there is no node to order after). Anything else
// records a missing-provider diagnostic for Validate. childConfig is the
// called module's config; its `required_providers` names the provider in the
// diagnostic.
func (g *Graph) resolvePassedProvider(
	childConfig *ast.Config, childKey string, passExpr hcl.Expression, parent *moduleScope,
) []pdag.Node {
	parentKey := providerExprKey(passExpr)
	if parentKey == "" {
		return nil
	}
	if _, ok := parent.config.Providers[parentKey]; ok {
		_, idx := g.newNode(NodeKey{Module: parent.path, ID: parentKey})
		return []pdag.Node{idx}
	}
	if parent.mod != nil {
		if _, ok := parent.mod.Providers[parentKey]; ok {
			_, idx := g.newNode(NodeKey{Module: parent.path, ID: parentKey})
			return []pdag.Node{idx}
		}
	}
	name, alias, aliased := strings.Cut(parentKey, ".")
	if !aliased && parent.parent == nil && parent.config.Terraform != nil {
		if _, ok := parent.config.Terraform.RequiredProviders[name]; ok {
			return nil
		}
	}
	addr := NodeKey{
		Module: parent.path,
		ID:     fmt.Sprintf("provider[%q]", ProviderFQN(childConfig.Terraform, strings.SplitN(childKey, ".", 2)[0])),
	}.String()
	if aliased {
		addr += "." + alias
	}
	g.recordMissingProvider(addr, passExpr.Range())
	return nil
}

// recordMissingProvider records a missing-provider diagnostic for Validate,
// deduplicated by provider address.
func (g *Graph) recordMissingProvider(addr string, rng hcl.Range) {
	if _, ok := g.missingProviders[addr]; ok {
		return
	}
	g.missingProviders[addr] = &hcl.Diagnostic{
		Severity: hcl.DiagError,
		Summary:  fmt.Sprintf("missing provider %s", addr),
		Subject:  rng.Ptr(),
	}
}

// ProviderFQN returns the fully-qualified address of the provider behind
// localName in the module whose `terraform` block is tfBlock: the
// `required_providers` source when declared, else the default namespace, both
// anchored at the default registry host.
func ProviderFQN(tfBlock *ast.Terraform, localName string) string {
	source := localName
	if tfBlock != nil {
		if req, ok := tfBlock.RequiredProviders[localName]; ok && req.Source != "" {
			source = req.Source
		}
	}
	switch parts := strings.Split(source, "/"); len(parts) {
	case 1:
		return "registry.opentofu.org/hashicorp/" + parts[0]
	case 2:
		return "registry.opentofu.org/" + source
	default:
		return source
	}
}

// LocalProviderName returns the name by which the module whose `terraform`
// block is tfBlock refers to the provider fqn. Provider configurations are
// shared across module boundaries by fully-qualified address, so a lookup
// crossing into another module must be re-keyed through this. fallback is
// returned when the module declares no name for fqn: an undeclared local name
// stands for itself.
func LocalProviderName(tfBlock *ast.Terraform, fqn, fallback string) string {
	if tfBlock == nil || ProviderFQN(tfBlock, fallback) == fqn {
		return fallback
	}
	local := ""
	for name := range tfBlock.RequiredProviders {
		if ProviderFQN(tfBlock, name) == fqn && (local == "" || name < local) {
			local = name
		}
	}
	if local == "" {
		return fallback
	}
	return local
}

// defaultProviderNode returns an edge to the node that registers the default
// (un-aliased) provider configuration `name` as seen from scope s: the
// nearest enclosing scope with a `provider "<name>"` block, or with a
// `providers = { <name> = ... }` entry on its own module call (whose shadow
// node stands in). Returns nil when no configuration exists anywhere — the
// reference then names the implicit empty default configuration, which needs
// no ordering edge.
func (g *Graph) defaultProviderNode(name string, s *moduleScope) []pdag.Node {
	if s == nil {
		return nil
	}
	fqn := ProviderFQN(s.config.Terraform, name)
	for ; s != nil; s = s.parent {
		local := LocalProviderName(s.config.Terraform, fqn, name)
		if _, ok := s.config.Providers[local]; ok {
			_, idx := g.newNode(NodeKey{Module: s.path, ID: local})
			return []pdag.Node{idx}
		}
		if s.mod != nil {
			if _, ok := s.mod.Providers[local]; ok {
				_, idx := g.newNode(NodeKey{Module: s.path, ID: local})
				return []pdag.Node{idx}
			}
		}
	}
	return nil
}

// providerExprKey returns "name" or "name.alias" from a provider-reference
// expression. Returns "" for anything that isn't a single one-or-two-step
// traversal.
func providerExprKey(expr hcl.Expression) string {
	if expr == nil {
		return ""
	}
	traversals := expr.Variables()
	if len(traversals) != 1 {
		return ""
	}
	t := traversals[0]
	if len(t) == 0 {
		return ""
	}
	name := t.RootName()
	if len(t) == 1 {
		return name
	}
	if attr, ok := t[1].(hcl.TraverseAttr); ok {
		return name + "." + attr.Name
	}
	return name
}

// packageNameFromResourceType mirrors run.packageNameFromResourceType: it
// extracts the provider package from an HCL resource type (e.g.
// "simple_resource" → "simple"). Duplicated here so graph stays free of run
// imports.
func packageNameFromResourceType(token string) string {
	return strings.SplitN(token, "_", 2)[0]
}

// traversalDepRefs resolves reference traversals (depends_on and friends) to
// depRefs, recording each reference; non-referencing traversals are skipped.
func (g *Graph) traversalDepRefs(path modulepath.Path, traversals ...hcl.Traversal) []depRef {
	var refs []depRef
	for _, t := range traversals {
		key, ok := TraversalKey(path, t)
		if !ok {
			continue
		}
		g.recordRef(key, t.SourceRange())
		refs = append(refs, depRef{key: key, traversal: t})
	}
	return refs
}

// resourceDeps extracts all dependencies from a resource declared in the
// module at path. It returns the block-level edge set for the node (unchanged
// coarse semantics: cycle detection, Validate, and completion ordering all
// stay block-granular) alongside the same dependencies in classified form for
// the engine's expansion layer. References in the body, count/for_each, and
// depends_on classify as whole-block or single-instance; every other
// dependency kind (provider refs, ResourceParent, DeletedWith, ReplaceWith,
// replace_triggered_by, aliases) stays static (block-granular).
func (g *Graph) resourceDeps(resource *ast.Resource, path modulepath.Path) (*BlockDeps, []pdag.Node) {
	c := g.newDepClassifier(path, true)

	c.classify(g.exprDepRefs(resource.Count, path, nil))
	c.classify(g.exprDepRefs(resource.ForEach, path, nil))
	c.classify(g.traversalDepRefs(path, resource.DependsOn...))
	if resource.Config != nil {
		c.classify(g.bodyDepRefs(resource.Config, path, nil))
	}

	c.addStaticRefs(g.traversalDepRefs(path, resource.ResourceParent))
	g.classifyProviderRef(c, resource.Provider, path)
	c.addStaticRefs(g.traversalDepRefs(path, resource.Providers...))
	c.addStaticRefs(g.traversalDepRefs(path, resource.DeletedWith))
	c.addStaticRefs(g.traversalDepRefs(path, resource.ReplaceWith...))
	if resource.Lifecycle != nil {
		for _, expr := range resource.Lifecycle.ReplaceTriggeredBy {
			c.addStaticRefs(g.exprDepRefs(expr, path, nil))
		}
	}
	c.addStaticRefs(g.exprDepRefs(resource.Aliases, path, nil))

	return c.finish()
}

// dataSourceDeps classifies a data block's dependencies: its meta-arguments,
// config body, and provider reference.
func (g *Graph) dataSourceDeps(ds *ast.DataSource, path modulepath.Path) (*BlockDeps, []pdag.Node) {
	c := g.newDepClassifier(path, true)

	c.classify(g.exprDepRefs(ds.Count, path, nil))
	c.classify(g.exprDepRefs(ds.ForEach, path, nil))
	c.classify(g.traversalDepRefs(path, ds.DependsOn...))
	if ds.Config != nil {
		c.classify(g.bodyDepRefs(ds.Config, path, nil))
	}

	c.addStaticRefs(g.traversalDepRefs(path, ds.ResourceParent))
	g.classifyProviderRef(c, ds.Provider, path)

	return c.finish()
}

// classifyProviderRef adds the static dependencies of a block's `provider`
// attribute. A bare `provider = name` reference (no alias) is a
// single-segment traversal, which exprDepRefs drops because it carries no
// attribute after the root. It names the default configuration, which
// resolves through inheritance and may be implicit, so order the block after
// whichever node registers it (if any). Aliased (`name.alias`) and call-based
// (`call.x.y`) provider expressions have further segments and are resolved by
// exprDepRefs.
func (g *Graph) classifyProviderRef(c *depClassifier, provider hcl.Expression, path modulepath.Path) {
	c.addStaticRefs(g.exprDepRefs(provider, path, nil))
	if provider == nil {
		return
	}
	if vars := provider.Variables(); len(vars) == 1 && len(vars[0]) == 1 {
		for _, dep := range g.defaultProviderNode(vars[0].RootName(), g.scopes[path]) {
			c.addStatic(dep)
		}
	}
}

// localDeps classifies a local value's dependencies. Unlike resourceDeps, the
// returned edge set omits same-scope resource/data blocks entirely: the
// engine wires those at instance granularity (completion or gate), so the
// local evaluates as soon as the instances it actually reads are available.
func (g *Graph) localDeps(local *ast.Local, path modulepath.Path) (*BlockDeps, []pdag.Node) {
	c := g.newDepClassifier(path, false)
	c.classify(g.exprDepRefs(local.Value, path, nil))
	return c.finish()
}

// depClassifier accumulates one block's dependencies into a BlockDeps and the
// pdag edge set for the block's graph node. blockEdges controls whether
// same-scope resource/data targets also get a block-level edge (resources
// keep them so completion ordering, cycle detection, and Validate stay
// block-granular; locals drop them so only the instance-level wiring orders
// the local's evaluation).
type depClassifier struct {
	g          *Graph
	path       modulepath.Path
	blockEdges bool
	bd         *BlockDeps
	seen       map[pdag.Node]bool
	staticSeen map[pdag.Node]bool
	wholeSeen  map[NodeKey]bool
	narrowSeen map[InstanceKey]bool
}

func (g *Graph) newDepClassifier(path modulepath.Path, blockEdges bool) *depClassifier {
	return &depClassifier{
		g:          g,
		path:       path,
		blockEdges: blockEdges,
		bd:         &BlockDeps{},
		seen:       make(map[pdag.Node]bool),
		staticSeen: make(map[pdag.Node]bool),
		wholeSeen:  make(map[NodeKey]bool),
		narrowSeen: make(map[InstanceKey]bool),
	}
}

func (c *depClassifier) addStatic(n pdag.Node) {
	c.seen[n] = true
	if !c.staticSeen[n] {
		c.staticSeen[n] = true
		c.bd.Static = append(c.bd.Static, n)
	}
}

func (c *depClassifier) addStaticRefs(refs []depRef) {
	for _, ref := range refs {
		_, n := c.g.newNode(ref.key)
		c.addStatic(n)
	}
}

func (c *depClassifier) classify(refs []depRef) {
	for _, ref := range refs {
		_, n := c.g.newNode(ref.key)
		id, sameScope := c.g.classifyDep(ref, c.path)
		if !sameScope {
			c.addStatic(n)
			continue
		}
		if c.blockEdges {
			c.seen[n] = true
		}
		if id.Suffix == "" {
			if !c.wholeSeen[ref.key] {
				c.wholeSeen[ref.key] = true
				c.bd.Whole = append(c.bd.Whole, ref.key)
			}
		} else if !c.narrowSeen[id] {
			c.narrowSeen[id] = true
			c.bd.Narrow = append(c.bd.Narrow, id)
		}
	}
}

// finish subsumes narrow entries under whole-block dependencies and returns
// the classified deps with the node's edge set.
func (c *depClassifier) finish() (*BlockDeps, []pdag.Node) {
	c.bd.Narrow = slices.DeleteFunc(c.bd.Narrow, func(d InstanceKey) bool { return c.wholeSeen[d.Node] })
	if len(c.bd.Narrow) == 0 {
		c.bd.Narrow = nil
	}
	return c.bd, slices.Collect(maps.Keys(c.seen))
}

// classifyDep reports whether ref names a resource/data block declared in the
// scope at path (sameScope), and if so which instance it addresses: a
// non-empty Suffix when the traversal indexes the block with a literal string
// or whole-number key, "" for a whole-block reference.
func (g *Graph) classifyDep(ref depRef, path modulepath.Path) (id InstanceKey, sameScope bool) {
	scope := g.scopes[path]
	if scope == nil || ref.traversal == nil || ref.key.Module != path {
		return InstanceKey{}, false
	}
	// The index step follows the two naming steps (type.name), or three for
	// data sources (data.type.name).
	idxPos := 2
	blockKey, isData := strings.CutPrefix(ref.key.ID, "data.")
	if isData {
		idxPos = 3
		if _, ok := scope.config.DataSources[blockKey]; !ok {
			return InstanceKey{}, false
		}
	} else if _, ok := scope.config.Resources[ref.key.ID]; !ok {
		return InstanceKey{}, false
	}
	return InstanceKey{Node: ref.key, Suffix: literalIndexSuffix(ref.traversal, idxPos)}, true
}

// literalIndexSuffix returns the instance-key suffix (`[0]`, `["x"]`) when the
// traversal step at idxPos indexes by a literal string or whole non-negative
// number, and "" otherwise — dynamic indexes, splats, and out-of-domain keys
// all fall back to whole-block granularity.
func literalIndexSuffix(t hcl.Traversal, idxPos int) string {
	if idxPos >= len(t) {
		return ""
	}
	idx, ok := t[idxPos].(hcl.TraverseIndex)
	if !ok || idx.Key.IsNull() || !idx.Key.IsKnown() {
		return ""
	}
	switch idx.Key.Type() {
	case cty.String:
		return fmt.Sprintf("[%q]", idx.Key.AsString())
	case cty.Number:
		i, acc := idx.Key.AsBigFloat().Int64()
		if acc != big.Exact || i < 0 {
			return ""
		}
		return fmt.Sprintf("[%d]", i)
	}
	return ""
}

// providerDeps extracts all dependencies from a provider block declared in the
// module at path. key is the block's own node key: a provider config that
// calls one of its own provider's functions would otherwise depend on itself.
func (g *Graph) providerDeps(provider *ast.Provider, key NodeKey, path modulepath.Path) []pdag.Node {
	seen := make(map[pdag.Node]bool)
	for _, dep := range g.exprDeps(provider.ForEach, path) {
		seen[dep] = true
	}
	if provider.Config != nil {
		for _, dep := range g.bodyDeps(provider.Config, path, nil) {
			seen[dep] = true
		}
	}
	_, self := g.newNode(key)
	delete(seen, self)
	return slices.Collect(maps.Keys(seen))
}

// bodyDeps extracts dependencies from an HCL body in the scope at path.
func (g *Graph) bodyDeps(body hcl.Body, path modulepath.Path, exclude map[string]bool) []pdag.Node {
	return g.refsToNodes(g.bodyDepRefs(body, path, exclude))
}

// propertyPathAttrs lists, per block type, the attributes whose entries are
// property paths of the enclosing resource rather than references to other
// nodes.
var propertyPathAttrs = map[string]map[string]bool{
	"lifecycle": {"ignore_changes": true},
	"pulumi":    {"hide_diffs": true, "replace_on_changes": true},
}

// bodyDepRefs extracts one depRef per referencing traversal in an HCL body.
func (g *Graph) bodyDepRefs(body hcl.Body, path modulepath.Path, exclude map[string]bool) []depRef {
	if eb, ok := body.(*ast.EscapedBody); ok {
		// Scan the underlying bodies so the native-syntax walk below still
		// sees dynamic and lifecycle blocks; the merged body hides them.
		return append(g.bodyDepRefs(eb.Base, path, exclude), g.bodyDepRefs(eb.Escape, path, exclude)...)
	}

	var refs []depRef

	attrs, _ := body.JustAttributes()
	for _, attr := range attrs {
		refs = append(refs, g.exprDepRefs(attr.Expr, path, exclude)...)
	}

	if syntaxBody, ok := body.(*hclsyntax.Body); ok {
		for _, block := range syntaxBody.Blocks {
			if block.Type == "dynamic" && len(block.Labels) > 0 {
				iterName := block.Labels[0]
				if iterAttr, ok := block.Body.Attributes["iterator"]; ok {
					if keyword := hcl.ExprAsKeyword(iterAttr.Expr); keyword != "" {
						iterName = keyword
					}
				}
				childExclude := make(map[string]bool, len(exclude)+1)
				maps.Copy(childExclude, exclude)
				childExclude[iterName] = true
				refs = append(refs, g.bodyDepRefs(block.Body, path, childExclude)...)
			} else if skip := propertyPathAttrs[block.Type]; skip != nil {
				// lifecycle and pulumi blocks contain attributes that hold
				// property paths (e.g. tags["env"]), not dependency
				// references. We must skip those attributes to avoid creating
				// spurious graph nodes.
				for attrName, attr := range block.Body.Attributes {
					if skip[attrName] {
						continue
					}
					refs = append(refs, g.exprDepRefs(attr.Expr, path, exclude)...)
				}
				// Still recurse into nested blocks (precondition, postcondition).
				for _, nested := range block.Body.Blocks {
					refs = append(refs, g.bodyDepRefs(nested.Body, path, exclude)...)
				}
			} else {
				refs = append(refs, g.bodyDepRefs(block.Body, path, exclude)...)
			}
		}
	}

	return refs
}

// outputDeps gathers an output node's dependencies: its value expression plus
// the condition and error-message expressions of every precondition, so an
// output is sequenced after everything its precondition references.
func (g *Graph) outputDeps(output *ast.Output, path modulepath.Path) []pdag.Node {
	deps := g.exprDeps(output.Value, path)
	for _, rule := range output.Preconditions {
		deps = append(deps, g.exprDeps(rule.Condition, path)...)
		deps = append(deps, g.exprDeps(rule.ErrorMessage, path)...)
	}
	return deps
}

// variableValidationDeps gathers the graph dependencies of a variable's
// `validation` rules — the condition and error-message expressions — minus a
// self-reference to the variable being validated, which is always in scope and
// would otherwise form a cycle.
func (g *Graph) variableValidationDeps(v *ast.Variable, key NodeKey, path modulepath.Path) []pdag.Node {
	_, self := g.newNode(key)
	var deps []pdag.Node
	for _, rule := range v.Validations {
		deps = append(deps, g.exprDeps(rule.Condition, path)...)
		deps = append(deps, g.exprDeps(rule.ErrorMessage, path)...)
	}
	return slices.DeleteFunc(deps, func(n pdag.Node) bool { return n == self })
}

// variableValueKeySuffix marks the internal value node of a variable whose
// validation rules were split onto a separate node. "!" cannot appear in an
// HCL identifier, so the key can never collide with a referenceable node.
const variableValueKeySuffix = "!value"

// addVariableNodes adds the graph node(s) for one variable declaration. key is
// the node key consumers reference (prefix + "var." + name); valueDeps are the
// dependencies of evaluating the variable's value (module init and the input
// expression from the calling module block).
//
// A validation rule may reference other objects — say a resource's computed
// output — and the rule must then run after that object, while the variable's
// value must stay available to everything ordered before it (such as nodes on
// the far side of an InjectAfter barrier). Keeping rules with such references
// on the value node would turn that ordering into a cycle, so they are split
// onto a NodeTypeVariableValidation node that owns the public key: consumers
// wait for the checks, and the value itself is evaluated by an internal value
// node that carries a copy of the declaration with the split rules removed.
func (g *Graph) addVariableNodes(key NodeKey, v *ast.Variable, modInfo *ModuleInfo, valueDeps []pdag.Node, path modulepath.Path) error {
	validationDeps := g.variableValidationDeps(v, key, path)
	if len(validationDeps) == 0 {
		return g.AddNode(&Node{
			Key:        key,
			Type:       NodeTypeVariable,
			Variable:   v,
			ModuleInfo: modInfo,
		}, valueDeps)
	}

	valueVar := *v
	valueVar.Validations = nil
	valueKey := NodeKey{Module: key.Module, ID: key.ID + variableValueKeySuffix}
	if err := g.AddNode(&Node{
		Key:        valueKey,
		Type:       NodeTypeVariable,
		Variable:   &valueVar,
		ModuleInfo: modInfo,
	}, valueDeps); err != nil {
		return err
	}
	_, valueIdx := g.newNode(valueKey)
	return g.AddNode(&Node{
		Key:        key,
		Type:       NodeTypeVariableValidation,
		Variable:   v,
		ModuleInfo: modInfo,
	}, append(validationDeps, valueIdx))
}

// depRef is one dependency occurrence extracted from an expression: the
// resolved node key plus the raw traversal it came from (nil for
// provider-function deps, which have no traversal).
type depRef struct {
	key       NodeKey
	traversal hcl.Traversal
}

// exprDeps extracts all dependencies from an expression in the scope at path.
func (g *Graph) exprDeps(expr hcl.Expression, path modulepath.Path) []pdag.Node {
	return g.refsToNodes(g.exprDepRefs(expr, path, nil))
}

// refsToNodes dedups refs by key and interns each as a graph node.
func (g *Graph) refsToNodes(refs []depRef) []pdag.Node {
	seen := make(map[NodeKey]bool, len(refs))
	var result []pdag.Node
	for _, ref := range refs {
		if seen[ref.key] {
			continue
		}
		seen[ref.key] = true
		_, n := g.newNode(ref.key)
		result = append(result, n)
	}
	return result
}

// exprDepRefs extracts one depRef per referencing traversal (no dedup, so a
// classifier can see each instance-keyed occurrence) in the scope at path.
func (g *Graph) exprDepRefs(expr hcl.Expression, path modulepath.Path, exclude map[string]bool) []depRef {
	if expr == nil {
		return nil
	}

	var refs []depRef

	for _, traversal := range expr.Variables() {
		namespace, _ := eval.ParseTraversal(traversal)
		if exclude[namespace] {
			continue
		}
		refs = append(refs, g.traversalDepRefs(path, traversal)...)
	}

	// A provider-defined function call routes through the provider block its
	// namespace resolves to, so order the caller after that block the same way
	// an implicit default-provider reference would.
	for _, providerName := range ast.ProviderFunctionCallsInExpr(expr) {
		if dep, ok := g.providerFunctionDep(path, providerName); ok {
			refs = append(refs, depRef{key: dep})
		}
	}

	return refs
}

// providerFunctionDep resolves the provider block a provider-defined function
// call in the scope at path routes through, mirroring the runtime's resolution
// order: the instantiating module call's pass-through providers, then the
// un-aliased provider block of the call's own module, then those of its
// ancestors. ok is false when no block is declared anywhere — the engine then
// falls back to the package's default provider, which needs no ordering edge.
func (g *Graph) providerFunctionDep(path modulepath.Path, providerName string) (NodeKey, bool) {
	for s := g.scopes[path]; s != nil; s = s.parent {
		if s.mod != nil {
			if passExpr, ok := s.mod.Providers[providerName]; ok {
				if parentKey := providerExprKey(passExpr); parentKey != "" {
					return NodeKey{Module: s.parentPath, ID: parentKey}, true
				}
			}
		}
		if _, ok := s.config.Providers[providerName]; ok {
			return NodeKey{Module: s.path, ID: providerName}, true
		}
	}
	return NodeKey{}, false
}

// moduleOutputName returns the output name from a `module.NAME[idx].OUTPUT`
// traversal (the index step is optional). Returns "" if there's no attribute
// step after the module name, meaning the reference is to the whole module.
func moduleOutputName(traversal hcl.Traversal) string {
	// traversal[0] is the root (`module`), traversal[1] is the module name.
	// The next step is either the output attr, or an index followed by the
	// output attr (for counted / for_each modules).
	i := 2
	if i < len(traversal) {
		if _, ok := traversal[i].(hcl.TraverseIndex); ok {
			i++
		}
	}
	if i < len(traversal) {
		if attr, ok := traversal[i].(hcl.TraverseAttr); ok {
			return attr.Name
		}
	}
	return ""
}

// Validate checks the graph for common issues.
func (g *Graph) Validate() []error {
	var errs []error

	// Sort keys for deterministic error ordering.
	keys := make([]NodeKey, 0, len(g.seen))
	for key, node := range g.seen {
		if node.n.Type == NodeTypeUnknown {
			keys = append(keys, key)
		}
	}
	slices.SortFunc(keys, func(a, b NodeKey) int { return cmp.Compare(a.String(), b.String()) })

	for _, key := range keys {
		diag := &hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  fmt.Sprintf("unknown node %q", key.String()),
		}
		if refs := g.references[key]; len(refs) > 0 {
			// Use the earliest reference (sorted by filename, then position)
			// as the subject so the error points at a deterministic location.
			subject := refs[0]
			for _, r := range refs[1:] {
				if rangeLess(r, subject) {
					subject = r
				}
			}
			diag.Subject = subject.Ptr()
		}
		errs = append(errs, diag)
	}

	for _, addr := range slices.Sorted(maps.Keys(g.missingProviders)) {
		errs = append(errs, g.missingProviders[addr])
	}

	return errs
}

// checkRemovedStillExists reports a removed block whose root-relative address
// is still declared by the configuration at path. A target nested deeper than
// this module is checked when its own module is inlined; a target whose
// module is gone is never reached, which is exactly a valid removal.
func checkRemovedStillExists(removed []*ast.Removed, path modulepath.Path, config *ast.Config) error {
	for _, rem := range removed {
		if rem.From.Module != path {
			continue
		}
		if rem.From.Type == "" {
			return fmt.Errorf("removed block for %s: this module block still exists in the configuration", rem.From)
		}
		if _, exists := config.Resources[ast.ResourceKey(rem.From.Type, rem.From.Name)]; exists {
			return fmt.Errorf("removed block for %s: this resource block still exists in the configuration", rem.From)
		}
	}
	return nil
}

// checkDuplicateRemovedProvisioners rejects two provisioner-carrying removed
// blocks for one root-relative address: two provisioner sets for one orphan
// would be ambiguous at destroy time.
func checkDuplicateRemovedProvisioners(removed []*ast.Removed) error {
	withProvisioners := map[string]hcl.Range{}
	for _, rem := range removed {
		if len(rem.Provisioners) == 0 {
			continue
		}
		addr := rem.From.String()
		if prev, ok := withProvisioners[addr]; ok {
			return fmt.Errorf(
				"duplicate removed block for %s: a removed block with provisioners for this address was already declared at %s; declare all of the address's provisioners in one removed block",
				addr, prev)
		}
		withProvisioners[addr] = rem.DeclRange
	}
	return nil
}

// Removed returns every removed block in the module tree, with child-declared
// addresses rewritten to be root-relative.
func (g *Graph) Removed() []*ast.Removed { return g.removed }

// rangeLess orders ranges by filename, then start line, then start column.
func rangeLess(a, b hcl.Range) bool {
	if a.Filename != b.Filename {
		return a.Filename < b.Filename
	}
	if a.Start.Line != b.Start.Line {
		return a.Start.Line < b.Start.Line
	}
	return a.Start.Column < b.Start.Column
}

// inlineModule loads a module and inlines its contents into the graph
// rooted at parentPath.
func (g *Graph) inlineModule(
	name string, mod *ast.Module, parentPath modulepath.Path,
	moduleLoader ModuleLoader, workDir string, parent *moduleScope,
) error {
	loaded, err := moduleLoader.LoadModule(mod.Source, mod.Version, workDir)
	if err != nil {
		return fmt.Errorf("loading module %s: %w", name, err)
	}

	path := parentPath.Append(modulepath.NewStep(name))
	// Rewrite child-declared removed addresses to root-relative before
	// validating, so the child's own declarations are checked against its
	// configuration along with any ancestor's.
	for _, rem := range loaded.Config.Removed {
		abs := *rem
		abs.From.Module = path.Join(rem.From.Module)
		g.removed = append(g.removed, &abs)
	}
	if err := checkRemovedStillExists(g.removed, path, loaded.Config); err != nil {
		return err
	}
	g.moved[path] = loaded.Config.Moved
	scope := &moduleScope{
		path:       path,
		config:     loaded.Config,
		parent:     parent,
		mod:        mod,
		parentPath: parentPath,
	}
	g.scopes[path] = scope
	modInfo := &ModuleInfo{
		Path:             path,
		Module:           mod,
		SourcePath:       loaded.SourcePath,
		Terraform:        loaded.Config.Terraform,
		ParentSourcePath: workDir,
	}

	// Init node: depends on count/for_each/depends_on from parent scope,
	// plus the parent module's init (so a nested module never initializes
	// before its enclosing module's instance/eval-context is registered).
	initKey := NodeKey{Module: path, ID: "__init__"}
	var initDeps []pdag.Node
	if !parentPath.IsRoot() {
		_, parentInitIdx := g.newNode(NodeKey{Module: parentPath, ID: "__init__"})
		initDeps = append(initDeps, parentInitIdx)
	}
	initDeps = append(initDeps, g.exprDeps(mod.Count, parentPath)...)
	initDeps = append(initDeps, g.exprDeps(mod.ForEach, parentPath)...)
	initDeps = append(initDeps, g.refsToNodes(g.traversalDepRefs(parentPath, mod.DependsOn...))...)
	// The module's component registration records the providers passed in
	// via `providers = {...}`, so init must run after those parent-scope
	// configurations are registered.
	for localKey, passExpr := range mod.Providers {
		initDeps = append(initDeps, g.resolvePassedProvider(loaded.Config, localKey, passExpr, parent)...)
	}
	if err := g.AddNode(&Node{
		Key:        initKey,
		Type:       NodeTypeModuleInit,
		Module:     mod,
		ModuleInfo: modInfo,
	}, initDeps); err != nil {
		return err
	}

	_, initIdx := g.newNode(initKey)

	// Init evaluates the call's arguments, name and protect flag to register
	// the component, so it should follow whatever they read. Recorded now,
	// added once the graph is whole: an edge that closes a cycle is one the
	// program cannot satisfy, and only the finished graph shows that.
	initReads := append(g.exprDeps(mod.PulumiName, parentPath), g.exprDeps(mod.Protect, parentPath)...)
	moduleInputAttrs, _ := mod.Config.JustAttributes()
	for _, name := range slices.Sorted(maps.Keys(moduleInputAttrs)) {
		initReads = append(initReads, g.exprDeps(moduleInputAttrs[name].Expr, parentPath)...)
	}
	g.initReads = append(g.initReads, initEdges{init: initIdx, reads: initReads})

	// Variables: each depends on init + the corresponding input expression from the module block.
	for varName, v := range loaded.Config.Variables {
		varDeps := []pdag.Node{initIdx}
		if inputAttr, ok := moduleInputAttrs[varName]; ok {
			varDeps = append(varDeps, g.exprDeps(inputAttr.Expr, parentPath)...)
		}
		if err := g.addVariableNodes(NodeKey{Module: path, ID: "var." + varName}, v, modInfo, varDeps, path); err != nil {
			return err
		}
	}

	// Locals
	for localName, local := range loaded.Config.Locals {
		deps := g.exprDeps(local.Value, path)
		deps = append(deps, initIdx)
		if err := g.AddNode(&Node{
			Key:        NodeKey{Module: path, ID: "local." + localName},
			Type:       NodeTypeLocal,
			Local:      local,
			ModuleInfo: modInfo,
		}, deps); err != nil {
			return err
		}
	}

	// Providers
	for key, provider := range loaded.Config.Providers {
		nodeKey := NodeKey{Module: path, ID: key}
		deps := g.providerDeps(provider, nodeKey, path)
		deps = append(deps, initIdx)
		if err := g.AddNode(&Node{
			Key:        nodeKey,
			Type:       NodeTypeProvider,
			Provider:   provider,
			ModuleInfo: modInfo,
		}, deps); err != nil {
			return err
		}
	}

	// Pass-through aliases: `providers = { simple.foo = simple.from_parent }`
	// creates a local name `simple.foo` in the child that has no in-module
	// `provider` block of its own. In-module resources referencing it would
	// otherwise leave behind an unresolved node and trip Validate. Register
	// it as a no-op shadow so the local edge resolves; the real resolution
	// (to the parent's provider) happens at runtime via mod.Providers. The
	// shadow depends on the parent-scope configuration it stands for, so that
	// a chain of pass-throughs stays ordered after the originating block —
	// and so every entry is checked against the parent scope, whether or not
	// anything in the child consumes it.
	for localKey, passExpr := range mod.Providers {
		deps := g.resolvePassedProvider(loaded.Config, localKey, passExpr, parent)
		shadowKey := NodeKey{Module: path, ID: localKey}
		if existing, ok := g.seen[shadowKey]; ok && existing.n.Type != NodeTypeUnknown {
			continue
		}
		if err := g.AddNode(&Node{
			Key:        shadowKey,
			Type:       NodeTypeBuiltin,
			ModuleInfo: modInfo,
		}, deps); err != nil {
			return err
		}
	}

	// Resources
	for key, resource := range loaded.Config.Resources {
		bd, deps := g.resourceDeps(resource, path)
		provDeps := g.defaultProviderDeps(resource.Provider, resource.Type, loaded.Config, path)
		passDeps := g.passThroughProviderDeps(resource.Provider, resource.Type, scope)
		provDeps = append(provDeps, passDeps...)
		if len(provDeps) == 0 {
			provDeps = g.inheritedProviderDeps(resource.Provider, resource.Type, parent)
		}
		bd.Static = append(bd.Static, initIdx)
		bd.Static = append(bd.Static, provDeps...)
		deps = append(deps, initIdx)
		deps = append(deps, provDeps...)
		if err := g.AddNode(&Node{
			Key:        NodeKey{Module: path, ID: key},
			Type:       NodeTypeResource,
			Resource:   resource,
			ModuleInfo: modInfo,
			Deps:       bd,
		}, deps); err != nil {
			return err
		}
	}

	// Data sources
	for key, ds := range loaded.Config.DataSources {
		bd, deps := g.dataSourceDeps(ds, path)
		provDeps := g.defaultProviderDeps(ds.Provider, ds.Type, loaded.Config, path)
		passDeps := g.passThroughProviderDeps(ds.Provider, ds.Type, scope)
		provDeps = append(provDeps, passDeps...)
		if len(provDeps) == 0 {
			provDeps = g.inheritedProviderDeps(ds.Provider, ds.Type, parent)
		}
		bd.Static = append(bd.Static, initIdx)
		bd.Static = append(bd.Static, provDeps...)
		deps = append(deps, initIdx)
		deps = append(deps, provDeps...)
		if err := g.AddNode(&Node{
			Key:        NodeKey{Module: path, ID: "data." + key},
			Type:       NodeTypeDataSource,
			DataSource: ds,
			ModuleInfo: modInfo,
			Deps:       bd,
		}, deps); err != nil {
			return err
		}
	}

	// Outputs
	for outputName, output := range loaded.Config.Outputs {
		deps := g.outputDeps(output, path)
		deps = append(deps, initIdx)
		if err := g.AddNode(&Node{
			Key:        NodeKey{Module: path, ID: "output." + outputName},
			Type:       NodeTypeOutput,
			Output:     output,
			ModuleInfo: modInfo,
		}, deps); err != nil {
			return err
		}
	}

	// Completion node: depends on init plus everything the module declares —
	// its resources, data sources, outputs, and nested module completions. The
	// completion key is the module call's identifier (without the trailing "."),
	// so that a whole-module reference like `module.<name>` in the parent scope
	// (an output-less `depends_on`, in particular) waits for the module's entire
	// contents, not only the resources that happen to feed an output. Node keys
	// for the nested modules are forward references resolved once they are
	// inlined below.
	completionKey := NodeKey{Module: parentPath, ID: "module." + name}
	completionDeps := []pdag.Node{initIdx}
	for key := range loaded.Config.Resources {
		_, idx := g.newNode(NodeKey{Module: path, ID: key})
		completionDeps = append(completionDeps, idx)
	}
	for key := range loaded.Config.DataSources {
		_, idx := g.newNode(NodeKey{Module: path, ID: "data." + key})
		completionDeps = append(completionDeps, idx)
	}
	for outputName := range loaded.Config.Outputs {
		_, idx := g.newNode(NodeKey{Module: path, ID: "output." + outputName})
		completionDeps = append(completionDeps, idx)
	}
	for nestedName := range loaded.Config.Modules {
		_, idx := g.newNode(NodeKey{Module: path, ID: "module." + nestedName})
		completionDeps = append(completionDeps, idx)
	}
	if err := g.AddNode(&Node{
		Key:        completionKey,
		Type:       NodeTypeModule,
		Module:     mod,
		ModuleInfo: modInfo,
	}, completionDeps); err != nil {
		return err
	}

	if err := g.addCallNodes(loaded.Config, path, modInfo); err != nil {
		return err
	}

	// Nested modules
	for nestedName, nestedMod := range loaded.Config.Modules {
		if err := g.inlineModule(nestedName, nestedMod, path, moduleLoader, loaded.SourcePath, scope); err != nil {
			return fmt.Errorf("inlining nested module %s: %w", nestedName, err)
		}
	}

	return nil
}

// addCallNodes adds call nodes from config into the graph in the scope at path.
func (g *Graph) addCallNodes(config *ast.Config, path modulepath.Path, modInfo *ModuleInfo) error {
	for key, call := range config.Calls {
		callKey := NodeKey{Module: path, ID: "call." + key}
		var deps []pdag.Node
		for resKey, res := range config.Resources {
			if res.Name == call.ResourceName {
				_, idx := g.newNode(NodeKey{Module: path, ID: resKey})
				deps = append(deps, idx)
				break
			}
		}
		if _, exists := config.Providers[call.ResourceName]; exists {
			_, idx := g.newNode(NodeKey{Module: path, ID: call.ResourceName})
			deps = append(deps, idx)
		}
		if call.Config != nil {
			deps = append(deps, g.bodyDeps(call.Config, path, nil)...)
		}
		if err := g.AddNode(&Node{
			Key:        callKey,
			Type:       NodeTypeCall,
			Call:       call,
			ModuleInfo: modInfo,
		}, deps); err != nil {
			return err
		}
	}
	return nil
}
