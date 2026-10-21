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

package codegen

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/pulumi/pulumi-hcl/pkg/hcl/ast"
	"github.com/pulumi/pulumi-hcl/pkg/hcl/bridge"
	"github.com/pulumi/pulumi-hcl/pkg/hcl/comments"
	"github.com/pulumi/pulumi-hcl/pkg/hcl/eval"
	"github.com/pulumi/pulumi-hcl/pkg/hcl/packages"
	"github.com/pulumi/pulumi-hcl/pkg/hcl/parser"
	"github.com/pulumi/pulumi-hcl/pkg/hcl/transform"
	"github.com/pulumi/pulumi-terraform-bridge/v3/pkg/tfbridge"
	"github.com/pulumi/pulumi/pkg/v3/codegen"
	"github.com/pulumi/pulumi/pkg/v3/codegen/hcl2/model"
	"github.com/pulumi/pulumi/pkg/v3/codegen/pcl"
	"github.com/pulumi/pulumi/pkg/v3/codegen/schema"
	"github.com/pulumi/pulumi/sdk/v3/go/common/tokens"
	"github.com/pulumi/pulumi/sdk/v3/go/common/util/contract"
	"github.com/zclconf/go-cty/cty"
)

// GenerateProgramOption configures GenerateProgram.
type GenerateProgramOption func(*generateProgramOptions)

type generateProgramOptions struct {
	skipRequiredProvidersVersion bool
	insideComponent              bool
}

// insideComponentSource marks the generated program as a component's module
// source: every resource and nested module call pins its Pulumi logical name
// (prefixed via pulumi.module.name) so instances keep the "<parent>-<child>"
// names PCL codegen produces in every other language.
func insideComponentSource() GenerateProgramOption {
	return func(o *generateProgramOptions) { o.insideComponent = true }
}

// SkipRequiredProvidersVersion omits the `version` attribute from each entry
// of an emitted `terraform { required_providers { ... } }` block. The `source`
// attribute is still emitted so symbol resolution remains unambiguous. Use
// for snippets embedded in SDK documentation, where pinning a version would
// bake the SDK's in-development version into every regenerated docstring.
func SkipRequiredProvidersVersion() GenerateProgramOption {
	return func(o *generateProgramOptions) { o.skipRequiredProvidersVersion = true }
}

// GenerateProgram generates Terraform HCL source code from a bound PCL program.
//
// The output preserves the program's source-file structure: each PCL file
// produces a corresponding ".tf" file containing the nodes that were declared
// in it. Required-providers entries are placed in the first source file that
// declared each package via a `package` block; references with no declaring
// file are placed in main.tf (or, if no main.pp exists, the first file in
// alphabetical order).
//
// mappings supplies the bridge body mappings that decide which properties
// are written as nested blocks: the Pulumi schema of a bridged provider
// types a MaxItems=1 block as an object, the same as an object attribute.
func GenerateProgram(
	ctx context.Context, program *pcl.Program, mappings *packages.Resolver, opts ...GenerateProgramOption,
) (map[string][]byte, hcl.Diagnostics, error) {
	contract.Requiref(mappings != nil, "mappings", "must not be nil")
	var o generateProgramOptions
	for _, opt := range opts {
		opt(&o)
	}

	var diags hcl.Diagnostics

	gen := &generator{
		program:                      program,
		mappings:                     mappings,
		comments:                     buildProgramComments(program),
		skipRequiredProvidersVersion: o.skipRequiredProvidersVersion,
		insideComponent:              o.insideComponent,
	}

	for _, node := range program.Nodes {
		gen.collectInvokes(node)
		gen.collectCalls(node)
	}

	source := program.Source()
	fileOrder := orderedSourceFiles(source)
	if len(fileOrder) == 0 {
		fileOrder = []string{"main.pp"}
	}
	primaryFile := fileOrder[0]
	knownFile := func(name string) string {
		if slices.Contains(fileOrder, name) {
			return name
		}
		return primaryFile
	}

	pkgsByFile := packageDeclarationsByFile(source)
	providersByFile := assignProvidersToFiles(program.PackageReferences(), fileOrder, pkgsByFile)

	invokesByFile := map[string][]spilledDataSource{}
	for _, ds := range gen.invokeDataSources {
		invokesByFile[knownFile(ds.sourceFile)] = append(invokesByFile[knownFile(ds.sourceFile)], ds)
	}
	callsByFile := map[string][]spilledCall{}
	for _, cb := range gen.callBlocks {
		callsByFile[knownFile(cb.sourceFile)] = append(callsByFile[knownFile(cb.sourceFile)], cb)
	}
	nodesByFile := map[string][]pcl.Node{}
	for _, node := range program.Nodes {
		f := knownFile(nodeSourceFile(node))
		nodesByFile[f] = append(nodesByFile[f], node)
	}

	pulumiBlocksByFile := map[string]*pcl.PulumiBlock{}
	for _, node := range program.Nodes {
		if pb, ok := node.(*pcl.PulumiBlock); ok {
			pulumiBlocksByFile[knownFile(nodeSourceFile(node))] = pb
		}
	}

	files := map[string][]byte{}
	for _, srcName := range fileOrder {
		f := hclwrite.NewEmptyFile()
		body := f.Body()

		d := gen.genTerraformHeader(body, providersByFile[srcName], pulumiBlocksByFile[srcName])
		diags = append(diags, d...)

		for _, ds := range invokesByFile[srcName] {
			d := gen.genInvokeDataSource(ctx, body, ds)
			diags = append(diags, d...)
		}
		if len(invokesByFile[srcName]) > 0 {
			body.AppendNewline()
		}

		for _, cb := range callsByFile[srcName] {
			d := gen.genCallBlock(body, cb)
			diags = append(diags, d...)
		}
		if len(callsByFile[srcName]) > 0 {
			body.AppendNewline()
		}

		hasNodes := len(nodesByFile[srcName]) > 0
		for _, node := range nodesByFile[srcName] {
			gen.emitLeadingComments(body, node.SyntaxNode())
			switch n := node.(type) {
			case *pcl.Resource:
				d := gen.genResource(ctx, body, n)
				diags = append(diags, d...)
			case *pcl.OutputVariable:
				d := gen.genOutput(body, n)
				diags = append(diags, d...)
			case *pcl.ConfigVariable:
				d := gen.genConfigVariable(body, n)
				diags = append(diags, d...)
			case *pcl.LocalVariable:
				d := gen.genLocalVariable(body, n)
				diags = append(diags, d...)
			case *pcl.PulumiBlock:
				// Emitted by genTerraformHeader above.
			case *pcl.Component:
				d := gen.genModule(body, n)
				diags = append(diags, d...)
			default:
				diags = append(diags, &hcl.Diagnostic{
					Severity: hcl.DiagError,
					Summary:  "unsupported PCL node type",
					Detail:   fmt.Sprintf("node type %T is not yet supported", node),
				})
			}
		}

		if !hasNodes && len(invokesByFile[srcName]) == 0 && len(callsByFile[srcName]) == 0 &&
			len(providersByFile[srcName]) == 0 {
			continue
		}
		files[outputFileName(srcName)] = f.Bytes()
	}

	if len(files) == 0 {
		// A program with no source content still needs at least one file so the
		// runtime can locate a module. Emit an empty main.tf.
		files["main.tf"] = nil
	}

	for componentDir, component := range program.CollectComponents() {
		subFiles, d, err := GenerateProgram(ctx, component.Program, mappings, insideComponentSource())
		diags = append(diags, d...)
		if err != nil {
			return nil, diags, err
		}
		subDirName := filepath.Base(componentDir)
		for name, content := range subFiles {
			files[filepath.Join(subDirName, name)] = content
		}
	}
	return files, diags, nil
}

// orderedSourceFiles returns source filenames in a deterministic order:
// "main.pp" first if present, then alphabetical.
func orderedSourceFiles(source map[string]string) []string {
	names := make([]string, 0, len(source))
	for name := range source {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool {
		if names[i] == "main.pp" {
			return true
		}
		if names[j] == "main.pp" {
			return false
		}
		return names[i] < names[j]
	})
	return names
}

// packageDeclarationsByFile parses each source file and returns a map from
// source filename to the set of package alias names declared by `package
// "<alias>" { ... }` blocks in that file.
func packageDeclarationsByFile(source map[string]string) map[string]map[string]bool {
	out := map[string]map[string]bool{}
	for name, content := range source {
		file, fdiags := hclsyntax.ParseConfig([]byte(content), name, hcl.Pos{Byte: 0, Line: 1, Column: 1})
		if fdiags.HasErrors() || file == nil {
			continue
		}
		body, ok := file.Body.(*hclsyntax.Body)
		if !ok {
			continue
		}
		for _, blk := range body.Blocks {
			if blk.Type != "package" || len(blk.Labels) == 0 {
				continue
			}
			if out[name] == nil {
				out[name] = map[string]bool{}
			}
			out[name][blk.Labels[0]] = true
		}
	}
	return out
}

// assignProvidersToFiles maps each PackageReference to the source file that
// should contain its required_providers entry. Each provider is assigned to
// the first file (in fileOrder) that declared it via a `package` block;
// providers with no declaring file are assigned to the primary file (main.pp
// if present, otherwise the first file in fileOrder).
func assignProvidersToFiles(
	refs []schema.PackageReference,
	fileOrder []string,
	pkgsByFile map[string]map[string]bool,
) map[string][]schema.PackageReference {
	out := map[string][]schema.PackageReference{}
	primary := ""
	if len(fileOrder) > 0 {
		primary = fileOrder[0]
	}
	for _, ref := range refs {
		assigned := primary
		for _, fname := range fileOrder {
			if pkgsByFile[fname][ref.Name()] {
				assigned = fname
				break
			}
		}
		out[assigned] = append(out[assigned], ref)
	}
	return out
}

// nodeSourceFile returns the base filename of the source file that declared
// node. Falls back to "main.pp" when the node has no source location.
func nodeSourceFile(node pcl.Node) string {
	if syn := node.SyntaxNode(); syn != nil {
		if rng := syn.Range(); rng.Filename != "" {
			return rng.Filename
		}
	}
	return "main.pp"
}

// outputFileName converts a PCL source filename to its Terraform output counterpart
// by replacing the ".pp" extension with ".tf".
func outputFileName(srcName string) string {
	return strings.TrimSuffix(srcName, ".pp") + ".tf"
}

type rangeKind int

const (
	rangeKindNone    rangeKind = iota
	rangeKindCount             // bool/number → count
	rangeKindForEach           // list/map → for_each
)

// dynamicBlockContext tracks the iterator variables of a dynamic block so that
// references to them can be rewritten in scopeTraversalTokens.
type dynamicBlockContext struct {
	blockName     string
	keyVariable   *model.Variable
	valueVariable *model.Variable
}

// generator holds state during code generation, including invoke data sources.
type generator struct {
	program           *pcl.Program
	mappings          *packages.Resolver
	invokeDataSources []spilledDataSource
	callBlocks        []spilledCall
	currentRangeKind  rangeKind
	dynamicBlock      *dynamicBlockContext
	// forDataBlock, when non-nil, indicates we are generating the body of a
	// data block hoisted from an invoke inside a for-comprehension. References
	// to its KeyVariable / ValueVariable are rewritten to each.key / each.value.
	forDataBlock *model.ForExpression
	// comments maps source filenames to leading-comment maps built from the
	// PCL source files of this program.
	comments map[string]*comments.Map
	// skipRequiredProvidersVersion suppresses the `version` attribute on
	// each `required_providers` entry. See SkipRequiredProvidersVersion.
	skipRequiredProvidersVersion bool
	// insideComponent marks this program as a component's module source.
	// See insideComponentSource.
	insideComponent bool
}

// buildProgramComments parses the source of every PCL file in the program and
// returns a per-filename map of leading comments. `package` blocks are
// excluded as anchors so any comments preceding them flow to the next emitted
// node.
func buildProgramComments(program *pcl.Program) map[string]*comments.Map {
	out := map[string]*comments.Map{}
	for name, content := range program.Source() {
		src := []byte(content)
		file, fdiags := hclsyntax.ParseConfig(src, name, hcl.Pos{Byte: 0, Line: 1, Column: 1})
		if fdiags.HasErrors() || file == nil {
			continue
		}
		body, ok := file.Body.(*hclsyntax.Body)
		if !ok {
			continue
		}
		out[name] = comments.Build(src, name, body, "package")
	}
	return out
}

// emitLeadingComments writes any source comments that precede node into body.
// node must be the SyntaxNode() of a bound PCL element; the binder guarantees
// it has a real syntax pointer.
func (g *generator) emitLeadingComments(body *hclwrite.Body, node hclsyntax.Node) {
	rng := node.Range()
	g.comments[rng.Filename].Emit(body, rng.Start.Byte)
}

// withTrailing returns valueTokens with the same-line trailing comment for
// the attribute identified by node (if any) appended. The combined tokens are
// suitable for hclwrite.Body.SetAttributeRaw, which keeps the comment on the
// same line as the value.
func (g *generator) withTrailing(valueTokens hclwrite.Tokens, node hclsyntax.Node) hclwrite.Tokens {
	rng := node.Range()
	return g.comments[rng.Filename].AppendTrailing(valueTokens, rng.Start.Byte)
}

type spilledDataSource struct {
	expr *model.FunctionCallExpression
	name string
	// parentResource is non-nil when the invoke closes over range.* from this
	// resource's for_each/count. The data block is emitted with a matching
	// for_each, and the resource's reference is indexed with [each.key].
	parentResource *pcl.Resource
	// enclosingForExpr is non-nil when the invoke closes over iteration
	// variables of a PCL for-comprehension. The data block is emitted with a
	// matching for_each over that comprehension's collection, and the
	// reference is indexed by the comprehension's key variable.
	enclosingForExpr *model.ForExpression
	// outerForExprs are ancestors of enclosingForExpr whose iter vars its
	// collection transitively references; the data block's for_each wraps
	// the collection in nested fors over these (outermost first) and
	// flattens, so the refs stay bound at top-level scope.
	outerForExprs []*model.ForExpression
	// sourceFile is the source PCL filename of the node that produced this
	// invoke; the spilled data block is emitted in the corresponding output
	// HCL file so file structure is preserved.
	sourceFile string
}

// stdTFFunc describes how to inline a pulumi-std invoke as a TF builtin function
// call. Used to reverse the forward mapping that pulumi-converter-terraform applies
// when turning TF builtins into std:index:* invokes.
type stdTFFunc struct {
	name   string
	inputs []string
}

// stdInvokeToTFFunction maps a pulumi-std invoke token back to its TF builtin
// function. Mirrors the forward mapping in pulumi-converter-terraform's
// tfFunctionStd. Only non-variadic (paramArgs=false) entries appear here —
// variadic functions receive their trailing args packed into a single list by
// the forward direction, which cannot be reliably unpacked without knowing the
// list length statically, so those remain hoisted as data blocks.
var stdInvokeToTFFunction = map[string]stdTFFunc{
	"std:index:abs":              {name: "abs", inputs: []string{"input"}},
	"std:index:abspath":          {name: "abspath", inputs: []string{"input"}},
	"std:index:alltrue":          {name: "alltrue", inputs: []string{"input"}},
	"std:index:anytrue":          {name: "anytrue", inputs: []string{"input"}},
	"std:index:base64decode":     {name: "base64decode", inputs: []string{"input"}},
	"std:index:base64encode":     {name: "base64encode", inputs: []string{"input"}},
	"std:index:base64gzip":       {name: "base64gzip", inputs: []string{"input"}},
	"std:index:base64sha256":     {name: "base64sha256", inputs: []string{"input"}},
	"std:index:base64sha512":     {name: "base64sha512", inputs: []string{"input"}},
	"std:index:basename":         {name: "basename", inputs: []string{"input"}},
	"std:index:bcrypt":           {name: "bcrypt", inputs: []string{"input", "cost"}},
	"std:index:ceil":             {name: "ceil", inputs: []string{"input"}},
	"std:index:chomp":            {name: "chomp", inputs: []string{"input"}},
	"std:index:chunklist":        {name: "chunklist", inputs: []string{"input", "size"}},
	"std:index:compact":          {name: "compact", inputs: []string{"input"}},
	"std:index:contains":         {name: "contains", inputs: []string{"input", "element"}},
	"std:index:cidrhost":         {name: "cidrhost", inputs: []string{"input", "host"}},
	"std:index:cidrnetmask":      {name: "cidrnetmask", inputs: []string{"input"}},
	"std:index:cidrsubnet":       {name: "cidrsubnet", inputs: []string{"input", "newbits", "netnum"}},
	"std:index:csvdecode":        {name: "csvdecode", inputs: []string{"input"}},
	"std:index:dirname":          {name: "dirname", inputs: []string{"input"}},
	"std:index:distinct":         {name: "distinct", inputs: []string{"input"}},
	"std:index:endswith":         {name: "endswith", inputs: []string{"input", "suffix"}},
	"std:index:file":             {name: "file", inputs: []string{"input"}},
	"std:index:filebase64":       {name: "filebase64", inputs: []string{"input"}},
	"std:index:filebase64sha256": {name: "filebase64sha256", inputs: []string{"input"}},
	"std:index:filebase64sha512": {name: "filebase64sha512", inputs: []string{"input"}},
	"std:index:fileexists":       {name: "fileexists", inputs: []string{"input"}},
	"std:index:filemd5":          {name: "filemd5", inputs: []string{"input"}},
	"std:index:filesha1":         {name: "filesha1", inputs: []string{"input"}},
	"std:index:filesha256":       {name: "filesha256", inputs: []string{"input"}},
	"std:index:filesha512":       {name: "filesha512", inputs: []string{"input"}},
	"std:index:flatten":          {name: "flatten", inputs: []string{"input"}},
	"std:index:floor":            {name: "floor", inputs: []string{"input"}},
	"std:index:indent":           {name: "indent", inputs: []string{"spaces", "input"}},
	"std:index:join":             {name: "join", inputs: []string{"separator", "input"}},
	"std:index:jsondecode":       {name: "jsondecode", inputs: []string{"input"}},
	"std:index:keys":             {name: "keys", inputs: []string{"input"}},
	"std:index:log":              {name: "log", inputs: []string{"base", "input"}},
	"std:index:lookup":           {name: "lookup", inputs: []string{"map", "key", "default"}},
	"std:index:lower":            {name: "lower", inputs: []string{"input"}},
	"std:index:md5":              {name: "md5", inputs: []string{"input"}},
	"std:index:parseint":         {name: "parseint", inputs: []string{"input", "base"}},
	"std:index:pathexpand":       {name: "pathexpand", inputs: []string{"input"}},
	"std:index:pow":              {name: "pow", inputs: []string{"base", "exponent"}},
	"std:index:range":            {name: "range", inputs: []string{"limit", "start", "step"}},
	"std:index:regex":            {name: "regex", inputs: []string{"pattern", "string"}},
	"std:index:regexall":         {name: "regexall", inputs: []string{"pattern", "string"}},
	"std:index:replace":          {name: "replace", inputs: []string{"text", "search", "replace"}},
	"std:index:rsadecrypt":       {name: "rsadecrypt", inputs: []string{"cipherText", "key"}},
	"std:index:sha1":             {name: "sha1", inputs: []string{"input"}},
	"std:index:sha256":           {name: "sha256", inputs: []string{"input"}},
	"std:index:sha512":           {name: "sha512", inputs: []string{"input"}},
	"std:index:signum":           {name: "signum", inputs: []string{"input"}},
	"std:index:slice":            {name: "slice", inputs: []string{"list", "from", "to"}},
	"std:index:sort":             {name: "sort", inputs: []string{"input"}},
	"std:index:split":            {name: "split", inputs: []string{"separator", "text"}},
	"std:index:startswith":       {name: "startswith", inputs: []string{"input", "prefix"}},
	"std:index:strrev":           {name: "strrev", inputs: []string{"input"}},
	"std:index:substr":           {name: "substr", inputs: []string{"input", "offset", "length"}},
	"std:index:sum":              {name: "sum", inputs: []string{"input"}},
	"std:index:timeadd":          {name: "timeadd", inputs: []string{"duration", "timestamp"}},
	"std:index:timecmp":          {name: "timecmp", inputs: []string{"timestampa", "timestampb"}},
	"std:index:timestamp":        {name: "timestamp", inputs: []string{}},
	"std:index:title":            {name: "title", inputs: []string{"input"}},
	"std:index:tobool":           {name: "tobool", inputs: []string{"input"}},
	"std:index:toset":            {name: "toset", inputs: []string{"input"}},
	"std:index:transpose":        {name: "transpose", inputs: []string{"input"}},
	"std:index:trim":             {name: "trim", inputs: []string{"input", "cutset"}},
	"std:index:trimprefix":       {name: "trimprefix", inputs: []string{"input", "prefix"}},
	"std:index:trimspace":        {name: "trimspace", inputs: []string{"input"}},
	"std:index:trimsuffix":       {name: "trimsuffix", inputs: []string{"input", "suffix"}},
	"std:index:upper":            {name: "upper", inputs: []string{"input"}},
	"std:index:urlencode":        {name: "urlencode", inputs: []string{"input"}},
	"std:index:uuid":             {name: "uuid", inputs: []string{}},
}

