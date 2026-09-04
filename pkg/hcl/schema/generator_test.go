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

package schema

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/blang/semver"
	"github.com/pulumi/pulumi-hcl/pkg/hcl/ast"
	"github.com/pulumi/pulumi-hcl/pkg/hcl/bridge"
	"github.com/pulumi/pulumi-hcl/pkg/hcl/packages"
	"github.com/pulumi/pulumi-hcl/pkg/hcl/parser"
	pulumiSchema "github.com/pulumi/pulumi/pkg/v3/codegen/schema"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource"
	"github.com/pulumi/pulumi/sdk/v3/go/common/tokens"
	"github.com/pulumi/pulumi/sdk/v3/go/common/util/cmdutil"
	"github.com/pulumi/pulumi/sdk/v3/go/property"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// componentToken assembles a component type token from its parts.
func componentToken(pkg, module, component string) tokens.Type {
	return tokens.Type(pkg + ":" + module + ":" + component)
}

// TestGenerateModuleSchemaGolden parses the HCL in each testdata case and
// asserts the Pulumi package schema produced by GenerateModuleSchema followed by
// ToPulumiPackageSchema against a golden schema.json. Run with PULUMI_ACCEPT=1 to
// (re)generate the golden files.
//
// These cases pass a nil binder, so they cover only variable, local, and literal
// typing — their fixtures reference no resources, data sources, or modules (a
// reference with no binder to resolve it is an error). Resolver- and
// loader-backed typing is covered by the focused tests below. The package and
// component identity (name, version, component name, module segment) is supplied
// by NewHCLProvider in production, so each case passes those values explicitly.
func TestGenerateModuleSchemaGolden(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name          string
		pkgName       string
		version       string
		componentName string
		module        string
	}{
		{name: "primitives", pkgName: "primitives", version: "1.2.3", componentName: "primitives", module: "index"},
		{name: "collections", pkgName: "collections", version: "0.0.0-dev", componentName: "widget", module: "infra"},
		{name: "sensitive", pkgName: "sensitive", version: "0.0.0-dev", componentName: "sensitive", module: "index"},
		{name: "required", pkgName: "required", version: "0.0.0-dev", componentName: "required", module: "index"},
		{name: "inference", pkgName: "inference", version: "0.0.0-dev", componentName: "inference", module: "index"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			caseDir := filepath.Join("testdata", tc.name)
			config, diags := parser.NewParser().ParseDirectory(caseDir)
			require.False(t, diags.HasErrors(), diags.Error())

			moduleSchema, err := GenerateModuleSchema(
				t.Context(), config, nil, componentToken(tc.pkgName, tc.module, tc.componentName), semver.MustParse(tc.version))
			require.NoError(t, err)

			pkgSpec := moduleSchema.ToPulumiPackageSchema()
			got, err := json.MarshalIndent(pkgSpec, "", "  ")
			require.NoError(t, err)
			got = append(got, '\n')

			// Bind the generated schema against the Pulumi package metaschema so
			// that an invalid spec (e.g. a constant default on a non-primitive
			// property) fails the test rather than only `pulumi schema check`.
			_, bindDiags, err := pulumiSchema.BindSpec(pkgSpec, errLoader{}, pulumiSchema.ValidationOptions{})
			require.NoError(t, err)
			require.False(t, bindDiags.HasErrors(), bindDiags.Error())

			goldenPath := filepath.Join(caseDir, "schema.json")
			if cmdutil.IsTruthy(os.Getenv("PULUMI_ACCEPT")) {
				require.NoError(t, os.WriteFile(goldenPath, got, 0o644))
				return
			}

			want, err := os.ReadFile(goldenPath)
			require.NoError(t, err, "golden file %s not found; run with PULUMI_ACCEPT=1 to generate", goldenPath)
			assert.Equal(t, string(want), string(got))
		})
	}
}

// TestGenerateModuleSchemaFileLocalUsesModuleDir verifies that schema inference
// evaluates file-backed locals relative to the module directory: a
// parameterized module is generally not located in the provider's working
// directory, so resolving path.module against "." would fail to find the file.
func TestGenerateModuleSchemaFileLocalUsesModuleDir(t *testing.T) {
	t.Parallel()

	moduleDir := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(moduleDir, "data.json"), []byte(`{"count":42}`), 0o644))

	config, diags := parser.NewParser().ParseSource(filepath.Join(moduleDir, "main.tf"), []byte(`
locals {
  from_file = jsondecode(file("${path.module}/data.json"))
  derived   = local.from_file.count
}

output "count" {
  value = local.derived
}
`))
	require.False(t, diags.HasErrors(), diags.Error())

	moduleSchema, err := GenerateModuleSchema(
		t.Context(), config, &Binder{ModuleDir: moduleDir},
		componentToken("pkg", "index", "pkg"), semver.MustParse("0.0.0-dev"))
	require.NoError(t, err)
	assert.Equal(t, &PropertySpec{Type: TypeNumber}, moduleSchema.OutputProperties["count"])
}

// TestGenerateModuleSchemaFileLocalMissingFile shows that a file-backed local
// whose file is absent at schema generation time types to any instead of
// failing schema generation: the file may only be produced at runtime.
func TestGenerateModuleSchemaFileLocalMissingFile(t *testing.T) {
	t.Parallel()

	moduleDir := t.TempDir()
	config, diags := parser.NewParser().ParseSource(filepath.Join(moduleDir, "main.tf"), []byte(`
locals {
  from_file = jsondecode(file("${path.module}/missing.json"))
}

output "count" {
  value = local.from_file.count
}
`))
	require.False(t, diags.HasErrors(), diags.Error())

	moduleSchema, err := GenerateModuleSchema(
		t.Context(), config, &Binder{ModuleDir: moduleDir},
		componentToken("pkg", "index", "pkg"), semver.MustParse("0.0.0-dev"))
	require.NoError(t, err)
	assert.Equal(t, &PropertySpec{Type: TypeAny}, moduleSchema.OutputProperties["count"])
}

