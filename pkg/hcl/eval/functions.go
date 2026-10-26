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

// Package eval implements expression evaluation for HCL configurations.
package eval

import (
	"bytes"
	"compress/gzip"
	"crypto/md5"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/asn1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/apparentlymart/go-cidr/cidr"
	"github.com/bmatcuk/doublestar/v4"
	"github.com/google/uuid"
	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/ext/customdecode"
	"github.com/hashicorp/hcl/v2/ext/tryfunc"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	homedir "github.com/mitchellh/go-homedir"
	"github.com/pulumi/pulumi-hcl/vendored/ipaddr"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource/archive"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource/asset"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource/urn"
	"github.com/pulumi/pulumi/sdk/v3/go/common/util/contract"
	ctyyaml "github.com/zclconf/go-cty-yaml"
	"github.com/zclconf/go-cty/cty"
	"github.com/zclconf/go-cty/cty/convert"
	"github.com/zclconf/go-cty/cty/function"
	"github.com/zclconf/go-cty/cty/function/stdlib"
	"golang.org/x/crypto/bcrypt"
	"golang.org/x/crypto/ssh"
	"golang.org/x/text/encoding/ianaindex"
)

var (
	// AssetCapsuleType is the cty capsule type for Pulumi assets.
	AssetCapsuleType = cty.Capsule("Asset", reflect.TypeFor[asset.Asset]())

	// ArchiveCapsuleType is the cty capsule type for Pulumi archives.
	ArchiveCapsuleType = cty.Capsule("Archive", reflect.TypeFor[archive.Archive]())
)

// Functions returns a map of all Terraform-compatible functions.
func Functions(baseDir string) map[string]function.Function {
	funcs := map[string]function.Function{
		// Numeric functions
		"abs":      stdlib.AbsoluteFunc,
		"ceil":     stdlib.CeilFunc,
		"floor":    stdlib.FloorFunc,
		"log":      stdlib.LogFunc,
		"max":      stdlib.MaxFunc,
		"min":      stdlib.MinFunc,
		"pow":      stdlib.PowFunc,
		"signum":   stdlib.SignumFunc,
		"parseint": stdlib.ParseIntFunc,

		// String functions
		"chomp":       stdlib.ChompFunc,
		"format":      stdlib.FormatFunc,
		"formatlist":  stdlib.FormatListFunc,
		"indent":      stdlib.IndentFunc,
		"join":        stdlib.JoinFunc,
		"lower":       stdlib.LowerFunc,
		"regex":       stdlib.RegexFunc,
		"regexall":    stdlib.RegexAllFunc,
		"replace":     replaceFunc,
		"split":       stdlib.SplitFunc,
		"strrev":      stdlib.ReverseFunc,
		"substr":      stdlib.SubstrFunc,
		"title":       stdlib.TitleFunc,
		"trim":        stdlib.TrimFunc,
		"trimprefix":  stdlib.TrimPrefixFunc,
		"trimsuffix":  stdlib.TrimSuffixFunc,
		"trimspace":   stdlib.TrimSpaceFunc,
		"upper":       stdlib.UpperFunc,
		"startswith":  startsWithFunc,
		"endswith":    endsWithFunc,
		"strcontains": strContainsFunc,

		// Collection functions
		"alltrue":         allTrueFunc,
		"anytrue":         anyTrueFunc,
		"chunklist":       stdlib.ChunklistFunc,
		"coalesce":        coalesceFunc,
		"coalescelist":    stdlib.CoalesceListFunc,
		"compact":         stdlib.CompactFunc,
		"concat":          stdlib.ConcatFunc,
		"contains":        stdlib.ContainsFunc,
		"distinct":        stdlib.DistinctFunc,
		"element":         stdlib.ElementFunc,
		"flatten":         stdlib.FlattenFunc,
		"index":           indexFunc,
		"keys":            stdlib.KeysFunc,
		"length":          lengthFunc,
		"list":            listFunc,
		"lookup":          lookupFunc,
		"map":             mapFunc,
		"matchkeys":       matchkeysFunc,
		"merge":           stdlib.MergeFunc,
		"one":             oneFunc,
		"range":           stdlib.RangeFunc,
		"reverse":         stdlib.ReverseListFunc,
		"setintersection": stdlib.SetIntersectionFunc,
		"setproduct":      stdlib.SetProductFunc,
		"setsubtract":     stdlib.SetSubtractFunc,
		"setunion":        stdlib.SetUnionFunc,
		"slice":           stdlib.SliceFunc,
		"sort":            stdlib.SortFunc,
		"sum":             sumFunc,
		"transpose":       transposeFunc,
		"values":          stdlib.ValuesFunc,
		"zipmap":          stdlib.ZipmapFunc,
		"entries":         entriesFunc,

		// Encoding functions
		"base64decode":     base64DecodeFunc,
		"base64encode":     base64EncodeFunc,
		"base64gzip":       base64GzipFunc,
		"base64gunzip":     base64GunzipFunc,
		"csvdecode":        stdlib.CSVDecodeFunc,
		"jsondecode":       stdlib.JSONDecodeFunc,
		"jsonencode":       stdlib.JSONEncodeFunc,
		"textdecodebase64": textDecodeBase64Func,
		"textencodebase64": textEncodeBase64Func,
		"urlencode":        urlEncodeFunc,
		"urldecode":        urlDecodeFunc,
		"yamldecode":       ctyyaml.YAMLDecodeFunc,
		"yamlencode":       ctyyaml.YAMLEncodeFunc,

		// Filesystem functions
		"abspath":    abspathFunc(baseDir),
		"dirname":    dirnameFunc,
		"pathexpand": pathExpandFunc,
		"basename":   basenameFunc,
		"file":       fileFunc(baseDir),
		"fileexists": fileExistsFunc(baseDir),
		"fileset":    filesetFunc(baseDir),
		"filebase64": fileBase64Func(baseDir),

		// Date and time functions
		"formatdate": stdlib.FormatDateFunc,
		"timeadd":    timeAddFunc,
		"timecmp":    timeCmpFunc,
		"timestamp":  timestampFunc,
		// Pulumi has no plan/apply split, so there is no distinct plan-time
		// clock: plantimestamp resolves to the current time like timestamp.
		"plantimestamp": timestampFunc,

		// Hash and crypto functions
		"base64sha256":     base64Sha256Func,
		"base64sha512":     base64Sha512Func,
		"bcrypt":           bcryptFunc,
		"filebase64sha256": fileBase64Sha256Func(baseDir),
		"filebase64sha512": fileBase64Sha512Func(baseDir),
		"filemd5":          fileMd5Func(baseDir),
		"filesha1":         fileSha1Func(baseDir),
		"filesha256":       fileSha256Func(baseDir),
		"filesha512":       fileSha512Func(baseDir),
		"md5":              md5Func,
		"rsadecrypt":       rsaDecryptFunc,
		"sha1":             sha1Func,
		"sha256":           sha256Func,
		"sha512":           sha512Func,
		"uuid":             uuidFunc,
		"uuidv5":           uuidv5Func,

		// IP network functions
		"cidrcontains": cidrContainsFunc,
		"cidrhost":     cidrHostFunc,
		"cidrnetmask":  cidrNetmaskFunc,
		"cidrsubnet":   cidrSubnetFunc,
		"cidrsubnets":  cidrSubnetsFunc,

		// Type conversion functions
		"can":             canFunc,
		"recover":         recoverFunc,
		"ephemeralasnull": ephemeralasnullFunc,
		"issensitive":     issensitiveFunc,
		"nonsensitive":    nonsensitiveFunc,
		"sensitive":       sensitiveFunc,
		"tobool":          makeToFunc(cty.Bool),
		"tolist":          makeToFunc(cty.List(cty.DynamicPseudoType)),
		"tomap":           makeToFunc(cty.Map(cty.DynamicPseudoType)),
		"tonumber":        makeToFunc(cty.Number),
		"toset":           makeToFunc(cty.Set(cty.DynamicPseudoType)),
		"tostring":        toStringFunc,
		"try":             tryfunc.TryFunc,
		"type":            typeFunc,

		// Pulumi-specific functions
		"pulumiresourcename": pulumiResourceNameFunc,
		"pulumiresourcetype": pulumiResourceTypeFunc,
		"pulumiresourceurn":  pulumiResourceURNFunc,

		// Asset and archive functions
		"fileasset":     fileAssetFunc(baseDir),
		"filearchive":   fileArchiveFunc(baseDir),
		"stringasset":   stringAssetFunc(),
		"assetarchive":  assetArchiveFunc(),
		"remoteasset":   remoteAssetFunc(),
		"remotearchive": remoteArchiveFunc(),
	}

	// templatefile and templatestring render a file or string as a template. The
	// rendered template may itself call the template functions: templatestring
	// passes the full table through unchanged, and templatefile replaces itself
	// inside each rendered template with a variant that counts recursion depth
	// and errors past the limit. The callback defers the table lookup so the map
	// can refer to itself, and so later additions to it (e.g. provider
	// functions) are visible inside templates too.
	funcsCb := func() map[string]function.Function { return funcs }
	funcs["templatefile"] = templateFileFunc(baseDir, funcsCb, 0)
	funcs["templatestring"] = templateStringFunc(funcsCb)

	return funcs
}

// recoverFunc implements PCL's recover(value, recovery): it returns value if
// value evaluates successfully, and otherwise evaluates recovery with the
// variable `error` bound to the failure message. It mirrors try()/can() by
// taking its arguments as unevaluated expression closures so it can catch the
// evaluation error of value — which happens when value references a resource
// that failed to create under continue-on-error.
var recoverFunc = function.New(&function.Spec{
	Params: []function.Parameter{
		{Name: "value", Type: customdecode.ExpressionClosureType},
		{Name: "recovery", Type: customdecode.ExpressionClosureType},
	},
	Type: func(args []cty.Value) (cty.Type, error) {
		v, err := recoverImpl(args)
		if err != nil {
			return cty.NilType, err
		}
		return v.Type(), nil
	},
	Impl: func(args []cty.Value, _ cty.Type) (cty.Value, error) {
		return recoverImpl(args)
	},
})

func recoverImpl(args []cty.Value) (cty.Value, error) {
	value := customdecode.ExpressionClosureFromVal(args[0])
	v, diags := value.Value()
	if !diags.HasErrors() {
		// value resolved (possibly to an unknown during preview); use it.
		return v, nil
	}

	// value failed to evaluate — recover by evaluating the recovery expression
	// with `error` bound to the failure message.
	recovery := customdecode.ExpressionClosureFromVal(args[1])
	childCtx := recovery.EvalContext.NewChild()
	childCtx.Variables = map[string]cty.Value{"error": cty.StringVal(diags.Error())}
	rv, rdiags := recovery.Expression.Value(childCtx)
	if rdiags.HasErrors() {
		return cty.NilVal, rdiags
	}
	return rv, nil
}