// lookupStdTFFunc finds a TF-builtin mapping for a pulumi-std invoke token.
// It accepts both the fully-qualified "std:index:name" form and the canonical
// "std::name" form that pcl's binder produces after token canonicalization.
func lookupStdTFFunc(token string) (stdTFFunc, bool) {
	if fn, ok := stdInvokeToTFFunction[token]; ok {
		return fn, true
	}
	pkg, _, member, diags := pcl.DecomposeToken(token, hcl.Range{})
	if diags.HasErrors() || pkg != "std" {
		return stdTFFunc{}, false
	}
	fn, ok := stdInvokeToTFFunction["std:index:"+member]
	return fn, ok
}

// inlinableStdFunc reports whether the given invoke call can be inlined as a TF
// builtin function. It checks both the token and the argument shape (the args
// object must be an ObjectConsExpression).
func inlinableStdFunc(call *model.FunctionCallExpression) (stdTFFunc, bool) {
	if call.Name != pcl.Invoke || len(call.Args) < 2 {
		return stdTFFunc{}, false
	}
	token, ok := extractStringLiteral(call.Args[0])
	if !ok {
		return stdTFFunc{}, false
	}
	fn, ok := lookupStdTFFunc(token)
	if !ok {
		return stdTFFunc{}, false
	}
	if _, ok := call.Args[1].(*model.ObjectConsExpression); !ok {
		return stdTFFunc{}, false
	}
	return fn, true
}

// invokeReferencesRange reports whether any scope traversal inside the invoke
// is rooted at `range`, meaning the invoke closes over an enclosing resource's
// for_each / count iterator.
func invokeReferencesRange(call *model.FunctionCallExpression) bool {
	var found bool
	_, _ = model.VisitExpression(call, nil, func(e model.Expression) (model.Expression, hcl.Diagnostics) {
		if st, ok := e.(*model.ScopeTraversalExpression); ok {
			if st.Traversal.RootName() == "range" {
				found = true
			}
		}
		return e, nil
	})
	return found
}

// invokeReferencesForVars reports whether any scope traversal inside the
// invoke references the given for-expression's iteration variables.
func invokeReferencesForVars(call *model.FunctionCallExpression, fe *model.ForExpression) bool {
	var found bool
	_, _ = model.VisitExpression(call, nil, func(e model.Expression) (model.Expression, hcl.Diagnostics) {
		st, ok := e.(*model.ScopeTraversalExpression)
		if !ok || len(st.Parts) == 0 {
			return e, nil
		}
		if v, ok := st.Parts[0].(*model.Variable); ok {
			if v == fe.ValueVariable || (fe.KeyVariable != nil && v == fe.KeyVariable) {
				found = true
			}
		}
		return e, nil
	})
	return found
}

func rootVariablesIn(expr model.Expression) map[*model.Variable]bool {
	out := map[*model.Variable]bool{}
	_, _ = model.VisitExpression(expr, nil, func(e model.Expression) (model.Expression, hcl.Diagnostics) {
		st, ok := e.(*model.ScopeTraversalExpression)
		if !ok || len(st.Parts) == 0 {
			return e, nil
		}
		if v, ok := st.Parts[0].(*model.Variable); ok {
			out[v] = true
		}
		return e, nil
	})
	return out
}

// outerForExprsNeededFor returns the subset of ancestors whose iter vars are
// transitively referenced by chosen.Collection, ordered outermost first.
func outerForExprsNeededFor(chosen *model.ForExpression, ancestors []*model.ForExpression) []*model.ForExpression {
	needed := rootVariablesIn(chosen.Collection)
	included := map[*model.ForExpression]bool{}
	for changed := true; changed; {
		changed = false
		for i := len(ancestors) - 1; i >= 0; i-- {
			outer := ancestors[i]
			if included[outer] {
				continue
			}
			if needed[outer.ValueVariable] || (outer.KeyVariable != nil && needed[outer.KeyVariable]) {
				included[outer] = true
				changed = true
				for v := range rootVariablesIn(outer.Collection) {
					needed[v] = true
				}
			}
		}
	}
	var out []*model.ForExpression
	for _, outer := range ancestors {
		if included[outer] {
			out = append(out, outer)
		}
	}
	return out
}

type spilledCall struct {
	expr         *model.FunctionCallExpression
	resourceName string
	methodName   string
	sourceFile   string
}

// genTerraformHeader emits the per-file `terraform { ... }` block, combining
// required-provider entries (sourced from the program's PackageReferences)
// with a PCL `pulumi { requiredVersionRange = ... }` node (PCL syntax) when present in
// this file. Returns no diags except those produced by genTerraformBlockBody.
//
// When neither providers nor a PulumiBlock node are present, no block is
// emitted (and no trailing newline is added).
func (g *generator) genTerraformHeader(
	body *hclwrite.Body, pkgRefs []schema.PackageReference, pb *pcl.PulumiBlock,
) hcl.Diagnostics {
	providers := make([]schema.PackageReference, 0, len(pkgRefs))
	for _, ref := range pkgRefs {
		// The "pulumi" package is built-in and should not be listed in required_providers.
		if ref.Name() == "pulumi" {
			continue
		}
		providers = append(providers, ref)
	}

	hasVersion := pb != nil && pb.RequiredVersion != nil
	if len(providers) == 0 && !hasVersion {
		return nil
	}

	block := body.AppendNewBlock("terraform", nil)
	var diags hcl.Diagnostics
	if hasVersion {
		d := g.genExpression(block.Body(), "required_version_range", pb.RequiredVersion, schema.StringType)
		diags = append(diags, d...)
	}
	if len(providers) > 0 {
		reqProviders := block.Body().AppendNewBlock("required_providers", nil)
		for _, ref := range providers {
			// All Pulumi packages emit under the `pulumi/` prefix so the HCL
			// runtime can distinguish them from terraform-provider sources.
			// Namespaced packages become a triple: pulumi/<ns>/<name>.
			source := "pulumi/" + ref.Name()
			if ns := ref.Namespace(); ns != "" {
				source = "pulumi/" + ns + "/" + ref.Name()
			}
			attrs := map[string]cty.Value{"source": cty.StringVal(source)}
			if v := ref.Version(); v != nil && !g.skipRequiredProvidersVersion {
				attrs["version"] = cty.StringVal(v.String())
			}
			reqProviders.Body().SetAttributeValue(ref.Name(), cty.ObjectVal(attrs))
		}
	}
	body.AppendNewline()
	return diags
}

// collectInvokes walks the node and collects all invoke function calls.
// Resource inputs pass the enclosing resource so invokes that close over its
// range iterator can be identified.
func (g *generator) collectInvokes(node pcl.Node) {
	srcFile := nodeSourceFile(node)
	switch n := node.(type) {
	case *pcl.Resource:
		for _, attr := range n.Inputs {
			g.collectInvokesInExpr(attr.Value, n, srcFile)
		}
		// Options are evaluated outside the resource's range scope, so no
		// enclosing resource is passed.
		if n.Options != nil {
			for _, expr := range []model.Expression{
				n.Options.Range, n.Options.ImportID, n.Options.ReplacementTrigger,
				n.Options.Version, n.Options.PluginDownloadURL,
			} {
				if expr != nil {
					g.collectInvokesInExpr(expr, nil, srcFile)
				}
			}
		}
	case *pcl.OutputVariable:
		g.collectInvokesInExpr(n.Value, nil, srcFile)
	case *pcl.LocalVariable:
		// When a local variable's value is directly an invoke that gets
		// promoted to a data source, name the data source after the local
		// variable (e.g. `local x = invoke(...)` becomes
		// `data "<token>" "x"`) instead of the auto-generated `invoke_N`.
		if call, ok := n.Definition.Value.(*model.FunctionCallExpression); ok && call.Name == pcl.Invoke {
			_, inlinable := inlinableStdFunc(call)
			if !inlinable && !call.Signature.MultiArgumentInputs {
				for _, arg := range call.Args {
					g.collectInvokesInExpr(arg, nil, srcFile)
				}
				g.invokeDataSources = append(g.invokeDataSources, spilledDataSource{
					expr:       call,
					name:       n.LogicalName(),
					sourceFile: srcFile,
				})
				return
			}
		}
		g.collectInvokesInExpr(n.Definition.Value, nil, srcFile)
	}
}

// collectInvokesInExpr walks an expression tree and collects invoke calls.
// Invokes that map to TF builtins (std:index:*) are skipped — they will be
// inlined at their reference site. Invokes that close over range.* from the
// enclosing resource are tagged so the hoisted data block can carry a matching
// for_each.
func (g *generator) collectInvokesInExpr(expr model.Expression, parent *pcl.Resource, sourceFile string) {
	var forStack []*model.ForExpression
	pre := func(e model.Expression) (model.Expression, hcl.Diagnostics) {
		if fe, ok := e.(*model.ForExpression); ok {
			forStack = append(forStack, fe)
		}
		return e, nil
	}
	post := func(e model.Expression) (model.Expression, hcl.Diagnostics) {
		if fe, ok := e.(*model.ForExpression); ok {
			contract.Assertf(len(forStack) > 0 && forStack[len(forStack)-1] == fe,
				"for-expression stack out of sync")
			forStack = forStack[:len(forStack)-1]
		}
		if call, ok := e.(*model.FunctionCallExpression); ok && call.Name == pcl.Invoke {
			if _, inlinable := inlinableStdFunc(call); inlinable {
				return e, nil
			}
			// Multi-argument invokes project as provider-defined functions
			// (provider::<name>::<fn>(...)), emitted inline rather than spilled
			// to a data block.
			if call.Signature.MultiArgumentInputs {
				return e, nil
			}
			ds := spilledDataSource{
				expr:       call,
				name:       fmt.Sprintf("invoke_%d", len(g.invokeDataSources)),
				sourceFile: sourceFile,
			}
			if parent != nil && parent.Options != nil && parent.Options.Range != nil &&
				invokeReferencesRange(call) {
				ds.parentResource = parent
			} else {
				// Walk from innermost for outward to find the one whose
				// iteration variables this invoke closes over.
				for i := len(forStack) - 1; i >= 0; i-- {
					if invokeReferencesForVars(call, forStack[i]) {
						ds.enclosingForExpr = forStack[i]
						ds.outerForExprs = outerForExprsNeededFor(
							forStack[i], forStack[:i])
						break
					}
				}
			}
			g.invokeDataSources = append(g.invokeDataSources, ds)
		}
		return e, nil
	}
	_, diags := model.VisitExpression(expr, pre, post)
	contract.Assertf(len(diags) == 0, "we never return diags")
}

// collectCalls walks the node and collects all call function expressions.
func (g *generator) collectCalls(node pcl.Node) {
	srcFile := nodeSourceFile(node)
	switch n := node.(type) {
	case *pcl.Resource:
		for _, attr := range n.Inputs {
			g.collectCallsInExpr(attr.Value, srcFile)
		}
		if n.Options != nil {
			for _, expr := range []model.Expression{
				n.Options.Aliases, n.Options.Range, n.Options.Parent,
				n.Options.Provider, n.Options.Providers, n.Options.DependsOn,
				n.Options.Protect, n.Options.RetainOnDelete, n.Options.IgnoreChanges,
				n.Options.HideDiffs, n.Options.ReplaceOnChanges, n.Options.DeleteBeforeReplace,
				n.Options.AdditionalSecretOutputs, n.Options.Version, n.Options.CustomTimeouts,
				n.Options.PluginDownloadURL, n.Options.DeletedWith, n.Options.ReplaceWith,
				n.Options.ImportID, n.Options.ReplacementTrigger, n.Options.EnvVarMappings,
				n.Options.Hooks,
			} {
				if expr != nil {
					g.collectCallsInExpr(expr, srcFile)
				}
			}
		}
	case *pcl.OutputVariable:
		g.collectCallsInExpr(n.Value, srcFile)
	case *pcl.LocalVariable:
		g.collectCallsInExpr(n.Definition.Value, srcFile)
	}
}

// collectCallsInExpr walks an expression tree and collects call expressions.
func (g *generator) collectCallsInExpr(expr model.Expression, sourceFile string) {
	_, diags := model.VisitExpression(expr, nil, func(expr model.Expression) (model.Expression, hcl.Diagnostics) {
		if call, ok := expr.(*model.FunctionCallExpression); ok {
			if call.Name == pcl.Call && len(call.Args) >= 2 {
				resourceName, methodName, ok := g.extractCallArgs(call)
				if ok {
					snakeMethod := transform.SnakeCaseFromPulumiCase(methodName)
					// Deduplicate: only add if we haven't already seen this resourceName.methodName
					duplicate := false
					for _, cb := range g.callBlocks {
						if cb.resourceName == resourceName && cb.methodName == snakeMethod {
							duplicate = true
							break
						}
					}
					if !duplicate {
						g.callBlocks = append(g.callBlocks, spilledCall{
							expr:         call,
							resourceName: resourceName,
							methodName:   snakeMethod,
							sourceFile:   sourceFile,
						})
					}
				}
			}
		}
		return expr, nil
	})
	contract.Assertf(len(diags) == 0, "we never return diags")
}

// extractCallArgs extracts (resourceName, methodName) from a pcl.Call expression.
// Returns ok=false if the args are not the expected form.
func (g *generator) extractCallArgs(call *model.FunctionCallExpression) (resourceName, methodName string, ok bool) {
	if len(call.Args) < 2 {
		return "", "", false
	}
	// First arg: resource reference
	scopeTraversal, isScopeTraversal := call.Args[0].(*model.ScopeTraversalExpression)
	if !isScopeTraversal || len(scopeTraversal.Parts) == 0 {
		return "", "", false
	}
	switch part := scopeTraversal.Parts[0].(type) {
	case *pcl.Resource:
		resourceName = part.LogicalName()
	default:
		// Could be a provider - use the traversal root name
		resourceName = scopeTraversal.Traversal.RootName()
	}
	// Second arg: method name string literal
	methodName, ok = extractStringLiteral(call.Args[1])
	return resourceName, methodName, ok
}

// genCallBlock generates a call block for a method invocation.
func (g *generator) genCallBlock(body *hclwrite.Body, cb spilledCall) hcl.Diagnostics {
	block := body.AppendNewBlock("call", []string{cb.resourceName, cb.methodName})

	if len(cb.expr.Args) < 3 {
		return nil
	}

	var diags hcl.Diagnostics
	argsExpr := cb.expr.Args[2]
	if objExpr, ok := argsExpr.(*model.ObjectConsExpression); ok {
		for _, item := range objExpr.Items {
			keyLit, ok := item.Key.(*model.LiteralValueExpression)
			if !ok {
				continue
			}
			keyName := keyLit.Value.AsString()
			hclName := transform.SnakeCaseFromPulumiCase(keyName)
			d := g.genExpression(block.Body(), hclName, item.Value, schema.AnyType)
			diags = append(diags, d...)
		}
	}

	return diags
}

func invokeSchemaPackage(f *schema.Function) schema.PackageReference {
	if f == nil {
		return nil
	}
	return f.PackageReference
}

// invokeSchemaToken returns the source-form token from the schema when available.
// PCL canonicalizes tokens by applying ModuleFormat once during binding, which
// strips the regex-matched suffix from the module component. Re-applying
// TokenToModule on the canonicalized token would produce an incorrect (empty)
// module, so codegen passes the schema's stored token instead.
func invokeSchemaToken(f *schema.Function, fallback string) string {
	if f == nil {
		return fallback
	}
	return f.Token
}

func resourceSchemaPackage(r *schema.Resource) schema.PackageReference {
	if r == nil {
		return nil
	}
	return r.PackageReference
}

func resourceSchemaToken(r *schema.Resource, fallback string) string {
	if r == nil {
		return fallback
	}
	return r.Token
}