// stubResolver resolves a fixed set of resources by TF type, plus optionally a
// fixed set of provider-defined functions, so reference typing can be tested
// without a provider schema.
type stubResolver struct {
	resources         map[string]*pulumiSchema.Resource
	providerFunctions map[string]map[string]packages.ProviderFunction
}

func (s stubResolver) ResolveResource(_ context.Context, tfType string) (*pulumiSchema.Resource, error) {
	return s.resources[tfType], nil
}

func (stubResolver) ResolveFunction(_ context.Context, _ string) (*pulumiSchema.Function, error) {
	return nil, nil
}
func (stubResolver) ResourceBodyMapping(_ context.Context, _ string) *bridge.BodyMapping { return nil }
func (stubResolver) DataSourceBodyMapping(_ context.Context, _ string) *bridge.BodyMapping {
	return nil
}

func (s stubResolver) ProviderFunctions(
	_ context.Context, providerName string,
) (map[string]packages.ProviderFunction, error) {
	fns, ok := s.providerFunctions[providerName]
	if !ok {
		return nil, fmt.Errorf("unknown provider %q", providerName)
	}
	return fns, nil
}

// TestResourceReferenceOutputsAreTyped shows that an output referencing a
// resource attribute is typed from the resolved provider schema (and that the
// synthetic id attribute is always available).
func TestResourceReferenceOutputsAreTyped(t *testing.T) {
	t.Parallel()

	const src = `
resource "random_pet" "name" {
  length = 2
}

output "id" {
  value = random_pet.name.id
}

output "pet_name" {
  value = random_pet.name.pet_name
}

output "length" {
  value = random_pet.name.length
}
`
	config, diags := parser.NewParser().ParseSource("main.tf", []byte(src))
	require.False(t, diags.HasErrors(), diags.Error())

	resolver := stubResolver{resources: map[string]*pulumiSchema.Resource{
		"random_pet": {
			Properties: []*pulumiSchema.Property{
				{Name: "petName", Type: pulumiSchema.StringType},
				{Name: "length", Type: pulumiSchema.IntType},
			},
		},
	}}

	moduleSchema, err := GenerateModuleSchema(
		t.Context(), config, &Binder{Resources: resolver}, componentToken("pkg", "index", "pkg"), semver.MustParse("0.0.0-dev"))
	require.NoError(t, err)

	assert.Equal(t, map[string]*PropertySpec{
		"id":       {Type: TypeString},
		"pet_name": {Type: TypeString},
		"length":   {Type: TypeNumber},
	}, moduleSchema.OutputProperties)
}

// TestRangedResourceOutputsAreTyped shows that a counted resource references as
// a list of its object and a for_each resource as a map of its object.
func TestRangedResourceOutputsAreTyped(t *testing.T) {
	t.Parallel()

	const src = `
resource "random_pet" "counted" {
  count  = 3
  length = 2
}

resource "random_pet" "keyed" {
  for_each = { a = 1 }
  length   = 2
}

variable "i" {
  type = number
}

output "counted_id" {
  value = random_pet.counted[0].id
}

output "all_counted" {
  value = random_pet.counted
}

output "keyed_id" {
  value = random_pet.keyed["a"].id
}

output "counted_instance" {
  value = random_pet.counted[0]
}

output "keyed_instance" {
  value = random_pet.keyed["a"]
}

output "indexed_by_var" {
  value = random_pet.counted[var.i]
}

output "parenthesized_length" {
  value = (random_pet.keyed)["a"].length
}
`
	config, diags := parser.NewParser().ParseSource("main.tf", []byte(src))
	require.False(t, diags.HasErrors(), diags.Error())

	resolver := stubResolver{resources: map[string]*pulumiSchema.Resource{
		"random_pet": {Properties: []*pulumiSchema.Property{{Name: "length", Type: pulumiSchema.IntType}}},
	}}
	moduleSchema, err := GenerateModuleSchema(
		t.Context(), config, &Binder{Resources: resolver}, componentToken("pkg", "index", "pkg"), semver.MustParse("0.0.0-dev"))
	require.NoError(t, err)

	elem := &PropertySpec{Type: TypeObject, Properties: map[string]*PropertySpec{
		"id":     {Type: TypeString},
		"length": {Type: TypeNumber},
	}, Required: []string{"length"}}
	assert.Equal(t, map[string]*PropertySpec{
		"counted_id":           {Type: TypeString},
		"keyed_id":             {Type: TypeString},
		"all_counted":          {Type: TypeArray, Items: elem},
		"counted_instance":     elem,
		"keyed_instance":       elem,
		"indexed_by_var":       elem,
		"parenthesized_length": {Type: TypeNumber},
	}, moduleSchema.OutputProperties)
	assert.Equal(t, []string{
		"all_counted", "counted_instance", "indexed_by_var", "keyed_instance", "parenthesized_length",
	}, moduleSchema.RequiredOutputs)
}

// TestIndexedVariableElementStaysNullable shows that only a ranged reference
// refines the instance an index yields: a list variable may hold null
// elements, so an element indexed out of it stays nullable, as do its
// attributes.
func TestIndexedVariableElementStaysNullable(t *testing.T) {
	t.Parallel()

	const src = `
variable "l" {
  type     = list(object({ a = string }))
  nullable = false
}

output "first" {
  value = var.l[0]
}

output "first_a" {
  value = var.l[0].a
}
`
	config, diags := parser.NewParser().ParseSource("main.tf", []byte(src))
	require.False(t, diags.HasErrors(), diags.Error())

	moduleSchema, err := GenerateModuleSchema(
		t.Context(), config, nil, componentToken("pkg", "index", "pkg"), semver.MustParse("0.0.0-dev"))
	require.NoError(t, err)
	assert.Equal(t, map[string]*PropertySpec{
		"first":   {Type: TypeObject, Properties: map[string]*PropertySpec{"a": {Type: TypeString}}},
		"first_a": {Type: TypeString},
	}, moduleSchema.OutputProperties)
	assert.Equal(t, []string{"l"}, moduleSchema.RequiredOutputs)
}