var canFunc = function.New(&function.Spec{
	Params: []function.Parameter{
		{
			Name: "expression",
			Type: customdecode.ExpressionClosureType,
		},
	},
	Type: function.StaticReturnType(cty.Bool),
	Impl: func(args []cty.Value, retType cty.Type) (cty.Value, error) {
		closure := customdecode.ExpressionClosureFromVal(args[0])
		v, diags := closure.Value()
		var marks cty.ValueMarks
		if v.HasMark(SensitiveMark) {
			marks = cty.NewValueMarks(SensitiveMark)
		}

		if diags.HasErrors() {
			return cty.False.WithMarks(marks), nil
		}

		if !v.IsWhollyKnown() {
			// If the value is not wholly known, we still cannot be certain that
			// the expression was valid. There may be yet index expressions which
			// will fail once values are completely known.
			return cty.UnknownVal(cty.Bool).WithMarks(marks), nil
		}

		return cty.True.WithMarks(marks), nil
	},
})

// TypeInferenceFunctions returns the Terraform-compatible function set with the
// functions whose result type collapses to dynamic on not-wholly-known inputs
// replaced by type-preserving variants. Schema generation evaluates every
// reference as an unknown of its static type, so the runtime functions would
// report such an output as the "any" type; these variants recover the type the
// output takes on the runtime happy path.
func TypeInferenceFunctions(baseDir string) map[string]function.Function {
	funcs := Functions(baseDir)
	funcs["try"] = typeInferenceTryFunc
	// The file-reading functions error when their file is absent. During
	// schema generation the file may legitimately not exist yet (it can be
	// produced at runtime, or its name may only resolve then), so a failed
	// read types as an unknown of the function's return type instead of
	// failing schema generation.
	for _, name := range []string{
		"file", "filebase64", "fileexists", "fileset", "templatefile",
		"filemd5", "filesha1", "filesha256", "filesha512",
		"filebase64sha256", "filebase64sha512", "fileasset", "filearchive",
	} {
		funcs[name] = unknownOnError(funcs[name])
	}
	return funcs
}

// unknownOnError wraps fn so a failed call yields an unknown of fn's return
// type — dynamic when even the return type cannot be computed — rather than an
// error. See TypeInferenceFunctions.
func unknownOnError(fn function.Function) function.Function {
	return function.New(&function.Spec{
		Params:   fn.Params(),
		VarParam: fn.VarParam(),
		Type: func(args []cty.Value) (cty.Type, error) {
			t, err := fn.ReturnTypeForValues(args)
			if err != nil {
				return cty.DynamicPseudoType, nil
			}
			return t, nil
		},
		Impl: func(args []cty.Value, retType cty.Type) (cty.Value, error) {
			v, err := fn.Call(args)
			if err != nil {
				return cty.UnknownVal(retType), nil
			}
			return v, nil
		},
	})
}

// typeInferenceTryFunc is a type-inference-only variant of tryfunc.TryFunc.
//
// The upstream try() returns a dynamic value as soon as an argument is not
// wholly known, because once the unknowns resolve a different argument might win
// and the result type could change. Schema generation evaluates references as
// unknowns of their static type, so that conservative rule would type every
// try()-wrapped output as the "any" type. This variant returns the type that
// every argument able to win shares — e.g. try(aws_vpc.this[0].id, null) is a
// string.
var typeInferenceTryFunc = function.New(&function.Spec{
	VarParam: &function.Parameter{
		Name: "expressions",
		Type: customdecode.ExpressionClosureType,
	},
	Type: func(args []cty.Value) (cty.Type, error) {
		v, err := typeInferenceTry(args)
		if err != nil {
			return cty.NilType, err
		}
		return v.Type(), nil
	},
	Impl: func(args []cty.Value, _ cty.Type) (cty.Value, error) {
		return typeInferenceTry(args)
	},
})

// typeInferenceTry returns an unknown of the type that every argument able to
// win at runtime shares. An argument that errors now errors at runtime too, and
// a wholly known argument always succeeds, so no later argument can win. A
// null of unknown type fits any type. When the arguments disagree, the result
// is dynamic, since try() returns the winning value unconverted. Since try()
// may fall through to a later argument at runtime (such as the null in
// try(x, null)), the result is refined not-null only when every candidate is.
func typeInferenceTry(args []cty.Value) (cty.Value, error) {
	if len(args) == 0 {
		return cty.NilVal, errors.New("at least one argument is required")
	}
	ty := cty.NilType
	found, couldBeNull := false, false
	for _, arg := range args {
		closure := customdecode.ExpressionClosureFromVal(arg)
		v, diags := closure.Value()
		if diags.HasErrors() {
			continue
		}
		v, _ = v.UnmarkDeep()
		found = true
		if v.Range().CouldBeNull() {
			couldBeNull = true
		}
		switch {
		case v.IsNull() && v.Type() == cty.DynamicPseudoType:
		case ty == cty.NilType:
			ty = v.Type()
		case !ty.Equals(v.Type()):
			return cty.DynamicVal, nil
		}
		if v.IsWhollyKnown() {
			break
		}
	}
	if !found {
		return cty.NilVal, errors.New("no expression succeeded")
	}
	if ty == cty.NilType {
		return cty.DynamicVal, nil
	}
	if couldBeNull {
		return cty.UnknownVal(ty), nil
	}
	return cty.UnknownVal(ty).RefineNotNull(), nil
}

// String functions

// replaceFunc matches OpenTofu's `replace`, which interprets a search string
// wrapped in forward slashes as a regular expression. The cty stdlib
// ReplaceFunc only performs literal substring replacement, so binding to it
// would silently drop the regexp form.
var replaceFunc = function.New(&function.Spec{
	Params: []function.Parameter{
		{Name: "str", Type: cty.String},
		{Name: "substr", Type: cty.String},
		{Name: "replace", Type: cty.String},
	},
	Type: function.StaticReturnType(cty.String),
	Impl: func(args []cty.Value, retType cty.Type) (cty.Value, error) {
		str := args[0].AsString()
		substr := args[1].AsString()
		replace := args[2].AsString()

		if len(substr) > 1 && substr[0] == '/' && substr[len(substr)-1] == '/' {
			re, err := regexp.Compile(substr[1 : len(substr)-1])
			if err != nil {
				return cty.UnknownVal(cty.String), err
			}

			return cty.StringVal(re.ReplaceAllString(str, replace)), nil
		}

		return cty.StringVal(strings.ReplaceAll(str, substr, replace)), nil
	},
})

var startsWithFunc = function.New(&function.Spec{
	Params: []function.Parameter{
		{Name: "string", Type: cty.String},
		{Name: "prefix", Type: cty.String},
	},
	Type: function.StaticReturnType(cty.Bool),
	Impl: func(args []cty.Value, retType cty.Type) (cty.Value, error) {
		str := args[0].AsString()
		prefix := args[1].AsString()
		return cty.BoolVal(strings.HasPrefix(str, prefix)), nil
	},
})

var endsWithFunc = function.New(&function.Spec{
	Params: []function.Parameter{
		{Name: "string", Type: cty.String},
		{Name: "suffix", Type: cty.String},
	},
	Type: function.StaticReturnType(cty.Bool),
	Impl: func(args []cty.Value, retType cty.Type) (cty.Value, error) {
		str := args[0].AsString()
		suffix := args[1].AsString()
		return cty.BoolVal(strings.HasSuffix(str, suffix)), nil
	},
})

var strContainsFunc = function.New(&function.Spec{
	Params: []function.Parameter{
		{Name: "string", Type: cty.String},
		{Name: "substr", Type: cty.String},
	},
	Type: function.StaticReturnType(cty.Bool),
	Impl: func(args []cty.Value, retType cty.Type) (cty.Value, error) {
		str := args[0].AsString()
		substr := args[1].AsString()
		return cty.BoolVal(strings.Contains(str, substr)), nil
	},
})

// Collection functions

var allTrueFunc = function.New(&function.Spec{
	Params: []function.Parameter{
		{Name: "list", Type: cty.List(cty.Bool)},
	},
	Type: function.StaticReturnType(cty.Bool),
	Impl: func(args []cty.Value, retType cty.Type) (cty.Value, error) {
		for it := args[0].ElementIterator(); it.Next(); {
			_, v := it.Element()
			if !v.IsKnown() {
				return cty.UnknownVal(cty.Bool), nil
			}
			if v.False() {
				return cty.False, nil
			}
		}
		return cty.True, nil
	},
})

var anyTrueFunc = function.New(&function.Spec{
	Params: []function.Parameter{
		{Name: "list", Type: cty.List(cty.Bool)},
	},
	Type: function.StaticReturnType(cty.Bool),
	Impl: func(args []cty.Value, retType cty.Type) (cty.Value, error) {
		var hasUnknown bool
		for it := args[0].ElementIterator(); it.Next(); {
			_, v := it.Element()
			if !v.IsKnown() {
				hasUnknown = true
				continue
			}
			if v.True() {
				return cty.True, nil
			}
		}
		if hasUnknown {
			return cty.UnknownVal(cty.Bool), nil
		}
		return cty.False, nil
	},
})

var lengthFunc = function.New(&function.Spec{
	Params: []function.Parameter{
		{
			Name:             "value",
			Type:             cty.DynamicPseudoType,
			AllowDynamicType: true,
			AllowUnknown:     true,
			AllowMarked:      true,
		},
	},
	Type: func(args []cty.Value) (cty.Type, error) {
		ty := args[0].Type()
		switch {
		case ty == cty.String, ty == cty.DynamicPseudoType,
			ty.IsTupleType(), ty.IsObjectType(),
			ty.IsListType(), ty.IsMapType(), ty.IsSetType():
			return cty.Number, nil
		default:
			return cty.Number, fmt.Errorf("argument must be a string, a collection type, or a structural type")
		}
	},
	Impl: func(args []cty.Value, retType cty.Type) (cty.Value, error) {
		v := args[0]
		ty := v.Type()
		marks := v.Marks()
		switch {
		case ty == cty.DynamicPseudoType:
			return cty.UnknownVal(cty.Number).WithMarks(marks), nil
		case ty == cty.String:
			return stdlib.Strlen(v)
		case ty.IsTupleType():
			return cty.NumberIntVal(int64(len(ty.TupleElementTypes()))).WithMarks(marks), nil
		case ty.IsObjectType():
			return cty.NumberIntVal(int64(len(ty.AttributeTypes()))).WithMarks(marks), nil
		case ty.IsListType(), ty.IsMapType(), ty.IsSetType():
			return v.Length(), nil
		default:
			return cty.UnknownVal(cty.Number), fmt.Errorf("impossible value type for length(...)")
		}
	},
})

var indexFunc = function.New(&function.Spec{
	Params: []function.Parameter{
		{Name: "list", Type: cty.DynamicPseudoType},
		{Name: "value", Type: cty.DynamicPseudoType},
	},
	Type: function.StaticReturnType(cty.Number),
	Impl: func(args []cty.Value, retType cty.Type) (cty.Value, error) {
		list := args[0]
		value := args[1]
		i := 0
		for it := list.ElementIterator(); it.Next(); {
			_, v := it.Element()
			if !v.IsWhollyKnown() {
				return cty.UnknownVal(cty.Number), nil
			}
			if v.Equals(value).True() {
				return cty.NumberIntVal(int64(i)), nil
			}
			i++
		}
		return cty.NilVal, fmt.Errorf("value not found in list")
	},
})

var listFunc = function.New(&function.Spec{
	Params: []function.Parameter{},
	VarParam: &function.Parameter{
		Name: "elements",
		Type: cty.DynamicPseudoType,
	},
	Type: func(args []cty.Value) (cty.Type, error) {
		if len(args) == 0 {
			return cty.List(cty.DynamicPseudoType), nil
		}
		return cty.List(args[0].Type()), nil
	},
	Impl: func(args []cty.Value, retType cty.Type) (cty.Value, error) {
		if len(args) == 0 {
			return cty.ListValEmpty(cty.DynamicPseudoType), nil
		}
		return cty.ListVal(args), nil
	},
})

