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

	"github.com/pulumi/pulumi-hcl/pkg/hcl/ast"
	"github.com/pulumi/pulumi-hcl/pkg/hcl/modulepath"
)

// recordRemovedBlockEntries records a destroy-dispatcher entry for each
// removed block in the module tree that carries provisioners, so the
// provisioners run when the engine deletes the orphaned instances. A resource
// whose state never recorded [destroyProvisionerHook] — it had no
// destroy-time provisioners when last registered — deletes without running
// the removed block's provisioners.
func (e *Engine) recordRemovedBlockEntries(ctx context.Context) error {
	if e.resmon == nil {
		return nil
	}
	for _, rem := range e.graph.Removed() {
		// A destroy = false block already carries a parse-time error
		// diagnostic; its provisioners must not run.
		if !rem.Destroy || len(rem.Provisioners) == 0 {
			continue
		}
		res := &ast.Resource{
			Type:         rem.From.Type,
			Name:         rem.From.Name,
			Provisioners: rem.Provisioners,
			DeclRange:    rem.DeclRange,
		}
		bound, err := e.resolver.BindResource(ctx, e.graph.ModuleTerraform(rem.From.Module), res)
		if err != nil {
			return fmt.Errorf("removed block for %s: resolving resource type: %w", rem.From, err)
		}
		probeURN, probeName := e.resmon.ResolveURN(e.stackURN, bound.Schema.Token, "probe")
		entry := &blockEntry{
			resmon:       e.resmon,
			dryRun:       e.dryRun,
			res:          res,
			bound:        bound,
			prefix:       strings.TrimSuffix(probeName, "probe"),
			evalCtx:      e.evaluator.Context(),
			config:       modulepath.NewAddress(rem.From.Module, modulepath.NewStep(rem.From.Name)),
			moduleTarget: !rem.From.Module.IsRoot(),
		}
		_, entry.parentChain = urnTypes(string(probeURN))
		e.dispatcher.putBlock(entry)
	}
	return nil
}
