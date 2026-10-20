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

package transform

import (
	"fmt"

	"github.com/pulumi/pulumi/pkg/v3/codegen"
	"github.com/pulumi/pulumi/pkg/v3/codegen/schema"
	"github.com/pulumi/pulumi/sdk/v3/go/property"
	"github.com/zclconf/go-cty/cty"
	"github.com/zclconf/go-cty/cty/function"
)

// ProviderFunctionImpl invokes a provider-defined function with the
// already-converted argument map and returns the raw invoke result. An empty
// result map signals that the call was skipped (unknown arguments during
// preview) and the return value is unknown.
type ProviderFunctionImpl func(args property.Map) (property.Map, error)

// ProviderFunctionReturnType returns the cty type a provider-defined
// function's calls evaluate to.
func ProviderFunctionReturnType(fn *schema.Function) cty.Type {
	if obj, ok := fn.ReturnType.(*schema.ObjectType); ok {
		return ctyObjectType(obj.Properties, nil, nil)
	}
	return ctyTypeFromType(fn.ReturnType, nil)
}

// ProviderFunction projects a Pulumi function with multi-argument inputs as a
// cty function callable from HCL as provider::<name>::<function>(...). The
// schema's input properties become positional parameters in declaration
// order; a parameter absent from the schema's required list accepts null.
// variadic marks the last input property — an array — as collecting the
// call's trailing arguments; the schema alone cannot record this, so it comes
// from the bridge mapping.
//
// Unknown and marked arguments are handled by the cty function machinery:
// an unknown argument short-circuits to an unknown result and marks
// (sensitivity, dependency URNs) transfer from arguments to the result, both
// without calling impl.
//
// A nil impl builds a type-only projection for schema generation: unknown
// arguments are let through so calls evaluate to a result that carries the
// return schema's per-attribute nullability (see RefinedUnknown) instead of
// short-circuiting to an unrefined unknown.
func ProviderFunction(fn *schema.Function, variadic bool, dryRun bool, impl ProviderFunctionImpl) (function.Function, error) {
	if !fn.MultiArgumentInputs {
		return function.Function{}, fmt.Errorf("%s does not take positional arguments", fn.Token)
	}
	var positional []*schema.Property
	if fn.Inputs != nil {
		positional = fn.Inputs.Properties
	}

	var variadicProp *schema.Property
	var variadicElem schema.Type
	if variadic {
		if len(positional) == 0 {
			return function.Function{}, fmt.Errorf("%s: variadic function has no input properties", fn.Token)
		}
		variadicProp = positional[len(positional)-1]
		positional = positional[:len(positional)-1]
		arr, ok := codegen.UnwrapType(variadicProp.Type).(*schema.ArrayType)
		if !ok {
			return function.Function{}, fmt.Errorf(
				"%s: variadic argument %q is not array-typed", fn.Token, variadicProp.Name)
		}
		variadicElem = arr.ElementType
	}

	typeOnly := impl == nil

	params := make([]function.Parameter, len(positional))
	for i, p := range positional {
		t := ctyTypeFromType(p.Type, nil)
		params[i] = function.Parameter{
			Name:             snakeCaseFromCamelCase(p.Name),
			Type:             t,
			AllowNull:        !p.IsRequired(),
			AllowUnknown:     typeOnly,
			AllowDynamicType: ctyTypeContainsDynamic(t),
		}
	}

	var varParam *function.Parameter
	if variadicProp != nil {
		t := ctyTypeFromType(variadicElem, nil)
		varParam = &function.Parameter{
			Name:             snakeCaseFromCamelCase(variadicProp.Name),
			Type:             t,
			AllowUnknown:     typeOnly,
			AllowDynamicType: ctyTypeContainsDynamic(t),
		}
	}

	retType := ProviderFunctionReturnType(fn)

	return function.New(&function.Spec{
		Params:   params,
		VarParam: varParam,
		Type:     function.StaticReturnType(retType),
		Impl: func(args []cty.Value, _ cty.Type) (cty.Value, error) {
			if impl == nil {
				return RefinedUnknown(retType, false), nil
			}
			inputs := make(map[string]property.Value, len(positional)+1)
			for i, p := range positional {
				v, err := ctyToResourceProperty(params[i].Name, args[i], p.Type, nil, p.Secret, p.Plain, nil)
				if err != nil {
					return cty.NilVal, err
				}
				if p.Secret {
					v = v.WithSecret(true)
				}
				inputs[p.Name] = v
			}
			if variadicProp != nil {
				rest := args[len(positional):]
				elems := make([]property.Value, len(rest))
				for i, arg := range rest {
					v, err := ctyToResourceProperty(
						fmt.Sprintf("%s[%d]", varParam.Name, i), arg, variadicElem, nil, variadicProp.Secret, variadicProp.Plain, nil)
					if err != nil {
						return cty.NilVal, err
					}
					elems[i] = v
				}
				v := property.New(elems)
				if variadicProp.Secret {
					v = v.WithSecret(true)
				}
				inputs[variadicProp.Name] = v
			}
			out, err := impl(property.NewMap(inputs))
			if err != nil {
				return cty.NilVal, err
			}
			return FunctionOutputToCty(out, fn, nil, dryRun)
		},
	}), nil
}