// lookupInvokeSchema resolves an invoke token against the program's loaded
// package references and returns the function schema. It returns the
// canonical token (with the module re-filled when PCL has stripped "index")
// alongside the schema. A nil schema with a nil diagnostic means the package
// is loaded but the function is not defined; callers should treat this as
// "schema unavailable" rather than an error.
func (g *generator) lookupInvokeSchema(token string) (*schema.Function, string, *hcl.Diagnostic) {
	for _, p := range g.program.PackageReferences() {
		if p.Name() != tokens.Type(token).Package().String() {
			continue
		}
		pkg, mod, name, _ := pcl.DecomposeToken(token, hcl.Range{})
		// PCL normalizes "pkg:index:name" to "pkg::name", so DecomposeToken
		// re-fills "index" for an empty module.
		canonical := pkg + ":" + mod + ":" + name
		f, ok, err := p.Functions().Get(canonical)
		if err != nil {
			return nil, canonical, &hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  "failed to get invoke " + canonical,
				Detail:   err.Error(),
			}
		}
		if !ok {
			// When PCL binds functions, it applies meta.moduleFormat to the
			// token. When we do a schema lookup, we need to use the original
			// token. Find the source-form token by matching each function's
			// TokenToModule against our module.
			for it := p.Functions().Range(); it.Next(); {
				t := it.Token()
				ftPkg, _, ftName, _ := pcl.DecomposeToken(t, hcl.Range{})
				if ftPkg == pkg && ftName == name && p.TokenToModule(t) == mod {
					ff, fErr := it.Function()
					if fErr != nil {
						return nil, canonical, &hcl.Diagnostic{
							Severity: hcl.DiagError,
							Summary:  "failed to load invoke " + t,
							Detail:   fErr.Error(),
						}
					}
					f, ok = ff, true
					break
				}
			}
		}
		if ok {
			return f, canonical, nil
		}
		return nil, canonical, nil
	}
	return nil, token, nil
}

// genInvokeDataSource generates a data source block for an invoke call.
// If ds.parentResource is set, the block is emitted with a for_each matching
// the resource's range so range.* references rewrite correctly.
func (g *generator) genInvokeDataSource(ctx context.Context, body *hclwrite.Body, ds spilledDataSource) hcl.Diagnostics {
	invoke := ds.expr
	dsName := ds.name
	if len(invoke.Args) < 2 {
		return hcl.Diagnostics{{
			Severity: hcl.DiagError,
			Summary:  "invalid invoke call",
			Detail:   "invoke requires at least 2 arguments: token and args",
		}}
	}

	// First arg is the token (function name)
	token, ok := extractStringLiteral(invoke.Args[0])
	if !ok {
		return hcl.Diagnostics{{
			Severity: hcl.DiagError,
			Summary:  "invalid invoke call",
			Detail:   "invoke token must be a string literal",
		}}
	}

	invokeSchema, schemaToken, diag := g.lookupInvokeSchema(token)
	if diag != nil {
		return hcl.Diagnostics{diag}
	}
	token = schemaToken

	dsType, diags := packages.PulumiFunctionTokenToHCL(invokeSchemaPackage(invokeSchema), invokeSchemaToken(invokeSchema, token))
	if diags.HasErrors() {
		return diags
	}

	block := body.AppendNewBlock("data", []string{dsType, dsName})

	// If this invoke closed over range.* from a for_each/count resource,
	// give the data block its own matching range so the rewrite in
	// scopeTraversalTokens (range.* → each.* / count.index) is well-bound.
	if ds.parentResource != nil && ds.parentResource.Options != nil && ds.parentResource.Options.Range != nil {
		_, d := g.genRange(block.Body(), ds.parentResource.Options.Range)
		diags = append(diags, d...)
		defer func() { g.currentRangeKind = rangeKindNone }()
	}

	// If this invoke closed over a for-comprehension's iteration variables,
	// emit a matching for_each over the comprehension's collection. References
	// to the for-expression's variables inside the body will be rewritten to
	// each.key / each.value via scopeTraversalTokens.
	if ds.enclosingForExpr != nil {
		collTokens, d := g.exprTokens(ds.enclosingForExpr.Collection, schema.AnyType)
		diags = append(diags, d...)
		if d.HasErrors() {
			return diags
		}
		// Wrap innermost-outer last so its iter vars are in scope for the
		// inner collection. flatten is recursive, so one call collapses all levels.
		if len(ds.outerForExprs) > 0 {
			for i := len(ds.outerForExprs) - 1; i >= 0; i-- {
				outer := ds.outerForExprs[i]
				outerCollTokens, od := g.exprTokens(outer.Collection, schema.AnyType)
				diags = append(diags, od...)
				if od.HasErrors() {
					return diags
				}
				collTokens = forExprWrapperTokens(outer, outerCollTokens, collTokens)
			}
			collTokens = hclwrite.TokensForFunctionCall("flatten", collTokens)
		}
		// HCL for_each requires a map or set. When the PCL comprehension has no
		// KeyVariable the collection is a list/tuple, so wrap it with toset() to
		// coerce into a set keyed by its own elements (each.key == each.value).
		if ds.enclosingForExpr.KeyVariable == nil {
			collTokens = hclwrite.TokensForFunctionCall("toset", collTokens)
		}
		block.Body().SetAttributeRaw("for_each", collTokens)
		g.forDataBlock = ds.enclosingForExpr
		defer func() { g.forDataBlock = nil }()
	}

	// Second arg is the args object
	if len(invoke.Args) >= 2 {
		argsExpr := invoke.Args[1]
		if objExpr, ok := argsExpr.(*model.ObjectConsExpression); ok {
			mapping := g.mappings.DataSourceBodyMapping(ctx, dsType)
			for _, item := range objExpr.Items {
				keyLit, ok := item.Key.(*model.LiteralValueExpression)
				if !ok {
					continue
				}
				keyName := keyLit.Value.AsString()
				var propType schema.Type
				if invokeSchema != nil && invokeSchema.Inputs != nil {
					if inputSchema, ok := invokeSchema.Inputs.Property(keyName); ok {
						propType = inputSchema.Type
					}
				}
				hclName := transform.SnakeCaseFromPulumiCase(keyName)
				target := inputBody(block.Body(), hclName, parser.DataReservedNames)
				fm := mapping.Lookup(hclName)
				if objType, ok := transform.HCLBlockType(propType, fm); ok {
					d := g.genBlocks(target, hclName, item.Value, objType, nestedMapping(fm))
					diags = append(diags, d...)
				} else {
					d := g.genExpression(target, hclName, item.Value, propType)
					diags = append(diags, d...)
				}
			}
		}
	}

	// Third arg is the invoke options object. Terraform-standard options
	// (provider, depends_on) stay on the data block; Pulumi-specific options
	// (parent, version, plugin_download_url) go in a nested `pulumi { }` block.
	if len(invoke.Args) >= 3 {
		if optsExpr, ok := invoke.Args[2].(*model.ObjectConsExpression); ok {
			var pulumiBlockBody *hclwrite.Body
			pulumiBody := func() *hclwrite.Body {
				if pulumiBlockBody == nil {
					pulumiBlockBody = block.Body().AppendNewBlock("pulumi", nil).Body()
				}
				return pulumiBlockBody
			}
			for _, item := range optsExpr.Items {
				keyLit, ok := item.Key.(*model.LiteralValueExpression)
				if !ok {
					continue
				}
				switch keyLit.Value.AsString() {
				case "provider":
					tokens, d := g.exprTokens(item.Value, schema.AnyType)
					diags = append(diags, d...)
					if !d.HasErrors() {
						block.Body().SetAttributeRaw("provider", tokens)
					}
				case "dependsOn":
					tokens, d := g.exprTokens(item.Value, schema.AnyType)
					diags = append(diags, d...)
					if !d.HasErrors() {
						block.Body().SetAttributeRaw("depends_on", tokens)
					}
				case "parent":
					tokens, d := g.exprTokens(item.Value, schema.AnyType)
					diags = append(diags, d...)
					if !d.HasErrors() {
						pulumiBody().SetAttributeRaw("parent", tokens)
					}
				case "version":
					tokens, d := g.exprTokens(item.Value, schema.StringType)
					diags = append(diags, d...)
					if !d.HasErrors() {
						pulumiBody().SetAttributeRaw("version", tokens)
					}
				case "pluginDownloadUrl":
					tokens, d := g.exprTokens(item.Value, schema.StringType)
					diags = append(diags, d...)
					if !d.HasErrors() {
						pulumiBody().SetAttributeRaw("plugin_download_url", tokens)
					}
				}
			}
		}
	}

	return diags
}

func (g *generator) genResource(ctx context.Context, body *hclwrite.Body, r *pcl.Resource) hcl.Diagnostics {
	defer func() { g.currentRangeKind = rangeKindNone }()

	token, _ := r.GetToken()

	if r.Schema != nil && r.Schema.IsProvider {
		return g.genProvider(ctx, body, r)
	}

	hclType, d := packages.PulumiResourceTokenToHCL(resourceSchemaPackage(r.Schema), resourceSchemaToken(r.Schema, token))
	if d.HasErrors() {
		return d
	}
	block := body.AppendNewBlock("resource", []string{hclType, r.LogicalName()})
	var diags hcl.Diagnostics

	d = g.genResourceOptions(block.Body(), r)
	diags = append(diags, d...)

	d = g.genInputs(block.Body(), r, g.mappings.ResourceBodyMapping(ctx, hclType), parser.ResourceReservedNames)
	return append(diags, d...)
}

// genInputs writes r's input properties to body. An input whose HCL name is
// in reserved goes in the block's `_` escaping block instead, so the runtime
// reads it as a property rather than as a meta-argument.
func (g *generator) genInputs(
	body *hclwrite.Body, r *pcl.Resource, mapping *bridge.BodyMapping, reserved map[string]bool,
) hcl.Diagnostics {
	var inputs []*schema.Property
	if r.Schema != nil {
		inputs = r.Schema.InputProperties
	}

	var diags hcl.Diagnostics
	for _, attr := range r.Inputs {
		inputType := findPropertyType(inputs, attr.Name)
		hclName := transform.SnakeCaseFromPulumiCase(attr.Name)
		target := inputBody(body, hclName, reserved)
		g.emitLeadingComments(target, attr.SyntaxNode())
		fm := mapping.Lookup(hclName)
		if objType, ok := transform.HCLBlockType(inputType, fm); ok {
			d := g.genBlocks(target, hclName, attr.Value, objType, nestedMapping(fm))
			diags = append(diags, d...)
		} else {
			d := g.genAttributeWithTrailing(target, hclName, attr.Value, inputType, attr.SyntaxNode())
			diags = append(diags, d...)
		}
	}
	return diags
}

// inputBody returns the body an argument named name is written to: body
// itself, or its `_` escaping block when name is one of the block type's
// reserved meta-argument names.
func inputBody(body *hclwrite.Body, name string, reserved map[string]bool) *hclwrite.Body {
	if !reserved[name] {
		return body
	}
	if escape := body.FirstMatchingBlock("_", nil); escape != nil {
		return escape.Body()
	}
	return body.AppendNewBlock("_", nil).Body()
}

// genProvider emits a `provider "<pkg>" { ... }` block for a PCL provider
// resource (token "pulumi:providers:<pkg>").
func (g *generator) genProvider(ctx context.Context, body *hclwrite.Body, r *pcl.Resource) hcl.Diagnostics {
	contract.Assertf(r.Schema == nil || r.Schema.IsProvider, "genProvider not given provider")
	token, _ := r.GetToken()
	pkgName := token
	if i := strings.LastIndex(token, ":"); i >= 0 {
		pkgName = token[i+1:]
	}
	block := body.AppendNewBlock("provider", []string{pkgName})
	if r.LogicalName() != "" && r.LogicalName() != pkgName {
		block.Body().SetAttributeValue("alias", cty.StringVal(r.LogicalName()))
	}

	var diags hcl.Diagnostics

	// Pulumi-specific options go in a nested `pulumi { }` block so they cannot
	// collide with the provider's own configuration attributes. `alias` and
	// `for_each` are Terraform-standard meta-arguments and stay at the top
	// level.
	opts := r.Options
	if opts != nil {
		if opts.Range != nil {
			d := g.genProviderRange(block.Body(), opts.Range)
			diags = append(diags, d...)
		}
		var pulumiBlockBody *hclwrite.Body
		pulumiBody := func() *hclwrite.Body {
			if pulumiBlockBody == nil {
				pulumiBlockBody = block.Body().AppendNewBlock("pulumi", nil).Body()
			}
			return pulumiBlockBody
		}
		if opts.PluginDownloadURL != nil {
			tokens, d := g.exprTokens(opts.PluginDownloadURL, schema.StringType)
			diags = append(diags, d...)
			if !d.HasErrors() {
				pulumiBody().SetAttributeRaw("plugin_download_url", tokens)
			}
		}
		if opts.Version != nil {
			tokens, d := g.exprTokens(opts.Version, schema.StringType)
			diags = append(diags, d...)
			if !d.HasErrors() {
				pulumiBody().SetAttributeRaw("version", tokens)
			}
		}
		if opts.AdditionalSecretOutputs != nil {
			g.genPropertyPathTraversalList(pulumiBody(), "additional_secret_outputs", opts.AdditionalSecretOutputs)
		}
		if opts.EnvVarMappings != nil {
			tokens, d := g.exprTokens(opts.EnvVarMappings, schema.AnyType)
			diags = append(diags, d...)
			if !d.HasErrors() {
				pulumiBody().SetAttributeRaw("env_var_mappings", tokens)
			}
		}
	}

	d := g.genInputs(block.Body(), r, g.mappings.ProviderConfigBodyMapping(ctx, pkgName), parser.ProviderReservedNames)
	return append(diags, d...)
}

// genResourceOptions generates HCL meta-arguments for a resource's options.
func (g *generator) genResourceOptions(body *hclwrite.Body, r *pcl.Resource) hcl.Diagnostics {
	var diags hcl.Diagnostics
	opts := r.Options

	// Collect schema-based replaceOnChanges property paths in camelCase (Pulumi convention).
	var schemaReplaceOnChanges []string
	if r.Schema != nil {
		schemaReplaceProps, _ := r.Schema.ReplaceOnChanges()
		// Keep property names in camelCase so the engine can match them against the diff.
		schemaReplaceOnChanges = schema.PropertyListJoinToString(schemaReplaceProps,
			func(s string) string { return s })
	}

	// pulumiBody lazily appends the `pulumi { }` block on first use so it is
	// omitted entirely when there are no Pulumi-specific options.
	var pulumiBlockBody *hclwrite.Body
	pulumiBody := func() *hclwrite.Body {
		if pulumiBlockBody == nil {
			pulumiBlockBody = body.AppendNewBlock("pulumi", nil).Body()
		}
		return pulumiBlockBody
	}

	var optReplaceOnChanges model.Expression
	if opts != nil {
		optReplaceOnChanges = opts.ReplaceOnChanges
	}
	emitReplaceOnChanges := func() {
		if len(schemaReplaceOnChanges) > 0 || len(extractPropertyNames(optReplaceOnChanges)) > 0 {
			g.genReplaceOnChanges(pulumiBody(), schemaReplaceOnChanges, optReplaceOnChanges, &diags)
		}
	}

	// Pin per-instance names to the "<parent>-<name>-<key>" scheme the other
	// languages' generated programs use; the runtime would otherwise derive
	// Terraform-style "<parent>.<name>[<key>]" names.
	pinName := func(naming instanceNaming) {
		if tokens := pinnedNameTokens(g.insideComponent, r.LogicalName(), naming); tokens != nil {
			pulumiBody().SetAttributeRaw("name", tokens)
		}
	}

	if opts == nil {
		pinName(instanceNamingNone)
		emitReplaceOnChanges()
		g.genLifecycleBlock(body, nil, &diags)
		return diags
	}

	// Terraform-standard meta-arguments stay at the top level.
	naming := instanceNamingNone
	if opts.Range != nil {
		var d hcl.Diagnostics
		naming, d = g.genRange(body, opts.Range)
		diags = append(diags, d...)
	}
	pinName(naming)

	if opts.Provider != nil {
		tokens, d := g.exprTokens(opts.Provider, schema.AnyType)
		diags = append(diags, d...)
		if !d.HasErrors() {
			body.SetAttributeRaw("provider", tokens)
		}
	}

	if opts.Providers != nil {
		tokens, d := g.genProvidersTokens(opts.Providers)
		diags = append(diags, d...)
		if !d.HasErrors() {
			body.SetAttributeRaw("providers", tokens)
		}
	}

	if opts.DependsOn != nil {
		tokens, d := g.exprTokens(opts.DependsOn, schema.AnyType)
		diags = append(diags, d...)
		if !d.HasErrors() {
			body.SetAttributeRaw("depends_on", tokens)
		}
	}

	// Pulumi-specific options go in the nested `pulumi { }` block.
	if opts.Parent != nil {
		tokens, d := g.exprTokens(opts.Parent, schema.AnyType)
		diags = append(diags, d...)
		if !d.HasErrors() {
			pulumiBody().SetAttributeRaw("parent", tokens)
		}
	}

	if opts.AdditionalSecretOutputs != nil {
		g.genPropertyPathTraversalList(pulumiBody(), "additional_secret_outputs", opts.AdditionalSecretOutputs)
	}

	if opts.Protect != nil {
		tokens, d := g.exprTokens(opts.Protect, schema.BoolType)
		diags = append(diags, d...)
		if !d.HasErrors() {
			pulumiBody().SetAttributeRaw("protect", tokens)
		}
	}

	if opts.RetainOnDelete != nil {
		tokens, d := g.exprTokens(opts.RetainOnDelete, schema.BoolType)
		diags = append(diags, d...)
		if !d.HasErrors() {
			pulumiBody().SetAttributeRaw("retain_on_delete", tokens)
		}
	}

	if opts.DeletedWith != nil {
		tokens, d := g.exprTokens(opts.DeletedWith, schema.AnyType)
		diags = append(diags, d...)
		if !d.HasErrors() {
			pulumiBody().SetAttributeRaw("deleted_with", tokens)
		}
	}

	if opts.ReplaceWith != nil {
		tokens, d := g.exprTokens(opts.ReplaceWith, schema.AnyType)
		diags = append(diags, d...)
		if !d.HasErrors() {
			pulumiBody().SetAttributeRaw("replace_with", tokens)
		}
	}

	if opts.HideDiffs != nil {
		g.genPropertyPathTraversalList(pulumiBody(), "hide_diffs", opts.HideDiffs)
	}

	emitReplaceOnChanges()

	if opts.ImportID != nil {
		tokens, d := g.exprTokens(opts.ImportID, schema.StringType)
		diags = append(diags, d...)
		if !d.HasErrors() {
			pulumiBody().SetAttributeRaw("import_id", tokens)
		}
	}

	if opts.EnvVarMappings != nil {
		tokens, d := g.exprTokens(opts.EnvVarMappings, schema.AnyType)
		diags = append(diags, d...)
		if !d.HasErrors() {
			pulumiBody().SetAttributeRaw("env_var_mappings", tokens)
		}
	}

	// HCL doesn't bake versions into generated code, so always emit version when specified.
	if opts.Version != nil {
		tokens, d := g.exprTokens(opts.Version, schema.StringType)
		diags = append(diags, d...)
		if !d.HasErrors() {
			pulumiBody().SetAttributeRaw("version", tokens)
		}
	}

	if opts.PluginDownloadURL != nil && pcl.NeedsPluginDownloadURLResourceOption(opts.PluginDownloadURL, r.Schema) {
		tokens, d := g.exprTokens(opts.PluginDownloadURL, schema.StringType)
		diags = append(diags, d...)
		if !d.HasErrors() {
			pulumiBody().SetAttributeRaw("plugin_download_url", tokens)
		}
	}

	if opts.Aliases != nil {
		g.genAliases(pulumiBody(), opts.Aliases, &diags)
	}

	// Block-form meta-arguments come after attributes, at the top level.
	g.genLifecycleBlock(body, opts, &diags)

	if opts.CustomTimeouts != nil {
		timeoutsBlock := body.AppendNewBlock("timeouts", nil)
		if obj, ok := opts.CustomTimeouts.(*model.ObjectConsExpression); ok {
			for _, item := range obj.Items {
				keyName, ok := extractStringLiteral(item.Key)
				if !ok {
					continue
				}
				hclName := strings.ToLower(keyName)
				tokens, d := g.exprTokens(item.Value, schema.StringType)
				diags = append(diags, d...)
				if !d.HasErrors() {
					timeoutsBlock.Body().SetAttributeRaw(hclName, tokens)
				}
			}
		}
	}

	return diags
}