var lookupFunc = function.New(&function.Spec{
	Params: []function.Parameter{
		{Name: "map", Type: cty.DynamicPseudoType, AllowMarked: true},
		{Name: "key", Type: cty.String, AllowMarked: true},
	},
	VarParam: &function.Parameter{
		Name:             "default",
		Type:             cty.DynamicPseudoType,
		AllowNull:        true,
		AllowUnknown:     true,
		AllowDynamicType: true,
		AllowMarked:      true,
	},
	Type: func(args []cty.Value) (cty.Type, error) {
		ty := args[0].Type()
		switch {
		case ty.IsObjectType():
			if !args[1].IsKnown() {
				return cty.DynamicPseudoType, nil
			}
			keyVal, _ := args[1].Unmark()
			key := keyVal.AsString()
			if ty.HasAttribute(key) {
				return args[0].GetAttr(key).Type(), nil
			} else if len(args) == 3 {
				// The default's own type is returned for objects, since an
				// object's attributes need not share a single element type.
				return args[2].Type(), nil
			}
			return cty.NilType, function.NewArgErrorf(0, "the given object has no attribute %q", key)
		case ty.IsMapType():
			if len(args) == 3 {
				if _, err := convert.Convert(args[2], ty.ElementType()); err != nil {
					return cty.NilType, function.NewArgErrorf(2,
						"the default value must have the same type as the map elements")
				}
			}
			return ty.ElementType(), nil
		default:
			return cty.NilType, function.NewArgErrorf(0, "lookup() requires a map as the first argument")
		}
	},
	Impl: func(args []cty.Value, retType cty.Type) (cty.Value, error) {
		// The collection and key marks always propagate to the result, but the
		// default's must not: when the key is present the default is never
		// consulted, so a sensitive default leaves a present value unmarked.
		// The default's marks are reapplied only when the default is returned.
		var marks []cty.ValueMarks

		mapVar, mapMarks := args[0].Unmark()
		marks = append(marks, mapMarks)

		keyVal, keyMarks := args[1].Unmark()
		if len(keyMarks) > 0 {
			marks = append(marks, keyMarks)
		}
		key := keyVal.AsString()

		switch {
		case mapVar.Type().IsMapType():
			idx := cty.StringVal(key)
			if mapVar.HasIndex(idx).True() {
				return mapVar.Index(idx).WithMarks(marks...), nil
			}
		case mapVar.Type().IsObjectType():
			if mapVar.Type().HasAttribute(key) {
				return mapVar.GetAttr(key).WithMarks(marks...), nil
			}
		}

		// The key is absent: return the default coerced to the return type (for
		// a map, the element type, so e.g. 30 becomes "30" in a map(string)),
		// carrying the collection/key marks alongside the default's own.
		if len(args) == 3 {
			defaultVal, err := convert.Convert(args[2], retType)
			if err != nil {
				return cty.NilVal, err
			}
			return defaultVal.WithMarks(marks...), nil
		}

		return cty.NilVal, fmt.Errorf("key %q not found", key)
	},
})

var mapFunc = function.New(&function.Spec{
	Params: []function.Parameter{},
	VarParam: &function.Parameter{
		Name: "pairs",
		Type: cty.DynamicPseudoType,
	},
	Type: function.StaticReturnType(cty.Map(cty.DynamicPseudoType)),
	Impl: func(args []cty.Value, retType cty.Type) (cty.Value, error) {
		if len(args)%2 != 0 {
			return cty.NilVal, fmt.Errorf("map requires an even number of arguments")
		}
		m := make(map[string]cty.Value)
		for i := 0; i < len(args); i += 2 {
			key := args[i].AsString()
			m[key] = args[i+1]
		}
		if len(m) == 0 {
			return cty.MapValEmpty(cty.DynamicPseudoType), nil
		}
		return cty.MapVal(m), nil
	},
})

var matchkeysFunc = function.New(&function.Spec{
	Params: []function.Parameter{
		{Name: "values", Type: cty.List(cty.DynamicPseudoType)},
		{Name: "keys", Type: cty.List(cty.DynamicPseudoType)},
		{Name: "searchset", Type: cty.List(cty.DynamicPseudoType)},
	},
	Type: func(args []cty.Value) (cty.Type, error) {
		ty, _ := convert.UnifyUnsafe([]cty.Type{args[1].Type(), args[2].Type()})
		if ty == cty.NilType {
			return cty.NilType, errors.New("keys and searchset must be of the same type")
		}
		return args[0].Type(), nil
	},
	Impl: func(args []cty.Value, retType cty.Type) (cty.Value, error) {
		if !args[0].IsKnown() {
			return cty.UnknownVal(cty.List(retType.ElementType())), nil
		}

		if args[0].LengthInt() != args[1].LengthInt() {
			return cty.ListValEmpty(retType.ElementType()), errors.New("length of keys and values should be equal")
		}

		values := args[0]

		// keys and searchset must be unified to a common type before comparing,
		// so that e.g. a numeric key can match a string search-set element. The
		// Type function already verified that they unify, so converting each to
		// the unified type cannot fail here.
		ty, _ := convert.UnifyUnsafe([]cty.Type{args[1].Type(), args[2].Type()})
		keys, err := convert.Convert(args[1], ty)
		contract.AssertNoErrorf(err, "keys were verified to unify to %s in the type check", ty)
		searchset, err := convert.Convert(args[2], ty)
		contract.AssertNoErrorf(err, "searchset was verified to unify to %s in the type check", ty)

		if searchset.LengthInt() == 0 {
			return cty.ListValEmpty(retType.ElementType()), nil
		}

		if !values.IsWhollyKnown() || !keys.IsWhollyKnown() {
			return cty.UnknownVal(retType), nil
		}

		output := make([]cty.Value, 0)
		i := 0
		for it := keys.ElementIterator(); it.Next(); {
			_, key := it.Element()
			for iter := searchset.ElementIterator(); iter.Next(); {
				_, search := iter.Element()
				eq, err := stdlib.Equal(key, search)
				if err != nil {
					return cty.NilVal, err
				}
				if !eq.IsKnown() {
					return cty.UnknownVal(retType), nil
				}
				if eq.True() {
					output = append(output, values.Index(cty.NumberIntVal(int64(i))))
					break
				}
			}
			i++
		}

		if len(output) == 0 {
			return cty.ListValEmpty(retType.ElementType()), nil
		}
		return cty.ListVal(output), nil
	},
})

var oneFunc = function.New(&function.Spec{
	Params: []function.Parameter{
		{Name: "list", Type: cty.DynamicPseudoType},
	},
	Type: func(args []cty.Value) (cty.Type, error) {
		ty := args[0].Type()
		if ty.IsListType() || ty.IsSetType() {
			return ty.ElementType(), nil
		}
		if ty.IsTupleType() {
			etys := ty.TupleElementTypes()
			if len(etys) == 0 {
				return cty.DynamicPseudoType, nil
			}
			return etys[0], nil
		}
		return cty.DynamicPseudoType, nil
	},
	Impl: func(args []cty.Value, retType cty.Type) (cty.Value, error) {
		list := args[0]
		// A set's length can be unknown even when the set itself is known,
		// because unknown elements might collapse with known ones. Matching
		// OpenTofu, defer to an unknown result rather than counting elements
		// at face value (which would wrongly report "more than one element").
		lenVal := list.Length()
		if !lenVal.IsKnown() {
			return cty.UnknownVal(retType), nil
		}
		l := list.LengthInt()
		if l == 0 {
			return cty.NullVal(retType), nil
		}
		if l > 1 {
			return cty.NilVal, fmt.Errorf("list has more than one element")
		}
		for it := list.ElementIterator(); it.Next(); {
			_, v := it.Element()
			return v, nil
		}
		return cty.NullVal(retType), nil
	},
})

// coalesceFunc returns the first argument that is neither null nor an empty
// string. cty's stdlib.CoalesceFunc only skips null, so it would return an empty
// string where Terraform skips it. Other "zero" values (the number 0, empty
// collections) are values in their own right and are returned as-is.
var coalesceFunc = function.New(&function.Spec{
	Params: []function.Parameter{},
	VarParam: &function.Parameter{
		Name:             "vals",
		Type:             cty.DynamicPseudoType,
		AllowUnknown:     true,
		AllowDynamicType: true,
		AllowNull:        true,
	},
	Type: func(args []cty.Value) (cty.Type, error) {
		argTypes := make([]cty.Type, len(args))
		for i, val := range args {
			argTypes[i] = val.Type()
		}
		retType, _ := convert.UnifyUnsafe(argTypes)
		if retType == cty.NilType {
			return cty.NilType, fmt.Errorf("all arguments must have the same type")
		}
		return retType, nil
	},
	RefineResult: func(b *cty.RefinementBuilder) *cty.RefinementBuilder { return b.NotNull() },
	Impl: func(args []cty.Value, retType cty.Type) (cty.Value, error) {
		for _, argVal := range args {
			argVal, _ = convert.Convert(argVal, retType)
			if !argVal.IsKnown() {
				return cty.UnknownVal(retType), nil
			}
			if argVal.IsNull() {
				continue
			}
			if retType == cty.String && argVal.RawEquals(cty.StringVal("")) {
				continue
			}
			return argVal, nil
		}
		return cty.NilVal, fmt.Errorf("no non-null, non-empty-string arguments")
	},
})

var sumFunc = function.New(&function.Spec{
	Params: []function.Parameter{
		{Name: "list", Type: cty.DynamicPseudoType},
	},
	Type:         function.StaticReturnType(cty.Number),
	RefineResult: func(b *cty.RefinementBuilder) *cty.RefinementBuilder { return b.NotNull() },
	Impl: func(args []cty.Value, retType cty.Type) (ret cty.Value, err error) {
		if !args[0].CanIterateElements() {
			return cty.NilVal, function.NewArgErrorf(0, "cannot sum noniterable")
		}

		if args[0].LengthInt() == 0 {
			return cty.NilVal, function.NewArgErrorf(0, "cannot sum an empty list")
		}

		arg := args[0].AsValueSlice()
		ty := args[0].Type()

		if !ty.IsListType() && !ty.IsSetType() && !ty.IsTupleType() {
			return cty.NilVal, function.NewArgErrorf(0, "argument must be list, set, or tuple. Received %s", ty.FriendlyName())
		}

		if !args[0].IsWhollyKnown() {
			return cty.UnknownVal(cty.Number), nil
		}

		// big.Float.Add can panic if the input values are opposing infinities,
		// so we must catch that here in order to remain within
		// the cty Function abstraction.
		defer func() {
			if r := recover(); r != nil {
				if _, ok := r.(big.ErrNaN); ok {
					ret = cty.NilVal
					err = fmt.Errorf("can't compute sum of opposing infinities")
				} else {
					// not a panic we recognize
					panic(r)
				}
			}
		}()

		s := arg[0]
		if s.IsNull() {
			return cty.NilVal, function.NewArgErrorf(0, "argument must be list, set, or tuple of number values")
		}
		s, err = convert.Convert(s, cty.Number)
		if err != nil {
			return cty.NilVal, function.NewArgErrorf(0, "argument must be list, set, or tuple of number values")
		}
		for _, v := range arg[1:] {
			if v.IsNull() {
				return cty.NilVal, function.NewArgErrorf(0, "argument must be list, set, or tuple of number values")
			}
			v, err = convert.Convert(v, cty.Number)
			if err != nil {
				return cty.NilVal, function.NewArgErrorf(0, "argument must be list, set, or tuple of number values")
			}
			s = s.Add(v)
		}

		return s, nil
	},
})

