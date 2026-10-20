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
	"fmt"
	"strings"

	"github.com/pulumi/pulumi-hcl/pkg/hcl/eval"
	"github.com/pulumi/pulumi-hcl/pkg/hcl/packages"
	"github.com/pulumi/pulumi-hcl/pkg/hcl/transform"
	"github.com/pulumi/pulumi/sdk/v3/go/property"
	"github.com/zclconf/go-cty/cty"
	"github.com/zclconf/go-cty/cty/convert"
	ctyjson "github.com/zclconf/go-cty/cty/json"
)

// lowerTerraformDataInputs adapts evaluated terraform_data inputs to Stash's
// input surface just before registration. It is a no-op for every other type.
//
// triggers_replace is not a Stash input: a change to it must force replacement
// (yielding a new id), which the engine's replacement trigger does — while a
// plain input change is an in-place update with a stable id. Its dependencies
// are already folded into DependsOn, so the property-level entry is dropped too.
//
// The engine treats a null trigger on either side of a diff as "no trigger",
// so a bare triggers_replace value could never diff against an absent one:
// setting or clearing triggers_replace would go unnoticed. terraform_data
// therefore always registers a non-null trigger, the fixed 2-tuple
// [triggers_replace, replace_triggered_by trigger] (each null when absent),
// which also keeps triggers_replace from clobbering a replace_triggered_by
// trigger already captured on the options.
//
// input is optional in terraform_data but required by Stash, so an omitted input
// is supplied as an explicit null: Stash stores it and mirrors it back as a null
// output, matching terraform_data used purely for its triggers_replace lifecycle.
func lowerTerraformDataInputs(resType string, inputs property.Map, opts *ResourceOptions) property.Map {
	if resType != packages.TerraformDataType {
		return inputs
	}
	// triggers_replace is not a Stash input but the engine's replacement trigger,
	// which processIgnoreChanges (input-only) cannot suppress. An ignore_changes
	// entry naming it is handled here instead: the trigger slot is held null so a
	// later value change never diffs against the stored trigger.
	//
	// Known limitation: this reconciles only against a trigger that was itself
	// recorded under ignore (null). If ignore_changes is added in the same update
	// that changes triggers_replace, the prior state still holds the real value,
	// so nulling the slot registers as a change and forces one replacement — the
	// engine substitutes prior state for ignored *inputs* only, and Stash rejects
	// triggers_replace as an input, so the language host cannot see it here.
	triggersIgnored := false
	kept := opts.IgnoreChanges[:0]
	for _, g := range opts.IgnoreChanges {
		text, err := g.MarshalText()
		if err != nil {
			kept = append(kept, g)
			continue
		}
		s := string(text)
		switch {
		// An ignore_changes path into input (input["k"], input[0]) ignores the
		// whole attribute: input is a single dynamic attribute, and its keys live
		// under the {type, value} wrapper in the engine's encoding, where a nested
		// glob would match nothing.
		case strings.HasPrefix(s, "input.") || strings.HasPrefix(s, "input["):
			kept = append(kept, property.GlobFromSegments(property.NewSegment("input")))
		// A glob rooted at triggers_replace (exact or nested) ignores the whole
		// trigger. It is dropped rather than kept: triggers_replace is no engine
		// property, so the engine would flag it as an unresolved ignore path.
		case s == "triggers_replace" ||
			strings.HasPrefix(s, "triggers_replace.") || strings.HasPrefix(s, "triggers_replace["):
			triggersIgnored = true
		// ignore_changes = all (the "[*]" splat) covers triggers_replace too; the
		// splat still applies to the Stash input, so it is kept.
		case s == "[*]":
			triggersIgnored = true
			kept = append(kept, g)
		default:
			kept = append(kept, g)
		}
	}
	opts.IgnoreChanges = kept
	triggers := inputs.Get("triggers_replace")
	inputs = inputs.Delete("triggers_replace")
	if triggersIgnored {
		triggers = property.New(property.Null)
	}
	opts.ReplacementTrigger = property.New([]property.Value{triggers, opts.ReplacementTrigger})
	if _, ok := inputs.GetOk("input"); !ok {
		inputs = inputs.Set("input", property.New(property.Null))
	}
	return inputs
}