// instanceNaming describes how a range shapes the Pulumi logical names of the
// instances it creates.
type instanceNaming int

const (
	instanceNamingNone  instanceNaming = iota // bool range: one unsuffixed instance
	instanceNamingIndex                       // number count: keyed by count.index
	instanceNamingKey                         // for_each: keyed by each.key
)

// genRange emits a count or for_each meta-argument based on the PCL range
// expression type, and reports how the range keys its instances' names.
func (g *generator) genRange(body *hclwrite.Body, rangeExpr model.Expression) (instanceNaming, hcl.Diagnostics) {
	rangeType := model.ResolveOutputs(rangeExpr.Type())

	switch {
	case model.InputType(model.BoolType).ConversionFrom(rangeType) == model.SafeConversion:
		tokens, d := g.exprTokens(rangeExpr, schema.AnyType)
		if d.HasErrors() {
			return instanceNamingNone, d
		}
		body.SetAttributeRaw("count", tokens)
		g.currentRangeKind = rangeKindCount
		return instanceNamingNone, nil

	case model.InputType(model.NumberType).ConversionFrom(rangeType) == model.SafeConversion:
		tokens, d := g.exprTokens(rangeExpr, schema.AnyType)
		if d.HasErrors() {
			return instanceNamingNone, d
		}
		body.SetAttributeRaw("count", tokens)
		g.currentRangeKind = rangeKindCount
		return instanceNamingIndex, nil

	default:
		return instanceNamingKey, g.genForEach(body, rangeExpr, rangeType)
	}
}

// genForEach emits a collection-valued PCL range option as a `for_each`
// meta-argument. A list range becomes a map keyed by the stringified index so
// `range.key` keeps indexing it.
func (g *generator) genForEach(body *hclwrite.Body, rangeExpr model.Expression, rangeType model.Type) hcl.Diagnostics {
	exprTokens, d := g.exprTokens(rangeExpr, schema.AnyType)
	if d.HasErrors() {
		return d
	}
	var tokens hclwrite.Tokens
	switch rangeType.(type) {
	case *model.ListType, *model.TupleType:
		tokens = wrapListAsMapForEach(exprTokens)
	default:
		tokens = exprTokens
	}
	body.SetAttributeRaw("for_each", tokens)
	g.currentRangeKind = rangeKindForEach
	return nil
}

// genProviderRange emits a provider resource's range option. A provider block
// accepts `for_each` only: an integer or boolean range has no `count`
// equivalent there, so it is a diagnostic.
func (g *generator) genProviderRange(body *hclwrite.Body, rangeExpr model.Expression) hcl.Diagnostics {
	rangeType := model.ResolveOutputs(rangeExpr.Type())
	if model.InputType(model.BoolType).ConversionFrom(rangeType) == model.SafeConversion ||
		model.InputType(model.NumberType).ConversionFrom(rangeType) == model.SafeConversion {
		return hcl.Diagnostics{{
			Severity: hcl.DiagError,
			Summary:  "unsupported provider range",
			Detail: "a provider block accepts only a collection-valued range (`for_each`); " +
				"a boolean or integer range has no `count` equivalent on a provider",
			Subject: rangeExpr.SyntaxNode().Range().Ptr(),
		}}
	}
	return g.genForEach(body, rangeExpr, rangeType)
}

// pinnedNameTokens builds the `pulumi { name = ... }` template that pins a
// generated resource's or module call's Pulumi logical names to the scheme
// PCL codegen produces in every other language: inside a component source the
// name is prefixed with the enclosing instance's name via pulumi.module.name,
// and ranged blocks get a "-${count.index}" / "-${each.key}" suffix (e.g.
// "${pulumi.module.name}-res-${each.key}"). Returns nil when the derived
// runtime name already matches (a root-level block with no range, or with a
// bool range's single unsuffixed instance).
func pinnedNameTokens(insideComponent bool, logicalName string, naming instanceNaming) hclwrite.Tokens {
	prefix := ""
	if insideComponent {
		prefix = "${pulumi.module.name}-"
	}
	suffix := ""
	switch naming {
	case instanceNamingIndex:
		suffix = "-${count.index}"
	case instanceNamingKey:
		suffix = "-${each.key}"
	}
	if prefix == "" && suffix == "" {
		return nil
	}
	return hclwrite.Tokens{
		{Type: hclsyntax.TokenQuotedLit, Bytes: fmt.Appendf(nil, `"%s%s%s"`, prefix, logicalName, suffix)},
	}
}

// wrapListAsMapForEach generates `{ for __key, __value in <expr> : tostring(__key) => __value }`.
func wrapListAsMapForEach(listTokens hclwrite.Tokens) hclwrite.Tokens {
	tok := func(t hclsyntax.TokenType, s string) *hclwrite.Token {
		return &hclwrite.Token{Type: t, Bytes: []byte(s)}
	}
	tokens := hclwrite.Tokens{
		tok(hclsyntax.TokenOBrace, "{"),
		tok(hclsyntax.TokenIdent, " for"),
		tok(hclsyntax.TokenIdent, " __key"),
		tok(hclsyntax.TokenComma, ","),
		tok(hclsyntax.TokenIdent, " __value"),
		tok(hclsyntax.TokenIdent, " in "),
	}
	tokens = append(tokens, listTokens...)
	tokens = append(tokens,
		tok(hclsyntax.TokenColon, " :"),
		tok(hclsyntax.TokenIdent, " tostring"),
		tok(hclsyntax.TokenOParen, "("),
		tok(hclsyntax.TokenIdent, "__key"),
		tok(hclsyntax.TokenCParen, ")"),
		tok(hclsyntax.TokenFatArrow, " =>"),
		tok(hclsyntax.TokenIdent, " __value"),
		tok(hclsyntax.TokenCBrace, " }"),
	)
	return tokens
}

// genProviders generates the HCL `providers` attribute as a list.
// PCL providers can be a list [p1, p2] or a map {pkg = p}; we always emit a list
// since the package name is recoverable from the provider resource type at runtime.
func (g *generator) genProvidersTokens(providers model.Expression) (hclwrite.Tokens, hcl.Diagnostics) {
	if providers, ok := providers.(*model.ObjectConsExpression); ok {
		elems := make([]model.Expression, 0, len(providers.Items))
		for _, v := range providers.Items {
			elems = append(elems, v.Value)
		}
		return g.exprTokens(&model.TupleConsExpression{
			Expressions: elems,
		}, &schema.ArrayType{ElementType: schema.AnyResourceType})
	}

	return g.exprTokens(providers, &schema.ArrayType{ElementType: schema.AnyResourceType})
}

// genAliases generates the HCL `aliases` attribute from a PCL aliases expression.
// PCL aliases can be URN strings or spec objects with fields like name, noParent, parent.
// HCL uses snake_case keys (no_parent, parent_urn) and parent is a resource URN string.
func (g *generator) genAliases(body *hclwrite.Body, aliases model.Expression, diags *hcl.Diagnostics) {
	tuple, ok := aliases.(*model.TupleConsExpression)
	if !ok {
		// Fallback: emit as-is
		t, d := g.exprTokens(aliases, schema.AnyType)
		*diags = append(*diags, d...)
		if !d.HasErrors() {
			body.SetAttributeRaw("aliases", t)
		}
		return
	}

	tokens := hclwrite.Tokens{
		{Type: hclsyntax.TokenOBrack, Bytes: []byte("[")},
	}
	for i, elem := range tuple.Expressions {
		if i > 0 {
			tokens = append(tokens, &hclwrite.Token{Type: hclsyntax.TokenComma, Bytes: []byte(",")})
		}
		elemTokens, d := g.aliasElemTokens(elem)
		*diags = append(*diags, d...)
		if d.HasErrors() {
			return
		}
		if len(elemTokens) > 0 {
			elemTokens[0].SpacesBefore = 1
		}
		tokens = append(tokens, elemTokens...)
	}
	tokens = append(tokens, &hclwrite.Token{Type: hclsyntax.TokenCBrack, Bytes: []byte("]")})
	body.SetAttributeRaw("aliases", tokens)
}

// aliasElemTokens generates HCL tokens for a single alias element.
// String elements are emitted as-is. Object elements have their keys renamed:
// noParent → no_parent, parent (resource ref) → parent_urn (resource URN string).
func (g *generator) aliasElemTokens(elem model.Expression) (hclwrite.Tokens, hcl.Diagnostics) {
	obj, ok := elem.(*model.ObjectConsExpression)
	if !ok {
		return g.exprTokens(elem, schema.AnyType)
	}

	tokens := hclwrite.Tokens{
		{Type: hclsyntax.TokenOBrace, Bytes: []byte("{")},
	}
	if len(obj.Items) == 0 {
		tokens = append(tokens, &hclwrite.Token{Type: hclsyntax.TokenCBrace, Bytes: []byte("}")})
		return tokens, nil
	}
	tokens = append(tokens, &hclwrite.Token{Type: hclsyntax.TokenNewline, Bytes: []byte("\n")})

	var diags hcl.Diagnostics
	for _, item := range obj.Items {
		keyStr, _ := extractStringLiteral(item.Key)

		var hclKey string
		var valTokens hclwrite.Tokens
		var d hcl.Diagnostics

		switch keyStr {
		case "noParent":
			hclKey = "no_parent"
			valTokens, d = g.exprTokens(item.Value, schema.BoolType)
		case "parent":
			// Transform resource reference to parent_urn = pulumiresourceurn(resource_type.name)
			hclKey = "parent_urn"
			baseTokens, d2 := g.exprTokens(item.Value, schema.AnyType)
			diags = append(diags, d2...)
			if d2.HasErrors() {
				return nil, diags
			}
			valTokens = pulumiResourceURNCallTokens(baseTokens)
		default:
			hclKey = transform.SnakeCaseFromPulumiCase(keyStr)
			valTokens, d = g.exprTokens(item.Value, schema.AnyType)
		}

		diags = append(diags, d...)
		if d.HasErrors() {
			return nil, diags
		}

		keyTokens := hclwrite.TokensForIdentifier(hclKey)
		keyTokens[0].SpacesBefore = 2
		tokens = append(tokens, keyTokens...)
		tokens = append(tokens, &hclwrite.Token{
			Type: hclsyntax.TokenEqual, Bytes: []byte("="), SpacesBefore: 1,
		})
		if len(valTokens) > 0 {
			valTokens[0].SpacesBefore = 1
		}
		tokens = append(tokens, valTokens...)
		tokens = append(tokens, &hclwrite.Token{Type: hclsyntax.TokenNewline, Bytes: []byte("\n")})
	}
	tokens = append(tokens, &hclwrite.Token{Type: hclsyntax.TokenCBrace, Bytes: []byte("}")})
	return tokens, diags
}

// extractPropertyNames extracts camelCase property names from a PCL expression like [value, otherProp].
func extractPropertyNames(expr model.Expression) []string {
	var names []string
	if expr == nil {
		return names
	}
	tuple, ok := expr.(*model.TupleConsExpression)
	if !ok {
		return names
	}
	for _, elem := range tuple.Expressions {
		if s, ok := extractStringLiteral(elem); ok {
			names = append(names, s)
		} else if traversal, ok := elem.(*model.ScopeTraversalExpression); ok {
			// PCL property traversals are already in camelCase (e.g., value, replaceProp)
			names = append(names, traversal.Traversal.RootName())
		}
	}
	return names
}

// genPropertyPathTraversalList generates an HCL list attribute for property
// paths emitted as bare traversals in TF snake_case (e.g.,
// hide_diffs = [replace_prop]), matching the form of ignore_changes. Used for
// hide_diffs, replace_on_changes, and additional_secret_outputs.
func (g *generator) genPropertyPathTraversalList(body *hclwrite.Body, attrName string, optsExpr model.Expression) {
	names := extractPropertyNames(optsExpr)
	if len(names) == 0 {
		return
	}
	body.SetAttributeRaw(attrName, makeTraversalListTokens(snakeCasePropertyPaths(names)))
}

// snakeCasePropertyPaths converts Pulumi camelCase property paths to their TF
// snake_case form, segment by segment, so emitted property-path options name
// properties in the same case as the resource's snake_case attributes.
func snakeCasePropertyPaths(paths []string) []string {
	out := make([]string, len(paths))
	for i, p := range paths {
		segs := strings.Split(p, ".")
		for j, s := range segs {
			segs[j] = transform.SnakeCaseFromPulumiCase(s)
		}
		out[i] = strings.Join(segs, ".")
	}
	return out
}

// makeTraversalListTokens generates HCL tokens for a list of property-path
// traversals: [a, b.c]. hide_diffs and replace_on_changes name properties by
// their attribute path, matching the bare-traversal form of ignore_changes.
func makeTraversalListTokens(paths []string) hclwrite.Tokens {
	tokens := hclwrite.Tokens{
		{Type: hclsyntax.TokenOBrack, Bytes: []byte("[")},
	}
	for i, p := range paths {
		if i > 0 {
			tokens = append(tokens, &hclwrite.Token{Type: hclsyntax.TokenComma, Bytes: []byte(",")})
		}
		tokens = append(tokens, &hclwrite.Token{
			Type: hclsyntax.TokenIdent, Bytes: []byte(p), SpacesBefore: 1,
		})
	}
	tokens = append(tokens, &hclwrite.Token{Type: hclsyntax.TokenCBrack, Bytes: []byte("]")})
	return tokens
}

// genLifecycleBlock writes the resource's `lifecycle { ... }` block.
//
// Pulumi defaults to create-then-delete; Terraform defaults to the opposite.
// To make the Pulumi default explicit, we emit `create_before_destroy = true`
// whenever `deleteBeforeReplace` is unset (or literal `false`). When it's
// literal `true`, the TF default already matches the user's intent, so we omit
// `create_before_destroy` entirely. Non-literal expressions fall through to
// `create_before_destroy = !<expr>`.
//
// If the block ends up empty, it's not emitted.
func (g *generator) genLifecycleBlock(body *hclwrite.Body, opts *pcl.ResourceOptions, diags *hcl.Diagnostics) {
	type attr struct {
		name   string
		tokens hclwrite.Tokens
	}
	var attrs []attr

	if opts != nil {
		if opts.IgnoreChanges != nil {
			tokens, d := g.exprTokens(opts.IgnoreChanges, schema.AnyType)
			*diags = append(*diags, d...)
			if !d.HasErrors() {
				attrs = append(attrs, attr{"ignore_changes", tokens})
			}
		}
		if opts.ReplacementTrigger != nil {
			tokens, d := g.exprTokens(opts.ReplacementTrigger, schema.AnyType)
			*diags = append(*diags, d...)
			if !d.HasErrors() {
				// TF expects a list. PCL's `replacementTrigger` is a single
				// expression, so wrap it in a 1-element list.
				attrs = append(attrs, attr{
					"replace_triggered_by",
					hclwrite.TokensForTuple([]hclwrite.Tokens{tokens}),
				})
			}
		}
	}

	cbdTokens := createBeforeDestroyTokens(g, opts, diags)
	if cbdTokens != nil {
		attrs = append(attrs, attr{"create_before_destroy", cbdTokens})
	}

	if len(attrs) == 0 {
		return
	}
	block := body.AppendNewBlock("lifecycle", nil).Body()
	for _, a := range attrs {
		block.SetAttributeRaw(a.name, a.tokens)
	}
}

// createBeforeDestroyTokens computes the value of `create_before_destroy` from
// PCL's `deleteBeforeReplace`. Returns nil when the attribute should not be
// emitted (TF default already matches the user's request).
func createBeforeDestroyTokens(g *generator, opts *pcl.ResourceOptions, diags *hcl.Diagnostics) hclwrite.Tokens {
	if opts == nil || opts.DeleteBeforeReplace == nil {
		return hclwrite.Tokens{{Type: hclsyntax.TokenIdent, Bytes: []byte("true")}}
	}
	if lit, ok := opts.DeleteBeforeReplace.(*model.LiteralValueExpression); ok && lit.Value.Type() == cty.Bool {
		if lit.Value.True() {
			return nil
		}
		return hclwrite.Tokens{{Type: hclsyntax.TokenIdent, Bytes: []byte("true")}}
	}
	tokens, d := g.exprTokens(opts.DeleteBeforeReplace, schema.BoolType)
	*diags = append(*diags, d...)
	if d.HasErrors() {
		return nil
	}
	return append(hclwrite.Tokens{
		{Type: hclsyntax.TokenBang, Bytes: []byte("!")},
	}, tokens...)
}

// genReplaceOnChanges generates the replace_on_changes attribute, merging
// schema-based and option-based paths. Paths are emitted as bare traversals in
// TF snake_case, matching the resource's snake_case attribute names.
func (g *generator) genReplaceOnChanges(body *hclwrite.Body, schemaPaths []string, optsExpr model.Expression, diags *hcl.Diagnostics) {
	optPaths := extractPropertyNames(optsExpr)
	if len(schemaPaths) == 0 && len(optPaths) == 0 {
		return
	}

	// Merge and deduplicate paths.
	seen := make(map[string]bool)
	var allPaths []string
	for _, p := range schemaPaths {
		if !seen[p] {
			seen[p] = true
			allPaths = append(allPaths, p)
		}
	}
	for _, p := range optPaths {
		if !seen[p] {
			seen[p] = true
			allPaths = append(allPaths, p)
		}
	}

	body.SetAttributeRaw("replace_on_changes", makeTraversalListTokens(snakeCasePropertyPaths(allPaths)))
}