var entriesFunc = function.New(&function.Spec{
	Params: []function.Parameter{
		{Name: "collection", Type: cty.DynamicPseudoType},
	},
	Type: func(args []cty.Value) (cty.Type, error) {
		ret := func(k, v cty.Type) (cty.Type, error) {
			return cty.List(cty.Object(map[string]cty.Type{
				"key":   k,
				"value": v,
			})), nil
		}

		t := args[0].Type()

		switch {
		case t.IsObjectType():
			// We can't return a list, since that would require that each "value" type is the same. We can't
			// return a tuple since we don't know the length given option fields.
			return cty.DynamicPseudoType, nil
		case t.IsMapType():
			return ret(cty.String, args[0].Type().ElementType())
		case t.IsListType(), t.IsTupleType():
			return ret(cty.Number, args[0].Type().ElementType())
		default:
			return cty.NilType, fmt.Errorf("entries: expected a Map, Object or List")
		}
	},
	Impl: func(args []cty.Value, retType cty.Type) (cty.Value, error) {
		v := args[0]
		t := v.Type()

		if !v.IsKnown() {
			return cty.UnknownVal(t), nil
		}

		if !t.IsCollectionType() && !t.IsObjectType() {
			return cty.Value{}, fmt.Errorf("entries: invalid input: %v", t)
		}

		elems := make([]cty.Value, 0, v.LengthInt())
		for it := v.ElementIterator(); it.Next(); {
			k, v := it.Element()
			elems = append(elems, cty.ObjectVal(map[string]cty.Value{
				"key":   k,
				"value": v,
			}))
		}

		if t.IsObjectType() || t.IsTupleType() {
			return cty.TupleVal(elems), nil
		}
		return cty.ListVal(elems), nil
	},
})

var transposeFunc = function.New(&function.Spec{
	Params: []function.Parameter{
		{Name: "values", Type: cty.Map(cty.List(cty.String))},
	},
	Type: function.StaticReturnType(cty.Map(cty.List(cty.String))),
	Impl: func(args []cty.Value, retType cty.Type) (cty.Value, error) {
		inputMap := args[0]
		if !inputMap.IsWhollyKnown() {
			return cty.UnknownVal(retType), nil
		}

		result := make(map[string][]string)
		path := make(cty.Path, 0, 2)
		for it := inputMap.ElementIterator(); it.Next(); {
			k, v := it.Element()
			key := k.AsString()
			keyPath := path.Index(k)
			if v.IsNull() {
				return cty.DynamicVal, function.NewArgErrorf(0,
					"cannot use null list for %s", formatCtyPath(keyPath))
			}
			for vit := v.ElementIterator(); vit.Next(); {
				idx, val := vit.Element()
				if val.IsNull() {
					return cty.DynamicVal, function.NewArgErrorf(0,
						"cannot use null string for %s", formatCtyPath(keyPath.Index(idx)))
				}
				valStr := val.AsString()
				result[valStr] = append(result[valStr], key)
			}
		}

		ctyResult := make(map[string]cty.Value)
		for k, v := range result {
			sort.Strings(v)
			vals := make([]cty.Value, len(v))
			for i, s := range v {
				vals[i] = cty.StringVal(s)
			}
			ctyResult[k] = cty.ListVal(vals)
		}

		if len(ctyResult) == 0 {
			return cty.MapValEmpty(cty.List(cty.String)), nil
		}
		return cty.MapVal(ctyResult), nil
	},
})

// formatCtyPath is a helper function to produce a user-friendly string
// representation of a cty.Path. The result uses a syntax similar to the
// HCL expression language in the hope of it being familiar to users.
//
// Inlined from github.com/opentofu/opentofu/internal/tfdiags/config_traversals.go
func formatCtyPath(path cty.Path) string {
	var buf bytes.Buffer
	for _, step := range path {
		switch ts := step.(type) {
		case cty.GetAttrStep:
			fmt.Fprintf(&buf, ".%s", ts.Name)
		case cty.IndexStep:
			buf.WriteByte('[')
			key := ts.Key
			keyTy := key.Type()
			switch {
			case key.IsNull():
				buf.WriteString("null")
			case !key.IsKnown():
				buf.WriteString("(not yet known)")
			case keyTy == cty.Number:
				bf := key.AsBigFloat()
				buf.WriteString(bf.Text('g', -1))
			case keyTy == cty.String:
				buf.WriteString(strconv.Quote(key.AsString()))
			default:
				buf.WriteString("...")
			}
			buf.WriteByte(']')
		}
	}
	return buf.String()
}

// Encoding functions

var base64DecodeFunc = function.New(&function.Spec{
	Params: []function.Parameter{
		{Name: "string", Type: cty.String},
	},
	Type: function.StaticReturnType(cty.String),
	Impl: func(args []cty.Value, retType cty.Type) (cty.Value, error) {
		decoded, err := base64.StdEncoding.DecodeString(args[0].AsString())
		if err != nil {
			return cty.NilVal, err
		}
		if !utf8.Valid(decoded) {
			return cty.NilVal, fmt.Errorf("the result of decoding the provided string is not valid UTF-8")
		}
		return cty.StringVal(string(decoded)), nil
	},
})

var base64EncodeFunc = function.New(&function.Spec{
	Params: []function.Parameter{
		{Name: "string", Type: cty.String},
	},
	Type: function.StaticReturnType(cty.String),
	Impl: func(args []cty.Value, retType cty.Type) (cty.Value, error) {
		encoded := base64.StdEncoding.EncodeToString([]byte(args[0].AsString()))
		return cty.StringVal(encoded), nil
	},
})

var base64GzipFunc = function.New(&function.Spec{
	Params: []function.Parameter{
		{Name: "string", Type: cty.String},
	},
	Type: function.StaticReturnType(cty.String),
	Impl: func(args []cty.Value, retType cty.Type) (cty.Value, error) {
		var buf bytes.Buffer
		w := gzip.NewWriter(&buf)
		if _, err := w.Write([]byte(args[0].AsString())); err != nil {
			return cty.NilVal, err
		}
		// OpenTofu flushes before closing, emitting an empty sync-flush block;
		// matching it keeps the base64 output byte-for-byte identical.
		if err := w.Flush(); err != nil {
			return cty.NilVal, err
		}
		if err := w.Close(); err != nil {
			return cty.NilVal, err
		}
		return cty.StringVal(base64.StdEncoding.EncodeToString(buf.Bytes())), nil
	},
})

// base64GunzipFunc base64-decodes a string and then gunzips the result. It is
// the inverse of base64GzipFunc.
var base64GunzipFunc = function.New(&function.Spec{
	Params: []function.Parameter{
		{Name: "string", Type: cty.String},
	},
	Type: function.StaticReturnType(cty.String),
	Impl: func(args []cty.Value, retType cty.Type) (cty.Value, error) {
		decoded, err := base64.StdEncoding.DecodeString(args[0].AsString())
		if err != nil {
			return cty.NilVal, fmt.Errorf("failed to decode base64 data %q", args[0].AsString())
		}
		gzipReader, err := gzip.NewReader(bytes.NewReader(decoded))
		if err != nil {
			return cty.NilVal, fmt.Errorf("failed to gunzip bytestream: %w", err)
		}
		gunzipped, err := io.ReadAll(gzipReader)
		if err != nil {
			return cty.NilVal, fmt.Errorf("failed to read gunzip raw data: %w", err)
		}
		return cty.StringVal(string(gunzipped)), nil
	},
})

// textDecodeBase64Func base64-decodes a string and then reinterprets the bytes
// using the named IANA character encoding.
var textDecodeBase64Func = function.New(&function.Spec{
	Params: []function.Parameter{
		{Name: "string", Type: cty.String},
		{Name: "encoding", Type: cty.String},
	},
	Type: function.StaticReturnType(cty.String),
	Impl: func(args []cty.Value, retType cty.Type) (cty.Value, error) {
		enc, err := ianaindex.IANA.Encoding(args[1].AsString())
		if err != nil || enc == nil {
			return cty.NilVal, fmt.Errorf(
				"%q is not a supported IANA encoding name or alias", args[1].AsString())
		}

		decoded, err := base64.StdEncoding.DecodeString(args[0].AsString())
		if err != nil {
			return cty.NilVal, fmt.Errorf("invalid source string: %w", err)
		}

		reinterpreted, err := enc.NewDecoder().Bytes(decoded)
		if err != nil || bytes.ContainsRune(reinterpreted, '�') {
			encName, err := ianaindex.IANA.Name(enc)
			if err != nil {
				encName = args[1].AsString()
			}
			return cty.NilVal, fmt.Errorf(
				"the given string contains symbols that are not defined for %s", encName)
		}
		return cty.StringVal(string(reinterpreted)), nil
	},
})

// textEncodeBase64Func re-encodes a UTF-8 string into the named IANA character
// encoding and then base64-encodes the result.
var textEncodeBase64Func = function.New(&function.Spec{
	Params: []function.Parameter{
		{Name: "string", Type: cty.String},
		{Name: "encoding", Type: cty.String},
	},
	Type: function.StaticReturnType(cty.String),
	Impl: func(args []cty.Value, retType cty.Type) (cty.Value, error) {
		enc, err := ianaindex.IANA.Encoding(args[1].AsString())
		if err != nil || enc == nil {
			return cty.NilVal, fmt.Errorf(
				"%q is not a supported IANA encoding name or alias", args[1].AsString())
		}
		encName, err := ianaindex.IANA.Name(enc)
		if err != nil {
			encName = args[1].AsString()
		}

		encoded, err := enc.NewEncoder().Bytes([]byte(args[0].AsString()))
		if err != nil {
			return cty.NilVal, fmt.Errorf(
				"the given string contains characters that cannot be represented in %s", encName)
		}
		return cty.StringVal(base64.StdEncoding.EncodeToString(encoded)), nil
	},
})

var urlEncodeFunc = function.New(&function.Spec{
	Params: []function.Parameter{
		{Name: "string", Type: cty.String},
	},
	Type: function.StaticReturnType(cty.String),
	Impl: func(args []cty.Value, retType cty.Type) (cty.Value, error) {
		return cty.StringVal(url.QueryEscape(args[0].AsString())), nil
	},
})

// urlDecodeFunc reverses query-string percent encoding. It is the inverse of
// urlEncodeFunc, and like url.QueryUnescape it maps a literal "+" to a space.
var urlDecodeFunc = function.New(&function.Spec{
	Params: []function.Parameter{
		{Name: "string", Type: cty.String},
	},
	Type: function.StaticReturnType(cty.String),
	Impl: func(args []cty.Value, retType cty.Type) (cty.Value, error) {
		query, err := url.QueryUnescape(args[0].AsString())
		if err != nil {
			return cty.NilVal, fmt.Errorf("failed to decode URL %q: %w", query, err)
		}
		return cty.StringVal(query), nil
	},
})

