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
	"strings"
	"sync"

	"github.com/hashicorp/hcl/v2"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource/urn"
	"github.com/zclconf/go-cty/cty"

	"github.com/pulumi/pulumi-hcl/pkg/hcl/ast"
	"github.com/pulumi/pulumi-hcl/pkg/hcl/eval"
	"github.com/pulumi/pulumi-hcl/pkg/hcl/modulepath"
	"github.com/pulumi/pulumi-hcl/pkg/hcl/packages"
	"github.com/pulumi/pulumi-hcl/pkg/provisioner/runtime"
)

// destroyProvisionerHook is the single BeforeDelete hook name bound on every
// instance with destroy-time provisioners. A name recorded in state must
// still be registered on every later run — including runs where the instance
// or its whole block is gone — so it carries no qualifier; the callback
// dispatches by URN instead.
const destroyProvisionerHook = "pulumi-hcl:provisioner:before-delete"

// DestroyDispatcher is the per-deployment dispatch table behind
// [destroyProvisionerHook], owned by whoever knows deployment identity: the
// langhost (one per Run) or the MLC provider (one per monitor endpoint,
// shared by that deployment's Constructs).
type DestroyDispatcher struct {
	mu         sync.RWMutex
	registered bool
	// instances holds each registered instance's runner; later insertion
	// overwrites.
	instances map[string]ResourceHookFunction
	// blocks serve orphans whose block is still in config (count = 0, a
	// dropped for_each key, a module count shrink); recorded even when the
	// block expands to zero instances.
	blocks []*blockEntry
}

func NewDestroyDispatcher() *DestroyDispatcher {
	return &DestroyDispatcher{instances: map[string]ResourceHookFunction{}}
}

func (t *DestroyDispatcher) claimRegistration() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.registered {
		return false
	}
	t.registered = true
	return true
}

func (t *DestroyDispatcher) putInstance(urn string, run ResourceHookFunction) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.instances[urn] = run
}

func (t *DestroyDispatcher) putBlock(entry *blockEntry) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.blocks = append(t.blocks, entry)
}

// dispatch resolves an invocation by URN, else by unique block match. No
// match means the block is gone from the program: the delete proceeds without
// provisioners, TF's semantics. Ambiguity no-ops with a warning — running the
// wrong command is worse than running none.
func (t *DestroyDispatcher) dispatch(ctx context.Context, args *ResourceHookArgs) error {
	t.mu.RLock()
	inst := t.instances[args.URN]
	blocks := t.blocks
	t.mu.RUnlock()

	if inst != nil {
		return inst(ctx, args)
	}

	var match *blockEntry
	matched := 0
	for _, b := range blocks {
		if b.matches(args) {
			match, matched = b, matched+1
			if matched == 2 {
				break
			}
		}
	}
	switch matched {
	case 0:
		return nil
	case 1:
		return match.run(ctx, args)
	default:
		_ = match.resmon.LogWarning(ctx, fmt.Sprintf(
			"%s: destroy-time provisioners skipped: multiple configuration blocks match this orphaned instance",
			args.URN))
		return nil
	}
}

// registerDestroyDispatcher registers the constant hook once per deployment.
// Failure warns and continues: it can only mean another pulumi-hcl process
// holds the name, and a no-op there is better than bricking the delete.
func (e *Engine) registerDestroyDispatcher(ctx context.Context) {
	if e.resmon == nil {
		return
	}
	if !e.dispatcher.claimRegistration() {
		return
	}
	if err := e.resmon.RegisterResourceHook(ctx, destroyProvisionerHook, e.dispatcher.dispatch, ResourceHookOptions{
		// The prevent_destroy guard must refuse at plan time, so the hook
		// fires during previews; provisioner entries gate themselves on the
		// deployment's dry-run flag instead.
		OnDryRun: true,
	}); err != nil {
		_ = e.resmon.LogWarning(ctx, fmt.Sprintf(
			"registering the destroy-time provisioner hook: %v", err))
	}
}