// lowerTerraformDataOutputs adapts Stash's outputs back to terraform_data's
// surface just after registration. It is a no-op for every other type.
//
// terraform_data's `output` mirrors `input`, read back from Stash's tracking
// `input` output; `triggers_replace` is not a Stash output, so it is echoed
// from the trigger tuple encoded by lowerTerraformDataInputs.
func lowerTerraformDataOutputs(resType string, outputs property.Map, opts *ResourceOptions) property.Map {
	if resType != packages.TerraformDataType {
		return outputs
	}
	outputs = outputs.Set("output", outputs.Get("input"))
	return outputs.Set("triggers_replace", opts.ReplacementTrigger.AsArray().Get(0))
}

// wrapTerraformDataInputs boxes terraform_data's evaluated inputs as
// {type, value} wrappers (see packages.WrapTerraformDataValue), unboxed
// wherever outputs re-expand to cty, including from prior state (e.g. a
// destroy-time provisioner's self). It is a no-op for every other type.
func wrapTerraformDataInputs(resType string, inputs property.Map, evaluated map[string]cty.Value) property.Map {
	if resType != packages.TerraformDataType {
		return inputs
	}
	for _, name := range []string{"input", "triggers_replace"} {
		v, ok := inputs.GetOk(name)
		if !ok {
			continue
		}
		src, ok := evaluated[name]
		if !ok {
			continue
		}
		inputs = inputs.Set(name, packages.WrapTerraformDataValue(src.Type(), v))
	}
	return inputs
}

// unwrapTerraformDataOutputs replaces the re-expanded cty values of
// terraform_data's dynamically typed attributes with their wrapper contents,
// read from the pre-expansion property outputs — where the wrapper's shape is
// exact, unlike its re-expanded form (whose cty shape depends on whether the
// two fields' types unify). It is a no-op for every other type.
func unwrapTerraformDataOutputs(resType string, outputs map[string]cty.Value, props property.Map) error {
	if resType != packages.TerraformDataType {
		return nil
	}
	for _, name := range []string{"input", "output", "triggers_replace"} {
		v, ok, err := unwrapTerraformDataValue(props.Get(name))
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		if ok {
			outputs[name] = v
		}
	}
	return nil
}

// unwrapTerraformDataValue unboxes one property-encoded {type, value} wrapper
// into the cty value of its recorded type. It reports false for the values at
// these positions that are legitimately not wrappers — the null and unknown
// placeholders of an absent or not-yet-known attribute — which re-expand
// correctly without help. Any map here is a wrapper (evaluated inputs are
// always wrapped), so one that fails to decode is corrupt state and an error.
func unwrapTerraformDataValue(pv property.Value) (cty.Value, bool, error) {
	if !pv.IsMap() {
		return cty.Value{}, false, nil
	}
	m := pv.AsMap()
	typeJSON, typeOk := m.GetOk(packages.TerraformDataTypeKey)
	value, valueOk := m.GetOk(packages.TerraformDataValueKey)
	if !typeOk || !valueOk || m.Len() != 2 || !typeJSON.IsString() {
		return cty.Value{}, false, fmt.Errorf("malformed {%s, %s} wrapper", packages.TerraformDataTypeKey, packages.TerraformDataValueKey)
	}
	recorded, err := ctyjson.UnmarshalType([]byte(typeJSON.AsString()))
	if err != nil {
		return cty.Value{}, false, fmt.Errorf("invalid recorded cty type %q: %w", typeJSON.AsString(), err)
	}
	inner := transform.PropertyValueToCty(value)
	if converted, err := convert.Convert(inner, recorded); err == nil {
		inner = converted
	}
	if pv.Secret() {
		inner = inner.Mark(eval.SensitiveMark)
	}
	return inner, true, nil
}