// Filesystem functions

func abspathFunc(baseDir string) function.Function {
	return function.New(&function.Spec{
		Params: []function.Parameter{
			{Name: "path", Type: cty.String},
		},
		Type: function.StaticReturnType(cty.String),
		Impl: func(args []cty.Value, retType cty.Type) (cty.Value, error) {
			path := args[0].AsString()
			if !filepath.IsAbs(path) {
				path = filepath.Join(baseDir, path)
			}
			absPath, err := filepath.Abs(path)
			if err != nil {
				return cty.NilVal, err
			}
			return cty.StringVal(absPath), nil
		},
	})
}

var dirnameFunc = function.New(&function.Spec{
	Params: []function.Parameter{
		{Name: "path", Type: cty.String},
	},
	Type: function.StaticReturnType(cty.String),
	Impl: func(args []cty.Value, retType cty.Type) (cty.Value, error) {
		return cty.StringVal(filepath.Dir(args[0].AsString())), nil
	},
})

var pathExpandFunc = function.New(&function.Spec{
	Params: []function.Parameter{
		{Name: "path", Type: cty.String},
	},
	Type: function.StaticReturnType(cty.String),
	Impl: func(args []cty.Value, retType cty.Type) (cty.Value, error) {
		homePath, err := homedir.Expand(args[0].AsString())
		return cty.StringVal(homePath), err
	},
})

var basenameFunc = function.New(&function.Spec{
	Params: []function.Parameter{
		{Name: "path", Type: cty.String},
	},
	Type: function.StaticReturnType(cty.String),
	Impl: func(args []cty.Value, retType cty.Type) (cty.Value, error) {
		return cty.StringVal(filepath.Base(args[0].AsString())), nil
	},
})

// resolveFilePath turns a path argument from one of the file* functions into a
// concrete path on disk, matching OpenTofu's openFile: a leading `~` is expanded
// to the user's home directory, a relative path is resolved against baseDir, and
// the result is cleaned for the host OS.
func resolveFilePath(baseDir, path string) (string, error) {
	path, err := homedir.Expand(path)
	if err != nil {
		return "", fmt.Errorf("failed to expand ~: %w", err)
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(baseDir, path)
	}
	return filepath.Clean(path), nil
}

func fileFunc(baseDir string) function.Function {
	return function.New(&function.Spec{
		Params: []function.Parameter{
			{Name: "path", Type: cty.String},
		},
		Type: function.StaticReturnType(cty.String),
		Impl: func(args []cty.Value, retType cty.Type) (cty.Value, error) {
			path, err := resolveFilePath(baseDir, args[0].AsString())
			if err != nil {
				return cty.NilVal, err
			}
			content, err := os.ReadFile(path)
			if err != nil {
				return cty.NilVal, err
			}
			return cty.StringVal(string(content)), nil
		},
	})
}

func fileExistsFunc(baseDir string) function.Function {
	return function.New(&function.Spec{
		Params: []function.Parameter{
			{Name: "path", Type: cty.String},
		},
		Type: function.StaticReturnType(cty.Bool),
		Impl: func(args []cty.Value, retType cty.Type) (cty.Value, error) {
			path, err := resolveFilePath(baseDir, args[0].AsString())
			if err != nil {
				return cty.NilVal, err
			}

			fi, err := os.Stat(path)
			if err != nil {
				if os.IsNotExist(err) {
					return cty.False, nil
				}
				return cty.NilVal, fmt.Errorf("failed to stat %s", path)
			}

			if fi.Mode().IsRegular() {
				return cty.True, nil
			}

			// Match OpenTofu: anything that exists but is not a regular file
			// (a directory, device node, pipe, socket, ...) is an error rather
			// than a silent false.
			fileType := fi.Mode().Type()
			switch {
			case (fileType & os.ModeDir) != 0:
				err = fmt.Errorf("%s is a directory, not a file", path)
			case (fileType & os.ModeDevice) != 0:
				err = fmt.Errorf("%s is a device node, not a regular file", path)
			case (fileType & os.ModeNamedPipe) != 0:
				err = fmt.Errorf("%s is a named pipe, not a regular file", path)
			case (fileType & os.ModeSocket) != 0:
				err = fmt.Errorf("%s is a unix domain socket, not a regular file", path)
			default:
				err = fmt.Errorf("%s is not a regular file", path)
			}
			return cty.False, err
		},
	})
}

func filesetFunc(baseDir string) function.Function {
	return function.New(&function.Spec{
		Params: []function.Parameter{
			{Name: "path", Type: cty.String},
			{Name: "pattern", Type: cty.String},
		},
		Type: function.StaticReturnType(cty.Set(cty.String)),
		Impl: func(args []cty.Value, retType cty.Type) (cty.Value, error) {
			path := args[0].AsString()
			pattern := args[1].AsString()
			if !filepath.IsAbs(path) {
				path = filepath.Join(baseDir, path)
			}

			// Join the path to the glob pattern, while ensuring the full
			// pattern is canonical for the host OS. The joined path is
			// automatically cleaned during this operation.
			pattern = filepath.Join(path, pattern)

			matches, err := doublestar.FilepathGlob(pattern)
			if err != nil {
				return cty.NilVal, fmt.Errorf("failed to glob pattern %s: %w", pattern, err)
			}

			var vals []cty.Value
			for _, match := range matches {
				fi, err := os.Stat(match)
				if err != nil {
					return cty.NilVal, fmt.Errorf("failed to stat %s: %w", match, err)
				}

				if !fi.Mode().IsRegular() {
					continue
				}

				rel, err := filepath.Rel(path, match)
				if err != nil {
					return cty.NilVal, fmt.Errorf("failed to trim path of match %s: %w", match, err)
				}

				vals = append(vals, cty.StringVal(filepath.ToSlash(rel)))
			}
			if len(vals) == 0 {
				return cty.SetValEmpty(cty.String), nil
			}
			return cty.SetVal(vals), nil
		},
	})
}

func fileBase64Func(baseDir string) function.Function {
	return function.New(&function.Spec{
		Params: []function.Parameter{
			{Name: "path", Type: cty.String},
		},
		Type: function.StaticReturnType(cty.String),
		Impl: func(args []cty.Value, retType cty.Type) (cty.Value, error) {
			path, err := resolveFilePath(baseDir, args[0].AsString())
			if err != nil {
				return cty.NilVal, err
			}
			content, err := os.ReadFile(path)
			if err != nil {
				return cty.NilVal, err
			}
			return cty.StringVal(base64.StdEncoding.EncodeToString(content)), nil
		},
	})
}

// templateMaxRecursionDepth returns the maximum number of nested templatefile
// renders allowed, from the TF_TEMPLATE_RECURSION_DEPTH environment variable
// or a default of 1024.
func templateMaxRecursionDepth() (int, error) {
	const envkey = "TF_TEMPLATE_RECURSION_DEPTH"
	if val := os.Getenv(envkey); val != "" {
		i, err := strconv.Atoi(val)
		if err != nil {
			return -1, fmt.Errorf("invalid value for %s: %w", envkey, err)
		}
		return i, nil
	}
	return 1024, nil
}

// templateFileFunc reads the file at its first argument and renders it as an HCL
// template using the variables in its second argument, just like OpenTofu: the
// file's interpolations may call functions and use `%{ for }` / `%{ if }`
// directives. funcsCb supplies the function table made available inside the
// template, with templatefile itself replaced by a variant whose recursion
// depth is one greater; depth counts how many templatefile renders are already
// on the stack, and rendering fails past the limit.
func templateFileFunc(
	baseDir string, funcsCb func() map[string]function.Function, depth int,
) function.Function {
	render := func(args []cty.Value) (cty.Value, error) {
		maxDepth, err := templateMaxRecursionDepth()
		if err != nil {
			return cty.NilVal, err
		}
		if depth > maxDepth {
			return cty.NilVal, fmt.Errorf("maximum recursion depth %d reached", maxDepth)
		}
		path := args[0].AsString()
		fullPath, err := resolveFilePath(baseDir, path)
		if err != nil {
			return cty.NilVal, err
		}
		content, err := os.ReadFile(fullPath)
		if err != nil {
			return cty.NilVal, err
		}
		given := funcsCb()
		nestedFuncs := make(map[string]function.Function, len(given))
		for name, fn := range given {
			if name == "templatefile" {
				fn = templateFileFunc(baseDir, funcsCb, depth+1)
			}
			nestedFuncs[name] = fn
		}
		return renderTemplate(path, cty.StringVal(string(content)), args[1], nestedFuncs)
	}
	return function.New(&function.Spec{
		Params: []function.Parameter{
			{Name: "path", Type: cty.String},
			{Name: "vars", Type: cty.DynamicPseudoType},
		},
		// The result type is whatever rendering produces (a single-interpolation
		// template can yield any type), so it is computed by rendering rather
		// than fixed; it is unknowable until the path and vars are known.
		Type: func(args []cty.Value) (cty.Type, error) {
			if !args[0].IsKnown() || !args[1].IsKnown() {
				return cty.DynamicPseudoType, nil
			}
			val, err := render(args)
			if err != nil {
				return cty.DynamicPseudoType, err
			}
			return val.Type(), nil
		},
		Impl: func(args []cty.Value, retType cty.Type) (cty.Value, error) {
			return render(args)
		},
	})
}

// templateStringFunc renders its first argument as an HCL template using the
// variables in its second argument. funcsCb supplies the function table made
// available inside the template, unchanged.
func templateStringFunc(funcsCb func() map[string]function.Function) function.Function {
	return function.New(&function.Spec{
		Params: []function.Parameter{
			{Name: "template", Type: cty.String},
			{Name: "vars", Type: cty.DynamicPseudoType},
		},
		// The result type is whatever rendering produces (a single-interpolation
		// template can yield any type), so it is computed by rendering rather
		// than fixed; it is unknowable until the template and vars are known.
		Type: func(args []cty.Value) (cty.Type, error) {
			if !args[0].IsKnown() || !args[1].IsKnown() {
				return cty.DynamicPseudoType, nil
			}
			val, err := renderTemplate("<templatestring>", args[0], args[1], funcsCb())
			if err != nil {
				return cty.DynamicPseudoType, err
			}
			return val.Type(), nil
		},
		Impl: func(args []cty.Value, retType cty.Type) (cty.Value, error) {
			return renderTemplate("<templatestring>", args[0], args[1], funcsCb())
		},
	})
}

// renderTemplate parses templateVal as an HCL template and evaluates it with
// varsVal's entries in scope plus the supplied functions, returning the
// rendered string. filename labels the template in any diagnostics. Marks on the
// inputs propagate to the result.
func renderTemplate(
	filename string, templateVal, varsVal cty.Value, funcs map[string]function.Function,
) (cty.Value, error) {
	templateVal, tmplMarks := templateVal.Unmark()

	expr, diags := hclsyntax.ParseTemplate(
		[]byte(templateVal.AsString()), filename, hcl.InitialPos)
	if diags.HasErrors() {
		return cty.NilVal, diags
	}

	vars := map[string]cty.Value{}
	if !varsVal.IsNull() {
		if ty := varsVal.Type(); !ty.IsObjectType() && !ty.IsMapType() {
			return cty.NilVal, fmt.Errorf(
				"vars argument must be an object, got %s", ty.FriendlyName())
		}
		for it := varsVal.ElementIterator(); it.Next(); {
			k, v := it.Element()
			vars[k.AsString()] = v
		}
	}

	val, diags := expr.Value(&hcl.EvalContext{Variables: vars, Functions: funcs})
	if diags.HasErrors() {
		return cty.NilVal, diags
	}

	// A template that is a single interpolation with no surrounding literal
	// text evaluates to the interpolated value's own type, so the result is
	// returned un-coerced rather than forced to a string.
	return val.WithMarks(tmplMarks), nil
}