// blockEntry runs a still-configured block's destroy provisioners for an
// orphaned instance, matched by static config address.
type blockEntry struct {
	resmon ResourceMonitor
	dryRun bool
	res    *ast.Resource
	bound  *packages.BoundResource

	// The token of bound.Schema and parentChain pin the orphan URN's type and
	// parent-type chain: a name shape alone collides across types and across
	// dynamic modules, which share one component token.
	parentChain string
	// prefix is the monitor's name prefix (the component name on the MLC
	// path), stripped from args.Name before shape matching.
	prefix string
	config modulepath.Address
	// modInstName and evalCtx are the live module instance that recorded the
	// entry; an orphan naming it exactly evaluates in its scope.
	modInstName string
	evalCtx     *eval.Context
	// overridden: a live `pulumi { name = ... }` override makes the derived
	// name shape non-invertible, so the entry never matches by shape.
	overridden bool
	// moduleTarget: the address descends into modules, so matching skips the
	// parent-chain check — a gone module call's component type names its
	// source, which is no longer in the configuration.
	moduleTarget bool
	// preventDestroy refuses the orphan's delete: the block is still in
	// configuration with its lifecycle guard set (a count shrink or dropped
	// for_each key), so its instances may not be destroyed. A known-null guard
	// errors the delete instead.
	preventDestroy preventDestroyGuard
	// guardErr is a prevent_destroy evaluation failure, surfaced when an
	// orphan of this block is actually deleted: a guard that cannot be
	// evaluated must refuse the delete, not silently allow it.
	guardErr error
}

// urnTypes returns u's own type and the qualified-type chain contributed by
// its parents (with its trailing "$", or "" at the root).
func urnTypes(u string) (typ, parentChain string) {
	typ = string(urn.URN(u).Type())
	return typ, strings.TrimSuffix(string(urn.URN(u).QualifiedType()), typ)
}

func (b *blockEntry) matches(args *ResourceHookArgs) bool {
	if b.overridden {
		return false
	}
	typ, parentChain := urnTypes(args.URN)
	if typ != b.bound.Schema.Token || (!b.moduleTarget && parentChain != b.parentChain) {
		return false
	}
	name := args.Name
	if b.prefix != "" {
		var ok bool
		if name, ok = strings.CutPrefix(name, b.prefix); !ok {
			return false
		}
	}
	addr, err := modulepath.ParseAddress(name)
	if err != nil {
		return false
	}
	return addr.InstanceOf(b.config)
}

// run evaluates the block's destroy provisioners for an orphan, in the
// recording module instance's scope when the orphan names it exactly, else in
// TF's destroy scope (self, count.index, each.key, path.*, terraform.*). In
// the latter, an out-of-scope reference warns and no-ops: a BeforeDelete
// error would make the resource undeletable.
func (b *blockEntry) run(ctx context.Context, args *ResourceHookArgs) error {
	name := strings.TrimPrefix(args.Name, b.prefix)
	if b.guardErr != nil {
		return b.guardErr
	}
	switch b.preventDestroy {
	case preventDestroyRefuse:
		return preventDestroyRefusal(name)
	case preventDestroyNull:
		return preventDestroyNullRefusal(name)
	}
	if b.dryRun {
		// Provisioners never run during plan.
		return nil
	}
	addr, err := modulepath.ParseAddress(name)
	if err != nil {
		return nil
	}
	leaf := addr.Resource()

	var index *int
	if i, ok := leaf.Index(); ok {
		index = &i
	} else if b.res.Count != nil {
		// A bool-derived count stores no index where TF supplies 0.
		zero := 0
		index = &zero
	}
	var key *string
	if k, ok := leaf.Key(); ok {
		key = &k
	}

	var hclCtx *hcl.EvalContext
	strict := false
	if addr.Module().LogicalName() == b.modInstName {
		var keyVal *cty.Value
		if key != nil {
			v := cty.StringVal(*key)
			keyVal = &v
		}
		hclCtx = b.evalCtx.HCLContextWithIteration(index, keyVal, nil)
	} else {
		hclCtx = b.tfDestroyScope(index, key)
		strict = true
	}

	outputs := args.OldOutputs
	// The registration-time trigger tuple is gone with the instance, so
	// triggers_replace stays absent rather than reporting a fabricated value.
	if b.res.Type == packages.TerraformDataType {
		outputs = outputs.Set("output", outputs.Get("input"))
	}
	selfCtx, err := selfBoundEvalCtx(hclCtx, outputs, args.ID, args.URN, b.res.Type, b.bound, b.dryRun)
	if err != nil {
		return fmt.Errorf("destroy provisioners for %s.%s: %w", b.res.Type, b.res.Name, err)
	}

	for i, prov := range b.res.Provisioners {
		if prov.When != "destroy" {
			continue
		}
		spec := &runtime.Spec{
			Type:   prov.Type,
			Config: prov.Config,
			Conn:   effectiveConnectionBody(prov, b.res),
		}
		if strict {
			if refErr := checkBodyReferences(selfCtx, spec.Config, spec.Conn); refErr != nil {
				_ = b.resmon.LogWarning(ctx, fmt.Sprintf(
					"%s: destroy-time provisioner %d skipped: %v (only self, count.index, each.key, "+
						"path.* and terraform.* are in scope for an instance of a no-longer-live module instance)",
					args.URN, i+1, refErr))
				continue
			}
		}
		if runErr := runtime.Run(ctx, spec, selfCtx); runErr != nil {
			if prov.OnFailure == "continue" {
				continue
			}
			return fmt.Errorf("provisioner %d (%s) for %s.%s: %w", i+1, prov.Type, b.res.Type, b.res.Name, runErr)
		}
	}
	return nil
}