func (g *generator) genConfigVariable(body *hclwrite.Body, cv *pcl.ConfigVariable) hcl.Diagnostics {
	block := body.AppendNewBlock("variable", []string{cv.LogicalName()})

	// Emit a type constraint. An explicitly typed config keeps its declared type;
	// an untyped one is `any`, so its structured config value is parsed as HCL
	// rather than kept as a literal string (HCL treats a typeless `variable {}` as
	// VariableParseLiteral, which would not decode an object or list value).
	hclTypeStr := "any"
	if len(cv.SyntaxNode().(*hclsyntax.Block).Labels) == 2 {
		hclTypeStr = pclTypeToHCL(cv.Type())
	}
	block.Body().SetAttributeRaw("type", hclwrite.Tokens{
		{Type: hclsyntax.TokenIdent, Bytes: []byte(hclTypeStr)},
	})

	if attr, ok := cv.Definition.Body.Attribute("default"); ok {
		g.emitLeadingComments(block.Body(), attr.SyntaxNode())
		diags := g.genAttributeWithTrailing(block.Body(), "default", cv.DefaultValue, schema.AnyType, attr.SyntaxNode())
		if diags.HasErrors() {
			return diags
		}
	}

	if attr, ok := cv.Definition.Body.Attribute("description"); ok && cv.Description != "" {
		g.emitLeadingComments(block.Body(), attr.SyntaxNode())
		block.Body().SetAttributeRaw("description",
			g.withTrailing(hclwrite.TokensForValue(cty.StringVal(cv.Description)), attr.SyntaxNode()))
	}

	return nil
}

// pclTypeToHCL converts a PCL model.Type to an HCL type constraint string.
// HCL config values are always nullable here, so union(T, None) wrappers
// produced by PCL's optional() are stripped.
func pclTypeToHCL(t model.Type) string {
	if out, ok := t.(*model.OutputType); ok {
		return pclTypeToHCL(out.ElementType)
	}
	if union, ok := t.(*model.UnionType); ok {
		nonNone := make([]model.Type, 0, len(union.ElementTypes))
		for _, e := range union.ElementTypes {
			if e != model.NoneType {
				nonNone = append(nonNone, e)
			}
		}
		if len(nonNone) == 1 {
			return pclTypeToHCL(nonNone[0])
		}
		return "any"
	}
	switch t {
	case model.StringType:
		return "string"
	case model.BoolType:
		return "bool"
	case model.NumberType, model.IntType:
		return "number"
	case model.DynamicType:
		return "any"
	}
	switch tt := t.(type) {
	case *model.ListType:
		return "list(" + pclTypeToHCL(tt.ElementType) + ")"
	case *model.MapType:
		return "map(" + pclTypeToHCL(tt.ElementType) + ")"
	case *model.SetType:
		return "set(" + pclTypeToHCL(tt.ElementType) + ")"
	case *model.ObjectType:
		keys := make([]string, 0, len(tt.Properties))
		for k := range tt.Properties {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, len(keys))
		for i, k := range keys {
			parts[i] = k + "=" + pclTypeToHCL(tt.Properties[k])
		}
		return "object({" + strings.Join(parts, ", ") + "})"
	case *model.TupleType:
		elems := make([]string, len(tt.ElementTypes))
		for i, et := range tt.ElementTypes {
			elems[i] = pclTypeToHCL(et)
		}
		return "tuple([" + strings.Join(elems, ", ") + "])"
	}
	return "any"
}

func (g *generator) genLocalVariable(body *hclwrite.Body, lv *pcl.LocalVariable) hcl.Diagnostics {
	if g.isInvokeLocal(lv) {
		return nil
	}
	block := body.AppendNewBlock("locals", nil)
	return g.genExpression(block.Body(), lv.LogicalName(), lv.Definition.Value, schema.AnyType)
}

// isInvokeLocal reports whether a local variable's definition is an invoke call
// that has been promoted to a data source block.
func (g *generator) isInvokeLocal(lv *pcl.LocalVariable) bool {
	call, ok := lv.Definition.Value.(*model.FunctionCallExpression)
	if !ok || call.Name != pcl.Invoke {
		return false
	}
	for _, ds := range g.invokeDataSources {
		if ds.expr == call {
			return true
		}
	}
	return false
}

func (g *generator) genOutput(body *hclwrite.Body, ov *pcl.OutputVariable) hcl.Diagnostics {
	block := body.AppendNewBlock("output", []string{ov.LogicalName()})
	attr, ok := ov.Definition.Body.Attribute("value")
	if !ok {
		return g.genExpression(block.Body(), "value", ov.Value, schema.AnyType)
	}
	g.emitLeadingComments(block.Body(), attr.SyntaxNode())
	return g.genAttributeWithTrailing(block.Body(), "value", ov.Value, schema.AnyType, attr.SyntaxNode())
}

func (g *generator) genModule(body *hclwrite.Body, c *pcl.Component) hcl.Diagnostics {
	defer func() { g.currentRangeKind = rangeKindNone }()

	block := body.AppendNewBlock("module", []string{c.LogicalName()})
	source := "./" + filepath.Base(c.DirPath())
	block.Body().SetAttributeValue("source", cty.StringVal(source))
	var diags hcl.Diagnostics

	naming := instanceNamingNone
	if c.Options != nil && c.Options.Range != nil {
		var d hcl.Diagnostics
		naming, d = g.genRange(block.Body(), c.Options.Range)
		diags = append(diags, d...)
	}
	var pulumiBlockBody *hclwrite.Body
	pulumiBody := func() *hclwrite.Body {
		if pulumiBlockBody == nil {
			pulumiBlockBody = block.Body().AppendNewBlock("pulumi", nil).Body()
		}
		return pulumiBlockBody
	}

	// Pin the component instances' names the same way ranged resources are
	// pinned, so nested components keep the "<parent>-<child>" names the
	// other languages produce.
	if tokens := pinnedNameTokens(g.insideComponent, c.LogicalName(), naming); tokens != nil {
		pulumiBody().SetAttributeRaw("name", tokens)
	}

	if c.Options != nil && c.Options.Providers != nil {
		d := g.genModuleProviders(block.Body(), c.Options.Providers)
		diags = append(diags, d...)
	}

	if c.Options != nil && c.Options.Protect != nil {
		tokens, d := g.exprTokens(c.Options.Protect, schema.BoolType)
		diags = append(diags, d...)
		if !d.HasErrors() {
			pulumiBody().SetAttributeRaw("protect", tokens)
		}
	}

	for _, attr := range c.Inputs {
		target := inputBody(block.Body(), attr.Name, parser.ModuleReservedNames)
		g.emitLeadingComments(target, attr.SyntaxNode())
		d := g.genAttributeWithTrailing(target, attr.Name, attr.Value, schema.AnyType, attr.SyntaxNode())
		diags = append(diags, d...)
	}
	return diags
}

// genModuleProviders emits a module block's `providers` map from a PCL
// component's providers option. PCL accepts a list of provider resources or a
// map of local provider name to provider resource; the list form infers each
// entry's key from the referenced provider's package name, which is also the
// local name the child module's required_providers declares.
func (g *generator) genModuleProviders(body *hclwrite.Body, providers model.Expression) hcl.Diagnostics {
	var entries []hclwrite.ObjectAttrTokens
	var diags hcl.Diagnostics

	badEntry := func(rng hcl.Range) hcl.Diagnostics {
		return append(diags, &hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  "invalid providers option",
			Detail:   "each providers entry must reference a provider resource",
			Subject:  &rng,
		})
	}

	addEntry := func(name, value model.Expression, extractName func(model.Expression) (string, bool)) {
		key, ok := extractName(name)
		if !ok {
			diags = badEntry(name.SyntaxNode().Range())
			return
		}
		v, d := g.exprTokens(value, schema.AnyResourceType)
		if !d.HasErrors() {
			entries = append(entries, hclwrite.ObjectAttrTokens{
				Name:  hclwrite.TokensForIdentifier(key),
				Value: v,
			})
		}
		diags = diags.Extend(d)
	}

	switch e := providers.(type) {
	case *model.TupleConsExpression:
		for _, elem := range e.Expressions {
			addEntry(elem, elem, providerPackageNameFromExpr)
		}
	case *model.ObjectConsExpression:
		for _, item := range e.Items {
			addEntry(item.Key, item.Value, extractStringLiteral)
		}
	default:
		return badEntry(providers.SyntaxNode().Range())
	}

	body.SetAttributeRaw("providers", hclwrite.TokensForObject(entries))
	return diags
}

// providerPackageNameFromExpr returns the package name of the provider
// resource an expression references, or ok=false when the expression is not a
// reference to a provider resource.
func providerPackageNameFromExpr(expr model.Expression) (string, bool) {
	st, ok := expr.(*model.ScopeTraversalExpression)
	if !ok || len(st.Parts) == 0 {
		return "", false
	}
	r, ok := st.Parts[0].(*pcl.Resource)
	if !ok || r.Schema == nil || !r.Schema.IsProvider {
		return "", false
	}
	token, _ := r.GetToken()
	return string(tokens.Type(token).Name()), true
}

// genBlocks writes expr as nested blocks named name. mapping is the bridge
// mapping of the block's body, or nil.
func (g *generator) genBlocks(
	body *hclwrite.Body, name string, expr model.Expression, objType *schema.ObjectType, mapping *bridge.BodyMapping,
) hcl.Diagnostics {
	switch e := expr.(type) {
	case *model.TupleConsExpression:
		var diags hcl.Diagnostics
		for _, elem := range e.Expressions {
			d := g.genBlock(body, name, elem, objType, mapping)
			diags = append(diags, d...)
		}
		return diags
	case *model.ForExpression:
		return g.genDynamicBlock(body, name, e, objType, mapping)
	default:
		return g.genBlock(body, name, expr, objType, mapping)
	}
}

func nestedMapping(fm *bridge.FieldMapping) *bridge.BodyMapping {
	if fm == nil {
		return nil
	}
	return fm.Nested
}

// genDynamicBlock generates a dynamic block from a PCL ForExpression.
//
//	dynamic "name" {
//	  for_each = <collection>
//	  content {
//	    <fields from Value expression, with iterator vars rewritten>
//	  }
//	}
func (g *generator) genDynamicBlock(
	body *hclwrite.Body, name string, expr *model.ForExpression, objType *schema.ObjectType, mapping *bridge.BodyMapping,
) hcl.Diagnostics {
	block := body.AppendNewBlock("dynamic", []string{name})

	collTokens, diags := g.exprTokens(expr.Collection, schema.AnyType)
	if diags.HasErrors() {
		return diags
	}
	block.Body().SetAttributeRaw("for_each", collTokens)

	prev := g.dynamicBlock
	g.dynamicBlock = &dynamicBlockContext{
		blockName:     name,
		keyVariable:   expr.KeyVariable,
		valueVariable: expr.ValueVariable,
	}
	defer func() { g.dynamicBlock = prev }()

	d := g.genBlock(block.Body(), "content", expr.Value, objType, mapping)
	diags = append(diags, d...)
	return diags
}

func (g *generator) genBlock(
	body *hclwrite.Body, name string, expr model.Expression, objType *schema.ObjectType, mapping *bridge.BodyMapping,
) hcl.Diagnostics {
	block := body.AppendNewBlock(name, nil)
	obj, ok := expr.(*model.ObjectConsExpression)
	if !ok {
		return g.genExpression(block.Body(), "content", expr, objType)
	}
	var diags hcl.Diagnostics
	for _, item := range obj.Items {
		keyName, _ := extractStringLiteral(item.Key)
		snakeName := transform.SnakeCaseFromPulumiCase(keyName)
		propType := schema.AnyType
		if p, ok := objType.Property(keyName); ok {
			propType = p.Type
		}
		var d hcl.Diagnostics
		fm := mapping.Lookup(snakeName)
		if innerObjType, ok := transform.HCLBlockType(propType, fm); ok {
			d = g.genBlocks(block.Body(), snakeName, item.Value, innerObjType, nestedMapping(fm))
		} else {
			d = g.genExpression(block.Body(), snakeName, item.Value, propType)
		}
		diags = append(diags, d...)
	}
	return diags
}

func (g *generator) genExpression(body *hclwrite.Body, name string, expr model.Expression, typ schema.Type) hcl.Diagnostics {
	tokens, diags := g.exprTokens(expr, typ)
	if diags.HasErrors() {
		return diags
	}
	body.SetAttributeRaw(name, tokens)
	return diags
}

// genAttributeWithTrailing is genExpression that also appends any same-line
// trailing comment on the source attribute identified by node, so the comment
// stays on the same line as the value in the output.
func (g *generator) genAttributeWithTrailing(
	body *hclwrite.Body, name string, expr model.Expression, typ schema.Type, node hclsyntax.Node,
) hcl.Diagnostics {
	tokens, diags := g.exprTokens(expr, typ)
	if diags.HasErrors() {
		return diags
	}
	body.SetAttributeRaw(name, g.withTrailing(tokens, node))
	return diags
}

// exprTokens converts a PCL expression into HCL tokens.
// Invoke calls are replaced with references to generated data sources.
func (g *generator) exprTokens(expr model.Expression, typ schema.Type) (hclwrite.Tokens, hcl.Diagnostics) {
	switch e := expr.(type) {
	case *model.LiteralValueExpression:
		return hclwrite.TokensForValue(e.Value), nil
	case *model.TemplateExpression:
		return g.templateTokens(e)
	case *model.FunctionCallExpression:
		// A multi-argument invoke projects as a provider-defined function call,
		// provider::<name>::<fn>(...), whose return object is accessed directly
		// (e.g. .result), so no data block or result-object wrapper is involved.
		if e.Name == pcl.Invoke && e.Signature.MultiArgumentInputs {
			return g.providerFunctionInvokeTokens(e)
		}
		// Check if this is an invoke or a TF builtin — inline it.
		// Standalone use (not wrapped in .result) wraps the call in a single-
		// field object so downstream .result access resolves correctly; the
		// common .result wrapping is intercepted in relativeTraversalTokens
		// to avoid the wrapper.
		if e.Name == pcl.Invoke {
			if fn, inlinable := inlinableStdFunc(e); inlinable {
				callTokens, diags := g.inlineStdInvoke(e, fn)
				if diags.HasErrors() {
					return nil, diags
				}
				return wrapInResultObject(callTokens), nil
			}
		}
		// Check if this is an invoke call that we've replaced with a data source
		if e.Name == pcl.Invoke {
			var matchedDS *spilledDataSource
			for i := range g.invokeDataSources {
				if g.invokeDataSources[i].expr == e {
					matchedDS = &g.invokeDataSources[i]
					break
				}
			}
			if matchedDS != nil {
				// Generate reference to data source: data.type.name
				token, ok := extractStringLiteral(e.Args[0])
				if !ok {
					return nil, hcl.Diagnostics{{
						Severity: hcl.DiagError,
						Summary:  "invalid invoke call",
						Detail:   "invoke token must be a string literal",
					}}
				}
				invokeSchema, schemaToken, _ := g.lookupInvokeSchema(token)
				dsType, diags := packages.PulumiFunctionTokenToHCL(invokeSchemaPackage(invokeSchema), invokeSchemaToken(invokeSchema, schemaToken))
				if diags.HasErrors() {
					return nil, diags
				}
				tokens := hclwrite.TokensForTraversal(hcl.Traversal{
					hcl.TraverseRoot{Name: "data"},
					hcl.TraverseAttr{Name: dsType},
					hcl.TraverseAttr{Name: matchedDS.name},
				})
				// If the data block carries a for_each matching the enclosing
				// resource's range, index into it so the reference is per-iteration.
				if matchedDS.parentResource != nil {
					tokens = append(tokens, perIterationIndexTokens(g.currentRangeKind)...)
				}
				// If the data block carries a for_each matching an enclosing
				// for-comprehension, index into it with a variable that remains
				// in scope at the reference site. Prefer the comprehension's key
				// variable; for no-key comprehensions the collection was wrapped
				// with toset(), so the element value is its own key.
				if fe := matchedDS.enclosingForExpr; fe != nil {
					indexName := ""
					if fe.KeyVariable != nil {
						indexName = fe.KeyVariable.Name
					} else if fe.ValueVariable != nil {
						indexName = fe.ValueVariable.Name
					}
					if indexName != "" {
						tokens = append(tokens,
							&hclwrite.Token{Type: hclsyntax.TokenOBrack, Bytes: []byte("[")},
							&hclwrite.Token{Type: hclsyntax.TokenIdent, Bytes: []byte(indexName)},
							&hclwrite.Token{Type: hclsyntax.TokenCBrack, Bytes: []byte("]")},
						)
					}
				}
				return tokens, nil
			}
		}
		// Check if this is a call expression that we've replaced with a call block
		if e.Name == pcl.Call {
			resourceName, methodName, ok := g.extractCallArgs(e)
			if ok {
				snakeMethod := transform.SnakeCaseFromPulumiCase(methodName)
				return hclwrite.TokensForTraversal(hcl.Traversal{
					hcl.TraverseRoot{Name: "call"},
					hcl.TraverseAttr{Name: resourceName},
					hcl.TraverseAttr{Name: snakeMethod},
				}), nil
			}
		}
		return g.funcCallTokens(e)
	case *model.ScopeTraversalExpression:
		return g.scopeTraversalTokens(e)
	case *model.TupleConsExpression:
		return g.tupleTokens(e, typ)
	case *model.ObjectConsExpression:
		return g.objectTokens(e, typ)
	case *model.IndexExpression:
		return g.indexExprTokens(e)
	case *model.RelativeTraversalExpression:
		return g.relativeTraversalTokens(e)
	case *model.BinaryOpExpression:
		return g.binaryOpTokens(e)
	case *model.UnaryOpExpression:
		return g.unaryOpTokens(e)
	case *model.ForExpression:
		return g.forExprTokens(e)
	case *model.SplatExpression:
		return g.splatTokens(e)
	case *model.ConditionalExpression:
		return g.conditionalTokens(e)
	default:
		return nil, hcl.Diagnostics{{
			Severity: hcl.DiagError,
			Summary:  "unsupported expression type",
			Detail:   fmt.Sprintf("expression type %T is not yet supported", expr),
		}}
	}
}

