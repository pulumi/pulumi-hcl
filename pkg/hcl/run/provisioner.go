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

	"github.com/hashicorp/hcl/v2"
	"github.com/pulumi/pulumi/pkg/v3/codegen/schema"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource/urn"
	"github.com/pulumi/pulumi/sdk/v3/go/property"
	"github.com/zclconf/go-cty/cty"

	"github.com/pulumi/pulumi-hcl/pkg/hcl/ast"
	"github.com/pulumi/pulumi-hcl/pkg/hcl/bridge"
	"github.com/pulumi/pulumi-hcl/pkg/hcl/eval"
	"github.com/pulumi/pulumi-hcl/pkg/hcl/graph"
	"github.com/pulumi/pulumi-hcl/pkg/hcl/transform"
	"github.com/pulumi/pulumi-hcl/pkg/provisioner/runtime"
)

// bindGlobalHooks binds the instance's hook machinery.
//
// The constant hook is used because a per-instance name recorded in state
// would be unregistered on a later run that no longer declares the instance,
// breaking its delete. The entry is keyed by a URN computed before
// registration, because a delete-before-replace delete fires while
// RegisterResource is still blocked.
func (e *Engine) bindGlobalHooks(
	ctx context.Context,
	res *ast.Resource,
	resSchema *schema.Resource,
	mapping *bridge.BodyMapping,
	instance *graph.ExpandedResource,
	evalCtx *eval.Context,
	opts *ResourceOptions,
	instanceURN urn.URN,
) error {
	if len(res.Provisioners) == 0 && opts.PreventDestroy == preventDestroyAllow {
		return nil
	}
	if opts.Hooks == nil {
		opts.Hooks = &ResourceHookBinding{}
	}

	destroyProvisioners, err := e.bindProvisionerHooks(ctx, res, resSchema, mapping, instance, evalCtx, opts, instanceURN)
	if err != nil {
		return err
	}
	guard := preventDestroyHook(opts, instance)
	if guard == nil && destroyProvisioners == nil {
		return nil
	}

	dryRun := e.dryRun
	e.dispatcher.putInstance(string(instanceURN),
		func(hookCtx context.Context, args *ResourceHookArgs) error {
			if guard != nil {
				return guard(hookCtx, args)
			}
			if dryRun {
				// Provisioners never run during plan.
				return nil
			}
			if destroyProvisioners != nil {
				return destroyProvisioners(hookCtx, args)
			}
			return nil
		})
	opts.Hooks.BeforeDelete = append(opts.Hooks.BeforeDelete, destroyProvisionerHook)
	return nil
}

// preventDestroyHook returns the hook refusing this instance's delete, or nil
// when the lifecycle guard allows it. A known-null guard errors the delete.
func preventDestroyHook(opts *ResourceOptions, instance *graph.ExpandedResource) ResourceHookFunction {
	addr := instance.Key.String()
	switch opts.PreventDestroy {
	case preventDestroyRefuse:
		return func(context.Context, *ResourceHookArgs) error {
			return preventDestroyRefusal(addr)
		}
	case preventDestroyNull:
		return func(context.Context, *ResourceHookArgs) error {
			return preventDestroyNullRefusal(addr)
		}
	default:
		return nil
	}
}