// Date and time functions

var timeAddFunc = function.New(&function.Spec{
	Params: []function.Parameter{
		{Name: "timestamp", Type: cty.String},
		{Name: "duration", Type: cty.String},
	},
	Type: function.StaticReturnType(cty.String),
	Impl: func(args []cty.Value, retType cty.Type) (cty.Value, error) {
		ts := args[0].AsString()
		durStr := args[1].AsString()

		t, err := time.Parse(time.RFC3339, ts)
		if err != nil {
			return cty.NilVal, fmt.Errorf("invalid timestamp: %s", err)
		}

		dur, err := time.ParseDuration(durStr)
		if err != nil {
			return cty.NilVal, fmt.Errorf("invalid duration: %s", err)
		}

		return cty.StringVal(t.Add(dur).Format(time.RFC3339)), nil
	},
})

var timeCmpFunc = function.New(&function.Spec{
	Params: []function.Parameter{
		{Name: "timestamp_a", Type: cty.String},
		{Name: "timestamp_b", Type: cty.String},
	},
	Type: function.StaticReturnType(cty.Number),
	Impl: func(args []cty.Value, retType cty.Type) (cty.Value, error) {
		tsA := args[0].AsString()
		tsB := args[1].AsString()

		tA, err := time.Parse(time.RFC3339, tsA)
		if err != nil {
			return cty.NilVal, fmt.Errorf("invalid timestamp_a: %s", err)
		}

		tB, err := time.Parse(time.RFC3339, tsB)
		if err != nil {
			return cty.NilVal, fmt.Errorf("invalid timestamp_b: %s", err)
		}

		switch {
		case tA.Before(tB):
			return cty.NumberIntVal(-1), nil
		case tA.After(tB):
			return cty.NumberIntVal(1), nil
		default:
			return cty.NumberIntVal(0), nil
		}
	},
})

var timestampFunc = function.New(&function.Spec{
	Params: []function.Parameter{},
	Type:   function.StaticReturnType(cty.String),
	Impl: func(args []cty.Value, retType cty.Type) (cty.Value, error) {
		return cty.StringVal(time.Now().UTC().Format(time.RFC3339)), nil
	},
})

// Hash and crypto functions

var md5Func = function.New(&function.Spec{
	Params: []function.Parameter{
		{Name: "string", Type: cty.String},
	},
	Type: function.StaticReturnType(cty.String),
	Impl: func(args []cty.Value, retType cty.Type) (cty.Value, error) {
		hash := md5.Sum([]byte(args[0].AsString()))
		return cty.StringVal(hex.EncodeToString(hash[:])), nil
	},
})

var sha1Func = function.New(&function.Spec{
	Params: []function.Parameter{
		{Name: "string", Type: cty.String},
	},
	Type: function.StaticReturnType(cty.String),
	Impl: func(args []cty.Value, retType cty.Type) (cty.Value, error) {
		hash := sha1.Sum([]byte(args[0].AsString()))
		return cty.StringVal(hex.EncodeToString(hash[:])), nil
	},
})

var sha256Func = function.New(&function.Spec{
	Params: []function.Parameter{
		{Name: "string", Type: cty.String},
	},
	Type: function.StaticReturnType(cty.String),
	Impl: func(args []cty.Value, retType cty.Type) (cty.Value, error) {
		hash := sha256.Sum256([]byte(args[0].AsString()))
		return cty.StringVal(hex.EncodeToString(hash[:])), nil
	},
})

var sha512Func = function.New(&function.Spec{
	Params: []function.Parameter{
		{Name: "string", Type: cty.String},
	},
	Type: function.StaticReturnType(cty.String),
	Impl: func(args []cty.Value, retType cty.Type) (cty.Value, error) {
		hash := sha512.Sum512([]byte(args[0].AsString()))
		return cty.StringVal(hex.EncodeToString(hash[:])), nil
	},
})

var base64Sha256Func = function.New(&function.Spec{
	Params: []function.Parameter{
		{Name: "string", Type: cty.String},
	},
	Type: function.StaticReturnType(cty.String),
	Impl: func(args []cty.Value, retType cty.Type) (cty.Value, error) {
		hash := sha256.Sum256([]byte(args[0].AsString()))
		return cty.StringVal(base64.StdEncoding.EncodeToString(hash[:])), nil
	},
})

var base64Sha512Func = function.New(&function.Spec{
	Params: []function.Parameter{
		{Name: "string", Type: cty.String},
	},
	Type: function.StaticReturnType(cty.String),
	Impl: func(args []cty.Value, retType cty.Type) (cty.Value, error) {
		hash := sha512.Sum512([]byte(args[0].AsString()))
		return cty.StringVal(base64.StdEncoding.EncodeToString(hash[:])), nil
	},
})

var bcryptFunc = function.New(&function.Spec{
	Params: []function.Parameter{
		{Name: "string", Type: cty.String},
	},
	VarParam: &function.Parameter{
		Name: "cost",
		Type: cty.Number,
	},
	Type: function.StaticReturnType(cty.String),
	Impl: func(args []cty.Value, retType cty.Type) (cty.Value, error) {
		cost := bcrypt.DefaultCost
		if len(args) > 1 {
			c, _ := args[1].AsBigFloat().Int64()
			cost = int(c)
		}
		hash, err := bcrypt.GenerateFromPassword([]byte(args[0].AsString()), cost)
		if err != nil {
			return cty.NilVal, err
		}
		return cty.StringVal(string(hash)), nil
	},
})

func fileBase64Sha256Func(baseDir string) function.Function {
	return function.New(&function.Spec{
		Params: []function.Parameter{
			{Name: "path", Type: cty.String},
		},
		Type: function.StaticReturnType(cty.String),
		Impl: func(args []cty.Value, retType cty.Type) (cty.Value, error) {
			path, err := resolveFilePath(baseDir, args[0].AsString())
			if err != nil {
				return cty.NilVal, err
			}
			content, err := os.ReadFile(path)
			if err != nil {
				return cty.NilVal, err
			}
			hash := sha256.Sum256(content)
			return cty.StringVal(base64.StdEncoding.EncodeToString(hash[:])), nil
		},
	})
}

func fileBase64Sha512Func(baseDir string) function.Function {
	return function.New(&function.Spec{
		Params: []function.Parameter{
			{Name: "path", Type: cty.String},
		},
		Type: function.StaticReturnType(cty.String),
		Impl: func(args []cty.Value, retType cty.Type) (cty.Value, error) {
			path, err := resolveFilePath(baseDir, args[0].AsString())
			if err != nil {
				return cty.NilVal, err
			}
			content, err := os.ReadFile(path)
			if err != nil {
				return cty.NilVal, err
			}
			hash := sha512.Sum512(content)
			return cty.StringVal(base64.StdEncoding.EncodeToString(hash[:])), nil
		},
	})
}

func fileMd5Func(baseDir string) function.Function {
	return function.New(&function.Spec{
		Params: []function.Parameter{
			{Name: "path", Type: cty.String},
		},
		Type: function.StaticReturnType(cty.String),
		Impl: func(args []cty.Value, retType cty.Type) (cty.Value, error) {
			path, err := resolveFilePath(baseDir, args[0].AsString())
			if err != nil {
				return cty.NilVal, err
			}
			content, err := os.ReadFile(path)
			if err != nil {
				return cty.NilVal, err
			}
			hash := md5.Sum(content)
			return cty.StringVal(hex.EncodeToString(hash[:])), nil
		},
	})
}

func fileSha1Func(baseDir string) function.Function {
	return function.New(&function.Spec{
		Params: []function.Parameter{
			{Name: "path", Type: cty.String},
		},
		Type: function.StaticReturnType(cty.String),
		Impl: func(args []cty.Value, retType cty.Type) (cty.Value, error) {
			path, err := resolveFilePath(baseDir, args[0].AsString())
			if err != nil {
				return cty.NilVal, err
			}
			content, err := os.ReadFile(path)
			if err != nil {
				return cty.NilVal, err
			}
			hash := sha1.Sum(content)
			return cty.StringVal(hex.EncodeToString(hash[:])), nil
		},
	})
}

func fileSha256Func(baseDir string) function.Function {
	return function.New(&function.Spec{
		Params: []function.Parameter{
			{Name: "path", Type: cty.String},
		},
		Type: function.StaticReturnType(cty.String),
		Impl: func(args []cty.Value, retType cty.Type) (cty.Value, error) {
			path, err := resolveFilePath(baseDir, args[0].AsString())
			if err != nil {
				return cty.NilVal, err
			}
			content, err := os.ReadFile(path)
			if err != nil {
				return cty.NilVal, err
			}
			hash := sha256.Sum256(content)
			return cty.StringVal(hex.EncodeToString(hash[:])), nil
		},
	})
}

func fileSha512Func(baseDir string) function.Function {
	return function.New(&function.Spec{
		Params: []function.Parameter{
			{Name: "path", Type: cty.String},
		},
		Type: function.StaticReturnType(cty.String),
		Impl: func(args []cty.Value, retType cty.Type) (cty.Value, error) {
			path, err := resolveFilePath(baseDir, args[0].AsString())
			if err != nil {
				return cty.NilVal, err
			}
			content, err := os.ReadFile(path)
			if err != nil {
				return cty.NilVal, err
			}
			hash := sha512.Sum512(content)
			return cty.StringVal(hex.EncodeToString(hash[:])), nil
		},
	})
}

var uuidFunc = function.New(&function.Spec{
	Params: []function.Parameter{},
	Type:   function.StaticReturnType(cty.String),
	Impl: func(args []cty.Value, retType cty.Type) (cty.Value, error) {
		return cty.StringVal(uuid.New().String()), nil
	},
})

var uuidv5Func = function.New(&function.Spec{
	Params: []function.Parameter{
		{Name: "namespace", Type: cty.String},
		{Name: "name", Type: cty.String},
	},
	Type: function.StaticReturnType(cty.String),
	Impl: func(args []cty.Value, retType cty.Type) (cty.Value, error) {
		nsStr := args[0].AsString()
		name := args[1].AsString()

		var ns uuid.UUID
		switch nsStr {
		case "dns", "DNS":
			ns = uuid.NameSpaceDNS
		case "url", "URL":
			ns = uuid.NameSpaceURL
		case "oid", "OID":
			ns = uuid.NameSpaceOID
		case "x500", "X500":
			ns = uuid.NameSpaceX500
		default:
			var err error
			ns, err = uuid.Parse(nsStr)
			if err != nil {
				return cty.NilVal, fmt.Errorf("invalid namespace: %s", err)
			}
		}

		return cty.StringVal(uuid.NewSHA1(ns, []byte(name)).String()), nil
	},
})