// forExprWrapperTokens emits `[for k, v in collTokens : valueTokens]` using
// fe's iter-var names. Unlike forExprTokens it accepts pre-rendered tokens for
// both the collection and body, so it can wrap an already-generated for_each.
func forExprWrapperTokens(fe *model.ForExpression, collTokens, valueTokens hclwrite.Tokens) hclwrite.Tokens {
	tokens := hclwrite.Tokens{
		{Type: hclsyntax.TokenOBrack, Bytes: []byte("[")},
		{Type: hclsyntax.TokenIdent, Bytes: []byte("for"), SpacesBefore: 0},
	}
	if fe.KeyVariable != nil {
		tokens = append(tokens,
			&hclwrite.Token{Type: hclsyntax.TokenIdent, Bytes: []byte(fe.KeyVariable.Name), SpacesBefore: 1},
			&hclwrite.Token{Type: hclsyntax.TokenComma, Bytes: []byte(",")},
		)
	}
	tokens = append(tokens,
		&hclwrite.Token{Type: hclsyntax.TokenIdent, Bytes: []byte(fe.ValueVariable.Name), SpacesBefore: 1},
		&hclwrite.Token{Type: hclsyntax.TokenIdent, Bytes: []byte("in"), SpacesBefore: 1},
	)
	if len(collTokens) > 0 {
		collTokens[0].SpacesBefore = 1
	}
	tokens = append(tokens, collTokens...)
	tokens = append(tokens, &hclwrite.Token{Type: hclsyntax.TokenColon, Bytes: []byte(":"), SpacesBefore: 1})
	if len(valueTokens) > 0 {
		valueTokens[0].SpacesBefore = 1
	}
	tokens = append(tokens, valueTokens...)
	tokens = append(tokens, &hclwrite.Token{Type: hclsyntax.TokenCBrack, Bytes: []byte("]")})
	return tokens
}

// forExprTokens generates HCL tokens for a PCL ForExpression.
//
// List result (Key == nil):  [for key, value in collection : valueExpr]
// Map result (Key != nil):   {for key, value in collection : keyExpr => valueExpr}
func (g *generator) forExprTokens(expr *model.ForExpression) (hclwrite.Tokens, hcl.Diagnostics) {
	var diags hcl.Diagnostics

	collTokens, d := g.exprTokens(expr.Collection, schema.AnyType)
	diags = append(diags, d...)
	if d.HasErrors() {
		return nil, diags
	}

	valueTokens, d := g.exprTokens(expr.Value, schema.AnyType)
	diags = append(diags, d...)
	if d.HasErrors() {
		return nil, diags
	}

	isMap := expr.Key != nil

	var open, close byte
	if isMap {
		open, close = '{', '}'
	} else {
		open, close = '[', ']'
	}

	tokens := hclwrite.Tokens{
		{Type: hclsyntax.TokenTemplateControl, Bytes: []byte{open}},
		{Type: hclsyntax.TokenIdent, Bytes: []byte("for"), SpacesBefore: 0},
	}

	if expr.KeyVariable != nil {
		tokens = append(tokens,
			&hclwrite.Token{Type: hclsyntax.TokenIdent, Bytes: []byte(expr.KeyVariable.Name), SpacesBefore: 1},
			&hclwrite.Token{Type: hclsyntax.TokenComma, Bytes: []byte(",")},
		)
	}

	tokens = append(tokens,
		&hclwrite.Token{Type: hclsyntax.TokenIdent, Bytes: []byte(expr.ValueVariable.Name), SpacesBefore: 1},
		&hclwrite.Token{Type: hclsyntax.TokenIdent, Bytes: []byte("in"), SpacesBefore: 1},
	)

	if len(collTokens) > 0 {
		collTokens[0].SpacesBefore = 1
	}
	tokens = append(tokens, collTokens...)
	tokens = append(tokens, &hclwrite.Token{Type: hclsyntax.TokenColon, Bytes: []byte(":"), SpacesBefore: 1})

	if isMap {
		keyTokens, d := g.exprTokens(expr.Key, schema.AnyType)
		diags = append(diags, d...)
		if d.HasErrors() {
			return nil, diags
		}
		if len(keyTokens) > 0 {
			keyTokens[0].SpacesBefore = 1
		}
		tokens = append(tokens, keyTokens...)
		tokens = append(tokens, &hclwrite.Token{Type: hclsyntax.TokenFatArrow, Bytes: []byte("=>"), SpacesBefore: 1})
	}

	if len(valueTokens) > 0 {
		valueTokens[0].SpacesBefore = 1
	}
	tokens = append(tokens, valueTokens...)

	if expr.Condition != nil {
		condTokens, d := g.exprTokens(expr.Condition, schema.AnyType)
		diags = append(diags, d...)
		if d.HasErrors() {
			return nil, diags
		}
		tokens = append(tokens, &hclwrite.Token{Type: hclsyntax.TokenIdent, Bytes: []byte("if"), SpacesBefore: 1})
		if len(condTokens) > 0 {
			condTokens[0].SpacesBefore = 1
		}
		tokens = append(tokens, condTokens...)
	}

	if expr.Group {
		tokens = append(tokens, &hclwrite.Token{Type: hclsyntax.TokenEllipsis, Bytes: []byte("..."), SpacesBefore: 0})
	}

	tokens = append(tokens, &hclwrite.Token{Type: hclsyntax.TokenTemplateSeqEnd, Bytes: []byte{close}})
	return tokens, diags
}

func (g *generator) funcCallTokens(expr *model.FunctionCallExpression) (hclwrite.Tokens, hcl.Diagnostics) {
	switch expr.Name {
	case "__convert":
		// __convert is a PCL internal type-coercion function; it's an identity operation at runtime.
		if len(expr.Args) == 1 {
			return g.exprTokens(expr.Args[0], schema.AnyType)
		}
		return g.passthroughFuncCallTokens(expr.Name, expr.Args)
	case "cwd":
		// PCL cwd() is the program working directory (the dir holding the
		// .tf files, after any Pulumi -main). In Terraform-semantic HCL
		// that's the absolute config root, which abspath(path.root)
		// materializes (path.root is "."). The eject path inverts this
		// exact pattern back to cwd().
		return hclwrite.TokensForFunctionCall("abspath",
			hclwrite.TokensForTraversal(hcl.Traversal{
				hcl.TraverseRoot{Name: "path"},
				hcl.TraverseAttr{Name: "root"},
			})), nil
	case "rootDirectory":
		// PCL rootDirectory() is the project root (where Pulumi.yaml lives,
		// the dir above any -main subdir). That matches Terraform's path.cwd
		// — the original cwd before any -chdir — which our engine populates
		// with the Pulumi RootDirectory.
		return hclwrite.TokensForTraversal(hcl.Traversal{
			hcl.TraverseRoot{Name: "path"},
			hcl.TraverseAttr{Name: "cwd"},
		}), nil
	case "stack":
		return hclwrite.TokensForTraversal(hcl.Traversal{
			hcl.TraverseRoot{Name: "pulumi"},
			hcl.TraverseAttr{Name: "stack"},
		}), nil
	case "project":
		return hclwrite.TokensForTraversal(hcl.Traversal{
			hcl.TraverseRoot{Name: "pulumi"},
			hcl.TraverseAttr{Name: "project"},
		}), nil
	case "organization":
		return hclwrite.TokensForTraversal(hcl.Traversal{
			hcl.TraverseRoot{Name: "pulumi"},
			hcl.TraverseAttr{Name: "organization"},
		}), nil
	case "getOutput":
		return g.getOutputTokens(expr)
	case "secret":
		return g.passthroughFuncCallTokens("sensitive", expr.Args)
	case "unsecret":
		return g.passthroughFuncCallTokens("nonsensitive", expr.Args)
	case "singleOrNone":
		return g.passthroughFuncCallTokens("one", expr.Args)
	case "toBase64":
		return g.passthroughFuncCallTokens("base64encode", expr.Args)
	case "fromBase64":
		return g.passthroughFuncCallTokens("base64decode", expr.Args)
	case "toJSON":
		return g.passthroughFuncCallTokens("jsonencode", expr.Args)
	case "readFile":
		return g.passthroughFuncCallTokens("file", expr.Args)
	case "fileAsset":
		return g.passthroughFuncCallTokens("fileasset", expr.Args)
	case "fileArchive":
		return g.passthroughFuncCallTokens("filearchive", expr.Args)
	case "stringAsset":
		return g.passthroughFuncCallTokens("stringasset", expr.Args)
	case "assetArchive":
		return g.passthroughFuncCallTokens("assetarchive", expr.Args)
	case "remoteAsset":
		return g.passthroughFuncCallTokens("remoteasset", expr.Args)
	case "remoteArchive":
		return g.passthroughFuncCallTokens("remotearchive", expr.Args)
	case "pulumiResourceName":
		return g.passthroughFuncCallTokens("pulumiresourcename", expr.Args)
	case "pulumiResourceType":
		return g.passthroughFuncCallTokens("pulumiresourcetype", expr.Args)
	case "notImplemented":
		return g.notImplementedTokens(expr)
	default:
		return g.passthroughFuncCallTokensExpand(expr.Name, expr.Args, expr.ExpandFinal)
	}
}

// providerFunctionInvokeTokens emits a multi-argument invoke as a
// provider-defined function call: provider::<localName>::<funcName>(a1, a2, …).
// The bound invoke is in object-argument form; its fields are re-expanded into
// positional arguments in the schema's declared input order. An optional input
// the program omitted is passed as null, since the runtime's cty projection
// binds one positional parameter per input (see transform.ProviderFunction).
func (g *generator) providerFunctionInvokeTokens(
	call *model.FunctionCallExpression,
) (hclwrite.Tokens, hcl.Diagnostics) {
	token, ok := extractStringLiteral(call.Args[0])
	if !ok {
		return nil, hcl.Diagnostics{{
			Severity: hcl.DiagError,
			Summary:  "invalid invoke call",
			Detail:   "invoke token must be a string literal",
		}}
	}
	fn, canonical, diag := g.lookupInvokeSchema(token)
	if diag != nil {
		return nil, hcl.Diagnostics{diag}
	}
	if fn == nil || fn.Inputs == nil {
		return nil, hcl.Diagnostics{{
			Severity: hcl.DiagError,
			Summary:  "invalid multi-argument invoke",
			Detail:   fmt.Sprintf("no input schema found for function %q", token),
		}}
	}

	pkgName, _, name, _ := pcl.DecomposeToken(canonical, hcl.Range{})
	funcName := tfbridge.PulumiToTerraformName(name, nil, nil)

	provided := map[string]model.Expression{}
	if obj, ok := call.Args[1].(*model.ObjectConsExpression); ok {
		for _, item := range obj.Items {
			if key, ok := extractStringLiteral(item.Key); ok {
				provided[key] = item.Value
			}
		}
	}

	args := make([]hclwrite.Tokens, 0, len(fn.Inputs.Properties))
	var diags hcl.Diagnostics
	for _, p := range fn.Inputs.Properties {
		arg, ok := provided[p.Name]
		if !ok {
			// A non-variadic provider function requires a value for every
			// parameter, so an omitted optional invoke argument is passed as an
			// explicit null. Both OpenTofu and the HCL runtime reject a missing
			// argument (see tfcompat TestL2ProviderFunctionPartial).
			args = append(args, hclwrite.TokensForValue(cty.NullVal(cty.DynamicPseudoType)))
			continue
		}
		argTokens, d := g.exprTokens(arg, p.Type)
		diags = append(diags, d...)
		if d.HasErrors() {
			return nil, diags
		}
		args = append(args, argTokens)
	}
	return hclwrite.TokensForFunctionCall(ast.ProviderFunctionName(pkgName, funcName), args...), diags
}

// notImplementedTokens handles PCL's notImplemented("expression") by extracting the original
// expression text and emitting it as HCL when the expression uses a function that HCL supports.
// If the expression doesn't parse, isn't a function call, or uses an unknown function,
// it falls through to emit notImplemented(...) verbatim.
func (g *generator) notImplementedTokens(expr *model.FunctionCallExpression) (hclwrite.Tokens, hcl.Diagnostics) {
	if len(expr.Args) != 1 {
		return g.passthroughFuncCallTokens(expr.Name, expr.Args)
	}

	exprText, ok := extractStringLiteral(expr.Args[0])
	if !ok {
		return g.passthroughFuncCallTokens(expr.Name, expr.Args)
	}

	parsed, diags := hclsyntax.ParseExpression([]byte(exprText), "", hcl.Pos{Line: 1, Column: 1})
	if diags.HasErrors() {
		return g.passthroughFuncCallTokens(expr.Name, expr.Args)
	}

	funcCall, ok := parsed.(*hclsyntax.FunctionCallExpr)
	if !ok {
		return g.passthroughFuncCallTokens(expr.Name, expr.Args)
	}

	knownFunctions := eval.Functions("")
	if _, known := knownFunctions[funcCall.Name]; !known {
		return g.passthroughFuncCallTokens(expr.Name, expr.Args)
	}

	syntaxTokens, diags := hclsyntax.LexExpression([]byte(exprText), "", hcl.Pos{Line: 1, Column: 1})
	if diags.HasErrors() {
		return g.passthroughFuncCallTokens(expr.Name, expr.Args)
	}

	var tokens hclwrite.Tokens
	for _, t := range syntaxTokens {
		if t.Type == hclsyntax.TokenEOF {
			continue
		}
		tokens = append(tokens, &hclwrite.Token{
			Type:  t.Type,
			Bytes: t.Bytes,
		})
	}
	return tokens, nil
}

// getOutputTokens generates tokens for getOutput(resource, "outputName").
// This produces resource_type.name.outputs["outputName"].
func (g *generator) getOutputTokens(expr *model.FunctionCallExpression) (hclwrite.Tokens, hcl.Diagnostics) {
	if len(expr.Args) != 2 {
		return nil, hcl.Diagnostics{{
			Severity: hcl.DiagError,
			Summary:  "invalid getOutput call",
			Detail:   "getOutput requires exactly 2 arguments: resource reference and output name",
		}}
	}

	// First arg is a scope traversal to the resource
	resRef, ok := expr.Args[0].(*model.ScopeTraversalExpression)
	if !ok {
		return nil, hcl.Diagnostics{{
			Severity: hcl.DiagError,
			Summary:  "invalid getOutput call",
			Detail:   "first argument must be a resource reference",
		}}
	}

	// The resource reference should resolve to a pcl.Resource
	res, ok := resRef.Parts[0].(*pcl.Resource)
	if !ok {
		return nil, hcl.Diagnostics{{
			Severity: hcl.DiagError,
			Summary:  "invalid getOutput call",
			Detail:   "first argument must reference a resource",
		}}
	}

	// Second arg is the output name (string literal, possibly wrapped in a template).
	outputName, ok := extractStringLiteral(expr.Args[1])
	if !ok {
		return nil, hcl.Diagnostics{{
			Severity: hcl.DiagError,
			Summary:  "invalid getOutput call",
			Detail:   "second argument must be a string literal",
		}}
	}

	// Get the HCL resource type
	token, _ := res.GetToken()
	hclType, diags := packages.PulumiResourceTokenToHCL(resourceSchemaPackage(res.Schema), resourceSchemaToken(res.Schema, token))
	if diags.HasErrors() {
		return nil, diags
	}

	// Generate: resource_type.name.outputs["outputName"]
	return hclwrite.TokensForTraversal(hcl.Traversal{
		hcl.TraverseRoot{Name: hclType},
		hcl.TraverseAttr{Name: res.LogicalName()},
		hcl.TraverseAttr{Name: "outputs"},
		hcl.TraverseIndex{Key: cty.StringVal(outputName)},
	}), nil
}

// schemaAwareRewriteTraversal rewrites a traversal's attribute names from PCL (camelCase)
// to HCL (snake_case), using schema property definitions to correctly map names through
// nested objects, maps, and arrays. When schema information is unavailable for a step,
// the remaining traversal is returned unchanged.
func schemaAwareRewriteTraversal(props []*schema.Property, traversal hcl.Traversal) hcl.Traversal {
	if len(traversal) == 0 || len(props) == 0 {
		return traversal
	}
	t, ok := traversal[0].(hcl.TraverseAttr)
	if !ok {
		return traversal
	}
	for _, p := range props {
		if p.Name == t.Name {
			t = hcl.TraverseAttr{Name: transform.SnakeCaseFromPulumiCase(p.Name), SrcRange: t.SrcRange}
			return append(hcl.Traversal{t}, schemaAwareRewriteTyped(p.Type, traversal[1:])...)
		}
	}
	return traversal
}

// schemaAwareRewriteTyped rewrites a traversal's attribute names using schema type
// information, dispatching to [schemaAwareRewriteTraversal] for object properties and
// recursing through map/array element types for index steps.
func schemaAwareRewriteTyped(typ schema.Type, traversal hcl.Traversal) hcl.Traversal {
	if len(traversal) == 0 {
		return traversal
	}

	switch t := traversal[0].(type) {
	case hcl.TraverseAttr:
		switch s := codegen.UnwrapType(typ).(type) {
		case *schema.ResourceType:
			return schemaAwareRewriteTraversal(s.Resource.Properties, traversal)
		case *schema.ObjectType:
			return schemaAwareRewriteTraversal(s.Properties, traversal)
		default:
			return traversal
		}
	case hcl.TraverseIndex:
		switch s := codegen.UnwrapType(typ).(type) {
		case *schema.MapType:
			return append(hcl.Traversal{t}, schemaAwareRewriteTyped(s.ElementType, traversal[1:])...)
		case *schema.ArrayType:
			return append(hcl.Traversal{t}, schemaAwareRewriteTyped(s.ElementType, traversal[1:])...)
		default:
			return traversal
		}
	default:
		return traversal
	}
}

// traversalStepsToTokens converts traversal steps (attrs and indexes) to HCL write tokens.
// Attribute names are emitted as-is — callers should rewrite names beforehand if needed.
func traversalStepsToTokens(traversal hcl.Traversal) hclwrite.Tokens {
	var tokens hclwrite.Tokens
	for _, step := range traversal {
		switch s := step.(type) {
		case hcl.TraverseAttr:
			tokens = append(tokens,
				&hclwrite.Token{Type: hclsyntax.TokenDot, Bytes: []byte(".")},
				&hclwrite.Token{Type: hclsyntax.TokenIdent, Bytes: []byte(s.Name)},
			)
		case hcl.TraverseIndex:
			keyTokens := hclwrite.TokensForValue(s.Key)
			tokens = append(tokens, &hclwrite.Token{Type: hclsyntax.TokenOBrack, Bytes: []byte("[")})
			tokens = append(tokens, keyTokens...)
			tokens = append(tokens, &hclwrite.Token{Type: hclsyntax.TokenCBrack, Bytes: []byte("]")})
		}
	}
	return tokens
}