// bindProvisionerHooks validates res's provisioners, registers and binds the
// create-time (AfterCreate) hooks, and returns the destroy-time runner for
// the dispatcher entry — nil when there are no destroy-time provisioners. TF
// does not re-run provisioners on update — no AfterUpdate binding. The HCL
// eval context is snapshotted now since the callbacks fire asynchronously,
// after processing has moved past this resource.
func (e *Engine) bindProvisionerHooks(
	ctx context.Context,
	res *ast.Resource,
	resSchema *schema.Resource,
	mapping *bridge.BodyMapping,
	instance *graph.ExpandedResource,
	evalCtx *eval.Context,
	opts *ResourceOptions,
	instanceURN urn.URN,
) (ResourceHookFunction, error) {
	hclSnapshot := evalCtx.HCLContext()
	dryRun := e.dryRun

	runOne := func(hookCtx context.Context, prov *ast.Provisioner, index int, outputs property.Map, id, urn string) error {
		spec := &runtime.Spec{
			Type:   prov.Type,
			Config: prov.Config,
			Conn:   effectiveConnectionBody(prov, res),
		}
		// Hooks receive raw engine outputs, so terraform_data's surface is
		// adapted the same way as on the registration path. This holds for
		// old outputs too: their {type, value} wrappers carry the
		// state-stored types, so a destroy-time self keeps them.
		outputs = lowerTerraformDataOutputs(res.Type, outputs, opts)
		selfCtx, err := selfBoundEvalCtx(hclSnapshot, outputs, id, urn, res.Type, resSchema, mapping, dryRun)
		if err != nil {
			return fmt.Errorf("provisioner %d for %s: %w", index, instance.Key, err)
		}
		if runErr := runtime.Run(hookCtx, spec, selfCtx); runErr != nil {
			if prov.OnFailure == "continue" {
				return nil
			}
			return fmt.Errorf("provisioner %d (%s) for %s: %w",
				index, prov.Type, instance.Key, runErr)
		}
		return nil
	}

	destroy := false
	for i, prov := range res.Provisioners {
		prov, index := prov, i+1
		if err := runtime.Validate(prov.Type); err != nil {
			return nil, fmt.Errorf("provisioner %d for %s: %w", index, instance.Key, err)
		}
		if prov.When == "destroy" {
			destroy = true
			continue
		}
		hookName := fmt.Sprintf("%s:provisioner:%d", instanceURN, i)
		callback := func(hookCtx context.Context, args *ResourceHookArgs) error {
			return runOne(hookCtx, prov, index, args.NewOutputs, args.ID, args.URN)
		}
		if err := e.resmon.RegisterResourceHook(ctx, hookName, callback, ResourceHookOptions{
			OnDryRun: false, // TF doesn't run provisioners during plan.
		}); err != nil {
			return nil, fmt.Errorf("registering provisioner hook: %w", err)
		}
		opts.Hooks.AfterCreate = append(opts.Hooks.AfterCreate, hookName)
	}
	if !destroy {
		return nil, nil
	}

	return func(hookCtx context.Context, args *ResourceHookArgs) error {
		for i, prov := range res.Provisioners {
			if prov.When != "destroy" {
				continue
			}
			if err := runOne(hookCtx, prov, i+1, args.OldOutputs, args.ID, args.URN); err != nil {
				return err
			}
		}
		return nil
	}, nil
}

// effectiveConnectionBody: provisioner-level overrides resource-level.
func effectiveConnectionBody(prov *ast.Provisioner, res *ast.Resource) hcl.Body {
	if prov.Connection != nil {
		return prov.Connection.Config
	}
	if res.Connection != nil {
		return res.Connection.Config
	}
	return nil
}

// selfBoundEvalCtx binds `self` to resource outputs + the synthetic id/urn
// the engine injects elsewhere (see registerResourceInstanceInContext).
func selfBoundEvalCtx(
	parent *hcl.EvalContext, outputs property.Map, id, urn, tfType string,
	resSchema *schema.Resource, mapping *bridge.BodyMapping, dryRun bool,
) (*hcl.EvalContext, error) {
	if outputs.Len() == 0 && id == "" && urn == "" {
		return parent, nil
	}
	outputObj, err := transform.ResourceOutputToCty(outputs, resSchema, mapping, dryRun)
	if err != nil {
		return nil, fmt.Errorf("converting outputs: %w", err)
	}
	if err := unwrapTerraformDataOutputs(tfType, outputObj, outputs); err != nil {
		return nil, fmt.Errorf("converting outputs: %w", err)
	}
	if id != "" {
		outputObj["id"] = cty.StringVal(id)
	}
	if urn != "" {
		outputObj["urn"] = cty.StringVal(urn)
	}
	child := parent.NewChild()
	child.Variables = map[string]cty.Value{"self": cty.ObjectVal(outputObj)}
	return child, nil
}