// tfDestroyScope builds TF's destroy-time scope; path.* and terraform.* are
// static properties of the block, copied from the recording context.
func (b *blockEntry) tfDestroyScope(index *int, key *string) *hcl.EvalContext {
	vars := map[string]cty.Value{}
	parent := b.evalCtx.HCLContext()
	for _, name := range []string{"path", "terraform"} {
		if v, diags := (hcl.Traversal{hcl.TraverseRoot{Name: name}}).TraverseAbs(parent); !diags.HasErrors() {
			vars[name] = v
		}
	}
	if index != nil {
		vars["count"] = cty.ObjectVal(map[string]cty.Value{"index": cty.NumberIntVal(int64(*index))})
	}
	if key != nil {
		vars["each"] = cty.ObjectVal(map[string]cty.Value{"key": cty.StringVal(*key)})
	}
	return &hcl.EvalContext{Variables: vars, Functions: parent.Functions}
}

// checkBodyReferences detects an out-of-scope reference before any command in
// the provisioner runs.
func checkBodyReferences(ctx *hcl.EvalContext, bodies ...hcl.Body) error {
	for _, body := range bodies {
		for _, traversal := range bodyTraversals(body) {
			if _, diags := traversal.TraverseAbs(ctx); diags.HasErrors() {
				return fmt.Errorf("%s", diags.Error())
			}
		}
	}
	return nil
}

// recordBlockEntry records the dispatch entry for one resource block × live
// module instance, whether or not the block expands to any instances.
func (e *Engine) recordBlockEntry(
	res *ast.Resource, bound *packages.BoundResource,
	evalCtx *eval.Context, parentURN urn.URN, modInst *moduleInstance,
) {
	// Per-instance symbols are rejected by evalPreventDestroy, so the block's
	// module scope evaluates the guard the same way the live-instance path does.
	guard, guardErr := evalPreventDestroy(res, evalCtx.HCLContext())
	if guardErr == nil && guard == preventDestroyAllow && !hasDestroyProvisioners(res) {
		return
	}

	probeURN, probeName := e.resmon.ResolveURN(parentURN, bound.Schema.Token, "probe")
	prefix := strings.TrimSuffix(probeName, "probe")

	entry := &blockEntry{
		resmon:         e.resmon,
		dryRun:         e.dryRun,
		res:            res,
		bound:          bound,
		prefix:         prefix,
		evalCtx:        evalCtx,
		preventDestroy: guard,
		guardErr:       guardErr,
	}
	_, entry.parentChain = urnTypes(string(probeURN))
	modConfig := modulepath.Root()
	if modInst != nil {
		entry.modInstName = modInst.Name
		modConfig = modInst.Path.Config()
		if modInst.Name != modInst.Path.LogicalName() {
			entry.overridden = true
		}
	}
	entry.config = modulepath.NewAddress(modConfig, modulepath.NewStep(res.Name))
	if res.PulumiName != nil {
		// An override that needs per-instance scope errors here; treat it as
		// live rather than guess.
		_, live, err := evaluatePulumiName(res.PulumiName, evalCtx.HCLContext(), res.Type+"."+res.Name)
		if err != nil || live {
			entry.overridden = true
		}
	}

	e.dispatcher.putBlock(entry)
}

func hasDestroyProvisioners(res *ast.Resource) bool {
	for _, prov := range res.Provisioners {
		if prov.When == "destroy" {
			return true
		}
	}
	return false
}