// TestUnsupportedAttributeIsError shows that a reference to an attribute the
// resolved schema lacks fails typing with HCL's own diagnostic.
func TestUnsupportedAttributeIsError(t *testing.T) {
	t.Parallel()

	const src = `
resource "random_pet" "name" {
  length = 2
}

output "missing" {
  value = random_pet.name.missing
}
`
	config, diags := parser.NewParser().ParseSource("main.tf", []byte(src))
	require.False(t, diags.HasErrors(), diags.Error())

	resolver := stubResolver{resources: map[string]*pulumiSchema.Resource{
		"random_pet": {Properties: []*pulumiSchema.Property{{Name: "length", Type: pulumiSchema.IntType}}},
	}}
	_, err := GenerateModuleSchema(
		t.Context(), config, &Binder{Resources: resolver}, componentToken("pkg", "index", "pkg"), semver.MustParse("0.0.0-dev"))
	require.EqualError(t, err, `typing output "missing": main.tf:7,26-34: Unsupported attribute; This object does not have an attribute named "missing".`)
}

// mappingResolver resolves resources and their bridge body mappings by TF type,
// so output typing that depends on the mapping (e.g. MaxItemsOne block shape)
// can be tested without a live provider.
type mappingResolver struct {
	resources          map[string]*pulumiSchema.Resource
	mappings           map[string]*bridge.BodyMapping
	dataSources        map[string]*pulumiSchema.Function
	dataSourceMappings map[string]*bridge.BodyMapping
}

func (s mappingResolver) ResolveResource(_ context.Context, tfType string) (*pulumiSchema.Resource, error) {
	return s.resources[tfType], nil
}

func (s mappingResolver) ResolveFunction(_ context.Context, tfType string) (*pulumiSchema.Function, error) {
	return s.dataSources[tfType], nil
}

func (s mappingResolver) ResourceBodyMapping(_ context.Context, tfType string) *bridge.BodyMapping {
	return s.mappings[tfType]
}

func (s mappingResolver) DataSourceBodyMapping(_ context.Context, tfType string) *bridge.BodyMapping {
	return s.dataSourceMappings[tfType]
}

func (mappingResolver) ProviderFunctions(
	_ context.Context, providerName string,
) (map[string]packages.ProviderFunction, error) {
	return nil, fmt.Errorf("unknown provider %q", providerName)
}

// a MaxItems:1 block is flattened to a single object on the Pulumi side, but TF/OpenTofu
// models it as a list, so HCL indexes it as `block[0].attr`. Output typing must re-wrap
// the flattened object as a list — mirroring the runtime — so the numeric index types
// instead of failing with "Invalid index".
func TestSingularBlockIndexedOutputIsTyped(t *testing.T) {
	t.Parallel()

	const src = `
resource "blocky_thing" "t" {
}

output "mode" {
  value = blocky_thing.t.settings[0].mode
}
`
	config, diags := parser.NewParser().ParseSource("main.tf", []byte(src))
	require.False(t, diags.HasErrors(), diags.Error())

	resolver := mappingResolver{
		resources: map[string]*pulumiSchema.Resource{
			"blocky_thing": {Properties: []*pulumiSchema.Property{
				{Name: "settings", Type: &pulumiSchema.ObjectType{Properties: []*pulumiSchema.Property{
					{Name: "mode", Type: pulumiSchema.StringType},
				}}},
			}},
		},
		mappings: map[string]*bridge.BodyMapping{
			"blocky_thing": {Fields: map[string]*bridge.FieldMapping{
				"settings": {
					TFName: "settings", PulumiName: "settings", TFBlock: true, MaxItemsOne: true,
					Nested: &bridge.BodyMapping{Fields: map[string]*bridge.FieldMapping{
						"mode": {TFName: "mode", PulumiName: "mode"},
					}},
				},
			}},
		},
	}

	moduleSchema, err := GenerateModuleSchema(
		t.Context(), config, &Binder{Resources: resolver}, componentToken("pkg", "index", "pkg"), semver.MustParse("0.0.0-dev"))
	require.NoError(t, err)

	assert.Equal(t, map[string]*PropertySpec{"mode": {Type: TypeString}}, moduleSchema.OutputProperties)
}

func TestTryWrappedScalarOutputIsTyped(t *testing.T) {
	t.Parallel()

	const src = `
resource "random_pet" "this" {
  length = 2
}

output "bare_id" {
  value = random_pet.this.id
}

output "try_id" {
  value = try(random_pet.this.id, null)
}
`
	config, diags := parser.NewParser().ParseSource("main.tf", []byte(src))
	require.False(t, diags.HasErrors(), diags.Error())

	resolver := stubResolver{resources: map[string]*pulumiSchema.Resource{
		"random_pet": {Properties: []*pulumiSchema.Property{{Name: "length", Type: pulumiSchema.IntType}}},
	}}
	moduleSchema, err := GenerateModuleSchema(
		t.Context(), config, &Binder{Resources: resolver}, componentToken("pkg", "index", "pkg"), semver.MustParse("0.0.0-dev"))
	require.NoError(t, err)

	assert.Equal(t, map[string]*PropertySpec{
		"bare_id": {Type: TypeString},
		"try_id":  {Type: TypeString},
	}, moduleSchema.OutputProperties)
}