var rsaDecryptFunc = function.New(&function.Spec{
	Params: []function.Parameter{
		{Name: "ciphertext", Type: cty.String},
		{Name: "privatekey", Type: cty.String},
	},
	Type: function.StaticReturnType(cty.String),
	Impl: func(args []cty.Value, retType cty.Type) (cty.Value, error) {
		// Ciphertext is base64-encoded
		ciphertextB64 := args[0].AsString()
		privateKeyPEM := args[1].AsString()

		// Decode base64 ciphertext
		ciphertext, err := base64.StdEncoding.DecodeString(ciphertextB64)
		if err != nil {
			return cty.NilVal, fmt.Errorf("invalid base64 ciphertext: %w", err)
		}

		// Parse the private key. ssh.ParseRawPrivateKey accepts PKCS#1,
		// PKCS#8, SEC1, and OpenSSH-format PEM blocks.
		rawKey, err := ssh.ParseRawPrivateKey([]byte(privateKeyPEM))
		if err != nil {
			var errStr string
			switch e := err.(type) {
			case asn1.SyntaxError:
				errStr = strings.ReplaceAll(e.Error(), "asn1: syntax error", "invalid ASN1 data in the given private key")
			case asn1.StructuralError:
				errStr = strings.ReplaceAll(e.Error(), "asn1: structure error", "invalid ASN1 data in the given private key")
			default:
				errStr = fmt.Sprintf("invalid private key: %s", e)
			}
			return cty.NilVal, errors.New(errStr)
		}
		privKey, ok := rawKey.(*rsa.PrivateKey)
		if !ok {
			return cty.NilVal, fmt.Errorf("invalid private key type %T", rawKey)
		}

		// Decrypt using PKCS1v15 (Terraform's default)
		//nolint:staticcheck // SA1019: Using deprecated function for Terraform compatibility
		plaintext, err := rsa.DecryptPKCS1v15(rand.Reader, privKey, ciphertext)
		if err != nil {
			return cty.NilVal, fmt.Errorf("decryption failed: %w", err)
		}

		return cty.StringVal(string(plaintext)), nil
	},
})

// IP network functions

// cidrContainsFunc reports whether the IP address or prefix in its second
// argument is contained within the prefix in its first argument.
var cidrContainsFunc = function.New(&function.Spec{
	Params: []function.Parameter{
		{Name: "containing_prefix", Type: cty.String},
		{Name: "contained_ip_or_prefix", Type: cty.String},
	},
	Type: function.StaticReturnType(cty.Bool),
	Impl: func(args []cty.Value, retType cty.Type) (cty.Value, error) {
		prefix := args[0].AsString()
		addr := args[1].AsString()

		_, containing, err := ipaddr.ParseCIDR(prefix)
		if err != nil {
			return cty.NilVal, err
		}

		// The second argument can be either a bare IP address or a CIDR prefix.
		// Try it as an address first, and fall back to a prefix, in which case
		// both ends of its range must be contained.
		startIP := ipaddr.ParseIP(addr)
		var endIP ipaddr.IP
		if startIP == nil {
			_, contained, err := ipaddr.ParseCIDR(addr)
			if err != nil {
				return cty.NilVal, fmt.Errorf("invalid IP address or prefix: %s", addr)
			}
			startIP, endIP = cidr.AddressRange(contained)
		}

		// Comparing across address families would silently return false, so
		// reject it as an error to distinguish from a genuine non-containment.
		if (startIP.To4() == nil) != (containing.IP.To4() == nil) {
			return cty.NilVal, fmt.Errorf("address family mismatch: %s vs. %s", prefix, addr)
		}

		result := containing.Contains(startIP)
		if endIP != nil {
			result = result && containing.Contains(endIP)
		}
		return cty.BoolVal(result), nil
	},
})

var cidrHostFunc = function.New(&function.Spec{
	Params: []function.Parameter{
		{Name: "prefix", Type: cty.String},
		{Name: "hostnum", Type: cty.Number},
	},
	Type: function.StaticReturnType(cty.String),
	Impl: func(args []cty.Value, retType cty.Type) (cty.Value, error) {
		prefix := args[0].AsString()
		hostnum, _ := args[1].AsBigFloat().Int(nil)

		_, network, err := ipaddr.ParseCIDR(prefix)
		if err != nil {
			return cty.NilVal, fmt.Errorf("invalid CIDR: %s", err)
		}

		ip, err := cidr.HostBig(network, hostnum)
		if err != nil {
			return cty.NilVal, err
		}

		return cty.StringVal(ip.String()), nil
	},
})

var cidrNetmaskFunc = function.New(&function.Spec{
	Params: []function.Parameter{
		{Name: "prefix", Type: cty.String},
	},
	Type: function.StaticReturnType(cty.String),
	Impl: func(args []cty.Value, retType cty.Type) (cty.Value, error) {
		prefix := args[0].AsString()

		_, network, err := ipaddr.ParseCIDR(prefix)
		if err != nil {
			return cty.NilVal, fmt.Errorf("invalid CIDR: %s", err)
		}

		if network.IP.To4() == nil {
			return cty.NilVal, fmt.Errorf("IPv6 addresses cannot have a netmask: %s", prefix)
		}

		mask := net.IP(network.Mask)
		return cty.StringVal(mask.String()), nil
	},
})

var cidrSubnetFunc = function.New(&function.Spec{
	Params: []function.Parameter{
		{Name: "prefix", Type: cty.String},
		{Name: "newbits", Type: cty.Number},
		{Name: "netnum", Type: cty.Number},
	},
	Type: function.StaticReturnType(cty.String),
	Impl: func(args []cty.Value, retType cty.Type) (cty.Value, error) {
		newbits, _ := args[1].AsBigFloat().Int64()
		netnum, _ := args[2].AsBigFloat().Int(nil)

		_, network, err := ipaddr.ParseCIDR(args[0].AsString())
		if err != nil {
			return cty.NilVal, fmt.Errorf("invalid CIDR: %s", err)
		}

		newNetwork, err := cidr.SubnetBig(network, int(newbits), netnum)
		if err != nil {
			return cty.NilVal, err
		}
		return cty.StringVal(newNetwork.String()), nil
	},
})

// cidrSubnetsFunc allocates a sequence of consecutive subnets of the given
// prefix lengths (in additional bits) under a common base prefix. The
// allocator advances by each subnet's actual size, so mixed prefix lengths
// pack without overlap — matching Terraform's behaviour.
var cidrSubnetsFunc = function.New(&function.Spec{
	Params: []function.Parameter{
		{Name: "prefix", Type: cty.String},
	},
	VarParam: &function.Parameter{
		Name: "newbits",
		Type: cty.Number,
	},
	Type: function.StaticReturnType(cty.List(cty.String)),
	Impl: func(args []cty.Value, retType cty.Type) (cty.Value, error) {
		_, network, err := ipaddr.ParseCIDR(args[0].AsString())
		if err != nil {
			return cty.NilVal, function.NewArgErrorf(0, "invalid CIDR expression: %s", err)
		}
		startPrefixLen, _ := network.Mask.Size()

		newbitsArgs := args[1:]
		if len(newbitsArgs) == 0 {
			return cty.ListValEmpty(cty.String), nil
		}

		firstNewbits, _ := newbitsArgs[0].AsBigFloat().Int64()
		current, _ := cidr.PreviousSubnet(network, startPrefixLen+int(firstNewbits))

		out := make([]cty.Value, len(newbitsArgs))
		for i, arg := range newbitsArgs {
			newbits, _ := arg.AsBigFloat().Int64()
			if newbits < 1 {
				return cty.NilVal, function.NewArgErrorf(i+1, "must extend prefix by at least one bit")
			}
			length := startPrefixLen + int(newbits)
			if length > len(network.IP)*8 {
				protocol := "IP"
				switch len(network.IP) * 8 {
				case 32:
					protocol = "IPv4"
				case 128:
					protocol = "IPv6"
				}
				return cty.NilVal, function.NewArgErrorf(i+1,
					"would extend prefix to %d bits, which is too long for an %s address",
					length, protocol)
			}

			next, rollover := cidr.NextSubnet(current, length)
			if rollover || !network.Contains(next.IP) {
				return cty.NilVal, function.NewArgErrorf(i+1,
					"not enough remaining address space for a subnet with a prefix of %d bits after %s",
					length, current.String())
			}
			current = next
			out[i] = cty.StringVal(current.String())
		}
		return cty.ListVal(out), nil
	},
})

// Type conversion functions

var nonsensitiveFunc = function.New(&function.Spec{
	Params: []function.Parameter{
		{
			Name:             "value",
			Type:             cty.DynamicPseudoType,
			AllowUnknown:     true,
			AllowNull:        true,
			AllowMarked:      true,
			AllowDynamicType: true,
		},
	},
	Type: func(args []cty.Value) (cty.Type, error) {
		val, _ := args[0].Unmark()
		return val.Type(), nil
	},
	Impl: func(args []cty.Value, retType cty.Type) (cty.Value, error) {
		val, marks := args[0].Unmark()
		delete(marks, SensitiveMark)
		if len(marks) == 0 {
			return val, nil
		}
		return val.WithMarks(marks), nil
	},
})

var sensitiveFunc = function.New(&function.Spec{
	Params: []function.Parameter{
		{
			Name:             "value",
			Type:             cty.DynamicPseudoType,
			AllowUnknown:     true,
			AllowNull:        true,
			AllowMarked:      true,
			AllowDynamicType: true,
		},
	},
	Type: func(args []cty.Value) (cty.Type, error) {
		return args[0].Type(), nil
	},
	Impl: func(args []cty.Value, retType cty.Type) (cty.Value, error) {
		return args[0].Mark(SensitiveMark), nil
	},
})

var ephemeralasnullFunc = function.New(&function.Spec{
	Params: []function.Parameter{
		{
			Name:             "value",
			Type:             cty.DynamicPseudoType,
			AllowUnknown:     true,
			AllowNull:        true,
			AllowMarked:      true,
			AllowDynamicType: true,
		},
	},
	Type: func(args []cty.Value) (cty.Type, error) {
		return args[0].Type(), nil
	},
	Impl: func(args []cty.Value, retType cty.Type) (cty.Value, error) {
		return cty.Transform(args[0], func(_ cty.Path, val cty.Value) (cty.Value, error) {
			nonEphemeralMarks := val.Marks()
			delete(nonEphemeralMarks, EphemeralMark)
			switch {
			case val.IsNull():
				return cty.NullVal(val.Type()).WithMarks(nonEphemeralMarks), nil
			case !val.IsKnown():
				// An unknown value's ephemerality is not yet finalized: an
				// expression like `var.cond ? var.ephemeral : "b"` only
				// resolves its mark once var.cond is known.
				return cty.UnknownVal(val.Type()).WithMarks(nonEphemeralMarks), nil
			case val.HasMark(EphemeralMark):
				return cty.NullVal(val.Type()).WithMarks(nonEphemeralMarks), nil
			default:
				return val, nil
			}
		})
	},
})

var issensitiveFunc = function.New(&function.Spec{
	Params: []function.Parameter{
		{
			Name:             "value",
			Type:             cty.DynamicPseudoType,
			AllowUnknown:     true,
			AllowNull:        true,
			AllowMarked:      true,
			AllowDynamicType: true,
		},
	},
	Type: function.StaticReturnType(cty.Bool),
	Impl: func(args []cty.Value, retType cty.Type) (cty.Value, error) {
		if !args[0].IsKnown() {
			// An unknown value's sensitivity is not yet finalized: an
			// expression like `var.cond ? sensitive("a") : "b"` only
			// resolves its mark once var.cond is known. Match OpenTofu and
			// report an unknown bool rather than committing to an answer.
			return cty.UnknownVal(cty.Bool), nil
		}
		return cty.BoolVal(args[0].HasMark(SensitiveMark)), nil
	},
})