// rewriteInvokeOutputTraversal rewrites a traversal of an invoke's outputs so
// that attribute names match the data source's emitted schema. When the
// function's output schema is available it is walked so non-object return
// types (e.g. array(object)) are handled correctly and so that traversal into
// untyped territory (e.g. map<string> values) stops being rewritten. When no
// schema is available we fall back to a naive camelCase→snake_case rewrite.
func rewriteInvokeOutputTraversal(fn *schema.Function, traversal hcl.Traversal) hcl.Traversal {
	if fn != nil && fn.ReturnType != nil {
		return schemaAwareRewriteTyped(fn.ReturnType, traversal)
	}
	return naiveRewriteTraversal(traversal)
}

// naiveRewriteTraversal rewrites all TraverseAttr names in a traversal from camelCase to
// snake_case without schema information. Used as a fallback when schema is unavailable.
func naiveRewriteTraversal(traversal hcl.Traversal) hcl.Traversal {
	rewritten := make(hcl.Traversal, len(traversal))
	for i, step := range traversal {
		if attr, ok := step.(hcl.TraverseAttr); ok {
			rewritten[i] = hcl.TraverseAttr{
				Name:     transform.SnakeCaseFromPulumiCase(attr.Name),
				SrcRange: attr.SrcRange,
			}
		} else {
			rewritten[i] = step
		}
	}
	return rewritten
}

// splatElementProps resolves the schema properties of the element type that a splat
// expression iterates over. It walks the Source expression's schema through its traversal
// to find the array type, then returns the element's properties.
func splatElementProps(source model.Expression) []*schema.Property {
	scope, ok := source.(*model.ScopeTraversalExpression)
	if !ok || len(scope.Parts) == 0 {
		return nil
	}
	res, ok := scope.Parts[0].(*pcl.Resource)
	if !ok || res.Schema == nil {
		return nil
	}
	// Walk through the source traversal to find the final schema type.
	// Start from the resource's properties, skip the root traversal step.
	var typ schema.Type = &schema.ObjectType{Properties: res.Schema.Properties}
	for _, step := range scope.Traversal[1:] {
		if typ == nil {
			return nil
		}
		typ = traverseSchemaType(typ, step)
	}
	// The source type should be an array; return the element's properties.
	if arr, ok := codegen.UnwrapType(typ).(*schema.ArrayType); ok {
		if obj, ok := codegen.UnwrapType(arr.ElementType).(*schema.ObjectType); ok {
			return obj.Properties
		}
	}
	return nil
}

// traverseSchemaType applies a single traversal step to a schema type, returning the
// resulting type. Returns nil if the step cannot be applied.
func traverseSchemaType(typ schema.Type, step hcl.Traverser) schema.Type {
	switch s := step.(type) {
	case hcl.TraverseAttr:
		switch t := codegen.UnwrapType(typ).(type) {
		case *schema.ResourceType:
			return findPropertyType(t.Resource.Properties, s.Name)
		case *schema.ObjectType:
			return findPropertyType(t.Properties, s.Name)
		}
	case hcl.TraverseIndex:
		switch t := codegen.UnwrapType(typ).(type) {
		case *schema.MapType:
			return t.ElementType
		case *schema.ArrayType:
			return t.ElementType
		}
	}
	return nil
}

func findPropertyType(props []*schema.Property, name string) schema.Type {
	for _, p := range props {
		if p.Name == name {
			return p.Type
		}
	}
	return nil
}

// traverseNameStep returns a TraverseAttr for valid HCL identifiers (e.g. `.foo`),
// or a TraverseIndex with a string key for names that contain special characters
// (e.g. `["aA-Alpha_alpha.🤯⁉️"]`).
func traverseNameStep(name string) hcl.Traverser {
	if hclsyntax.ValidIdentifier(name) {
		return hcl.TraverseAttr{Name: name}
	}
	return hcl.TraverseIndex{Key: cty.StringVal(name)}
}

// scopeTraversalTokens generates HCL tokens for a scope traversal expression.
// PCL config variables become HCL `var.<name>`, local variables become `local.<name>`,
// and resource references become `<resource_type>.<name>.<property>`.
func (g *generator) scopeTraversalTokens(expr *model.ScopeTraversalExpression) (hclwrite.Tokens, hcl.Diagnostics) {
	if len(expr.Parts) == 0 {
		return hclwrite.TokensForTraversal(expr.Traversal), nil
	}

	traversal := expr.Traversal
	switch part := expr.Parts[0].(type) {
	case *pcl.ConfigVariable:
		// Rewrite "aMap.x" → "var.aMap.x".
		rewritten := make(hcl.Traversal, 0, len(traversal)+1)
		rewritten = append(rewritten, hcl.TraverseRoot{Name: "var"})
		rewritten = append(rewritten, traverseNameStep(part.LogicalName()))
		return hclwrite.TokensForTraversal(append(rewritten, traversal[1:]...)), nil
	case *pcl.LocalVariable:
		// If this local is backed by an invoke call, substitute the data source reference
		// directly so that resource dependency tracking works correctly.
		if call, ok := part.Definition.Value.(*model.FunctionCallExpression); ok && call.Name == pcl.Invoke {
			for _, ds := range g.invokeDataSources {
				if ds.expr == call {
					token, ok := extractStringLiteral(call.Args[0])
					if ok {
						invokeSchema, schemaToken, _ := g.lookupInvokeSchema(token)
						dsType, d := packages.PulumiFunctionTokenToHCL(invokeSchemaPackage(invokeSchema), invokeSchemaToken(invokeSchema, schemaToken))
						if !d.HasErrors() {
							rewritten := hcl.Traversal{
								hcl.TraverseRoot{Name: "data"},
								hcl.TraverseAttr{Name: dsType},
								hcl.TraverseAttr{Name: ds.name},
							}
							return hclwrite.TokensForTraversal(append(rewritten, rewriteInvokeOutputTraversal(invokeSchema, traversal[1:])...)), nil
						}
					}
					break
				}
			}
		}
		// Rewrite "myLocal.x" → "local.myLocal.x".
		rewritten := make(hcl.Traversal, 0, len(traversal)+1)
		rewritten = append(rewritten, hcl.TraverseRoot{Name: "local"})
		rewritten = append(rewritten, traverseNameStep(part.LogicalName()))
		return hclwrite.TokensForTraversal(append(rewritten, traversal[1:]...)), nil
	case *pcl.Resource:
		// Rewrite "myResource.property" → "resource_type.myResource.property"
		// for normal resources, or "<pkg>.<alias>.property" for providers
		// (which are declared with a `provider` block, not a `resource` block).
		//
		// TODO: Resource traversal needs to be type (and schema) aware. It needs to invoke
		// [transform.SnakeCaseFromPulumiCase] on property values, and the invoke the standard ["<key>"]
		// & [<idx>] operators otherwise.
		token, _ := part.GetToken()
		rewritten := make(hcl.Traversal, 0, len(traversal)+1)
		if part.Schema != nil && part.Schema.IsProvider {
			pkgName := token
			if i := strings.LastIndex(token, ":"); i >= 0 {
				pkgName = token[i+1:]
			}
			rewritten = append(rewritten, hcl.TraverseRoot{Name: pkgName})
			rewritten = append(rewritten, traverseNameStep(part.LogicalName()))
			var props []*schema.Property
			if part.Schema != nil {
				props = part.Schema.Properties
			}
			return hclwrite.TokensForTraversal(append(rewritten, schemaAwareRewriteTraversal(props, traversal[1:])...)), nil
		}
		hclType, diags := packages.PulumiResourceTokenToHCL(resourceSchemaPackage(part.Schema), resourceSchemaToken(part.Schema, token))
		if diags.HasErrors() {
			return nil, diags
		}
		rewritten = append(rewritten, hcl.TraverseRoot{Name: hclType})
		rewritten = append(rewritten, traverseNameStep(part.LogicalName()))
		// PCL exposes a resource's URN as a `.urn` attribute; HCL resource
		// values have no such attribute, so the traversal becomes a
		// pulumiResourceURN call on the resource (or indexed instance).
		if rest := traversal[1:]; len(rest) > 0 {
			if attr, ok := rest[len(rest)-1].(hcl.TraverseAttr); ok && attr.Name == "urn" && allIndexSteps(rest[:len(rest)-1]) {
				return pulumiResourceURNCallTokens(hclwrite.TokensForTraversal(append(rewritten, rest[:len(rest)-1]...))), nil
			}
		}
		// part.Schema can be nil when the binder ran with SkipResourceTypechecking
		// (or similar relaxed options); fall back to the unmodified traversal.
		var props []*schema.Property
		if part.Schema != nil {
			props = part.Schema.Properties
		}
		return hclwrite.TokensForTraversal(append(rewritten, schemaAwareRewriteTraversal(props, traversal[1:])...)), nil
	case *pcl.Component:
		// Rewrite "someComponent.output" → "module.someComponent.output".
		rewritten := make(hcl.Traversal, 0, len(traversal)+1)
		rewritten = append(rewritten, hcl.TraverseRoot{Name: "module"})
		rewritten = append(rewritten, traverseNameStep(part.LogicalName()))
		return hclwrite.TokensForTraversal(append(rewritten, traversal[1:]...)), nil
	default:
		if traversal.RootName() == "range" {
			switch g.currentRangeKind {
			case rangeKindCount:
				return hclwrite.TokensForTraversal(hcl.Traversal{
					hcl.TraverseRoot{Name: "count"},
					hcl.TraverseAttr{Name: "index"},
				}), nil
			default: // rangeKindForEach
				return hclwrite.TokensForTraversal(append(
					hcl.Traversal{hcl.TraverseRoot{Name: "each"}}, traversal[1:]...,
				)), nil
			}
		}
		if fe := g.forDataBlock; fe != nil && len(expr.Parts) > 0 {
			if v, ok := expr.Parts[0].(*model.Variable); ok {
				var attr string
				switch v {
				case fe.ValueVariable:
					attr = "value"
				case fe.KeyVariable:
					attr = "key"
				}
				if attr != "" {
					rewritten := hcl.Traversal{
						hcl.TraverseRoot{Name: "each"},
						hcl.TraverseAttr{Name: attr},
					}
					return hclwrite.TokensForTraversal(append(rewritten, traversal[1:]...)), nil
				}
			}
		}
		if db := g.dynamicBlock; db != nil {
			// Rewrite references to the for-expression's iterator variables:
			//   valueVar.field → blockName.value.field
			//   keyVar         → blockName.key
			if db.valueVariable != nil && part == db.valueVariable {
				rewritten := hcl.Traversal{
					hcl.TraverseRoot{Name: db.blockName},
					hcl.TraverseAttr{Name: "value"},
				}
				return hclwrite.TokensForTraversal(append(rewritten, traversal[1:]...)), nil
			}
			if db.keyVariable != nil && part == db.keyVariable {
				rewritten := hcl.Traversal{
					hcl.TraverseRoot{Name: db.blockName},
					hcl.TraverseAttr{Name: "key"},
				}
				return hclwrite.TokensForTraversal(append(rewritten, traversal[1:]...)), nil
			}
		}
		return hclwrite.TokensForTraversal(traversal), nil
	}
}

// pulumiResourceURNCallTokens wraps resource-reference tokens in a
// pulumiresourceurn(...) call.
func pulumiResourceURNCallTokens(resTokens hclwrite.Tokens) hclwrite.Tokens {
	tokens := hclwrite.Tokens{
		{Type: hclsyntax.TokenIdent, Bytes: []byte("pulumiresourceurn")},
		{Type: hclsyntax.TokenOParen, Bytes: []byte("(")},
	}
	tokens = append(tokens, resTokens...)
	return append(tokens, &hclwrite.Token{Type: hclsyntax.TokenCParen, Bytes: []byte(")")})
}

// allIndexSteps reports whether every traversal step is an index step.
func allIndexSteps(steps hcl.Traversal) bool {
	for _, step := range steps {
		if _, ok := step.(hcl.TraverseIndex); !ok {
			return false
		}
	}
	return true
}

// passthroughFuncCallTokens generates tokens for a function call: name(arg1, arg2, ...).
func (g *generator) passthroughFuncCallTokens(name string, args []model.Expression) (hclwrite.Tokens, hcl.Diagnostics) {
	return g.passthroughFuncCallTokensExpand(name, args, false)
}

func (g *generator) passthroughFuncCallTokensExpand(
	name string, args []model.Expression, expandFinal bool,
) (hclwrite.Tokens, hcl.Diagnostics) {
	tokens := hclwrite.Tokens{
		{Type: hclsyntax.TokenIdent, Bytes: []byte(name)},
		{Type: hclsyntax.TokenOParen, Bytes: []byte("(")},
	}
	var diags hcl.Diagnostics
	for i, arg := range args {
		if i > 0 {
			tokens = append(tokens, &hclwrite.Token{Type: hclsyntax.TokenComma, Bytes: []byte(",")})
		}
		argTokens, d := g.exprTokens(arg, schema.AnyType)
		diags = append(diags, d...)
		if d.HasErrors() {
			return nil, diags
		}
		tokens = append(tokens, argTokens...)
	}
	if expandFinal && len(args) > 0 {
		tokens = append(tokens, &hclwrite.Token{Type: hclsyntax.TokenEllipsis, Bytes: []byte("...")})
	}
	tokens = append(tokens, &hclwrite.Token{Type: hclsyntax.TokenCParen, Bytes: []byte(")")})
	return tokens, diags
}

// indexExprTokens generates tokens for an index expression: collection[key].
func (g *generator) indexExprTokens(expr *model.IndexExpression) (hclwrite.Tokens, hcl.Diagnostics) {
	collTokens, diags := g.exprTokens(expr.Collection, schema.AnyType)
	if diags.HasErrors() {
		return nil, diags
	}
	keyTokens, d := g.exprTokens(expr.Key, schema.AnyType)
	diags = append(diags, d...)
	if d.HasErrors() {
		return nil, diags
	}

	tokens := append(collTokens,
		&hclwrite.Token{Type: hclsyntax.TokenOBrack, Bytes: []byte("[")},
	)
	tokens = append(tokens, keyTokens...)
	tokens = append(tokens, &hclwrite.Token{Type: hclsyntax.TokenCBrack, Bytes: []byte("]")})
	return tokens, diags
}

// relativeTraversalTokens generates tokens for a relative traversal expression: source.attr.
// When the source is an inlinable std invoke followed by .result, the .result
// step is stripped so we emit the TF builtin call directly (e.g. lookup(...) instead
// of {result = lookup(...)}.result).
func (g *generator) relativeTraversalTokens(expr *model.RelativeTraversalExpression) (hclwrite.Tokens, hcl.Diagnostics) {
	if call, ok := expr.Source.(*model.FunctionCallExpression); ok {
		if fn, inlinable := inlinableStdFunc(call); inlinable && traversalStartsWithResult(expr.Traversal) {
			tokens, diags := g.inlineStdInvoke(call, fn)
			if diags.HasErrors() {
				return nil, diags
			}
			return append(tokens, traversalStepsToTokens(naiveRewriteTraversal(expr.Traversal[1:]))...), nil
		}
	}
	sourceTokens, diags := g.exprTokens(expr.Source, schema.AnyType)
	if diags.HasErrors() {
		return nil, diags
	}
	return append(sourceTokens, traversalStepsToTokens(naiveRewriteTraversal(expr.Traversal))...), diags
}

// traversalStartsWithResult reports whether the first step of a relative traversal
// is the attribute `.result`.
func traversalStartsWithResult(traversal hcl.Traversal) bool {
	if len(traversal) == 0 {
		return false
	}
	attr, ok := traversal[0].(hcl.TraverseAttr)
	return ok && attr.Name == "result"
}

// inlineStdInvoke emits a TF builtin function call for a std:index:* invoke.
// The call's named args are looked up by the positional names in fn.inputs;
// trailing missing args (e.g. an omitted `default` on lookup) are omitted from
// the emitted call. Missing required args return unchanged args in positional
// order, which is the responsibility of the PCL binder to validate.
func (g *generator) inlineStdInvoke(call *model.FunctionCallExpression, fn stdTFFunc) (hclwrite.Tokens, hcl.Diagnostics) {
	argsObj, ok := call.Args[1].(*model.ObjectConsExpression)
	if !ok {
		return nil, hcl.Diagnostics{{
			Severity: hcl.DiagError,
			Summary:  "invalid std invoke",
			Detail:   "expected args object",
		}}
	}
	byName := make(map[string]model.Expression, len(argsObj.Items))
	for _, item := range argsObj.Items {
		name, ok := extractStringLiteral(item.Key)
		if !ok {
			continue
		}
		byName[name] = item.Value
	}
	// Collect args in the declared order, dropping trailing nil entries so that
	// optional trailing params (e.g. lookup's `default`) don't show up as empty
	// slots. Non-trailing missing params are left unresolved — the binder should
	// have rejected that already.
	var positional []model.Expression
	for _, name := range fn.inputs {
		positional = append(positional, byName[name])
	}
	for len(positional) > 0 && positional[len(positional)-1] == nil {
		positional = positional[:len(positional)-1]
	}
	return g.passthroughFuncCallTokens(fn.name, positional)
}

// wrapInResultObject wraps a token stream in `{ result = <tokens> }` so that
// later `.result` access on the wrapped expression resolves to the original
// tokens. Used when an inlinable std invoke is referenced without an immediate
// .result traversal (e.g. assigned to a local).
func wrapInResultObject(inner hclwrite.Tokens) hclwrite.Tokens {
	tokens := hclwrite.Tokens{
		{Type: hclsyntax.TokenOBrace, Bytes: []byte("{")},
		{Type: hclsyntax.TokenIdent, Bytes: []byte(" result ")},
		{Type: hclsyntax.TokenEqual, Bytes: []byte("=")},
	}
	if len(inner) > 0 {
		inner[0].SpacesBefore = 1
	}
	tokens = append(tokens, inner...)
	tokens = append(tokens, &hclwrite.Token{Type: hclsyntax.TokenCBrace, Bytes: []byte(" }")})
	return tokens
}