// TestOutputRequiredness shows that an output is required exactly when its value
// can never be null: non-null literals, non-null inputs, and required resource
// attributes are required, while nullable inputs, optional attributes,
// try(_, null), and a null conditional branch make the output optional. Inputs
// are echoed as outputs, so a non-null input is also a required output.
func TestOutputRequiredness(t *testing.T) {
	t.Parallel()

	const src = `
variable "req" {
  type     = string
  nullable = false
}

variable "opt" {
  type = string
}

resource "random_pet" "this" {
  length = 2
}

output "literal"      { value = "x" }
output "from_req_var" { value = var.req }
output "from_opt_var" { value = var.opt }
output "req_attr"     { value = random_pet.this.length }
output "opt_attr"     { value = random_pet.this.nick }
output "try_null"     { value = try(random_pet.this.length, null) }
output "conditional"  { value = var.opt != null ? random_pet.this.length : null }
output "config"       { value = random_pet.this.config }
`
	config, diags := parser.NewParser().ParseSource("main.tf", []byte(src))
	require.False(t, diags.HasErrors(), diags.Error())

	resolver := stubResolver{resources: map[string]*pulumiSchema.Resource{
		"random_pet": {Properties: []*pulumiSchema.Property{
			{Name: "length", Type: pulumiSchema.IntType},
			{Name: "nick", Type: &pulumiSchema.OptionalType{ElementType: pulumiSchema.StringType}},
			{Name: "config", Type: &pulumiSchema.ObjectType{Properties: []*pulumiSchema.Property{
				{Name: "host", Type: pulumiSchema.StringType},
				{Name: "port", Type: &pulumiSchema.OptionalType{ElementType: pulumiSchema.IntType}},
			}}},
		}},
	}}
	moduleSchema, err := GenerateModuleSchema(
		t.Context(), config, &Binder{Resources: resolver}, componentToken("pkg", "index", "pkg"), semver.MustParse("0.0.0-dev"))
	require.NoError(t, err)

	assert.Equal(t, []string{"config", "from_req_var", "literal", "req", "req_attr"}, moduleSchema.RequiredOutputs)

	assert.Equal(t, &PropertySpec{
		Type:     "object",
		Required: []string{"host"},
		Properties: map[string]*PropertySpec{
			"host": {Type: TypeString},
			"port": {Type: TypeNumber},
		},
	}, moduleSchema.OutputProperties["config"])
}

// stubModuleLoader resolves child modules from parsed configs keyed by source.
type stubModuleLoader struct {
	configs map[string]*ast.Config
}

func (s stubModuleLoader) LoadModule(_ context.Context, source, _, _ string) (*ast.Config, string, error) {
	cfg, ok := s.configs[source]
	if !ok {
		return nil, "", fmt.Errorf("no module %q", source)
	}
	return cfg, source, nil
}

// TestModuleOutputsAreTyped shows that a module.<name>.<output> reference is
// typed by recursively typing the child module's outputs.
func TestModuleOutputsAreTyped(t *testing.T) {
	t.Parallel()

	child, childDiags := parser.NewParser().ParseSource("child.tf", []byte(`
variable "name" {
  type = string
}

output "greeting" {
  value = "hello ${var.name}"
}

output "n" {
  value = 42
}
`))
	require.False(t, childDiags.HasErrors(), childDiags.Error())

	const parent = `
module "child" {
  source = "./child"
  name   = "world"
}

output "greeting" {
  value = module.child.greeting
}

output "n" {
  value = module.child.n
}
`
	config, diags := parser.NewParser().ParseSource("main.tf", []byte(parent))
	require.False(t, diags.HasErrors(), diags.Error())

	binder := &Binder{
		Modules:   stubModuleLoader{configs: map[string]*ast.Config{"./child": child}},
		ModuleDir: ".",
	}
	moduleSchema, err := GenerateModuleSchema(
		t.Context(), config, binder, componentToken("pkg", "index", "pkg"), semver.MustParse("0.0.0-dev"))
	require.NoError(t, err)

	assert.Equal(t, map[string]*PropertySpec{
		"greeting": {Type: TypeString},
		"n":        {Type: TypeNumber},
	}, moduleSchema.OutputProperties)
}

// TestModuleOutputRequirednessPropagates shows that a child module output's
// nullability rides through a module.<name>.<output> reference, so the parent
// output is required exactly when the child output can never be null.
func TestModuleOutputRequirednessPropagates(t *testing.T) {
	t.Parallel()

	child, childDiags := parser.NewParser().ParseSource("child.tf", []byte(`
variable "name" {
  type     = string
  nullable = false
}

output "always" {
  value = "hello ${var.name}"
}

output "maybe" {
  value = try(var.name, null)
}
`))
	require.False(t, childDiags.HasErrors(), childDiags.Error())

	const parent = `
module "child" {
  source = "./child"
  name   = "world"
}

output "always" {
  value = module.child.always
}

output "maybe" {
  value = module.child.maybe
}
`
	config, diags := parser.NewParser().ParseSource("main.tf", []byte(parent))
	require.False(t, diags.HasErrors(), diags.Error())

	binder := &Binder{
		Modules:   stubModuleLoader{configs: map[string]*ast.Config{"./child": child}},
		ModuleDir: ".",
	}
	moduleSchema, err := GenerateModuleSchema(
		t.Context(), config, binder, componentToken("pkg", "index", "pkg"), semver.MustParse("0.0.0-dev"))
	require.NoError(t, err)

	assert.Equal(t, []string{"always"}, moduleSchema.RequiredOutputs)
}

// TestUnresolvableResourceIsError shows that a resource whose type cannot be
// resolved raises an error rather than degrading to an untyped output.
func TestUnresolvableResourceIsError(t *testing.T) {
	t.Parallel()

	const src = `
resource "mystery_thing" "x" {
}

output "id" {
  value = mystery_thing.x.id
}
`
	config, diags := parser.NewParser().ParseSource("main.tf", []byte(src))
	require.False(t, diags.HasErrors(), diags.Error())

	_, err := GenerateModuleSchema(
		t.Context(), config, &Binder{Resources: stubResolver{}}, componentToken("pkg", "index", "pkg"), semver.MustParse("0.0.0-dev"))
	require.EqualError(t, err, `resolving resource "mystery_thing.x": no schema for type "mystery_thing"`)
}