// makeToFunc constructs a "to..." conversion function like OpenTofu's
// MakeToFunc. The argument passes through verbatim as cty.DynamicPseudoType and
// the conversion to wantTy happens inside Type and Impl via the cty convert
// package. Passing cty.List(cty.DynamicPseudoType) and friends means "list of
// any single type", which causes cty to unify the element types of a tuple or
// object rather than rejecting a mismatch.
func makeToFunc(wantTy cty.Type) function.Function {
	return function.New(&function.Spec{
		Params: []function.Parameter{
			{
				Name:             "v",
				Type:             cty.DynamicPseudoType,
				AllowNull:        true,
				AllowMarked:      true,
				AllowDynamicType: true,
			},
		},
		Type: func(args []cty.Value) (cty.Type, error) {
			gotTy := args[0].Type()
			if gotTy.Equals(wantTy) {
				return wantTy, nil
			}
			conv := convert.GetConversionUnsafe(args[0].Type(), wantTy)
			if conv == nil {
				switch {
				case gotTy.IsTupleType() && wantTy.IsTupleType():
					return cty.NilType, function.NewArgErrorf(0, "incompatible tuple type for conversion: %s", convert.MismatchMessage(gotTy, wantTy))
				case gotTy.IsObjectType() && wantTy.IsObjectType():
					return cty.NilType, function.NewArgErrorf(0, "incompatible object type for conversion: %s", convert.MismatchMessage(gotTy, wantTy))
				default:
					return cty.NilType, function.NewArgErrorf(0, "cannot convert %s to %s", gotTy.FriendlyName(), wantTy.FriendlyNameForConstraint())
				}
			}
			return wantTy, nil
		},
		Impl: func(args []cty.Value, retType cty.Type) (cty.Value, error) {
			ret, err := convert.Convert(args[0], retType)
			if err != nil {
				val, _ := args[0].UnmarkDeep()
				gotTy := val.Type()
				switch {
				case args[0].HasMark(SensitiveMark):
					return cty.NilVal, function.NewArgErrorf(0, "cannot convert this sensitive %s to %s", gotTy.FriendlyName(), wantTy.FriendlyNameForConstraint())
				case gotTy == cty.String && wantTy == cty.Bool:
					what := "string"
					if !val.IsNull() {
						what = strconv.Quote(val.AsString())
					}
					return cty.NilVal, function.NewArgErrorf(0, `cannot convert %s to bool; only the strings "true" or "false" are allowed`, what)
				case gotTy == cty.String && wantTy == cty.Number:
					what := "string"
					if !val.IsNull() {
						what = strconv.Quote(val.AsString())
					}
					return cty.NilVal, function.NewArgErrorf(0, `cannot convert %s to number; given string must be a decimal representation of a number`, what)
				default:
					return cty.NilVal, function.NewArgErrorf(0, "cannot convert %s to %s", gotTy.FriendlyName(), wantTy.FriendlyNameForConstraint())
				}
			}
			return ret, nil
		},
	})
}

var toStringFunc = function.New(&function.Spec{
	Params: []function.Parameter{
		{Name: "value", Type: cty.DynamicPseudoType, AllowNull: true, AllowDynamicType: true},
	},
	Type: function.StaticReturnType(cty.String),
	Impl: func(args []cty.Value, retType cty.Type) (cty.Value, error) {
		val := args[0]
		if val.IsNull() {
			return cty.NullVal(cty.String), nil
		}
		switch val.Type() {
		case cty.String:
			return val, nil
		case cty.Number:
			return convert.Convert(val, cty.String)
		case cty.Bool:
			return cty.StringVal(fmt.Sprintf("%t", val.True())), nil
		// For complex types, JSON encode
		default:
			if !val.IsWhollyKnown() {
				return cty.UnknownVal(retType), nil
			}
			jsonBytes, err := json.Marshal(ctyToGo(val))
			if err != nil {
				return cty.NilVal, err
			}
			return cty.StringVal(string(jsonBytes)), nil
		}
	},
})

var typeFunc = function.New(&function.Spec{
	Params: []function.Parameter{
		{Name: "value", Type: cty.DynamicPseudoType},
	},
	Type: function.StaticReturnType(cty.String),
	Impl: func(args []cty.Value, retType cty.Type) (cty.Value, error) {
		return cty.StringVal(args[0].Type().FriendlyName()), nil
	},
})

// Helper to convert cty.Value to Go any
func ctyToGo(val cty.Value) any {
	if val.IsNull() {
		return nil
	}

	ty := val.Type()
	switch {
	case ty == cty.String:
		return val.AsString()
	case ty == cty.Number:
		f, _ := val.AsBigFloat().Float64()
		return f
	case ty == cty.Bool:
		return val.True()
	case ty.IsListType() || ty.IsTupleType() || ty.IsSetType():
		var result []any
		for it := val.ElementIterator(); it.Next(); {
			_, v := it.Element()
			result = append(result, ctyToGo(v))
		}
		return result
	case ty.IsMapType() || ty.IsObjectType():
		result := make(map[string]any)
		for it := val.ElementIterator(); it.Next(); {
			k, v := it.Element()
			result[k.AsString()] = ctyToGo(v)
		}
		return result
	default:
		return nil
	}
}

// Pulumi-specific functions

// pulumiResourceNameFunc returns the logical name of a Pulumi resource by
// extracting it from the resource's URN.
var pulumiResourceNameFunc = resourceUrnFuncHelper("pulumiresourcename", func(u urn.URN) (cty.Value, error) {
	return cty.StringVal(u.Name()), nil
})

// pulumiResourceTypeFunc returns the type token of a Pulumi resource by
// extracting it from the resource's URN.
var pulumiResourceTypeFunc = resourceUrnFuncHelper("pulumiresourcetype", func(u urn.URN) (cty.Value, error) {
	return cty.StringVal(u.Type().String()), nil
})

// pulumiResourceURNFunc returns the URN of a Pulumi resource.
var pulumiResourceURNFunc = resourceUrnFuncHelper("pulumiresourceurn", func(u urn.URN) (cty.Value, error) {
	return cty.StringVal(string(u)), nil
})

func resourceUrnFuncHelper(fnName string, f func(urn.URN) (cty.Value, error)) function.Function {
	return function.New(&function.Spec{
		Params: []function.Parameter{
			{
				Name:        "resource",
				Type:        cty.DynamicPseudoType,
				AllowMarked: true,
			},
		},
		Type: function.StaticReturnType(cty.String),
		Impl: func(args []cty.Value, retType cty.Type) (cty.Value, error) {
			res := args[0]
			if !res.IsKnown() {
				return cty.UnknownVal(cty.String), nil
			}
			if u, ok := ResourceReferenceURN(res); ok {
				return f(u)
			}
			return cty.NilVal, fmt.Errorf("%s: argument must be a resource reference", fnName)
		},
	})
}

// Asset and archive functions

func fileAssetFunc(baseDir string) function.Function {
	return function.New(&function.Spec{
		Params: []function.Parameter{{Name: "path", Type: cty.String}},
		Type:   function.StaticReturnType(AssetCapsuleType),
		Impl: func(args []cty.Value, retType cty.Type) (cty.Value, error) {
			path := args[0].AsString()
			a, err := asset.FromPathWithWD(path, baseDir)
			if err != nil {
				return cty.NilVal, fmt.Errorf("fileasset: %w", err)
			}
			return cty.CapsuleVal(AssetCapsuleType, a), nil
		},
	})
}

func fileArchiveFunc(baseDir string) function.Function {
	return function.New(&function.Spec{
		Params: []function.Parameter{{Name: "path", Type: cty.String}},
		Type:   function.StaticReturnType(ArchiveCapsuleType),
		Impl: func(args []cty.Value, retType cty.Type) (cty.Value, error) {
			path := args[0].AsString()
			a, err := archive.FromPathWithWD(path, baseDir)
			if err != nil {
				return cty.NilVal, fmt.Errorf("filearchive: %w", err)
			}
			return cty.CapsuleVal(ArchiveCapsuleType, a), nil
		},
	})
}

func stringAssetFunc() function.Function {
	return function.New(&function.Spec{
		Params: []function.Parameter{{Name: "text", Type: cty.String}},
		Type:   function.StaticReturnType(AssetCapsuleType),
		Impl: func(args []cty.Value, retType cty.Type) (cty.Value, error) {
			text := args[0].AsString()
			a := &asset.Asset{Sig: asset.AssetSig, Text: text}
			return cty.CapsuleVal(AssetCapsuleType, a), nil
		},
	})
}

func assetArchiveFunc() function.Function {
	return function.New(&function.Spec{
		Params: []function.Parameter{{Name: "assets", Type: cty.DynamicPseudoType}},
		Type:   function.StaticReturnType(ArchiveCapsuleType),
		Impl: func(args []cty.Value, retType cty.Type) (cty.Value, error) {
			val := args[0]
			if !val.Type().IsObjectType() && !val.Type().IsMapType() {
				return cty.NilVal, fmt.Errorf("assetarchive: argument must be an object or map")
			}
			assets := make(map[string]any)
			for it := val.ElementIterator(); it.Next(); {
				k, v := it.Element()
				key := k.AsString()
				switch {
				case v.Type().Equals(AssetCapsuleType):
					assets[key] = v.EncapsulatedValue().(*asset.Asset)
				case v.Type().Equals(ArchiveCapsuleType):
					assets[key] = v.EncapsulatedValue().(*archive.Archive)
				default:
					return cty.NilVal, fmt.Errorf("assetarchive: value for key %q must be an asset or archive, got %s",
						key, v.Type().FriendlyName())
				}
			}
			a := &archive.Archive{Sig: archive.ArchiveSig, Assets: assets}
			return cty.CapsuleVal(ArchiveCapsuleType, a), nil
		},
	})
}

func remoteAssetFunc() function.Function {
	return function.New(&function.Spec{
		Params: []function.Parameter{{Name: "uri", Type: cty.String}},
		Type:   function.StaticReturnType(AssetCapsuleType),
		Impl: func(args []cty.Value, retType cty.Type) (cty.Value, error) {
			uri := args[0].AsString()
			a, err := asset.FromURI(uri)
			if err != nil {
				return cty.NilVal, fmt.Errorf("remoteasset: %w", err)
			}
			return cty.CapsuleVal(AssetCapsuleType, a), nil
		},
	})
}

func remoteArchiveFunc() function.Function {
	return function.New(&function.Spec{
		Params: []function.Parameter{{Name: "uri", Type: cty.String}},
		Type:   function.StaticReturnType(ArchiveCapsuleType),
		Impl: func(args []cty.Value, retType cty.Type) (cty.Value, error) {
			uri := args[0].AsString()
			a, err := archive.FromURI(uri)
			if err != nil {
				return cty.NilVal, fmt.Errorf("remotearchive: %w", err)
			}
			return cty.CapsuleVal(ArchiveCapsuleType, a), nil
		},
	})
}