// perIterationIndexTokens returns the index expression that selects the current
// iteration's entry from a data block hoisted with a matching for_each / count.
// For for_each this is `[each.key]`; for count this is `[count.index]`.
func perIterationIndexTokens(kind rangeKind) hclwrite.Tokens {
	open := &hclwrite.Token{Type: hclsyntax.TokenOBrack, Bytes: []byte("[")}
	close := &hclwrite.Token{Type: hclsyntax.TokenCBrack, Bytes: []byte("]")}
	switch kind {
	case rangeKindCount:
		return hclwrite.Tokens{
			open,
			{Type: hclsyntax.TokenIdent, Bytes: []byte("count")},
			{Type: hclsyntax.TokenDot, Bytes: []byte(".")},
			{Type: hclsyntax.TokenIdent, Bytes: []byte("index")},
			close,
		}
	default:
		return hclwrite.Tokens{
			open,
			{Type: hclsyntax.TokenIdent, Bytes: []byte("each")},
			{Type: hclsyntax.TokenDot, Bytes: []byte(".")},
			{Type: hclsyntax.TokenIdent, Bytes: []byte("key")},
			close,
		}
	}
}

// splatTokens generates HCL tokens for a PCL SplatExpression.
//
// PCL:  source.details[*].value
// HCL:  source.details[*].value
//
// The PCL binder merges the relative traversal after [*] into the
// ScopeTraversalExpression rooted at the SplatVariable. So Each is typically a
// ScopeTraversalExpression with Parts[0]=SplatVariable and traversal steps
// [TraverseRoot, TraverseAttr("value"), ...]. We emit source, [*], then the
// traversal steps after the root.
func (g *generator) splatTokens(expr *model.SplatExpression) (hclwrite.Tokens, hcl.Diagnostics) {
	sourceTokens, diags := g.exprTokens(expr.Source, schema.AnyType)
	if diags.HasErrors() {
		return nil, diags
	}

	tokens := append(sourceTokens,
		&hclwrite.Token{Type: hclsyntax.TokenOBrack, Bytes: []byte("[")},
		&hclwrite.Token{Type: hclsyntax.TokenStar, Bytes: []byte("*")},
		&hclwrite.Token{Type: hclsyntax.TokenCBrack, Bytes: []byte("]")},
	)

	// Extract the traversal from the Each expression, skipping the root
	// (which is the SplatVariable, already represented by [*]).
	var eachTraversal hcl.Traversal
	switch each := expr.Each.(type) {
	case *model.ScopeTraversalExpression:
		eachTraversal = each.Traversal[1:]
	case *model.RelativeTraversalExpression:
		eachTraversal = each.Traversal
	default:
		return nil, hcl.Diagnostics{{
			Severity: hcl.DiagError,
			Summary:  "unsupported splat each expression",
			Detail:   fmt.Sprintf("splat each expression type %T is not yet supported", expr.Each),
		}}
	}

	// Rewrite attribute names using schema info from the source's element type,
	// falling back to naive camelCase → snake_case conversion.
	if props := splatElementProps(expr.Source); len(props) > 0 {
		eachTraversal = schemaAwareRewriteTraversal(props, eachTraversal)
	} else {
		eachTraversal = naiveRewriteTraversal(eachTraversal)
	}

	tokens = append(tokens, traversalStepsToTokens(eachTraversal)...)
	return tokens, diags
}

// pickUnionVariantFromObjectExpr resolves a const-discriminated union to
// its matching object variant by reading the discriminator off expr.
// Returns nil when no match can be made.
func pickUnionVariantFromObjectExpr(u *schema.UnionType, expr *model.ObjectConsExpression) *schema.ObjectType {
	candidates := u.ElementTypes
	if u.DefaultType != nil {
		candidates = append([]schema.Type{u.DefaultType}, candidates...)
	}
	type cand struct {
		obj  *schema.ObjectType
		disc *schema.Property
	}
	var withConst []cand
	for _, t := range candidates {
		obj, ok := codegen.UnwrapType(t).(*schema.ObjectType)
		if !ok {
			continue
		}
		var disc *schema.Property
		for _, p := range obj.Properties {
			if p.ConstValue != nil {
				disc = p
				break
			}
		}
		if disc == nil {
			continue
		}
		withConst = append(withConst, cand{obj: obj, disc: disc})
	}
	if len(withConst) == 0 {
		return nil
	}
	discName := withConst[0].disc.Name
	for _, c := range withConst[1:] {
		if c.disc.Name != discName {
			return nil
		}
	}
	for _, item := range expr.Items {
		keyName, ok := extractStringLiteral(item.Key)
		if !ok || keyName != discName {
			continue
		}
		val, ok := extractCtyLiteral(item.Value)
		if !ok {
			return nil
		}
		for _, c := range withConst {
			if transform.CtyEqualsConst(val, c.disc.ConstValue) {
				return c.obj
			}
		}
		return nil
	}
	return nil
}

// extractCtyLiteral pulls a static cty value out of a PCL literal
// expression, unwrapping single-part templates. Reports false when the
// expression isn't a static literal.
func extractCtyLiteral(expr model.Expression) (cty.Value, bool) {
	switch e := expr.(type) {
	case *model.LiteralValueExpression:
		return e.Value, true
	case *model.TemplateExpression:
		if len(e.Parts) == 1 {
			return extractCtyLiteral(e.Parts[0])
		}
	}
	return cty.NilVal, false
}

// extractStringLiteral extracts a string from a literal expression,
// unwrapping TemplateExpressions that contain a single literal part.
func extractStringLiteral(expr model.Expression) (string, bool) {
	switch e := expr.(type) {
	case *model.LiteralValueExpression:
		if e.Value.Type() == cty.String {
			return e.Value.AsString(), true
		}
	case *model.TemplateExpression:
		if len(e.Parts) == 1 {
			return extractStringLiteral(e.Parts[0])
		}
	}
	return "", false
}

func (g *generator) tupleTokens(expr *model.TupleConsExpression, typ schema.Type) (hclwrite.Tokens, hcl.Diagnostics) {
	elemType := schema.Type(schema.AnyType)
	if arr, ok := codegen.UnwrapType(typ).(*schema.ArrayType); ok {
		elemType = arr.ElementType
	}
	tokens := hclwrite.Tokens{
		{Type: hclsyntax.TokenOBrack, Bytes: []byte("[")},
	}
	var diags hcl.Diagnostics
	for i, elem := range expr.Expressions {
		if i > 0 {
			tokens = append(tokens, &hclwrite.Token{Type: hclsyntax.TokenComma, Bytes: []byte(",")})
		}
		elemTokens, d := g.exprTokens(elem, elemType)
		diags = append(diags, d...)
		if d.HasErrors() {
			return nil, diags
		}
		if len(elemTokens) > 0 {
			elemTokens[0].SpacesBefore = 1
		}
		tokens = append(tokens, elemTokens...)
	}
	tokens = append(tokens, &hclwrite.Token{Type: hclsyntax.TokenCBrack, Bytes: []byte("]")})
	return tokens, diags
}

func (g *generator) objectTokens(expr *model.ObjectConsExpression, typ schema.Type) (hclwrite.Tokens, hcl.Diagnostics) {
	tokens := hclwrite.Tokens{
		{Type: hclsyntax.TokenOBrace, Bytes: []byte("{")},
	}
	if len(expr.Items) == 0 {
		tokens = append(tokens, &hclwrite.Token{Type: hclsyntax.TokenCBrace, Bytes: []byte("}")})
		return tokens, nil
	}
	tokens = append(tokens, &hclwrite.Token{Type: hclsyntax.TokenNewline, Bytes: []byte("\n")})

	keyName := func(key model.Expression) (hclwrite.Tokens, hcl.Diagnostics) {
		return g.exprTokens(key, schema.StringType)
	}
	propType := func(key model.Expression) schema.Type {
		return schema.AnyType
	}
	// Narrow a const-discriminated union to its variant so object keys
	// are emitted as snake_case identifiers, not quoted map keys.
	narrowedTyp := codegen.UnwrapType(typ)
	if u, ok := narrowedTyp.(*schema.UnionType); ok {
		if obj := pickUnionVariantFromObjectExpr(u, expr); obj != nil {
			narrowedTyp = obj
		}
	}
	switch typ := narrowedTyp.(type) {
	case *schema.ObjectType:
		keyName = func(key model.Expression) (hclwrite.Tokens, hcl.Diagnostics) {
			name, _ := extractStringLiteral(key)
			return hclwrite.TokensForIdentifier(transform.SnakeCaseFromPulumiCase(name)), nil
		}
		propType = func(key model.Expression) schema.Type {
			name, _ := extractStringLiteral(key)
			if p, ok := typ.Property(name); ok {
				return p.Type
			}
			return schema.AnyType
		}
	case *schema.MapType:
		propType = func(model.Expression) schema.Type {
			return typ.ElementType
		}
	}

	var diags hcl.Diagnostics
	for _, item := range expr.Items {
		keyTokens, d := keyName(item.Key)
		diags = append(diags, d...)
		if d.HasErrors() {
			return nil, diags
		}
		valTokens, d := g.exprTokens(item.Value, propType(item.Key))
		diags = append(diags, d...)
		if d.HasErrors() {
			return nil, diags
		}
		if len(keyTokens) > 0 {
			keyTokens[0].SpacesBefore = 2
		}
		tokens = append(tokens, keyTokens...)
		tokens = append(tokens, &hclwrite.Token{
			Type: hclsyntax.TokenEqual, Bytes: []byte("="), SpacesBefore: 1,
		})
		if len(valTokens) > 0 {
			valTokens[0].SpacesBefore = 1
		}
		tokens = append(tokens, valTokens...)
		tokens = append(tokens, &hclwrite.Token{Type: hclsyntax.TokenNewline, Bytes: []byte("\n")})
	}
	tokens = append(tokens, &hclwrite.Token{Type: hclsyntax.TokenCBrace, Bytes: []byte("}")})
	return tokens, diags
}

// templateTokens generates HCL tokens for a template expression.
// For a single literal part, it returns the literal value directly.
// For multiple parts, it generates a template string like "${expr}suffix".
func (g *generator) templateTokens(expr *model.TemplateExpression) (hclwrite.Tokens, hcl.Diagnostics) {
	if delim, indented, ok := heredocOpen(expr); ok {
		return g.heredocTemplateTokens(expr, delim, indented)
	}

	// If template has a single literal part, just return that literal.
	if len(expr.Parts) == 1 {
		if lit, ok := expr.Parts[0].(*model.LiteralValueExpression); ok {
			return hclwrite.TokensForValue(lit.Value), nil
		}
	}

	// Build an interpolated string: "prefix${expr}suffix"
	var buf strings.Builder
	buf.WriteString(`"`)
	var diags hcl.Diagnostics

	for _, part := range expr.Parts {
		switch p := part.(type) {
		case *model.LiteralValueExpression:
			if p.Value.Type() == cty.String {
				buf.WriteString(p.Value.AsString())
			} else {
				fmt.Fprintf(&buf, "${%s}", p.Value.GoString())
			}
		default:
			partTokens, d := g.exprTokens(part, schema.AnyType)
			diags = append(diags, d...)
			if d.HasErrors() {
				return nil, diags
			}
			buf.WriteString("${")
			for _, tok := range partTokens {
				buf.Write(tok.Bytes)
			}
			buf.WriteString("}")
		}
	}
	buf.WriteString(`"`)

	return hclwrite.Tokens{
		{Type: hclsyntax.TokenQuotedLit, Bytes: []byte(buf.String())},
	}, diags
}

// heredocTemplateTokens emits a template expression as a heredoc, preserving
// the source delimiter and indent-strip style. Interpolated parts are wrapped
// in `${...}` exactly as in the source; literal parts are written verbatim so
// embedded newlines stay as newlines (not `\n` escapes) inside the heredoc.
func (g *generator) heredocTemplateTokens(
	expr *model.TemplateExpression, delim string, indented bool,
) (hclwrite.Tokens, hcl.Diagnostics) {
	prefix := "<<"
	if indented {
		prefix = "<<-"
	}
	tokens := hclwrite.Tokens{
		{Type: hclsyntax.TokenOHeredoc, Bytes: []byte(prefix + delim + "\n")},
	}
	var diags hcl.Diagnostics
	var trailingNewline bool
	for i, part := range expr.Parts {
		switch p := part.(type) {
		case *model.LiteralValueExpression:
			s := ""
			if p.Value.Type() == cty.String {
				s = p.Value.AsString()
			} else {
				s = fmt.Sprintf("%v", p.Value.GoString())
			}
			tokens = append(tokens, &hclwrite.Token{
				Type: hclsyntax.TokenStringLit, Bytes: []byte(s),
			})
			trailingNewline = i == len(expr.Parts)-1 && strings.HasSuffix(s, "\n")
		default:
			partTokens, d := g.exprTokens(part, schema.AnyType)
			diags = append(diags, d...)
			if d.HasErrors() {
				return nil, diags
			}
			tokens = append(tokens,
				&hclwrite.Token{Type: hclsyntax.TokenTemplateInterp, Bytes: []byte("${")},
			)
			tokens = append(tokens, partTokens...)
			tokens = append(tokens,
				&hclwrite.Token{Type: hclsyntax.TokenTemplateSeqEnd, Bytes: []byte("}")},
			)
			trailingNewline = false
		}
	}
	// HCL requires the closing delimiter to start a new line; if the literal
	// content does not already end with a newline, insert one so the heredoc
	// is well-formed. The trailing newline becomes part of the parsed value,
	// matching how the source heredoc would have been read.
	if !trailingNewline {
		tokens = append(tokens, &hclwrite.Token{
			Type: hclsyntax.TokenStringLit, Bytes: []byte("\n"),
		})
	}
	tokens = append(tokens, &hclwrite.Token{
		Type: hclsyntax.TokenCHeredoc, Bytes: []byte(delim + "\n"),
	})
	return tokens, diags
}

// heredocOpen inspects expr's recorded open token to recover the heredoc form
// from the source PCL. It returns the delimiter identifier (e.g. "EOT") and
// whether the source used the indent-stripping `<<-` variant. ok is false if
// expr did not originate from a heredoc.
func heredocOpen(expr *model.TemplateExpression) (delim string, indented bool, ok bool) {
	if expr.Tokens == nil {
		return "", false, false
	}
	open := expr.Tokens.GetOpen()
	if open.Raw.Type != hclsyntax.TokenOHeredoc {
		return "", false, false
	}
	// Open token bytes are "<<EOT\n" or "<<-EOT\n"; strip leading "<<" /
	// "<<-" and the trailing newline to recover the bare delimiter.
	raw := strings.TrimRight(string(open.Raw.Bytes), "\r\n")
	raw = strings.TrimPrefix(raw, "<<")
	if after, ok := strings.CutPrefix(raw, "-"); ok {
		return after, true, true
	}
	return raw, false, true
}

// binaryOpTokens generates HCL tokens for a binary operation expression.
func (g *generator) binaryOpTokens(expr *model.BinaryOpExpression) (hclwrite.Tokens, hcl.Diagnostics) {
	leftTokens, diags := g.exprTokens(expr.LeftOperand, schema.AnyType)
	if diags.HasErrors() {
		return nil, diags
	}

	rightTokens, d := g.exprTokens(expr.RightOperand, schema.AnyType)
	diags = append(diags, d...)
	if d.HasErrors() {
		return nil, diags
	}

	var opStr string
	switch expr.Operation {
	case hclsyntax.OpLogicalOr:
		opStr = "||"
	case hclsyntax.OpLogicalAnd:
		opStr = "&&"
	case hclsyntax.OpEqual:
		opStr = "=="
	case hclsyntax.OpNotEqual:
		opStr = "!="
	case hclsyntax.OpGreaterThan:
		opStr = ">"
	case hclsyntax.OpGreaterThanOrEqual:
		opStr = ">="
	case hclsyntax.OpLessThan:
		opStr = "<"
	case hclsyntax.OpLessThanOrEqual:
		opStr = "<="
	case hclsyntax.OpAdd:
		opStr = "+"
	case hclsyntax.OpSubtract:
		opStr = "-"
	case hclsyntax.OpMultiply:
		opStr = "*"
	case hclsyntax.OpDivide:
		opStr = "/"
	case hclsyntax.OpModulo:
		opStr = "%"
	default:
		return nil, hcl.Diagnostics{{
			Severity: hcl.DiagError,
			Summary:  "unsupported binary operation",
			Detail:   fmt.Sprintf("binary operation %v is not yet supported", expr.Operation),
		}}
	}

	tokens := leftTokens
	tokens = append(tokens, &hclwrite.Token{
		Type: hclsyntax.TokenIdent, Bytes: []byte(opStr), SpacesBefore: 1,
	})
	if len(rightTokens) > 0 {
		rightTokens[0].SpacesBefore = 1
	}
	tokens = append(tokens, rightTokens...)
	return tokens, diags
}

// conditionalTokens generates HCL tokens for a conditional expression (condition ? true : false).
func (g *generator) conditionalTokens(expr *model.ConditionalExpression) (hclwrite.Tokens, hcl.Diagnostics) {
	condTokens, diags := g.exprTokens(expr.Condition, schema.AnyType)
	if diags.HasErrors() {
		return nil, diags
	}

	trueTokens, d := g.exprTokens(expr.TrueResult, schema.AnyType)
	diags = append(diags, d...)
	if d.HasErrors() {
		return nil, diags
	}

	falseTokens, d := g.exprTokens(expr.FalseResult, schema.AnyType)
	diags = append(diags, d...)
	if d.HasErrors() {
		return nil, diags
	}

	tokens := condTokens
	tokens = append(tokens, &hclwrite.Token{
		Type: hclsyntax.TokenQuestion, Bytes: []byte("?"), SpacesBefore: 1,
	})
	if len(trueTokens) > 0 {
		trueTokens[0].SpacesBefore = 1
	}
	tokens = append(tokens, trueTokens...)
	tokens = append(tokens, &hclwrite.Token{
		Type: hclsyntax.TokenColon, Bytes: []byte(":"), SpacesBefore: 1,
	})
	if len(falseTokens) > 0 {
		falseTokens[0].SpacesBefore = 1
	}
	tokens = append(tokens, falseTokens...)
	return tokens, diags
}

// unaryOpTokens generates HCL tokens for a unary operation expression.
func (g *generator) unaryOpTokens(expr *model.UnaryOpExpression) (hclwrite.Tokens, hcl.Diagnostics) {
	operandTokens, diags := g.exprTokens(expr.Operand, schema.AnyType)
	if diags.HasErrors() {
		return nil, diags
	}

	var opStr string
	switch expr.Operation {
	case hclsyntax.OpLogicalNot:
		opStr = "!"
	case hclsyntax.OpNegate:
		opStr = "-"
	default:
		return nil, hcl.Diagnostics{{
			Severity: hcl.DiagError,
			Summary:  "unsupported unary operation",
			Detail:   fmt.Sprintf("unary operation %v is not yet supported", expr.Operation),
		}}
	}

	tokens := hclwrite.Tokens{
		{Type: hclsyntax.TokenIdent, Bytes: []byte(opStr)},
	}
	tokens = append(tokens, operandTokens...)
	return tokens, diags
}