// TestModuleCycleIsError shows that a cycle among modules raises an error rather
// than silently terminating with untyped references.
func TestModuleCycleIsError(t *testing.T) {
	t.Parallel()

	parse := func(src string) *ast.Config {
		cfg, diags := parser.NewParser().ParseSource("m.tf", []byte(src))
		require.False(t, diags.HasErrors(), diags.Error())
		return cfg
	}

	a := parse(`
module "b" {
  source = "./b"
}
output "x" {
  value = module.b.y
}
`)
	b := parse(`
module "a" {
  source = "./a"
}
output "y" {
  value = module.a.x
}
`)
	root := parse(`
module "a" {
  source = "./a"
}
output "z" {
  value = module.a.x
}
`)
	binder := &Binder{
		Modules:   stubModuleLoader{configs: map[string]*ast.Config{"./a": a, "./b": b}},
		ModuleDir: ".",
	}
	_, err := GenerateModuleSchema(t.Context(), root, binder, componentToken("pkg", "index", "pkg"), semver.MustParse("0.0.0-dev"))
	require.Error(t, err)
}

// TestBoundaryNameConversion shows that the Construct boundary renames object
// field names (snake_case ↔ camelCase) at every depth in both directions, while
// leaving the dynamic keys of a map untouched.
func TestBoundaryNameConversion(t *testing.T) {
	t.Parallel()

	s := &ModuleSchema{
		InputProperties: map[string]*PropertySpec{
			"object_in": {Type: TypeObject, Properties: map[string]*PropertySpec{
				"field_one": {Type: TypeString},
			}},
			"map_in": {Type: TypeObject, AdditionalProperties: &PropertySpec{Type: TypeString}},
		},
		OutputProperties: map[string]*PropertySpec{
			"object_out": {Type: TypeObject, Properties: map[string]*PropertySpec{
				"field_two": {Type: TypeString},
			}},
			"map_out": {Type: TypeObject, AdditionalProperties: &PropertySpec{Type: TypeString}},
		},
	}

	propMap := func(m map[string]any) property.Map {
		return resource.FromResourcePropertyMap(resource.NewPropertyMapFromMap(m))
	}

	// Inputs arrive camelCase (top-level names and object fields). A map's keys
	// are user data, so a key that looks like snake_case stays verbatim.
	assert.Equal(t, propMap(map[string]any{
		"object_in": map[string]any{"field_one": "a"},
		"map_in":    map[string]any{"user_key": "b"},
	}), s.InputsToHCL(propMap(map[string]any{
		"objectIn": map[string]any{"fieldOne": "a"},
		"mapIn":    map[string]any{"user_key": "b"},
	})))

	// Outputs arrive snake_case from HCL; object fields become camelCase, map
	// keys are preserved verbatim.
	assert.Equal(t, propMap(map[string]any{
		"objectOut": map[string]any{"fieldTwo": "c"},
		"mapOut":    map[string]any{"user_key": "d"},
	}), s.OutputsToPulumi(propMap(map[string]any{
		"object_out": map[string]any{"field_two": "c"},
		"map_out":    map[string]any{"user_key": "d"},
	})))
}

// TestUnionBoundaryNameConversion shows that a union output's fields are
// renamed whichever member the value is, including a field only one member
// declares and a nested object inside a member.
func TestUnionBoundaryNameConversion(t *testing.T) {
	t.Parallel()

	s := &ModuleSchema{
		OutputProperties: map[string]*PropertySpec{
			"union_out": {OneOf: []*PropertySpec{
				{Type: TypeObject, Properties: map[string]*PropertySpec{
					"shared_field": {Type: TypeString},
					"only_first": {Type: TypeObject, Properties: map[string]*PropertySpec{
						"nested_field": {Type: TypeString},
					}},
				}},
				{Type: TypeObject, Properties: map[string]*PropertySpec{
					"shared_field": {Type: TypeString},
					"only_second":  {Type: TypeString},
				}},
			}},
		},
	}
	propMap := func(m map[string]any) property.Map {
		return resource.FromResourcePropertyMap(resource.NewPropertyMapFromMap(m))
	}

	assert.Equal(t, propMap(map[string]any{
		"unionOut": map[string]any{"sharedField": "a", "onlyFirst": map[string]any{"nestedField": "b"}},
	}), s.OutputsToPulumi(propMap(map[string]any{
		"union_out": map[string]any{"shared_field": "a", "only_first": map[string]any{"nested_field": "b"}},
	})))
	assert.Equal(t, propMap(map[string]any{
		"unionOut": map[string]any{"sharedField": "c", "onlySecond": "d"},
	}), s.OutputsToPulumi(propMap(map[string]any{
		"union_out": map[string]any{"shared_field": "c", "only_second": "d"},
	})))
}

// TestAnyTypedVariableIsAny reproduces
// https://github.com/pulumi/pulumi-hcl/issues/515: a module input declared
// `type = any` (or with no type constraint) must surface in the Pulumi schema
// as the Any type, not as a bare `object` — a bare `object` type spec means a
// map of string, so generated SDKs reject a plain string value. An empty
// object type is not any: it stays a named object type.
func TestAnyTypedVariableIsAny(t *testing.T) {
	t.Parallel()

	const src = `
variable "source_path" {
  type = any
}

variable "untyped" {
}

variable "empty_object" {
  type = object({})
}
`
	config, diags := parser.NewParser().ParseSource("main.tf", []byte(src))
	require.False(t, diags.HasErrors(), diags.Error())

	moduleSchema, err := GenerateModuleSchema(
		t.Context(), config, nil, componentToken("lambda", "index", "Module"), semver.MustParse("0.0.0-dev"))
	require.NoError(t, err)

	pkgSpec := moduleSchema.ToPulumiPackageSchema()
	_, bindDiags, err := pulumiSchema.BindSpec(pkgSpec, errLoader{}, pulumiSchema.ValidationOptions{})
	require.NoError(t, err)
	require.False(t, bindDiags.HasErrors(), bindDiags.Error())

	inputs := pkgSpec.Resources["lambda:index:Module"].InputProperties
	anyType := pulumiSchema.TypeSpec{Ref: "pulumi.json#/Any"}
	assert.Equal(t, anyType, inputs["sourcePath"].TypeSpec)
	assert.Equal(t, anyType, inputs["untyped"].TypeSpec)
	assert.Equal(t, pulumiSchema.TypeSpec{Ref: "#/types/lambda:index:EmptyObject"}, inputs["emptyObject"].TypeSpec)
}

// TestProviderFunctionOutputIsTyped shows that an output calling a
// provider-defined function (`provider::<name>::<fn>(...)`) is typed from the
// function's declared return type, using the multi-argument-inputs
// projection the resolver supplies for the referenced provider.
func TestProviderFunctionOutputIsTyped(t *testing.T) {
	t.Parallel()

	const src = `
variable "x" {
  type = string
}

output "arn" {
  value = provider::simple::parse_arn(var.x)
}
`
	config, diags := parser.NewParser().ParseSource("main.tf", []byte(src))
	require.False(t, diags.HasErrors(), diags.Error())

	resolver := stubResolver{providerFunctions: map[string]map[string]packages.ProviderFunction{
		"simple": {
			"parse_arn": {Function: &pulumiSchema.Function{
				Token:               "simple:index:parseArn",
				MultiArgumentInputs: true,
				Inputs: &pulumiSchema.ObjectType{Properties: []*pulumiSchema.Property{
					{Name: "arn", Type: pulumiSchema.StringType},
				}},
				ReturnType: pulumiSchema.StringType,
			}},
		},
	}}

	moduleSchema, err := GenerateModuleSchema(
		t.Context(), config, &Binder{Resources: resolver}, componentToken("pkg", "index", "pkg"), semver.MustParse("0.0.0-dev"))
	require.NoError(t, err)

	assert.Equal(t, map[string]*PropertySpec{
		"arn": {Type: TypeString},
	}, moduleSchema.OutputProperties)
}

// TestProviderFunctionObjectReturnThroughLocal shows that a provider-defined
// function's structured return type survives the trip through a local: a
// field projected from the result types as that field, and the whole result
// types as an object whose property names are the TF-side snake_case names
// and whose required list reflects the return schema's optionality.
func TestProviderFunctionObjectReturnThroughLocal(t *testing.T) {
	t.Parallel()

	const src = `
variable "x" {
  type = string
}

locals {
  parsed = provider::simple::parse_arn(var.x)
}

output "service" {
  value = local.parsed.service
}

output "parsed" {
  value = local.parsed
}
`
	config, diags := parser.NewParser().ParseSource("main.tf", []byte(src))
	require.False(t, diags.HasErrors(), diags.Error())

	resolver := stubResolver{providerFunctions: map[string]map[string]packages.ProviderFunction{
		"simple": {
			"parse_arn": {Function: &pulumiSchema.Function{
				Token:               "simple:index:parseArn",
				MultiArgumentInputs: true,
				Inputs: &pulumiSchema.ObjectType{Properties: []*pulumiSchema.Property{
					{Name: "arn", Type: pulumiSchema.StringType},
				}},
				ReturnType: &pulumiSchema.ObjectType{Properties: []*pulumiSchema.Property{
					{Name: "service", Type: pulumiSchema.StringType},
					{Name: "accountId", Type: &pulumiSchema.OptionalType{ElementType: pulumiSchema.StringType}},
				}},
			}},
		},
	}}

	moduleSchema, err := GenerateModuleSchema(
		t.Context(), config, &Binder{Resources: resolver}, componentToken("pkg", "index", "pkg"), semver.MustParse("0.0.0-dev"))
	require.NoError(t, err)

	assert.Equal(t, map[string]*PropertySpec{
		"service": {Type: TypeString},
		"parsed": {
			Type: TypeObject,
			Properties: map[string]*PropertySpec{
				"service":    {Type: TypeString},
				"account_id": {Type: TypeString},
			},
			Required: []string{"service"},
		},
	}, moduleSchema.OutputProperties)
	assert.Equal(t, []string{"parsed", "service"}, moduleSchema.RequiredOutputs)
}

// TestProviderFunctionUnknownProviderIsError shows that a call to a provider
// the resolver doesn't recognize raises an error naming that provider, rather
// than degrading to an untyped output.
func TestProviderFunctionUnknownProviderIsError(t *testing.T) {
	t.Parallel()

	const src = `
variable "x" {
  type = string
}

output "arn" {
  value = provider::unknown::parse_arn(var.x)
}
`
	config, diags := parser.NewParser().ParseSource("main.tf", []byte(src))
	require.False(t, diags.HasErrors(), diags.Error())

	resolver := stubResolver{providerFunctions: map[string]map[string]packages.ProviderFunction{}}

	_, err := GenerateModuleSchema(
		t.Context(), config, &Binder{Resources: resolver}, componentToken("pkg", "index", "pkg"), semver.MustParse("0.0.0-dev"))
	require.EqualError(t, err, `resolving provider functions for "unknown": unknown provider "unknown"`)
}

// errLoader is a schema.Loader that fails if asked to load any package. The
// component schemas under test reference no external packages, so binding never
// invokes it; supplying it keeps BindSpec from constructing a real plugin loader.
type errLoader struct{}

func (errLoader) LoadPackage(pkg string, version *semver.Version) (*pulumiSchema.Package, error) {
	return nil, assert.AnError
}

func (errLoader) LoadPackageV2(
	ctx context.Context, descriptor *pulumiSchema.PackageDescriptor,
) (*pulumiSchema.Package, error) {
	return nil, assert.AnError
}

func TestPackageSchema(t *testing.T) {
	t.Parallel()

	makeComponent := func(module string) *ModuleSchema {
		return &ModuleSchema{
			PackageName:   "pkg",
			Version:       "1.0.0",
			ComponentName: "Module",
			Module:        module,
			Description:   module + " description",
			InputProperties: map[string]*PropertySpec{
				"nested": {
					Type:       TypeObject,
					Properties: map[string]*PropertySpec{"field": {Type: TypeString}},
				},
			},
		}
	}

	t.Run("merges components and types", func(t *testing.T) {
		t.Parallel()
		root := makeComponent("index")
		spec, err := PackageSchema(root, []*ModuleSchema{root, makeComponent("alpha")})
		require.NoError(t, err)
		require.Equal(t, "pkg", spec.Name)
		require.Equal(t, "index description", spec.Description)
		require.Equal(t, []string{"pkg:alpha:Module", "pkg:index:Module"}, slices.Sorted(maps.Keys(spec.Resources)))
		require.Equal(t, []string{"pkg:alpha:Nested", "pkg:index:Nested"}, slices.Sorted(maps.Keys(spec.Types)))
		require.Empty(t, spec.Language)
	})

	t.Run("no root leaves the description empty", func(t *testing.T) {
		t.Parallel()
		spec, err := PackageSchema(nil, []*ModuleSchema{makeComponent("alpha")})
		require.NoError(t, err)
		require.Equal(t, "", spec.Description)
	})

	t.Run("duplicate component token", func(t *testing.T) {
		t.Parallel()
		_, err := PackageSchema(nil, []*ModuleSchema{makeComponent("alpha"), makeComponent("alpha")})
		require.EqualError(t, err, `component token "pkg:alpha:Module" is defined twice`)
	})

	t.Run("duplicate type token across module segment", func(t *testing.T) {
		t.Parallel()
		other := makeComponent("alpha")
		other.ComponentName = "Other"
		_, err := PackageSchema(nil, []*ModuleSchema{makeComponent("alpha"), other})
		require.EqualError(t, err, `type token "pkg:alpha:Nested" is defined twice`)
	})

	t.Run("empty", func(t *testing.T) {
		t.Parallel()
		_, err := PackageSchema(nil, nil)
		require.EqualError(t, err, "no components to combine")
	})

	t.Run("go overrides for invalid package names", func(t *testing.T) {
		t.Parallel()
		spec, err := PackageSchema(nil, []*ModuleSchema{
			makeComponent("index"), makeComponent("user-data"), makeComponent("2fa"), makeComponent("type"),
		})
		require.NoError(t, err)
		var info struct {
			ModuleToPackage      map[string]string `json:"moduleToPackage"`
			RespectSchemaVersion bool              `json:"respectSchemaVersion"`
		}
		require.NoError(t, json.Unmarshal(spec.Language["go"], &info))
		require.Equal(t, map[string]string{
			"user-data": "userdata",
			"2fa":       "_2fa",
			"type":      "_type",
		}, info.ModuleToPackage)
		require.True(t, info.RespectSchemaVersion)
	})

	t.Run("colliding go package names", func(t *testing.T) {
		t.Parallel()
		_, err := PackageSchema(nil, []*ModuleSchema{makeComponent("user-data"), makeComponent("userdata")})
		require.EqualError(t, err, `modules "user-data" and "userdata" both map to Go package "userdata"`)
	})
}

// TestConditionalResourceDataSourceOutputIsTyped covers the "create or adopt"
// idiom: a conditional selects between a counted resource and its matching data
// source, whose reference types differ (here in the `timeouts` operations and in
// a resource-only attribute). At runtime only one branch ever holds a value, so
// an attribute read through the local must type from the selected object.
func TestConditionalResourceDataSourceOutputIsTyped(t *testing.T) {
	t.Parallel()

	const src = `
variable "create" {
  type = bool
}

resource "pfx_res" "this" {
  count = var.create ? 1 : 0
}

data "pfx_res" "this" {
  count = var.create ? 0 : 1
}

locals {
  selected = var.create ? pfx_res.this[0] : data.pfx_res.this[0]
}

output "selected_id" {
  value = local.selected.id
}

output "selected_attr" {
  value = local.selected.attr
}

output "selected" {
  value = local.selected
}

output "wrapped" {
  value = { inner = local.selected }
}
`
	config, diags := parser.NewParser().ParseSource("main.tf", []byte(src))
	require.False(t, diags.HasErrors(), diags.Error())

	resolver := mappingResolver{
		resources: map[string]*pulumiSchema.Resource{
			"pfx_res": {Properties: []*pulumiSchema.Property{{Name: "attr", Type: pulumiSchema.StringType}}},
		},
		mappings: map[string]*bridge.BodyMapping{
			"pfx_res": {Timeouts: []string{"create", "read", "update", "delete"}},
		},
		dataSources: map[string]*pulumiSchema.Function{
			"pfx_res": {ReturnType: &pulumiSchema.ObjectType{Properties: []*pulumiSchema.Property{
				{Name: "id", Type: pulumiSchema.StringType},
			}}},
		},
		dataSourceMappings: map[string]*bridge.BodyMapping{
			"pfx_res": {Timeouts: []string{"read"}},
		},
	}
	moduleSchema, err := GenerateModuleSchema(
		t.Context(), config, &Binder{Resources: resolver}, componentToken("pkg", "index", "pkg"), semver.MustParse("0.0.0-dev"))
	require.NoError(t, err)

	// The synthetic `id` and `timeouts` of a resource reference are optional, so
	// only the declared required property is required; a data source's `id` is a
	// declared property.
	resource := &PropertySpec{Type: TypeObject, Properties: map[string]*PropertySpec{
		"attr": {Type: TypeString},
		"id":   {Type: TypeString},
		"timeouts": {Type: TypeObject, Properties: map[string]*PropertySpec{
			"create": {Type: TypeString},
			"delete": {Type: TypeString},
			"read":   {Type: TypeString},
			"update": {Type: TypeString},
		}},
	}, Required: []string{"attr"}}
	dataSource := &PropertySpec{Type: TypeObject, Properties: map[string]*PropertySpec{
		"id": {Type: TypeString},
		"timeouts": {Type: TypeObject, Properties: map[string]*PropertySpec{
			"read": {Type: TypeString},
		}},
	}, Required: []string{"id"}}
	selected := &PropertySpec{OneOf: []*PropertySpec{resource, dataSource}}
	assert.Equal(t, map[string]*PropertySpec{
		"selected_id":   {Type: TypeString},
		"selected_attr": {Type: TypeString},
		"selected":      selected,
		"wrapped": {Type: TypeObject, Properties: map[string]*PropertySpec{
			"inner": selected,
		}, Required: []string{"inner"}},
	}, moduleSchema.OutputProperties)
	assert.Equal(t, []string{"selected", "selected_attr", "wrapped"}, moduleSchema.RequiredOutputs)

	spec := moduleSchema.ToPulumiPackageSchema()
	assert.Equal(t, pulumiSchema.TypeSpec{OneOf: []pulumiSchema.TypeSpec{
		{Ref: "#/types/pkg:index:Selected0"},
		{Ref: "#/types/pkg:index:Selected1"},
	}}, spec.Resources["pkg:index:pkg"].Properties["selected"].TypeSpec)
	assert.Equal(t, []string{"attr", "id", "timeouts"}, slices.Sorted(maps.Keys(spec.Types["pkg:index:Selected0"].Properties)))
	assert.Equal(t, []string{"id", "timeouts"}, slices.Sorted(maps.Keys(spec.Types["pkg:index:Selected1"].Properties)))
	assert.Equal(t, pulumiSchema.TypeSpec{OneOf: []pulumiSchema.TypeSpec{
		{Ref: "#/types/pkg:index:WrappedInner0"},
		{Ref: "#/types/pkg:index:WrappedInner1"},
	}}, spec.Types["pkg:index:Wrapped"].Properties["inner"].TypeSpec)
}

// TestConditionalUnifiedObjectsOutputIsMap shows that a conditional over objects
// whose attribute types unify keeps HCL's own result: the branches become a map,
// as they do at runtime, rather than a union.
func TestConditionalUnifiedObjectsOutputIsMap(t *testing.T) {
	t.Parallel()

	const src = `
variable "flag" {
  type = bool
}

output "disjoint" {
  value = var.flag ? { a = "x" } : { b = 1 }
}
`
	config, diags := parser.NewParser().ParseSource("main.tf", []byte(src))
	require.False(t, diags.HasErrors(), diags.Error())

	moduleSchema, err := GenerateModuleSchema(
		t.Context(), config, nil, componentToken("pkg", "index", "pkg"), semver.MustParse("0.0.0-dev"))
	require.NoError(t, err)
	assert.Equal(t, map[string]*PropertySpec{
		"disjoint": {Type: TypeObject, AdditionalProperties: &PropertySpec{Type: TypeString}},
	}, moduleSchema.OutputProperties)
}

// TestConditionalUnionOutsideTraversal shows the two ways a union leaves the
// walker's precise handling: an attribute no member has is HCL's own error, and
// a union that flows through a function call degrades to any.
func TestConditionalUnionOutsideTraversal(t *testing.T) {
	t.Parallel()

	// A list attribute against a string attribute cannot unify, so the
	// conditional is a union rather than HCL's map.
	resolver := mappingResolver{
		resources: map[string]*pulumiSchema.Resource{
			"pfx_res": {Properties: []*pulumiSchema.Property{
				{Name: "attr", Type: &pulumiSchema.ArrayType{ElementType: pulumiSchema.StringType}},
			}},
		},
		dataSources: map[string]*pulumiSchema.Function{
			"pfx_res": {ReturnType: &pulumiSchema.ObjectType{Properties: []*pulumiSchema.Property{
				{Name: "id", Type: pulumiSchema.StringType},
			}}},
		},
	}
	const prelude = `
variable "create" {
  type = bool
}

resource "pfx_res" "this" {
  count = var.create ? 1 : 0
}

data "pfx_res" "this" {
  count = var.create ? 0 : 1
}

locals {
  selected = var.create ? pfx_res.this[0] : data.pfx_res.this[0]
}
`
	generate := func(t *testing.T, src string) (*ModuleSchema, error) {
		config, diags := parser.NewParser().ParseSource("main.tf", []byte(prelude+src))
		require.False(t, diags.HasErrors(), diags.Error())
		return GenerateModuleSchema(
			t.Context(), config, &Binder{Resources: resolver}, componentToken("pkg", "index", "pkg"), semver.MustParse("0.0.0-dev"))
	}

	t.Run("attribute of no member", func(t *testing.T) {
		t.Parallel()
		_, err := generate(t, `
output "missing" {
  value = local.selected.missing
}
`)
		require.EqualError(t, err, `typing output "missing": main.tf:19,25-33: Unsupported attribute; This object does not have an attribute named "missing".`)
	})

	t.Run("through a function call", func(t *testing.T) {
		t.Parallel()
		moduleSchema, err := generate(t, `
output "merged" {
  value = merge(local.selected, {})
}
`)
		require.NoError(t, err)
		assert.Equal(t, map[string]*PropertySpec{"merged": {Type: TypeAny}}, moduleSchema.OutputProperties)
	})
}
