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

package run_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/blang/semver"
	"github.com/pulumi/pulumi-hcl/pkg/hcl/ast"
	"github.com/pulumi/pulumi-hcl/pkg/hcl/modules"
	"github.com/pulumi/pulumi-hcl/pkg/hcl/parser"
	"github.com/pulumi/pulumi-hcl/pkg/hcl/run"
	"github.com/pulumi/pulumi-hcl/pkg/hcl/transform"
	"github.com/pulumi/pulumi-hcl/tests/testutil"
	"github.com/pulumi/pulumi-hcl/tests/testutil/schemaloader"
	"github.com/pulumi/pulumi/pkg/v3/codegen/schema"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource/urn"
	"github.com/pulumi/pulumi/sdk/v3/go/property"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// errLoader is a schema.Loader that fails if asked to load any package. The
// specs under test reference no external packages, so binding never invokes
// it; supplying it keeps BindSpec from constructing a real plugin loader.
type errLoader struct{}

func (errLoader) LoadPackage(pkg string, version *semver.Version) (*schema.Package, error) {
	return nil, assert.AnError
}

func (errLoader) LoadPackageV2(
	ctx context.Context, descriptor *schema.PackageDescriptor,
) (*schema.Package, error) {
	return nil, assert.AnError
}

// testModuleLoader builds a live module loader for the engine. These tests do
// not exercise child modules, so it is never invoked; the engine just requires a
// non-nil loader.
func testModuleLoader(t *testing.T) *modules.Loader {
	return modules.NewLoader(func(packageSource, versionConstraint, callerDir string) (string, string, error) {
		t.Fail()
		return "", "", fmt.Errorf("attempted to load (%q, %q, %q)",
			packageSource, versionConstraint, callerDir)
	})
}

// testLiveModuleLoader is for the tests that exercise child modules. Their
// sources are all local paths, so the live resolver resolves them from the
// filesystem with no network access.
func testLiveModuleLoader(t *testing.T) *modules.Loader {
	return modules.NewLoader(modules.LiveResolver(t.Context()))
}

func newTestEngine(t *testing.T, config *ast.Config, opts *run.EngineOptions) *run.Engine {
	t.Helper()
	engine, err := run.NewEngine(t.Context(), config, opts)
	require.NoError(t, err)
	return engine
}

func TestEngine_BasicResource(t *testing.T) {
	t.Parallel()

	src := []byte(`
variable "name" {
  type    = string
  default = "test"
}

resource "aws_instance" "web" {
  ami = "ami-12345"
  instance_type = var.name
}
`)

	p := parser.NewParser()
	config, diags := p.ParseSource("test.hcl", src)
	if diags.HasErrors() {
		t.Fatalf("parse error: %s", diags.Error())
	}

	mock := &testutil.MockResourceMonitor{}
	engine := newTestEngine(t, config, &run.EngineOptions{
		ModuleLoader:    testModuleLoader(t),
		ProjectName:     "test-project",
		StackName:       "dev",
		ResourceMonitor: mock,
		WorkDir:         t.TempDir(),
		RootDir:         t.TempDir(),
		SchemaLoader: schemaloader.New(t, schema.PackageSpec{
			Name: "aws",
			Resources: map[string]schema.ResourceSpec{
				"aws:index:Instance": {
					InputProperties: map[string]schema.PropertySpec{
						"ami":          {TypeSpec: schema.TypeSpec{Type: "string"}},
						"instanceType": {TypeSpec: schema.TypeSpec{Type: "string"}},
					},
					ObjectTypeSpec: schema.ObjectTypeSpec{
						Properties: map[string]schema.PropertySpec{
							"ami":          {TypeSpec: schema.TypeSpec{Type: "string"}},
							"instanceType": {TypeSpec: schema.TypeSpec{Type: "string"}},
						},
					},
				},
			},
		}),
	})

	err := engine.Run(t.Context())
	if err != nil {
		t.Fatalf("run error: %v", err)
	}

	// Should have registered the stack + one resource
	if len(mock.RegisteredResources) != 2 {
		t.Fatalf("expected 2 registered resources (stack + resource), got %d", len(mock.RegisteredResources))
	}

	// First resource should be the stack
	if mock.RegisteredResources[0].Type != "pulumi:pulumi:Stack" {
		t.Errorf("expected first resource to be stack, got %s", mock.RegisteredResources[0].Type)
	}

	req := mock.RegisteredResources[1]
	if req.Name != "web" {
		t.Errorf("expected resource name 'web', got %s", req.Name)
	}
	if req.Inputs.Get("ami").AsString() != "ami-12345" {
		t.Errorf("expected ami 'ami-12345', got %v", req.Inputs.Get("ami"))
	}
	if req.Inputs.Get("instanceType").AsString() != "test" {
		t.Errorf("expected instanceType 'test', got %v", req.Inputs.Get("instanceType"))
	}
}

func TestEngine_StackReferenceUsesRead(t *testing.T) {
	t.Parallel()

	src := []byte(`
resource "pulumi_stack_reference" "ref" {
  name = "org/project/stack"
}
`)

	p := parser.NewParser()
	config, diags := p.ParseSource("test.hcl", src)
	require.Empty(t, diags)

	// "pulumi" is a reserved package name, so it must be bound with
	// AllowPulumiPackage rather than through schemaloader.New.
	pulumiPkg, diag, err := schema.BindSpec(schema.PackageSpec{
		Name: "pulumi",
		Resources: map[string]schema.ResourceSpec{
			"pulumi:pulumi:StackReference": {
				InputProperties: map[string]schema.PropertySpec{
					"name": {TypeSpec: schema.TypeSpec{Type: "string"}},
				},
				ObjectTypeSpec: schema.ObjectTypeSpec{
					Properties: map[string]schema.PropertySpec{
						"name": {TypeSpec: schema.TypeSpec{Type: "string"}},
					},
				},
			},
		},
	}, errLoader{}, schema.ValidationOptions{AllowPulumiPackage: true})
	require.NoError(t, err)
	require.Empty(t, diag)

	mock := &testutil.MockResourceMonitor{}
	engine := newTestEngine(t, config, &run.EngineOptions{
		ProjectName:     "test-project",
		StackName:       "dev",
		ResourceMonitor: mock,
		WorkDir:         t.TempDir(),
		RootDir:         t.TempDir(),
		ModuleLoader:    modules.NewLoader(modules.LiveResolver(t.Context())),
		SchemaLoader:    schemaloader.Mock{"pulumi": pulumiPkg},
	})

	require.NoError(t, engine.Run(t.Context()))

	// The stack reference must be resolved with a Read, not a Create: only the
	// stack (a Create) should appear among registrations.
	require.Len(t, mock.RegisteredResources, 1)
	assert.Equal(t, "pulumi:pulumi:Stack", mock.RegisteredResources[0].Type)

	require.Len(t, mock.ReadResources, 1)
	read := mock.ReadResources[0]
	assert.Equal(t, "pulumi:pulumi:StackReference", read.Type)
	assert.Equal(t, "ref", read.Name)
	assert.Equal(t, "org/project/stack", read.ID)
	assert.Equal(t, "org/project/stack", read.Inputs.Get("name").AsString())
}

func TestEngine_PulumiResourceNameRejectsWrappedResource(t *testing.T) {
	t.Parallel()

	src := []byte(`
resource "aws_instance" "web" {
  ami = "ami-12345"
}

output "name" {
  value = pulumiresourcename({ key = aws_instance.web })
}
`)

	p := parser.NewParser()
	config, diags := p.ParseSource("test.hcl", src)
	require.Empty(t, diags)

	mock := &testutil.MockResourceMonitor{}
	engine := newTestEngine(t, config, &run.EngineOptions{
		ModuleLoader:    testModuleLoader(t),
		ProjectName:     "test-project",
		StackName:       "dev",
		ResourceMonitor: mock,
		WorkDir:         t.TempDir(),
		RootDir:         t.TempDir(),
		SchemaLoader: schemaloader.New(t, schema.PackageSpec{
			Name: "aws",
			Resources: map[string]schema.ResourceSpec{
				"aws:index:Instance": {
					InputProperties: map[string]schema.PropertySpec{
						"ami": {TypeSpec: schema.TypeSpec{Type: "string"}},
					},
					ObjectTypeSpec: schema.ObjectTypeSpec{
						Properties: map[string]schema.PropertySpec{
							"ami": {TypeSpec: schema.TypeSpec{Type: "string"}},
						},
					},
				},
			},
		}),
	})

	err := engine.Run(t.Context())
	require.Error(t, err, "pulumiResourceName on an object wrapping a resource should be rejected")
	require.ErrorContains(t, err, "must be a resource reference")
}

func TestEngine_PulumiResourceNameCountAndForEach(t *testing.T) {
	t.Parallel()

	// pulumiResourceName must resolve on an individual instance of a count or
	// for_each resource, addressed by index or key.
	src := []byte(`
resource "aws_instance" "counted" {
  count = 2
  ami   = "ami-${count.index}"
}

resource "aws_instance" "mapped" {
  for_each = toset(["a", "b"])
  ami      = "ami-${each.key}"
}

output "count_name" {
  value = pulumiresourcename(aws_instance.counted[1])
}

output "each_name" {
  value = pulumiresourcename(aws_instance.mapped["a"])
}
`)

	p := parser.NewParser()
	config, diags := p.ParseSource("test.hcl", src)
	require.Empty(t, diags)

	mock := &testutil.MockResourceMonitor{}
	engine := newTestEngine(t, config, &run.EngineOptions{
		ModuleLoader:    testModuleLoader(t),
		ProjectName:     "test-project",
		StackName:       "dev",
		ResourceMonitor: mock,
		WorkDir:         t.TempDir(),
		RootDir:         t.TempDir(),
		SchemaLoader: schemaloader.New(t, schema.PackageSpec{
			Name: "aws",
			Resources: map[string]schema.ResourceSpec{
				"aws:index:Instance": {
					InputProperties: map[string]schema.PropertySpec{
						"ami": {TypeSpec: schema.TypeSpec{Type: "string"}},
					},
					ObjectTypeSpec: schema.ObjectTypeSpec{
						Properties: map[string]schema.PropertySpec{
							"ami": {TypeSpec: schema.TypeSpec{Type: "string"}},
						},
					},
				},
			},
		}),
	})

	require.NoError(t, engine.Run(t.Context()))

	countName, ok := mock.StackOutputs.GetOk("count_name")
	require.True(t, ok, "expected count_name output")
	require.Equal(t, "counted[1]", countName.AsString())

	eachName, ok := mock.StackOutputs.GetOk("each_name")
	require.True(t, ok, "expected each_name output")
	require.Equal(t, `mapped["a"]`, eachName.AsString())
}

// A `pulumi { name = ... }` option overrides the derived instance name; the
// expression is evaluated per instance with count.index/each.key in scope.
func TestEngine_PulumiNameOverride(t *testing.T) {
	t.Parallel()

	src := []byte(`
resource "aws_instance" "counted" {
  count = 2
  ami   = "ami-${count.index}"

  pulumi {
    name = "counted-${count.index}"
  }
}

resource "aws_instance" "mapped" {
  for_each = toset(["a", "b"])
  ami      = "ami-${each.key}"

  pulumi {
    name = "mapped-${each.key}"
  }
}

resource "aws_instance" "single" {
  ami = "ami-single"

  pulumi {
    name = "renamed"
  }
}
`)

	p := parser.NewParser()
	config, diags := p.ParseSource("test.hcl", src)
	require.Empty(t, diags)

	mock := &testutil.MockResourceMonitor{}
	engine := newTestEngine(t, config, &run.EngineOptions{
		ModuleLoader:    testModuleLoader(t),
		ProjectName:     "test-project",
		StackName:       "dev",
		ResourceMonitor: mock,
		WorkDir:         t.TempDir(),
		RootDir:         t.TempDir(),
		SchemaLoader: schemaloader.New(t, schema.PackageSpec{
			Name: "aws",
			Resources: map[string]schema.ResourceSpec{
				"aws:index:Instance": {
					InputProperties: map[string]schema.PropertySpec{
						"ami": {TypeSpec: schema.TypeSpec{Type: "string"}},
					},
					ObjectTypeSpec: schema.ObjectTypeSpec{
						Properties: map[string]schema.PropertySpec{
							"ami": {TypeSpec: schema.TypeSpec{Type: "string"}},
						},
					},
				},
			},
		}),
	})

	require.NoError(t, engine.Run(t.Context()))

	names := make(map[string]string)
	for _, req := range mock.RegisteredResources {
		if req.Type == "aws:index:Instance" {
			names[req.Name] = req.Inputs.Get("ami").AsString()
		}
	}
	assert.Equal(t, map[string]string{
		"counted-0": "ami-0",
		"counted-1": "ami-1",
		"mapped-a":  "ami-a",
		"mapped-b":  "ami-b",
		"renamed":   "ami-single",
	}, names)
}

// A module's `pulumi { name = ... }` pins the component instance's name, the
// pinned name prefixes derived child names and is exposed inside the module
// as pulumi.module.name, and a resource override is absolute (no module
// prefix). A null override means no override.
func TestEngine_PulumiNameOverrideModules(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	moduleDir := tmpDir + "/modules/child"
	require.NoError(t, os.MkdirAll(moduleDir, 0o755))

	moduleMain := `
resource "aws_instance" "derived" {
  ami = "ami-derived"
}

resource "aws_instance" "pinned" {
  ami = "ami-pinned"

  pulumi {
    name = "${pulumi.module.name}-pinned"
  }
}

resource "aws_instance" "null_override" {
  ami = "ami-null"

  pulumi {
    name = null
  }
}
`
	require.NoError(t, os.WriteFile(moduleDir+"/main.tf", []byte(moduleMain), 0o644))

	rootMain := `
module "plain" {
  source = "./modules/child"
}

module "named" {
  source = "./modules/child"

  pulumi {
    name = "renamed"
  }
}

module "keyed" {
  source   = "./modules/child"
  for_each = toset(["k"])

  pulumi {
    name = "keyed-${each.key}"
  }
}
`
	require.NoError(t, os.WriteFile(tmpDir+"/main.tf", []byte(rootMain), 0o644))

	p := parser.NewParser()
	config, diags := p.ParseDirectory(tmpDir)
	require.False(t, diags.HasErrors(), "parse error: %s", diags.Error())

	mock := &testutil.MockResourceMonitor{}
	engine := newTestEngine(t, config, &run.EngineOptions{
		ModuleLoader:    testLiveModuleLoader(t),
		ProjectName:     "test-project",
		StackName:       "dev",
		ResourceMonitor: mock,
		WorkDir:         tmpDir,
		RootDir:         tmpDir,
		SchemaLoader: schemaloader.New(t, schema.PackageSpec{
			Name: "aws",
			Resources: map[string]schema.ResourceSpec{
				"aws:index:Instance": {
					InputProperties: map[string]schema.PropertySpec{
						"ami": {TypeSpec: schema.TypeSpec{Type: "string"}},
					},
					ObjectTypeSpec: schema.ObjectTypeSpec{
						Properties: map[string]schema.PropertySpec{
							"ami": {TypeSpec: schema.TypeSpec{Type: "string"}},
						},
					},
				},
			},
		}),
	})

	require.NoError(t, engine.Run(t.Context()))

	componentNames := []string{}
	instanceNames := []string{}
	for _, req := range mock.RegisteredResources {
		switch {
		case strings.HasPrefix(req.Type, "components:index:"):
			componentNames = append(componentNames, req.Name)
		case req.Type == "aws:index:Instance":
			instanceNames = append(instanceNames, req.Name)
		}
	}
	assert.ElementsMatch(t, []string{"plain", "renamed", "keyed-k"}, componentNames)
	assert.ElementsMatch(t, []string{
		"plain.derived", "plain-pinned", "plain.null_override",
		"renamed.derived", "renamed-pinned", "renamed.null_override",
		"keyed-k.derived", "keyed-k-pinned", "keyed-k.null_override",
	}, instanceNames)
}

// Version constraints are enforced per module: the root's
// required_version_range and a child module's language block both reach
// CheckPulumiVersion, and a failing child constraint names the module.
func TestEngine_ModulePulumiVersionCheck(t *testing.T) {
	t.Parallel()

	setup := func(t *testing.T) (*ast.Config, string) {
		tmpDir := t.TempDir()
		moduleDir := tmpDir + "/modules/child"
		require.NoError(t, os.MkdirAll(moduleDir, 0o755))

		moduleMain := `
language {
  compatible_with {
    opentofu = ">= 1.12"
    pulumi   = ">= 999.0.0"
  }
}
`
		require.NoError(t, os.WriteFile(moduleDir+"/main.tf", []byte(moduleMain), 0o644))

		rootMain := `
terraform {
  required_version_range = ">= 3.0.0"
}

module "child" {
  source = "./modules/child"
}
`
		require.NoError(t, os.WriteFile(tmpDir+"/main.tf", []byte(rootMain), 0o644))

		config, diags := parser.NewParser().ParseDirectory(tmpDir)
		require.False(t, diags.HasErrors(), "parse error: %s", diags.Error())
		return config, tmpDir
	}

	newEngine := func(t *testing.T, config *ast.Config, tmpDir string, mock *testutil.MockResourceMonitor) *run.Engine {
		return newTestEngine(t, config, &run.EngineOptions{
			ModuleLoader:    testLiveModuleLoader(t),
			ProjectName:     "test-project",
			StackName:       "dev",
			ResourceMonitor: mock,
			WorkDir:         tmpDir,
			RootDir:         tmpDir,
			SchemaLoader:    schemaloader.New(t, schema.PackageSpec{Name: "aws"}),
		})
	}

	t.Run("both_constraints_checked", func(t *testing.T) {
		t.Parallel()
		config, tmpDir := setup(t)

		var mu sync.Mutex
		var ranges []string
		mock := &testutil.MockResourceMonitor{
			CheckPulumiVersionHandler: func(ctx context.Context, versionRange string) error {
				mu.Lock()
				defer mu.Unlock()
				ranges = append(ranges, versionRange)
				return nil
			},
		}

		require.NoError(t, newEngine(t, config, tmpDir, mock).Run(t.Context()))
		assert.ElementsMatch(t, []string{">= 3.0.0", ">= 999.0.0"}, ranges)
	})

	t.Run("failing_child_constraint_names_module", func(t *testing.T) {
		t.Parallel()
		config, tmpDir := setup(t)

		mock := &testutil.MockResourceMonitor{
			CheckPulumiVersionHandler: func(ctx context.Context, versionRange string) error {
				if versionRange == ">= 999.0.0" {
					return fmt.Errorf("Pulumi CLI version does not satisfy %q", versionRange)
				}
				return nil
			},
		}

		err := newEngine(t, config, tmpDir, mock).Run(t.Context())
		require.Error(t, err)
		assert.ErrorContains(t, err, `module ./modules/child: Pulumi CLI version does not satisfy ">= 999.0.0"`)
	})
}

// Distinct addresses must derive distinct names even when labels and keys
// contain dashes: instance keys are bracket-quoted and module prefixes join
// with ".", neither of which can appear in an HCL label.
func TestEngine_ModuleResourceNamesDoNotCollide(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	moduleDir := tmpDir + "/modules/child"
	require.NoError(t, os.MkdirAll(moduleDir, 0o755))

	moduleMain := `
resource "aws_instance" "r" {
  ami = "ami-r"
}

resource "aws_instance" "b-r" {
  ami = "ami-b-r"
}
`
	require.NoError(t, os.WriteFile(moduleDir+"/main.tf", []byte(moduleMain), 0o644))

	rootMain := `
module "a" {
  source = "./modules/child"
}

module "a-b" {
  source = "./modules/child"
}
`
	require.NoError(t, os.WriteFile(tmpDir+"/main.tf", []byte(rootMain), 0o644))

	p := parser.NewParser()
	config, diags := p.ParseDirectory(tmpDir)
	require.False(t, diags.HasErrors(), "parse error: %s", diags.Error())

	mock := &testutil.MockResourceMonitor{}
	engine := newTestEngine(t, config, &run.EngineOptions{
		ModuleLoader:    testLiveModuleLoader(t),
		ProjectName:     "test-project",
		StackName:       "dev",
		ResourceMonitor: mock,
		WorkDir:         tmpDir,
		RootDir:         tmpDir,
		SchemaLoader: schemaloader.New(t, schema.PackageSpec{
			Name: "aws",
			Resources: map[string]schema.ResourceSpec{
				"aws:index:Instance": {
					InputProperties: map[string]schema.PropertySpec{
						"ami": {TypeSpec: schema.TypeSpec{Type: "string"}},
					},
					ObjectTypeSpec: schema.ObjectTypeSpec{
						Properties: map[string]schema.PropertySpec{
							"ami": {TypeSpec: schema.TypeSpec{Type: "string"}},
						},
					},
				},
			},
		}),
	})

	require.NoError(t, engine.Run(t.Context()))

	names := []string{}
	for _, req := range mock.RegisteredResources {
		if req.Type == "aws:index:Instance" {
			names = append(names, req.Name)
		}
	}
	assert.ElementsMatch(t, []string{"a.r", "a.b-r", "a-b.r", "a-b.b-r"}, names)
}

func TestEngine_PulumiResourceNamePreviewUnknown(t *testing.T) {
	t.Parallel()

	// During preview a resource's computed attributes (id) are unknown, so the
	// resource value carries unknowns. resourceValue hashes that value and
	// pulumiResourceName re-hashes it to confirm identity — both must tolerate
	// unknown values rather than panic, and the name (from the known URN) still
	// resolves. The result feeds another resource's input so it is observable in
	// preview via the registration.
	src := []byte(`
resource "aws_instance" "web" {
  ami = "ami-12345"
}

resource "aws_instance" "named" {
  ami = pulumiresourcename(aws_instance.web)
}
`)

	p := parser.NewParser()
	config, diags := p.ParseSource("test.hcl", src)
	require.Empty(t, diags)

	mock := &testutil.MockResourceMonitor{
		DryRun: true,
		RegisterResourceHandler: func(ctx context.Context, req run.RegisterResourceRequest) (*run.RegisterResourceResponse, error) {
			return &run.RegisterResourceResponse{
				URN:     urn.URN("urn:pulumi:test::project::" + req.Type + "::" + req.Name),
				ID:      "", // unknown id during preview
				Outputs: req.Inputs,
			}, nil
		},
	}
	engine := newTestEngine(t, config, &run.EngineOptions{
		ModuleLoader:    testModuleLoader(t),
		ProjectName:     "test-project",
		StackName:       "dev",
		ResourceMonitor: mock,
		WorkDir:         t.TempDir(),
		RootDir:         t.TempDir(),
		DryRun:          true,
		SchemaLoader: schemaloader.New(t, schema.PackageSpec{
			Name: "aws",
			Resources: map[string]schema.ResourceSpec{
				"aws:index:Instance": {
					InputProperties: map[string]schema.PropertySpec{
						"ami": {TypeSpec: schema.TypeSpec{Type: "string"}},
					},
					ObjectTypeSpec: schema.ObjectTypeSpec{
						Properties: map[string]schema.PropertySpec{
							"ami": {TypeSpec: schema.TypeSpec{Type: "string"}},
						},
					},
				},
			},
		}),
	})

	require.NoError(t, engine.Run(t.Context()))

	var named *run.RegisterResourceRequest
	for i := range mock.RegisteredResources {
		if mock.RegisteredResources[i].Name == "named" {
			named = &mock.RegisteredResources[i]
		}
	}
	require.NotNil(t, named, "named resource should register")
	require.Equal(t, "web", named.Inputs.Get("ami").AsString())
}

func TestEngine_LocalsAndVariables(t *testing.T) {
	t.Parallel()

	src := []byte(`
variable "env" {
  type    = string
  default = "dev"
}

locals {
  prefix = "myapp-${var.env}"
}

resource "aws_s3_bucket" "mybucket" {
  bucket = "${local.prefix}-bucket"
}
`)

	p := parser.NewParser()
	config, diags := p.ParseSource("test.hcl", src)
	if diags.HasErrors() {
		t.Fatalf("parse error: %s", diags.Error())
	}

	mock := &testutil.MockResourceMonitor{}
	engine := newTestEngine(t, config, &run.EngineOptions{
		ModuleLoader:    testModuleLoader(t),
		ProjectName:     "test-project",
		StackName:       "dev",
		ResourceMonitor: mock,
		WorkDir:         t.TempDir(),
		RootDir:         t.TempDir(),
		SchemaLoader: schemaloader.New(t, schema.PackageSpec{
			Name: "aws",
			Resources: map[string]schema.ResourceSpec{
				"aws:s3:Bucket": {
					InputProperties: map[string]schema.PropertySpec{
						"bucket": {TypeSpec: schema.TypeSpec{Type: "string"}},
					},
					ObjectTypeSpec: schema.ObjectTypeSpec{
						Properties: map[string]schema.PropertySpec{
							"bucket": {TypeSpec: schema.TypeSpec{Type: "string"}},
						},
					},
				},
			},
		}),
	})

	err := engine.Run(t.Context())
	if err != nil {
		t.Fatalf("run error: %v", err)
	}

	if len(mock.RegisteredResources) != 2 {
		t.Fatalf("expected 2 registered resources (stack + resource), got %d", len(mock.RegisteredResources))
	}

	req := mock.RegisteredResources[1]
	bucketName := req.Inputs.Get("bucket").AsString()
	if bucketName != "myapp-dev-bucket" {
		t.Errorf("expected bucket 'myapp-dev-bucket', got %s", bucketName)
	}
}

func TestEngine_ResourceDependencies(t *testing.T) {
	t.Parallel()

	src := []byte(`
resource "aws_vpc" "main" {
  cidr_block = "10.0.0.0/16"
}

resource "aws_subnet" "main" {
  vpc_id     = aws_vpc.main.id
  cidr_block = "10.0.1.0/24"
}
`)

	p := parser.NewParser()
	config, diags := p.ParseSource("test.hcl", src)
	if diags.HasErrors() {
		t.Fatalf("parse error: %s", diags.Error())
	}

	mock := &testutil.MockResourceMonitor{}
	engine := newTestEngine(t, config, &run.EngineOptions{
		ModuleLoader:    testModuleLoader(t),
		ProjectName:     "test-project",
		StackName:       "dev",
		ResourceMonitor: mock,
		WorkDir:         t.TempDir(),
		RootDir:         t.TempDir(),
		SchemaLoader: schemaloader.New(t, schema.PackageSpec{
			Name: "aws",
			Resources: map[string]schema.ResourceSpec{
				"aws:index:Vpc": {
					InputProperties: map[string]schema.PropertySpec{
						"cidrBlock": {TypeSpec: schema.TypeSpec{Type: "string"}},
					},
					ObjectTypeSpec: schema.ObjectTypeSpec{
						Properties: map[string]schema.PropertySpec{
							"cidrBlock": {TypeSpec: schema.TypeSpec{Type: "string"}},
						},
					},
				},
				"aws:index:Subnet": {
					InputProperties: map[string]schema.PropertySpec{
						"vpcId":     {TypeSpec: schema.TypeSpec{Type: "string"}},
						"cidrBlock": {TypeSpec: schema.TypeSpec{Type: "string"}},
					},
					ObjectTypeSpec: schema.ObjectTypeSpec{
						Properties: map[string]schema.PropertySpec{
							"vpcId":     {TypeSpec: schema.TypeSpec{Type: "string"}},
							"cidrBlock": {TypeSpec: schema.TypeSpec{Type: "string"}},
						},
					},
				},
			},
		}),
	})

	err := engine.Run(t.Context())
	if err != nil {
		t.Fatalf("run error: %v", err)
	}

	// Should have stack + 2 resources registered in dependency order
	if len(mock.RegisteredResources) != 3 {
		t.Fatalf("expected 3 registered resources (stack + 2 resources), got %d", len(mock.RegisteredResources))
	}

	// VPC should be registered first (after stack)
	if mock.RegisteredResources[1].Name != "main" {
		t.Errorf("expected main first, got %s", mock.RegisteredResources[1].Name)
	}

	// Subnet should be registered second
	if mock.RegisteredResources[2].Name != "main" {
		t.Errorf("expected main second, got %s", mock.RegisteredResources[2].Name)
	}
}

func TestValidate(t *testing.T) {
	t.Parallel()

	t.Run("valid config", func(t *testing.T) {
		t.Parallel()

		src := []byte(`
variable "name" {
  type = string
}

resource "aws_instance" "web" {
  ami = var.name
}
`)
		p := parser.NewParser()
		config, diags := p.ParseSource("test.hcl", src)
		if diags.HasErrors() {
			t.Fatalf("parse error: %s", diags.Error())
		}

		errs := run.Validate(config)
		if len(errs) != 0 {
			t.Errorf("expected no errors, got %v", errs)
		}
	})

	t.Run("missing dependency", func(t *testing.T) {
		t.Parallel()

		src := []byte(`
resource "aws_instance" "web" {
  ami = nonexistent_resource.foo.id
}
`)
		p := parser.NewParser()
		config, diags := p.ParseSource("test.hcl", src)
		if diags.HasErrors() {
			t.Fatalf("parse error: %s", diags.Error())
		}

		errs := run.Validate(config)
		// Should have a warning about missing dependency
		if len(errs) == 0 {
			t.Error("expected validation errors for missing dependency")
		}
	})
}

func TestEngine_DependsOn(t *testing.T) {
	t.Parallel()

	src := []byte(`
resource "aws_s3_bucket" "mybucket" {
  bucket = "my-bucket"
}

resource "aws_instance" "web" {
  ami = "ami-12345"

  depends_on = [aws_s3_bucket.mybucket]
}
`)

	p := parser.NewParser()
	config, diags := p.ParseSource("test.hcl", src)
	if diags.HasErrors() {
		t.Fatalf("parse error: %s", diags.Error())
	}

	mock := &testutil.MockResourceMonitor{}
	engine := newTestEngine(t, config, &run.EngineOptions{
		ModuleLoader:    testModuleLoader(t),
		ProjectName:     "test-project",
		StackName:       "dev",
		ResourceMonitor: mock,
		WorkDir:         t.TempDir(),
		RootDir:         t.TempDir(),
		SchemaLoader: schemaloader.New(t, schema.PackageSpec{
			Name: "aws",
			Resources: map[string]schema.ResourceSpec{
				"aws:index:Instance": {
					InputProperties: map[string]schema.PropertySpec{
						"ami": {TypeSpec: schema.TypeSpec{Type: "string"}},
					},
					ObjectTypeSpec: schema.ObjectTypeSpec{
						Properties: map[string]schema.PropertySpec{
							"ami": {TypeSpec: schema.TypeSpec{Type: "string"}},
						},
					},
				},
				"aws:s3:Bucket": {
					InputProperties: map[string]schema.PropertySpec{
						"bucket": {TypeSpec: schema.TypeSpec{Type: "string"}},
					},
					ObjectTypeSpec: schema.ObjectTypeSpec{
						Properties: map[string]schema.PropertySpec{
							"bucket": {TypeSpec: schema.TypeSpec{Type: "string"}},
						},
					},
				},
			},
		}),
	})

	err := engine.Run(t.Context())
	if err != nil {
		t.Fatalf("run error: %v", err)
	}

	// Should have stack + 2 resources, bucket first due to depends_on
	if len(mock.RegisteredResources) != 3 {
		t.Fatalf("expected 3 registered resources (stack + 2 resources), got %d", len(mock.RegisteredResources))
	}

	// Bucket should be first (after stack)
	if mock.RegisteredResources[1].Name != "mybucket" {
		t.Errorf("expected bucket first, got %s", mock.RegisteredResources[1].Name)
	}

	// Instance should have depends_on set
	if len(mock.RegisteredResources[2].Dependencies) == 0 {
		t.Error("expected instance to have dependencies from depends_on")
	}
}

func TestEngine_Lifecycle(t *testing.T) {
	t.Parallel()

	src := []byte(`
resource "aws_instance" "web" {
  ami = "ami-12345"

  lifecycle {
    prevent_destroy = true
    ignore_changes  = [tags]
  }
}
`)

	p := parser.NewParser()
	config, diags := p.ParseSource("test.hcl", src)
	if diags.HasErrors() {
		t.Fatalf("parse error: %s", diags.Error())
	}

	mock := &testutil.MockResourceMonitor{}
	engine := newTestEngine(t, config, &run.EngineOptions{
		ModuleLoader:    testModuleLoader(t),
		ProjectName:     "test-project",
		StackName:       "dev",
		ResourceMonitor: mock,
		WorkDir:         t.TempDir(),
		RootDir:         t.TempDir(),
		SchemaLoader: schemaloader.New(t, schema.PackageSpec{
			Name: "aws",
			Resources: map[string]schema.ResourceSpec{
				"aws:index:Instance": {
					InputProperties: map[string]schema.PropertySpec{
						"ami":  {TypeSpec: schema.TypeSpec{Type: "string"}},
						"tags": {TypeSpec: schema.TypeSpec{Type: "object", AdditionalProperties: &schema.TypeSpec{Type: "string"}}},
					},
					ObjectTypeSpec: schema.ObjectTypeSpec{
						Properties: map[string]schema.PropertySpec{
							"ami":  {TypeSpec: schema.TypeSpec{Type: "string"}},
							"tags": {TypeSpec: schema.TypeSpec{Type: "object", AdditionalProperties: &schema.TypeSpec{Type: "string"}}},
						},
					},
				},
			},
		}),
	})

	err := engine.Run(t.Context())
	if err != nil {
		t.Fatalf("run error: %v", err)
	}

	if len(mock.RegisteredResources) != 2 {
		t.Fatalf("expected 2 registered resources (stack + resource), got %d", len(mock.RegisteredResources))
	}

	req := mock.RegisteredResources[1]

	// prevent_destroy binds the destroy dispatcher hook rather than the
	// engine's Protect option, so the guard lifts when the block leaves the
	// configuration.
	assert.False(t, req.Protect)
	require.NotNil(t, req.Hooks)
	assert.Equal(t, []string{"pulumi-hcl:provisioner:before-delete"}, req.Hooks.BeforeDelete)

	assert.Equal(t, []property.Glob{property.GlobFromSegments(property.NewSegment("tags"))}, req.IgnoreChanges)
}

// TestEngine_PreventDestroyExpression verifies that prevent_destroy accepts a
// non-literal expression, evaluated against the program's scope.
func TestEngine_PreventDestroyExpression(t *testing.T) {
	t.Parallel()

	runEngine := func(t *testing.T, lifecycle string) (*testutil.MockResourceMonitor, error) {
		src := []byte(`
variable "flag" {
  default = true
}

resource "aws_instance" "web" {
  ami = "ami-12345"

  lifecycle {
    prevent_destroy = ` + lifecycle + `
  }
}
`)
		p := parser.NewParser()
		config, diags := p.ParseSource("test.hcl", src)
		if diags.HasErrors() {
			t.Fatalf("parse error: %s", diags.Error())
		}

		mock := &testutil.MockResourceMonitor{}
		engine := newTestEngine(t, config, &run.EngineOptions{
			ModuleLoader:    testModuleLoader(t),
			ProjectName:     "test-project",
			StackName:       "dev",
			ResourceMonitor: mock,
			WorkDir:         t.TempDir(),
			RootDir:         t.TempDir(),
			SchemaLoader: schemaloader.New(t, schema.PackageSpec{
				Name: "aws",
				Resources: map[string]schema.ResourceSpec{
					"aws:index:Instance": {
						InputProperties: map[string]schema.PropertySpec{
							"ami": {TypeSpec: schema.TypeSpec{Type: "string"}},
						},
						ObjectTypeSpec: schema.ObjectTypeSpec{
							Properties: map[string]schema.PropertySpec{
								"ami": {TypeSpec: schema.TypeSpec{Type: "string"}},
							},
						},
					},
				},
			}),
		})
		return mock, engine.Run(t.Context())
	}

	t.Run("variable", func(t *testing.T) {
		t.Parallel()
		mock, err := runEngine(t, "var.flag")
		require.NoError(t, err)
		require.Len(t, mock.RegisteredResources, 2)
		req := mock.RegisteredResources[1]
		assert.False(t, req.Protect)
		require.NotNil(t, req.Hooks)
		assert.Equal(t, []string{"pulumi-hcl:provisioner:before-delete"}, req.Hooks.BeforeDelete)
	})

	t.Run("negated variable", func(t *testing.T) {
		t.Parallel()
		mock, err := runEngine(t, "!var.flag")
		require.NoError(t, err)
		require.Len(t, mock.RegisteredResources, 2)
		req := mock.RegisteredResources[1]
		assert.False(t, req.Protect)
		assert.Nil(t, req.Hooks)
	})

	t.Run("invalid type", func(t *testing.T) {
		t.Parallel()
		_, err := runEngine(t, "[true]")
		require.ErrorContains(t, err, `invalid prevent_destroy value on "aws_instance.web"`)
	})

	t.Run("per-instance reference", func(t *testing.T) {
		t.Parallel()
		_, err := runEngine(t, "count.index == 0")
		require.ErrorContains(t, err, "invalid reference in prevent_destroy")
	})
}

// TestEngine_PulumiProtect verifies that a resource or module `pulumi {
// protect = ... }` maps to the engine's Protect option.
func TestEngine_PulumiProtect(t *testing.T) {
	t.Parallel()

	awsSchema := func(t *testing.T) schema.ReferenceLoader {
		return schemaloader.New(t, schema.PackageSpec{
			Name: "aws",
			Resources: map[string]schema.ResourceSpec{
				"aws:index:Instance": {
					InputProperties: map[string]schema.PropertySpec{
						"ami": {TypeSpec: schema.TypeSpec{Type: "string"}},
					},
					ObjectTypeSpec: schema.ObjectTypeSpec{
						Properties: map[string]schema.PropertySpec{
							"ami": {TypeSpec: schema.TypeSpec{Type: "string"}},
						},
					},
				},
			},
		})
	}

	t.Run("resource", func(t *testing.T) {
		t.Parallel()
		src := []byte(`
resource "aws_instance" "web" {
  ami = "ami-12345"

  pulumi {
    protect = true
  }
}
`)
		p := parser.NewParser()
		config, diags := p.ParseSource("test.hcl", src)
		require.False(t, diags.HasErrors(), "parse error: %s", diags.Error())

		mock := &testutil.MockResourceMonitor{}
		engine := newTestEngine(t, config, &run.EngineOptions{
			ModuleLoader:    testModuleLoader(t),
			ProjectName:     "test-project",
			StackName:       "dev",
			ResourceMonitor: mock,
			WorkDir:         t.TempDir(),
			RootDir:         t.TempDir(),
			SchemaLoader:    awsSchema(t),
		})
		require.NoError(t, engine.Run(t.Context()))

		require.Len(t, mock.RegisteredResources, 2)
		req := mock.RegisteredResources[1]
		assert.True(t, req.Protect)
		assert.Nil(t, req.Hooks)
	})

	t.Run("module", func(t *testing.T) {
		t.Parallel()
		tmpDir := t.TempDir()
		moduleDir := tmpDir + "/modules/child"
		require.NoError(t, os.MkdirAll(moduleDir, 0o755))
		require.NoError(t, os.WriteFile(moduleDir+"/main.tf", []byte(`
resource "aws_instance" "r" {
  ami = "ami-r"
}
`), 0o644))
		require.NoError(t, os.WriteFile(tmpDir+"/main.tf", []byte(`
module "a" {
  source = "./modules/child"

  pulumi {
    protect = true
  }
}
`), 0o644))

		p := parser.NewParser()
		config, diags := p.ParseDirectory(tmpDir)
		require.False(t, diags.HasErrors(), "parse error: %s", diags.Error())

		mock := &testutil.MockResourceMonitor{}
		engine := newTestEngine(t, config, &run.EngineOptions{
			ModuleLoader:    testLiveModuleLoader(t),
			ProjectName:     "test-project",
			StackName:       "dev",
			ResourceMonitor: mock,
			WorkDir:         tmpDir,
			RootDir:         tmpDir,
			SchemaLoader:    awsSchema(t),
		})
		require.NoError(t, engine.Run(t.Context()))

		protects := map[string]bool{}
		for _, req := range mock.RegisteredResources {
			if req.Type != "pulumi:pulumi:Stack" {
				protects[req.Type] = req.Protect
			}
		}
		assert.Equal(t, map[string]bool{
			"components:index:Child": true,
			"aws:index:Instance":     false,
		}, protects)
	})
}

func TestEngine_CreateBeforeDestroy(t *testing.T) {
	t.Parallel()

	src := []byte(`
resource "aws_instance" "web" {
  ami = "ami-12345"

  lifecycle {
    create_before_destroy = true
  }
}
`)

	p := parser.NewParser()
	config, diags := p.ParseSource("test.hcl", src)
	if diags.HasErrors() {
		t.Fatalf("parse error: %s", diags.Error())
	}

	mock := &testutil.MockResourceMonitor{}
	engine := newTestEngine(t, config, &run.EngineOptions{
		ModuleLoader:    testModuleLoader(t),
		ProjectName:     "test-project",
		StackName:       "dev",
		ResourceMonitor: mock,
		WorkDir:         t.TempDir(),
		RootDir:         t.TempDir(),
		SchemaLoader: schemaloader.New(t, schema.PackageSpec{
			Name: "aws",
			Resources: map[string]schema.ResourceSpec{
				"aws:index:Instance": {
					InputProperties: map[string]schema.PropertySpec{
						"ami": {TypeSpec: schema.TypeSpec{Type: "string"}},
					},
					ObjectTypeSpec: schema.ObjectTypeSpec{
						Properties: map[string]schema.PropertySpec{
							"ami": {TypeSpec: schema.TypeSpec{Type: "string"}},
						},
					},
				},
			},
		}),
	})

	err := engine.Run(t.Context())
	if err != nil {
		t.Fatalf("run error: %v", err)
	}

	if len(mock.RegisteredResources) != 2 {
		t.Fatalf("expected 2 registered resources (stack + resource), got %d", len(mock.RegisteredResources))
	}

	req := mock.RegisteredResources[1]

	// create_before_destroy = true should map to DeleteBeforeReplace = false
	// (opposite semantics: TF "create before destroy" vs Pulumi "delete before replace")
	if !req.DeleteBeforeReplaceDef {
		t.Error("expected DeleteBeforeReplaceDef=true when create_before_destroy is set")
	}
	if req.DeleteBeforeReplace {
		t.Error("expected DeleteBeforeReplace=false from create_before_destroy=true")
	}
}

func TestEngine_CreateBeforeDestroyFalse(t *testing.T) {
	t.Parallel()

	// Explicit create_before_destroy = false should enable Terraform's default
	// behavior (delete-then-create), which maps to Pulumi's deleteBeforeReplace = true
	src := []byte(`
resource "aws_instance" "web" {
  ami = "ami-12345"

  lifecycle {
    create_before_destroy = false
  }
}
`)

	p := parser.NewParser()
	config, diags := p.ParseSource("test.hcl", src)
	if diags.HasErrors() {
		t.Fatalf("parse error: %s", diags.Error())
	}

	mock := &testutil.MockResourceMonitor{}
	engine := newTestEngine(t, config, &run.EngineOptions{
		ModuleLoader:    testModuleLoader(t),
		ProjectName:     "test-project",
		StackName:       "dev",
		ResourceMonitor: mock,
		WorkDir:         t.TempDir(),
		RootDir:         t.TempDir(),
		SchemaLoader: schemaloader.New(t, schema.PackageSpec{
			Name: "aws",
			Resources: map[string]schema.ResourceSpec{
				"aws:index:Instance": {
					InputProperties: map[string]schema.PropertySpec{
						"ami": {TypeSpec: schema.TypeSpec{Type: "string"}},
					},
					ObjectTypeSpec: schema.ObjectTypeSpec{
						Properties: map[string]schema.PropertySpec{
							"ami": {TypeSpec: schema.TypeSpec{Type: "string"}},
						},
					},
				},
			},
		}),
	})

	err := engine.Run(t.Context())
	if err != nil {
		t.Fatalf("run error: %v", err)
	}

	if len(mock.RegisteredResources) != 2 {
		t.Fatalf("expected 2 registered resources (stack + resource), got %d", len(mock.RegisteredResources))
	}

	req := mock.RegisteredResources[1]

	// create_before_destroy = false should map to DeleteBeforeReplace = true
	// (Terraform's default: delete old, then create new)
	if !req.DeleteBeforeReplaceDef {
		t.Error("expected DeleteBeforeReplaceDef=true when create_before_destroy is explicitly set")
	}
	if !req.DeleteBeforeReplace {
		t.Error("expected DeleteBeforeReplace=true from create_before_destroy=false")
	}
}

func TestEngine_VariableFromConfig(t *testing.T) {
	t.Parallel()

	src := []byte(`
variable "region" {
  type    = string
  default = "us-east-1"
}

output "region_value" {
  value = var.region
}
`)

	p := parser.NewParser()
	config, diags := p.ParseSource("test.hcl", src)
	if diags.HasErrors() {
		t.Fatalf("parse error: %s", diags.Error())
	}

	mock := &testutil.MockResourceMonitor{}
	engine := newTestEngine(t, config, &run.EngineOptions{
		ModuleLoader:    testModuleLoader(t),
		ProjectName:     "test-project",
		StackName:       "dev",
		ResourceMonitor: mock,
		WorkDir:         t.TempDir(),
		RootDir:         t.TempDir(),
		SchemaLoader: schemaloader.New(t, schema.PackageSpec{
			Name: "aws",
			Resources: map[string]schema.ResourceSpec{
				"aws:index:Instance": {
					InputProperties: map[string]schema.PropertySpec{
						"ami": {TypeSpec: schema.TypeSpec{Type: "string"}},
					},
					ObjectTypeSpec: schema.ObjectTypeSpec{
						Properties: map[string]schema.PropertySpec{
							"ami": {TypeSpec: schema.TypeSpec{Type: "string"}},
						},
					},
				},
			},
		}),
		Config: map[string]run.ConfigValue{
			"test-project:region": run.UntypedConfigValue("us-west-2", false),
		},
	})

	err := engine.Run(t.Context())
	if err != nil {
		t.Fatalf("run error: %v", err)
	}

	// Check the stack outputs - region should be us-west-2 from config, not default
	if mock.StackOutputs.Len() == 0 {
		t.Fatal("expected stack outputs")
	}
	regionOutput, ok := mock.StackOutputs.GetOk("region_value")
	if !ok {
		t.Fatal("expected region_value output")
	}
	if regionOutput.AsString() != "us-west-2" {
		t.Errorf("expected region_value=%q from config, got %q", "us-west-2", regionOutput.AsString())
	}
}

// TestEngine_VariableFromSecretConfig covers the untyped-secret path: a config
// value supplied as an untyped string and flagged secret marks the variable
// sensitive, so a value derived from it surfaces as a secret while round-tripping
// intact.
func TestEngine_VariableFromSecretConfig(t *testing.T) {
	t.Parallel()

	src := []byte(`
variable "region" {
  type = string
}

output "region_value" {
  value = var.region
}
`)

	p := parser.NewParser()
	config, diags := p.ParseSource("test.hcl", src)
	require.False(t, diags.HasErrors(), "parse error: %s", diags.Error())

	mock := &testutil.MockResourceMonitor{}
	engine := newTestEngine(t, config, &run.EngineOptions{
		ModuleLoader:    testModuleLoader(t),
		ProjectName:     "test-project",
		StackName:       "dev",
		ResourceMonitor: mock,
		WorkDir:         t.TempDir(),
		RootDir:         t.TempDir(),
		SchemaLoader:    schemaloader.New(t, schema.PackageSpec{Name: "aws"}),
		Config: map[string]run.ConfigValue{
			"test-project:region": run.UntypedConfigValue("us-west-2", true),
		},
	})

	require.NoError(t, engine.Run(t.Context()))

	regionOutput, ok := mock.StackOutputs.GetOk("region_value")
	require.True(t, ok, "expected region_value output")
	require.True(t, regionOutput.Secret(),
		"a secret config value should make the variable, and a value derived from it, secret")
	require.Equal(t, "us-west-2", regionOutput.AsString(),
		"the secret value should reach the program intact")
}

func TestEngine_TerraformWorkspace(t *testing.T) {
	t.Parallel()

	src := []byte(`
output "ws" {
  value = terraform.workspace
}
`)

	p := parser.NewParser()
	config, diags := p.ParseSource("test.hcl", src)
	require.False(t, diags.HasErrors(), "parse error: %s", diags.Error())

	mock := &testutil.MockResourceMonitor{}
	engine := newTestEngine(t, config, &run.EngineOptions{
		ModuleLoader:    testModuleLoader(t),
		ProjectName:     "test-project",
		StackName:       "dev",
		ResourceMonitor: mock,
		WorkDir:         t.TempDir(),
		RootDir:         t.TempDir(),
		SchemaLoader:    schemaloader.New(t, schema.PackageSpec{Name: "aws"}),
	})

	require.NoError(t, engine.Run(t.Context()))

	ws, ok := mock.StackOutputs.GetOk("ws")
	require.True(t, ok, "expected ws output")
	assert.Equal(t, "dev", ws.AsString())
}

// TestEngine_VariableFromTfvars covers the engine wiring of the
// automatically-loaded variable-value files: a declared variable takes its
// value from the file, a name the root module does not declare is reported and
// never evaluated, and a value that does not fit the declared type is an error.
func TestEngine_VariableFromTfvars(t *testing.T) {
	t.Parallel()

	run1 := func(t *testing.T, program, tfvars string) (*testutil.MockResourceMonitor, error) {
		workDir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(workDir, "terraform.tfvars"), []byte(tfvars), 0o600))

		p := parser.NewParser()
		config, diags := p.ParseSource("test.hcl", []byte(program))
		require.False(t, diags.HasErrors(), "%s", diags)

		mock := &testutil.MockResourceMonitor{}
		engine := newTestEngine(t, config, &run.EngineOptions{
			ModuleLoader:    testModuleLoader(t),
			ProjectName:     "test-project",
			StackName:       "dev",
			ResourceMonitor: mock,
			RootModule:      true,
			WorkDir:         workDir,
			RootDir:         t.TempDir(),
			SchemaLoader:    schemaloader.New(t, schema.PackageSpec{Name: "empty"}),
		})
		return mock, engine.Run(t.Context())
	}

	const program = `
variable "greeting" {
  type    = string
  default = "from-default"
}

output "greeting" {
  value = var.greeting
}
`

	t.Run("value and undeclared warning", func(t *testing.T) {
		t.Parallel()
		mock, err := run1(t, program, "greeting = \"from-tfvars\"\nundeclared = upper(\"x\")\n")
		require.NoError(t, err)

		greeting, ok := mock.StackOutputs.GetOk("greeting")
		require.True(t, ok, "expected greeting output")
		assert.Equal(t, "from-tfvars", greeting.AsString())
		assert.Equal(t, []string{
			`Value for undeclared variable: the root module does not declare a variable named ` +
				`"undeclared" but a value was found in file "terraform.tfvars". If you meant to use ` +
				`this value, add a "variable" block to the configuration.`,
		}, mock.Warnings)
	})

	t.Run("value must fit the declared type", func(t *testing.T) {
		t.Parallel()
		_, err := run1(t, "variable \"n\" {\n  type = number\n}\n", "n = \"abc\"\n")
		assert.EqualError(t, err,
			`variable "n": the given value is not suitable for var.n: a number is required`)
	})

	// A declared default is held to the declared type too.
	t.Run("default must fit the declared type", func(t *testing.T) {
		t.Parallel()
		_, err := run1(t, "variable \"n\" {\n  type    = number\n  default = \"abc\"\n}\n", "")
		assert.EqualError(t, err, `variable "n": this default value is not compatible with `+
			`the variable's type constraint: a number is required`)
	})

	// An explicit null from a file is rejected like any other supplied null:
	// the default is substituted.
	t.Run("null substitutes the default when nullable is false", func(t *testing.T) {
		t.Parallel()
		program := `
variable "greeting" {
  type     = string
  default  = "from-default"
  nullable = false
}

output "greeting" {
  value = var.greeting
}
`
		mock, err := run1(t, program, "greeting = null\n")
		require.NoError(t, err)
		greeting, ok := mock.StackOutputs.GetOk("greeting")
		require.True(t, ok, "expected greeting output")
		assert.Equal(t, "from-default", greeting.AsString())
	})
}

func TestEngine_VariableFromEnv(t *testing.T) {
	src := []byte(`
variable "region" {
  type    = string
  default = "us-east-1"
}

output "region_value" {
  value = var.region
}
`)

	p := parser.NewParser()
	config, diags := p.ParseSource("test.hcl", src)
	if diags.HasErrors() {
		t.Fatalf("parse error: %s", diags.Error())
	}

	// Set environment variable
	t.Setenv("TF_VAR_region", "eu-west-1")

	mock := &testutil.MockResourceMonitor{}
	engine := newTestEngine(t, config, &run.EngineOptions{
		ModuleLoader:    testModuleLoader(t),
		ProjectName:     "test-project",
		StackName:       "dev",
		ResourceMonitor: mock,
		WorkDir:         t.TempDir(),
		RootDir:         t.TempDir(),
		SchemaLoader: schemaloader.New(t, schema.PackageSpec{
			Name: "aws",
			Resources: map[string]schema.ResourceSpec{
				"aws:index:Instance": {
					InputProperties: map[string]schema.PropertySpec{
						"ami": {TypeSpec: schema.TypeSpec{Type: "string"}},
					},
					ObjectTypeSpec: schema.ObjectTypeSpec{
						Properties: map[string]schema.PropertySpec{
							"ami": {TypeSpec: schema.TypeSpec{Type: "string"}},
						},
					},
				},
			},
		}),
	})

	require.NoError(t, engine.Run(t.Context()))

	regionOutput, ok := mock.StackOutputs.GetOk("region_value")
	require.True(t, ok, "expected region_value output")
	assert.Equal(t, "eu-west-1", regionOutput.AsString())
}

func TestEngine_VariableDefaultCoercedToType(t *testing.T) {
	t.Parallel()

	// A variable's `default` is coerced to its declared `type` exactly like a
	// supplied value: a list(string) default written with non-string elements
	// becomes a list of strings.
	src := []byte(`
variable "lst" {
  type    = list(string)
  default = ["a", 1, true]
}

output "lst_value" {
  value = var.lst
}
`)

	p := parser.NewParser()
	config, diags := p.ParseSource("test.hcl", src)
	if diags.HasErrors() {
		t.Fatalf("parse error: %s", diags.Error())
	}

	mock := &testutil.MockResourceMonitor{}
	engine := newTestEngine(t, config, &run.EngineOptions{
		ModuleLoader:    testModuleLoader(t),
		ProjectName:     "test-project",
		StackName:       "dev",
		ResourceMonitor: mock,
		WorkDir:         t.TempDir(),
		RootDir:         t.TempDir(),
		SchemaLoader:    schemaloader.New(t, schema.PackageSpec{Name: "aws"}),
	})

	err := engine.Run(t.Context())
	require.NoError(t, err)

	lstOutput, ok := mock.StackOutputs.GetOk("lst_value")
	require.True(t, ok, "expected lst_value output")
	assert.Equal(t, property.New([]property.Value{
		property.New("a"),
		property.New("1"),
		property.New("true"),
	}), lstOutput)
}

func TestEngine_VariableRequired(t *testing.T) {
	t.Parallel()

	// A variable declared without a default is required, regardless of its
	// `nullable` setting. (The `nullable` attribute only governs whether a
	// provided value may be the null literal.)
	tests := []struct {
		name string
		src  string
	}{
		{
			name: "no_nullable_attribute",
			src: `
variable "required_var" {
  type = string
}
`,
		},
		{
			name: "nullable_true",
			src: `
variable "required_var" {
  type     = string
  nullable = true
}
`,
		},
		{
			name: "nullable_false",
			src: `
variable "required_var" {
  type     = string
  nullable = false
}
`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			p := parser.NewParser()
			config, diags := p.ParseSource("test.hcl", []byte(tc.src))
			require.False(t, diags.HasErrors(), "parse error: %s", diags.Error())

			mock := &testutil.MockResourceMonitor{}
			engine := newTestEngine(t, config, &run.EngineOptions{
				ModuleLoader:    testModuleLoader(t),
				ProjectName:     "test-project",
				StackName:       "dev",
				ResourceMonitor: mock,
				WorkDir:         t.TempDir(),
				RootDir:         t.TempDir(),
				SchemaLoader:    schemaloader.New(t, schema.PackageSpec{Name: "empty"}),
			})

			err := engine.Run(t.Context())
			assert.EqualError(t, err, `variable "required_var" is required but no value was provided. `+
				`Set it in a variable-value file such as terraform.tfvars, with the `+
				`TF_VAR_required_var environment variable, or with Pulumi config: `+
				`pulumi config set required_var <value>`)
		})
	}
}

// TestEngine_ModuleNullableFalseNullInput covers a `nullable = false` child
// module variable that receives `null` from its caller. With a default,
// Terraform/OpenTofu substitute the default; without one, it is an error.
func TestEngine_ModuleNullableFalseNullInput(t *testing.T) {
	t.Parallel()

	const childWithDefault = `
variable "items" {
  type     = list(string)
  default  = ["a", "b"]
  nullable = false
}

output "count" {
  value = length(var.items)
}
`
	const childNoDefault = `
variable "items" {
  type     = list(string)
  nullable = false
}

output "count" {
  value = length(var.items)
}
`
	const rootMain = `
module "child" {
  source = "./modules/child"
  items  = null
}

output "item_count" {
  value = module.child.count
}
`

	run := func(t *testing.T, childMain string) (*testutil.MockResourceMonitor, error) {
		tmpDir := t.TempDir()
		moduleDir := tmpDir + "/modules/child"
		require.NoError(t, os.MkdirAll(moduleDir, 0o755))
		require.NoError(t, os.WriteFile(moduleDir+"/main.tf", []byte(childMain), 0o644))
		require.NoError(t, os.WriteFile(tmpDir+"/main.tf", []byte(rootMain), 0o644))

		p := parser.NewParser()
		config, diags := p.ParseDirectory(tmpDir)
		require.False(t, diags.HasErrors(), "parse error: %s", diags.Error())

		mock := &testutil.MockResourceMonitor{}
		engine := newTestEngine(t, config, &run.EngineOptions{
			ModuleLoader:    testLiveModuleLoader(t),
			ProjectName:     "test-project",
			StackName:       "dev",
			ResourceMonitor: mock,
			WorkDir:         tmpDir,
			RootDir:         tmpDir,
			SchemaLoader:    schemaloader.New(t, schema.PackageSpec{Name: "empty"}),
		})
		return mock, engine.Run(t.Context())
	}

	t.Run("with_default", func(t *testing.T) {
		t.Parallel()

		mock, err := run(t, childWithDefault)
		require.NoError(t, err)

		output, ok := mock.StackOutputs.GetOk("item_count")
		require.True(t, ok, "expected item_count output")
		assert.Equal(t, float64(2), output.AsNumber())
	})

	t.Run("no_default", func(t *testing.T) {
		t.Parallel()

		_, err := run(t, childNoDefault)
		assert.EqualError(t, err, `variable "items" must not be set to null: `+
			`it is declared with nullable = false and has no default`)
	})
}

func TestEngine_ModuleVariableValidation(t *testing.T) {
	t.Parallel()

	const childMain = `
variable "name" {
  type = string

  validation {
    condition     = length(var.name) > 3
    error_message = "name must be longer than three characters"
  }
}

output "name" {
  value = var.name
}
`
	run := func(t *testing.T, name string) (*testutil.MockResourceMonitor, error) {
		tmpDir := t.TempDir()
		moduleDir := tmpDir + "/modules/child"
		require.NoError(t, os.MkdirAll(moduleDir, 0o755))
		require.NoError(t, os.WriteFile(moduleDir+"/main.tf", []byte(childMain), 0o644))
		rootMain := `
module "child" {
  source = "./modules/child"
  name   = "` + name + `"
}

output "name" {
  value = module.child.name
}
`
		require.NoError(t, os.WriteFile(tmpDir+"/main.tf", []byte(rootMain), 0o644))

		p := parser.NewParser()
		config, diags := p.ParseDirectory(tmpDir)
		require.False(t, diags.HasErrors(), "parse error: %s", diags.Error())

		mock := &testutil.MockResourceMonitor{}
		engine := newTestEngine(t, config, &run.EngineOptions{
			ModuleLoader:    testLiveModuleLoader(t),
			ProjectName:     "test-project",
			StackName:       "dev",
			ResourceMonitor: mock,
			WorkDir:         tmpDir,
			RootDir:         tmpDir,
			SchemaLoader:    schemaloader.New(t, schema.PackageSpec{Name: "empty"}),
		})
		return mock, engine.Run(t.Context())
	}

	t.Run("valid", func(t *testing.T) {
		t.Parallel()

		mock, err := run(t, "widget")
		require.NoError(t, err)

		output, ok := mock.StackOutputs.GetOk("name")
		require.True(t, ok, "expected name output")
		assert.Equal(t, "widget", output.AsString())
	})

	t.Run("invalid", func(t *testing.T) {
		t.Parallel()

		_, err := run(t, "xy")
		assert.EqualError(t, err,
			`validation failed for variable "name": name must be longer than three characters`)
	})
}

func TestEngine_OutputPrecondition(t *testing.T) {
	t.Parallel()

	newEngine := func(t *testing.T, config *ast.Config, workDir string) (*testutil.MockResourceMonitor, *run.Engine) {
		mock := &testutil.MockResourceMonitor{}
		engine := newTestEngine(t, config, &run.EngineOptions{
			ModuleLoader:    testLiveModuleLoader(t),
			ProjectName:     "test-project",
			StackName:       "dev",
			ResourceMonitor: mock,
			WorkDir:         workDir,
			RootDir:         workDir,
			SchemaLoader:    schemaloader.New(t, schema.PackageSpec{Name: "empty"}),
		})
		return mock, engine
	}

	t.Run("root_pass", func(t *testing.T) {
		t.Parallel()

		src := []byte(`
variable "enabled" {
  type    = bool
  default = true
}

output "result" {
  value = "ok"

  precondition {
    condition     = var.enabled
    error_message = "must be enabled"
  }
}
`)
		config, diags := parser.NewParser().ParseSource("test.hcl", src)
		require.False(t, diags.HasErrors(), "parse error: %s", diags.Error())

		mock, engine := newEngine(t, config, t.TempDir())
		require.NoError(t, engine.Run(t.Context()))

		output, ok := mock.StackOutputs.GetOk("result")
		require.True(t, ok, "expected result output")
		assert.Equal(t, "ok", output.AsString())
	})

	t.Run("root_fail", func(t *testing.T) {
		t.Parallel()

		src := []byte(`
variable "enabled" {
  type    = bool
  default = false
}

output "result" {
  value = "ok"

  precondition {
    condition     = var.enabled
    error_message = "must be enabled"
  }
}
`)
		config, diags := parser.NewParser().ParseSource("test.hcl", src)
		require.False(t, diags.HasErrors(), "parse error: %s", diags.Error())

		_, engine := newEngine(t, config, t.TempDir())
		assert.EqualError(t, engine.Run(t.Context()),
			`processing output result: precondition for output "result": must be enabled`)
	})

	t.Run("module_fail", func(t *testing.T) {
		t.Parallel()

		tmpDir := t.TempDir()
		moduleDir := tmpDir + "/modules/child"
		require.NoError(t, os.MkdirAll(moduleDir, 0o755))
		require.NoError(t, os.WriteFile(moduleDir+"/main.tf", []byte(`
variable "ok" {
  type    = bool
  default = false
}

output "result" {
  value = "ok"

  precondition {
    condition     = var.ok
    error_message = "child output not ok"
  }
}
`), 0o644))
		require.NoError(t, os.WriteFile(tmpDir+"/main.tf", []byte(`
module "child" {
  source = "./modules/child"
}

output "child_result" {
  value = module.child.result
}
`), 0o644))

		config, diags := parser.NewParser().ParseDirectory(tmpDir)
		require.False(t, diags.HasErrors(), "parse error: %s", diags.Error())

		_, engine := newEngine(t, config, tmpDir)
		assert.EqualError(t, engine.Run(t.Context()),
			`precondition for output "result": child output not ok`)
	})
}

func TestEngine_ModuleSensitiveOutput(t *testing.T) {
	t.Parallel()

	const childMain = `
variable "in" {
  type    = string
  default = "hunter2"
}

output "token" {
  value     = var.in
  sensitive = true
}
`
	const rootMain = `
module "child" {
  source = "./modules/child"
}

output "is_sensitive" {
  value = issensitive(module.child.token)
}
`
	tmpDir := t.TempDir()
	moduleDir := tmpDir + "/modules/child"
	require.NoError(t, os.MkdirAll(moduleDir, 0o755))
	require.NoError(t, os.WriteFile(moduleDir+"/main.tf", []byte(childMain), 0o644))
	require.NoError(t, os.WriteFile(tmpDir+"/main.tf", []byte(rootMain), 0o644))

	p := parser.NewParser()
	config, diags := p.ParseDirectory(tmpDir)
	require.False(t, diags.HasErrors(), "parse error: %s", diags.Error())

	mock := &testutil.MockResourceMonitor{}
	engine := newTestEngine(t, config, &run.EngineOptions{
		ModuleLoader:    testLiveModuleLoader(t),
		ProjectName:     "test-project",
		StackName:       "dev",
		ResourceMonitor: mock,
		WorkDir:         tmpDir,
		RootDir:         tmpDir,
		SchemaLoader:    schemaloader.New(t, schema.PackageSpec{Name: "empty"}),
	})
	require.NoError(t, engine.Run(t.Context()))

	output, ok := mock.StackOutputs.GetOk("is_sensitive")
	require.True(t, ok, "expected is_sensitive output")
	assert.True(t, output.AsBool(), "a sensitive module output must be sensitive in the caller")
}

func TestEngine_VariableValidationPass(t *testing.T) {
	t.Parallel()

	src := []byte(`
variable "instance_type" {
  type    = string
  default = "t3.micro"

  validation {
    condition     = startswith(var.instance_type, "t3.")
    error_message = "Must be a t3 instance type."
  }
}

output "instance_type" {
  value = var.instance_type
}
`)

	p := parser.NewParser()
	config, diags := p.ParseSource("test.hcl", src)
	if diags.HasErrors() {
		t.Fatalf("parse error: %s", diags.Error())
	}

	mock := &testutil.MockResourceMonitor{}
	engine := newTestEngine(t, config, &run.EngineOptions{
		ModuleLoader:    testModuleLoader(t),
		ProjectName:     "test-project",
		StackName:       "dev",
		ResourceMonitor: mock,
		WorkDir:         t.TempDir(),
		RootDir:         t.TempDir(),
		SchemaLoader: schemaloader.New(t, schema.PackageSpec{
			Name: "aws",
			Resources: map[string]schema.ResourceSpec{
				"aws:index:Instance": {
					InputProperties: map[string]schema.PropertySpec{
						"ami": {TypeSpec: schema.TypeSpec{Type: "string"}},
					},
					ObjectTypeSpec: schema.ObjectTypeSpec{
						Properties: map[string]schema.PropertySpec{
							"ami": {TypeSpec: schema.TypeSpec{Type: "string"}},
						},
					},
				},
			},
		}),
	})

	err := engine.Run(t.Context())
	if err != nil {
		t.Fatalf("run error: %v", err)
	}

	// Should pass validation
	output, ok := mock.StackOutputs.GetOk("instance_type")
	if !ok {
		t.Fatal("expected instance_type output")
	}
	if output.AsString() != "t3.micro" {
		t.Errorf("expected t3.micro, got %q", output.AsString())
	}
}

func TestEngine_VariableValidationFail(t *testing.T) {
	t.Parallel()

	src := []byte(`
variable "instance_type" {
  type    = string
  default = "m5.large"

  validation {
    condition     = startswith(var.instance_type, "t3.")
    error_message = "Must be a t3 instance type."
  }
}
`)

	p := parser.NewParser()
	config, diags := p.ParseSource("test.hcl", src)
	if diags.HasErrors() {
		t.Fatalf("parse error: %s", diags.Error())
	}

	mock := &testutil.MockResourceMonitor{}
	engine := newTestEngine(t, config, &run.EngineOptions{
		ModuleLoader:    testModuleLoader(t),
		ProjectName:     "test-project",
		StackName:       "dev",
		ResourceMonitor: mock,
		WorkDir:         t.TempDir(),
		RootDir:         t.TempDir(),
		SchemaLoader: schemaloader.New(t, schema.PackageSpec{
			Name: "aws",
			Resources: map[string]schema.ResourceSpec{
				"aws:index:Instance": {
					InputProperties: map[string]schema.PropertySpec{
						"ami": {TypeSpec: schema.TypeSpec{Type: "string"}},
					},
					ObjectTypeSpec: schema.ObjectTypeSpec{
						Properties: map[string]schema.PropertySpec{
							"ami": {TypeSpec: schema.TypeSpec{Type: "string"}},
						},
					},
				},
			},
		}),
	})

	err := engine.Run(t.Context())

	// Should error because validation fails
	if err == nil {
		t.Fatal("expected error for validation failure")
	}
	if !strings.Contains(err.Error(), "Must be a t3 instance type") {
		t.Errorf("expected error message from validation, got: %v", err)
	}
}

// A variable's validation condition may reference another variable; the
// referencing variable must be sequenced after the one it reads, so the rule
// evaluates deterministically rather than racing the dependency.
func TestEngine_VariableValidationCrossReference(t *testing.T) {
	t.Parallel()

	runProgram := func(t *testing.T, name string) error {
		src := []byte(`
variable "min" {
  type    = number
  default = 3
}

variable "name" {
  type    = string
  default = "` + name + `"

  validation {
    condition     = length(var.name) >= var.min
    error_message = "name is shorter than min"
  }
}

output "name" {
  value = var.name
}
`)
		config, diags := parser.NewParser().ParseSource("test.hcl", src)
		require.False(t, diags.HasErrors(), "parse error: %s", diags.Error())

		engine := newTestEngine(t, config, &run.EngineOptions{
			ModuleLoader:    testModuleLoader(t),
			ProjectName:     "test-project",
			StackName:       "dev",
			ResourceMonitor: &testutil.MockResourceMonitor{},
			WorkDir:         t.TempDir(),
			RootDir:         t.TempDir(),
			SchemaLoader:    schemaloader.New(t, schema.PackageSpec{Name: "empty"}),
		})
		return engine.Run(t.Context())
	}

	t.Run("pass", func(t *testing.T) {
		t.Parallel()
		require.NoError(t, runProgram(t, "widget"))
	})

	t.Run("fail", func(t *testing.T) {
		t.Parallel()
		assert.EqualError(t, runProgram(t, "xy"),
			`validation failed for variable "name": name is shorter than min`)
	})
}

func TestEngine_VariableValidationFail_SensitiveErrorMessage(t *testing.T) {
	t.Parallel()

	src := []byte(`
variable "password" {
  type      = string
  sensitive = true
  default   = "abc"

  validation {
    condition     = length(var.password) >= 8
    error_message = "Password is too short: '${var.password}'"
  }
}
`)

	p := parser.NewParser()
	config, diags := p.ParseSource("test.hcl", src)
	if diags.HasErrors() {
		t.Fatalf("parse error: %s", diags.Error())
	}

	engine := newTestEngine(t, config, &run.EngineOptions{
		ModuleLoader:    testModuleLoader(t),
		ProjectName:     "test-project",
		StackName:       "dev",
		ResourceMonitor: &testutil.MockResourceMonitor{},
		WorkDir:         t.TempDir(),
		RootDir:         t.TempDir(),
		SchemaLoader:    schemaloader.New(t, schema.PackageSpec{Name: "aws"}),
	})

	err := engine.Run(t.Context())

	if err == nil {
		t.Fatal("expected error for validation failure")
	}
	if !strings.Contains(err.Error(), "Error message refers to sensitive values") {
		t.Errorf("expected sensitive-reference error, got: %v", err)
	}
	if strings.Contains(err.Error(), "abc") {
		t.Errorf("sensitive value leaked into error: %v", err)
	}
}

func TestEngine_Precondition(t *testing.T) {
	t.Parallel()

	// Precondition checks startswith on a variable that feeds into the resource
	// input. This validates that preconditions can reference resource inputs and
	// that they run before RegisterResource.
	hclTemplate := `
variable "field_value" {
  type    = string
  default = "%s"
}

resource "test_resource" "res" {
  field = var.field_value

  lifecycle {
    precondition {
      condition     = startswith(var.field_value, "valid-")
      error_message = "Field must start with 'valid-'."
    }
  }
}
`

	runWithField := func(t *testing.T, value string) (*testutil.MockResourceMonitor, error) {
		t.Helper()
		src := fmt.Appendf(nil, hclTemplate, value)
		p := parser.NewParser()
		config, diags := p.ParseSource("test.hcl", src)
		require.False(t, diags.HasErrors(), diags.Error())

		mock := &testutil.MockResourceMonitor{}
		engine := newTestEngine(t, config, &run.EngineOptions{
			ModuleLoader:    testModuleLoader(t),
			ProjectName:     "test-project",
			StackName:       "dev",
			ResourceMonitor: mock,
			WorkDir:         t.TempDir(),
			RootDir:         t.TempDir(),
			SchemaLoader:    schemaloader.New(t, testSchema()),
		})
		return mock, engine.Run(t.Context())
	}

	t.Run("true condition", func(t *testing.T) {
		t.Parallel()
		mock, err := runWithField(t, "valid-value")
		require.NoError(t, err)
		require.True(t, hasRegisteredResource(mock, "test:index:Resource"),
			"expected resource to be registered when precondition passes")
	})

	t.Run("false condition", func(t *testing.T) {
		t.Parallel()
		mock, err := runWithField(t, "bad")
		require.ErrorContains(t, err, "Field must start with 'valid-'.")
		require.False(t, hasRegisteredResource(mock, "test:index:Resource"),
			"resource must not be registered when precondition fails")
	})
}

func TestEngine_Precondition_MultipleFailures(t *testing.T) {
	t.Parallel()

	// When several check rules fail, every failing rule's message is reported,
	// not just the first.
	runProgram := func(t *testing.T, src string) error {
		t.Helper()
		p := parser.NewParser()
		config, diags := p.ParseSource("test.hcl", []byte(src))
		require.False(t, diags.HasErrors(), diags.Error())

		engine := newTestEngine(t, config, &run.EngineOptions{
			ModuleLoader:    testModuleLoader(t),
			ProjectName:     "test-project",
			StackName:       "dev",
			ResourceMonitor: &testutil.MockResourceMonitor{},
			WorkDir:         t.TempDir(),
			RootDir:         t.TempDir(),
			SchemaLoader:    schemaloader.New(t, testSchema()),
		})
		return engine.Run(t.Context())
	}

	t.Run("preconditions", func(t *testing.T) {
		t.Parallel()
		err := runProgram(t, `
resource "test_resource" "res" {
  field = "x"

  lifecycle {
    precondition {
      condition     = 1 > 5
      error_message = "FIRST_RULE_FAILED"
    }
    precondition {
      condition     = 1 > 10
      error_message = "SECOND_RULE_FAILED"
    }
  }
}
`)
		require.ErrorContains(t, err, "FIRST_RULE_FAILED")
		require.ErrorContains(t, err, "SECOND_RULE_FAILED")
	})

	t.Run("postconditions", func(t *testing.T) {
		t.Parallel()
		err := runProgram(t, `
resource "test_resource" "res" {
  field = "x"

  lifecycle {
    postcondition {
      condition     = self.field == "a"
      error_message = "FIRST_RULE_FAILED"
    }
    postcondition {
      condition     = self.field == "b"
      error_message = "SECOND_RULE_FAILED"
    }
  }
}
`)
		require.ErrorContains(t, err, "FIRST_RULE_FAILED")
		require.ErrorContains(t, err, "SECOND_RULE_FAILED")
	})

	t.Run("output preconditions", func(t *testing.T) {
		t.Parallel()
		err := runProgram(t, `
output "result" {
  value = "x"

  precondition {
    condition     = 1 > 5
    error_message = "FIRST_RULE_FAILED"
  }
  precondition {
    condition     = 1 > 10
    error_message = "SECOND_RULE_FAILED"
  }
}
`)
		require.ErrorContains(t, err, "FIRST_RULE_FAILED")
		require.ErrorContains(t, err, "SECOND_RULE_FAILED")
	})
}

func TestEngine_Precondition_SensitiveErrorMessage(t *testing.T) {
	t.Parallel()

	src := []byte(`
variable "secret" {
  type      = string
  sensitive = true
  default   = "abc"
}

resource "test_resource" "res" {
  field = "x"

  lifecycle {
    precondition {
      condition     = length(var.secret) >= 8
      error_message = "Secret too short: '${var.secret}'"
    }
  }
}
`)

	p := parser.NewParser()
	config, diags := p.ParseSource("test.hcl", src)
	require.False(t, diags.HasErrors(), diags.Error())

	mock := &testutil.MockResourceMonitor{}
	engine := newTestEngine(t, config, &run.EngineOptions{
		ModuleLoader:    testModuleLoader(t),
		ProjectName:     "test-project",
		StackName:       "dev",
		ResourceMonitor: mock,
		WorkDir:         t.TempDir(),
		RootDir:         t.TempDir(),
		SchemaLoader:    schemaloader.New(t, testSchema()),
	})

	err := engine.Run(t.Context())
	require.ErrorContains(t, err, "Error message refers to sensitive values")
	require.NotContains(t, err.Error(), "abc", "sensitive value leaked into precondition error")
	require.False(t, hasRegisteredResource(mock, "test:index:Resource"),
		"resource must not be registered when precondition fails")
}

func TestEngine_Precondition_ReferencesOtherResource(t *testing.T) {
	t.Parallel()

	// The dependent resource's precondition references the upstream resource's
	// output. The graph should add an implicit dep so the hook fires with known
	// values, and the engine should register both resources.
	src := []byte(`
resource "test_resource" "dependent" {
  field = "downstream"

  lifecycle {
    precondition {
      condition     = test_resource.upstream.field == "known"
      error_message = "upstream must be known"
    }
  }
}

resource "test_resource" "upstream" {
  field = "known"
}
`)

	p := parser.NewParser()
	config, diags := p.ParseSource("test.hcl", src)
	require.False(t, diags.HasErrors(), diags.Error())

	mock := &testutil.MockResourceMonitor{}
	engine := newTestEngine(t, config, &run.EngineOptions{
		ModuleLoader:    testModuleLoader(t),
		ProjectName:     "test-project",
		StackName:       "dev",
		ResourceMonitor: mock,
		WorkDir:         t.TempDir(),
		RootDir:         t.TempDir(),
		SchemaLoader:    schemaloader.New(t, testSchema()),
	})
	require.NoError(t, engine.Run(t.Context()))

	var dependentReq *run.RegisterResourceRequest
	for i := range mock.RegisteredResources {
		if mock.RegisteredResources[i].Name == "dependent" {
			dependentReq = &mock.RegisteredResources[i]
		}
	}
	require.NotNil(t, dependentReq, "dependent resource should be registered")

	require.NotNil(t, dependentReq.Hooks, "dependent resource should have hooks bound")
	require.Len(t, dependentReq.Hooks.BeforeCreate, 1)
	require.Len(t, dependentReq.Hooks.BeforeUpdate, 1)
}

func TestEngine_Precondition_UnknownDuringPreview(t *testing.T) {
	t.Parallel()

	// During preview the upstream's id is unknown. A precondition that depends
	// on it must defer (no error) rather than fail, mirroring Terraform's
	// "known after apply" behaviour.
	src := []byte(`
resource "test_resource" "upstream" {
  field = "value"
}

resource "test_resource" "dependent" {
  field = "downstream"

  lifecycle {
    precondition {
      condition     = test_resource.upstream.id == "expected-id"
      error_message = "id must be expected-id"
    }
  }
}
`)

	p := parser.NewParser()
	config, diags := p.ParseSource("test.hcl", src)
	require.False(t, diags.HasErrors(), diags.Error())

	mock := &testutil.MockResourceMonitor{
		DryRun: true,
		RegisterResourceHandler: func(ctx context.Context, req run.RegisterResourceRequest) (*run.RegisterResourceResponse, error) {
			return &run.RegisterResourceResponse{
				URN:     urn.URN("urn:pulumi:test::project::" + req.Type + "::" + req.Name),
				ID:      "",
				Outputs: req.Inputs,
			}, nil
		},
	}
	engine := newTestEngine(t, config, &run.EngineOptions{
		ModuleLoader:    testModuleLoader(t),
		ProjectName:     "test-project",
		StackName:       "dev",
		ResourceMonitor: mock,
		WorkDir:         t.TempDir(),
		RootDir:         t.TempDir(),
		DryRun:          true,
		SchemaLoader:    schemaloader.New(t, testSchema()),
	})
	require.NoError(t, engine.Run(t.Context()),
		"engine should defer unknown precondition during preview rather than fail")
	require.True(t, hasRegisteredResource(mock, "test:index:Resource"),
		"dependent resource should still register when precondition is unknown")
}

func TestEngine_Postcondition(t *testing.T) {
	t.Parallel()

	// Postcondition checks self.field against a known value. The mock echoes
	// inputs as outputs, so self.field == the input value.
	// A failing postcondition does NOT undo the resource creation — the resource
	// is already registered with the engine. It only fails the current deploy.
	hclTemplate := `
resource "test_resource" "res" {
  field = "%s"

  lifecycle {
    postcondition {
      condition     = self.field == "expected"
      error_message = "Field must be 'expected'."
    }
  }
}
`

	runWithField := func(t *testing.T, value string) (*testutil.MockResourceMonitor, error) {
		t.Helper()
		src := fmt.Appendf(nil, hclTemplate, value)
		p := parser.NewParser()
		config, diags := p.ParseSource("test.hcl", src)
		require.False(t, diags.HasErrors(), diags.Error())

		mock := &testutil.MockResourceMonitor{}
		engine := newTestEngine(t, config, &run.EngineOptions{
			ModuleLoader:    testModuleLoader(t),
			ProjectName:     "test-project",
			StackName:       "dev",
			ResourceMonitor: mock,
			WorkDir:         t.TempDir(),
			RootDir:         t.TempDir(),
			SchemaLoader:    schemaloader.New(t, testSchema()),
		})
		return mock, engine.Run(t.Context())
	}

	t.Run("true condition", func(t *testing.T) {
		t.Parallel()
		mock, err := runWithField(t, "expected")
		require.NoError(t, err)
		require.True(t, hasRegisteredResource(mock, "test:index:Resource"),
			"expected resource to be registered when postcondition passes")
	})

	t.Run("false condition", func(t *testing.T) {
		t.Parallel()
		mock, err := runWithField(t, "wrong")
		require.ErrorContains(t, err, "Field must be 'expected'.")
		// The resource IS registered even though postcondition failed — postconditions
		// run after resource creation and only fail the deploy, they don't undo the
		// resource registration.
		require.True(t, hasRegisteredResource(mock, "test:index:Resource"),
			"resource should still be registered even when postcondition fails")
	})
}

// Provisioners run in-process via Pulumi hooks: AfterCreate for default
// (create-time), BeforeDelete for `when = destroy`. These tests assert
// on the side effects of running local-exec, not on registered resources.

func TestEngine_LocalExecProvisioner(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	marker := tmpDir + "/marker"

	src := fmt.Appendf(nil, `
resource "aws_instance" "web" {
  ami = "ami-12345"

  provisioner "local-exec" {
    command = "echo Hello > %s"
  }
}
`, marker)

	p := parser.NewParser()
	config, diags := p.ParseSource("test.hcl", src)
	require.False(t, diags.HasErrors(), diags.Error())

	mock := &testutil.MockResourceMonitor{}
	engine := newTestEngine(t, config, &run.EngineOptions{
		ModuleLoader:    testModuleLoader(t),
		ProjectName:     "test-project",
		StackName:       "dev",
		ResourceMonitor: mock,
		WorkDir:         t.TempDir(),
		RootDir:         t.TempDir(),
		SchemaLoader: schemaloader.New(t, schema.PackageSpec{
			Name: "aws",
			Resources: map[string]schema.ResourceSpec{
				"aws:index:Instance": {
					InputProperties: map[string]schema.PropertySpec{
						"ami": {TypeSpec: schema.TypeSpec{Type: "string"}},
					},
					ObjectTypeSpec: schema.ObjectTypeSpec{
						Properties: map[string]schema.PropertySpec{
							"ami": {TypeSpec: schema.TypeSpec{Type: "string"}},
						},
					},
				},
			},
		}),
	})

	require.NoError(t, engine.Run(t.Context()))

	got, err := os.ReadFile(marker)
	require.NoError(t, err, "local-exec did not create marker file")
	assert.Equal(t, "Hello\n", string(got))
}

func TestEngine_MultipleProvisioners(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	marker := tmpDir + "/marker"

	src := fmt.Appendf(nil, `
resource "aws_instance" "web" {
  ami = "ami-12345"

  provisioner "local-exec" {
    command = "echo First >> %s"
  }

  provisioner "local-exec" {
    command = "echo Second >> %s"
  }
}
`, marker, marker)

	p := parser.NewParser()
	config, diags := p.ParseSource("test.hcl", src)
	require.False(t, diags.HasErrors(), diags.Error())

	mock := &testutil.MockResourceMonitor{}
	engine := newTestEngine(t, config, &run.EngineOptions{
		ModuleLoader:    testModuleLoader(t),
		ProjectName:     "test-project",
		StackName:       "dev",
		ResourceMonitor: mock,
		WorkDir:         t.TempDir(),
		RootDir:         t.TempDir(),
		SchemaLoader: schemaloader.New(t, schema.PackageSpec{
			Name: "aws",
			Resources: map[string]schema.ResourceSpec{
				"aws:index:Instance": {
					InputProperties: map[string]schema.PropertySpec{
						"ami": {TypeSpec: schema.TypeSpec{Type: "string"}},
					},
					ObjectTypeSpec: schema.ObjectTypeSpec{
						Properties: map[string]schema.PropertySpec{
							"ami": {TypeSpec: schema.TypeSpec{Type: "string"}},
						},
					},
				},
			},
		}),
	})

	require.NoError(t, engine.Run(t.Context()))

	got, err := os.ReadFile(marker)
	require.NoError(t, err)
	assert.Equal(t, "First\nSecond\n", string(got))
}

// An unknown provisioner type is rejected when its hook is bound, before the
// resource is registered — not when the hook would first run at apply.
func TestEngine_UnknownProvisionerType(t *testing.T) {
	t.Parallel()

	src := []byte(`
resource "aws_instance" "web" {
  ami = "ami-12345"

  provisioner "habitat" {
    command = "echo hi"
  }
}
`)

	p := parser.NewParser()
	config, diags := p.ParseSource("test.hcl", src)
	require.False(t, diags.HasErrors(), diags.Error())

	mock := &testutil.MockResourceMonitor{}
	engine := newTestEngine(t, config, &run.EngineOptions{
		ModuleLoader:    testModuleLoader(t),
		ProjectName:     "test-project",
		StackName:       "dev",
		ResourceMonitor: mock,
		WorkDir:         t.TempDir(),
		RootDir:         t.TempDir(),
		SchemaLoader: schemaloader.New(t, schema.PackageSpec{
			Name: "aws",
			Resources: map[string]schema.ResourceSpec{
				"aws:index:Instance": {
					InputProperties: map[string]schema.PropertySpec{
						"ami": {TypeSpec: schema.TypeSpec{Type: "string"}},
					},
					ObjectTypeSpec: schema.ObjectTypeSpec{
						Properties: map[string]schema.PropertySpec{
							"ami": {TypeSpec: schema.TypeSpec{Type: "string"}},
						},
					},
				},
			},
		}),
	})

	err := engine.Run(t.Context())
	require.ErrorContains(t, err, `unsupported provisioner type: "habitat"`)
	require.False(t, hasRegisteredResource(mock, "aws:index:Instance"),
		"resource must not be registered when a provisioner type is unknown")
}

// Create-time provisioners bind per-instance AfterCreate names; destroy-time
// provisioners bind the single constant BeforeDelete name that stays
// registered on every later run, when the instance may be orphaned.
func TestEngine_DestroyProvisionerHookBinding(t *testing.T) {
	t.Parallel()

	src := []byte(`
resource "aws_instance" "web" {
  ami = "ami-12345"

  provisioner "local-exec" {
    command = "true"
  }

  provisioner "local-exec" {
    when    = destroy
    command = "true"
  }
}
`)

	p := parser.NewParser()
	config, diags := p.ParseSource("test.hcl", src)
	require.False(t, diags.HasErrors(), diags.Error())

	mock := &testutil.MockResourceMonitor{}
	engine := newTestEngine(t, config, &run.EngineOptions{
		ModuleLoader:    testModuleLoader(t),
		ProjectName:     "test-project",
		StackName:       "dev",
		ResourceMonitor: mock,
		WorkDir:         t.TempDir(),
		RootDir:         t.TempDir(),
		SchemaLoader: schemaloader.New(t, schema.PackageSpec{
			Name: "aws",
			Resources: map[string]schema.ResourceSpec{
				"aws:index:Instance": {
					InputProperties: map[string]schema.PropertySpec{
						"ami": {TypeSpec: schema.TypeSpec{Type: "string"}},
					},
					ObjectTypeSpec: schema.ObjectTypeSpec{
						Properties: map[string]schema.PropertySpec{
							"ami": {TypeSpec: schema.TypeSpec{Type: "string"}},
						},
					},
				},
			},
		}),
	})

	require.NoError(t, engine.Run(t.Context()))

	var instance *run.RegisterResourceRequest
	for i, req := range mock.RegisteredResources {
		if req.Type == "aws:index:Instance" {
			instance = &mock.RegisteredResources[i]
		}
	}
	require.NotNil(t, instance)
	assert.Equal(t, &run.ResourceHookBinding{
		AfterCreate:  []string{"aws_instance.web:provisioner:0"},
		BeforeDelete: []string{"pulumi-hcl:provisioner:before-delete"},
	}, instance.Hooks)
}

// TestEngine_ProvisionerReference covers a provisioner whose command
// interpolates another resource's outputs: the referent must be created
// first, and its created value must reach the command.
func TestEngine_ProvisionerReference(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	marker := tmpDir + "/marker"

	src := fmt.Appendf(nil, `
resource "aws_instance" "upstream" {
  ami = "ami-upstream"
}

resource "aws_instance" "dependent" {
  ami = "ami-12345"

  provisioner "local-exec" {
    command = "echo ${aws_instance.upstream.ami} > %s"
  }
}
`, marker)

	p := parser.NewParser()
	config, diags := p.ParseSource("test.hcl", src)
	require.False(t, diags.HasErrors(), diags.Error())

	mock := &testutil.MockResourceMonitor{}
	engine := newTestEngine(t, config, &run.EngineOptions{
		ModuleLoader:    testModuleLoader(t),
		ProjectName:     "test-project",
		StackName:       "dev",
		ResourceMonitor: mock,
		WorkDir:         t.TempDir(),
		RootDir:         t.TempDir(),
		SchemaLoader: schemaloader.New(t, schema.PackageSpec{
			Name: "aws",
			Resources: map[string]schema.ResourceSpec{
				"aws:index:Instance": {
					InputProperties: map[string]schema.PropertySpec{
						"ami": {TypeSpec: schema.TypeSpec{Type: "string"}},
					},
					ObjectTypeSpec: schema.ObjectTypeSpec{
						Properties: map[string]schema.PropertySpec{
							"ami": {TypeSpec: schema.TypeSpec{Type: "string"}},
						},
					},
				},
			},
		}),
	})

	require.NoError(t, engine.Run(t.Context()))

	got, err := os.ReadFile(marker)
	require.NoError(t, err, "local-exec did not create marker file")
	assert.Equal(t, "ami-upstream\n", string(got))

	require.Len(t, mock.RegisteredResources, 3)
	assert.Equal(t, "upstream", mock.RegisteredResources[1].Name)
	assert.Equal(t, "dependent", mock.RegisteredResources[2].Name)
}

func TestEngine_ProvisionerWithSelf(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	marker := tmpDir + "/marker"

	src := fmt.Appendf(nil, `
resource "aws_instance" "web" {
  ami = "ami-12345"

  provisioner "local-exec" {
    command = "echo ${self.id} > %s"
  }
}
`, marker)

	p := parser.NewParser()
	config, diags := p.ParseSource("test.hcl", src)
	require.False(t, diags.HasErrors(), diags.Error())

	mock := &testutil.MockResourceMonitor{}
	engine := newTestEngine(t, config, &run.EngineOptions{
		ModuleLoader:    testModuleLoader(t),
		ProjectName:     "test-project",
		StackName:       "dev",
		ResourceMonitor: mock,
		WorkDir:         t.TempDir(),
		RootDir:         t.TempDir(),
		SchemaLoader: schemaloader.New(t, schema.PackageSpec{
			Name: "aws",
			Resources: map[string]schema.ResourceSpec{
				"aws:index:Instance": {
					InputProperties: map[string]schema.PropertySpec{
						"ami": {TypeSpec: schema.TypeSpec{Type: "string"}},
					},
					ObjectTypeSpec: schema.ObjectTypeSpec{
						Properties: map[string]schema.PropertySpec{
							"ami": {TypeSpec: schema.TypeSpec{Type: "string"}},
						},
					},
				},
			},
		}),
	})

	require.NoError(t, engine.Run(t.Context()))

	got, err := os.ReadFile(marker)
	require.NoError(t, err)
	assert.Equal(t, "web-id\n", string(got))
}

// A module consumed through `pulumi package add hcl module <source>` is unpacked
// from its bundle into numbered directories, so the resolved source path carries
// no name. The component type token must come from the declared source address.
//
// https://github.com/pulumi/pulumi-hcl/issues/451
func TestEngine_ComponentTypeFromDeclaredSource(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	unpackDir := filepath.Join(tmpDir, "pulumi-hcl-module-504221242", "15")
	require.NoError(t, os.MkdirAll(unpackDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(unpackDir, "main.tf"), []byte(`
output "id" {
  value = "net"
}
`), 0o644))

	rootDir := filepath.Join(tmpDir, "root")
	require.NoError(t, os.MkdirAll(rootDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(rootDir, "main.tf"), []byte(`
module "net" {
  source = "../vendor/acme/aws/modules/networking"
}
`), 0o644))

	p := parser.NewParser()
	config, diags := p.ParseDirectory(rootDir)
	require.False(t, diags.HasErrors(), "%s", diags.Error())

	mock := &testutil.MockResourceMonitor{}
	engine := newTestEngine(t, config, &run.EngineOptions{
		// Stands in for the bundle resolver: every source resolves to a
		// numbered directory under the unpacked bundle.
		ModuleLoader: modules.NewLoader(func(source, version, callerDir string) (string, string, error) {
			return unpackDir, "", nil
		}),
		ProjectName:     "test-project",
		StackName:       "dev",
		ResourceMonitor: mock,
		WorkDir:         rootDir,
		RootDir:         rootDir,
		SchemaLoader:    schemaloader.New(t),
	})

	require.NoError(t, engine.Run(t.Context()))

	var componentTypes []string
	for _, r := range mock.RegisteredResources {
		if strings.HasPrefix(r.Type, "components:index:") {
			componentTypes = append(componentTypes, r.Type)
		}
	}
	assert.Equal(t, []string{"components:index:Networking"}, componentTypes)
}

func TestEngine_SimpleModule(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()

	// Create module directory
	moduleDir := tmpDir + "/modules/vpc"
	if err := os.MkdirAll(moduleDir, 0o755); err != nil {
		t.Fatalf("creating module dir: %v", err)
	}

	// Write module files
	moduleMain := `
variable "name" {
  type = string
}

variable "cidr" {
  type    = string
  default = "10.0.0.0/16"
}

resource "aws_vpc" "main" {
  cidr_block = var.cidr
  tags = {
    Name = var.name
  }
}

output "vpc_id" {
  value = aws_vpc.main.id
}

output "cidr_block" {
  value = var.cidr
}
`
	if err := os.WriteFile(moduleDir+"/main.tf", []byte(moduleMain), 0o644); err != nil {
		t.Fatalf("writing module file: %v", err)
	}

	// Write root configuration
	rootMain := `
module "vpc" {
  source = "./modules/vpc"
  name   = "my-vpc"
}

output "vpc_id" {
  value = module.vpc.vpc_id
}
`
	if err := os.WriteFile(tmpDir+"/main.tf", []byte(rootMain), 0o644); err != nil {
		t.Fatalf("writing root file: %v", err)
	}

	// Parse the root configuration
	p := parser.NewParser()
	config, diags := p.ParseDirectory(tmpDir)
	if diags.HasErrors() {
		t.Fatalf("parse error: %s", diags.Error())
	}

	mock := &testutil.MockResourceMonitor{}
	engine := newTestEngine(t, config, &run.EngineOptions{
		ModuleLoader:    testLiveModuleLoader(t),
		ProjectName:     "test-project",
		StackName:       "dev",
		ResourceMonitor: mock,
		WorkDir:         tmpDir,
		RootDir:         tmpDir,
		SchemaLoader: schemaloader.New(t, schema.PackageSpec{
			Name: "aws",
			Resources: map[string]schema.ResourceSpec{
				"aws:index:Vpc": {
					InputProperties: map[string]schema.PropertySpec{
						"cidrBlock": {TypeSpec: schema.TypeSpec{Type: "string"}},
						"tags": {TypeSpec: schema.TypeSpec{
							Type:                 "object",
							AdditionalProperties: &schema.TypeSpec{Type: "string"},
						}},
					},
					ObjectTypeSpec: schema.ObjectTypeSpec{
						Properties: map[string]schema.PropertySpec{
							"cidrBlock": {TypeSpec: schema.TypeSpec{Type: "string"}},
							"tags": {TypeSpec: schema.TypeSpec{
								Type:                 "object",
								AdditionalProperties: &schema.TypeSpec{Type: "string"},
							}},
						},
					},
				},
			},
		}),
	})

	err := engine.Run(t.Context())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should have registered: stack + module component + vpc resource
	if len(mock.RegisteredResources) < 3 {
		t.Fatalf("expected at least 3 registered resources, got %d", len(mock.RegisteredResources))
	}

	// Find the module component (type: components:index:{TypeName})
	var moduleComponent *run.RegisterResourceRequest
	for i := range mock.RegisteredResources {
		if strings.HasPrefix(mock.RegisteredResources[i].Type, "components:index:") {
			moduleComponent = &mock.RegisteredResources[i]
			break
		}
	}

	require.NotNil(t, moduleComponent, "expected module component to be registered")

	// Verify the component type token format
	expectedType := "components:index:Vpc"
	if moduleComponent.Type != expectedType {
		t.Errorf("expected module type %q, got %q", expectedType, moduleComponent.Type)
	}

	// Check that the module name is the logical module name (without "module." prefix)
	assert.Equal(t, "vpc", moduleComponent.Name)

	// Find the VPC resource
	var vpcResource *run.RegisterResourceRequest
	for i := range mock.RegisteredResources {
		if mock.RegisteredResources[i].Type == "aws:index:Vpc" {
			vpcResource = &mock.RegisteredResources[i]
			break
		}
	}

	require.NotNil(t, vpcResource, "expected aws:index:Vpc resource to be registered")

	// Check that the VPC has the correct cidr_block
	if cidr, ok := vpcResource.Inputs.GetOk("cidrBlock"); ok {
		if cidr.AsString() != "10.0.0.0/16" {
			t.Errorf("expected cidr_block '10.0.0.0/16', got %s", cidr.AsString())
		}
	} else {
		t.Error("expected 'cidr_block' input to be set")
	}
}

// TestEngine_AbsolutePaths verifies that with EngineOptions.AbsolutePaths set —
// the Construct entry points, where the module tree lives outside the Pulumi
// program — path.module and path.root evaluate to absolute directories in the
// root module and in child modules.
func TestEngine_AbsolutePaths(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()

	moduleDir := filepath.Join(tmpDir, "modules", "child")
	require.NoError(t, os.MkdirAll(moduleDir, 0o755))

	moduleMain := `
resource "aws_vpc" "inner" {
  module_path = path.module
  root_path   = path.root
}
`
	require.NoError(t, os.WriteFile(filepath.Join(moduleDir, "main.tf"), []byte(moduleMain), 0o644))

	rootMain := `
resource "aws_vpc" "main" {
  module_path = path.module
  root_path   = path.root
}

module "child" {
  source = "./modules/child"
}
`
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "main.tf"), []byte(rootMain), 0o644))

	p := parser.NewParser()
	config, diags := p.ParseDirectory(tmpDir)
	require.False(t, diags.HasErrors(), "parse error: %s", diags.Error())

	pathProperties := map[string]schema.PropertySpec{
		"modulePath": {TypeSpec: schema.TypeSpec{Type: "string"}},
		"rootPath":   {TypeSpec: schema.TypeSpec{Type: "string"}},
	}
	mock := &testutil.MockResourceMonitor{}
	engine := newTestEngine(t, config, &run.EngineOptions{
		ModuleLoader:    testLiveModuleLoader(t),
		ProjectName:     "test-project",
		StackName:       "dev",
		ResourceMonitor: mock,
		WorkDir:         tmpDir,
		RootDir:         tmpDir,
		AbsolutePaths:   true,
		SchemaLoader: schemaloader.New(t, schema.PackageSpec{
			Name: "aws",
			Resources: map[string]schema.ResourceSpec{
				"aws:index:Vpc": {
					InputProperties: pathProperties,
					ObjectTypeSpec:  schema.ObjectTypeSpec{Properties: pathProperties},
				},
			},
		}),
	})

	require.NoError(t, engine.Run(t.Context()))

	inputsByName := make(map[string]property.Map)
	for _, req := range mock.RegisteredResources {
		if req.Type == "aws:index:Vpc" {
			inputsByName[req.Name] = req.Inputs
		}
	}

	assert.Equal(t, map[string]property.Map{
		"main": property.NewMap(map[string]property.Value{
			"modulePath": property.New(tmpDir),
			"rootPath":   property.New(tmpDir),
		}),
		"child.inner": property.NewMap(map[string]property.Value{
			"modulePath": property.New(moduleDir),
			"rootPath":   property.New(tmpDir),
		}),
	}, inputsByName)
}

// TestEngine_ModuleNameWithDot verifies that module names containing a "."
// are preserved verbatim when computing the component's logical resource name,
// rather than being split apart by dot-based key parsing.
func TestEngine_ModuleNameWithDot(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()

	moduleDir := tmpDir + "/modules/vpc"
	require.NoError(t, os.MkdirAll(moduleDir, 0o755))

	moduleMain := `
resource "aws_vpc" "main" {
  cidr_block = "10.0.0.0/16"
}
`
	require.NoError(t, os.WriteFile(moduleDir+"/main.tf", []byte(moduleMain), 0o644))

	rootMain := `
module "vpc.primary" {
  source = "./modules/vpc"
}
`
	require.NoError(t, os.WriteFile(tmpDir+"/main.tf", []byte(rootMain), 0o644))

	p := parser.NewParser()
	config, diags := p.ParseDirectory(tmpDir)
	require.False(t, diags.HasErrors(), "parse error: %s", diags.Error())

	mock := &testutil.MockResourceMonitor{}
	engine := newTestEngine(t, config, &run.EngineOptions{
		ModuleLoader:    testLiveModuleLoader(t),
		ProjectName:     "test-project",
		StackName:       "dev",
		ResourceMonitor: mock,
		WorkDir:         tmpDir,
		RootDir:         tmpDir,
		SchemaLoader: schemaloader.New(t, schema.PackageSpec{
			Name: "aws",
			Resources: map[string]schema.ResourceSpec{
				"aws:index:Vpc": {
					InputProperties: map[string]schema.PropertySpec{
						"cidrBlock": {TypeSpec: schema.TypeSpec{Type: "string"}},
					},
					ObjectTypeSpec: schema.ObjectTypeSpec{
						Properties: map[string]schema.PropertySpec{
							"cidrBlock": {TypeSpec: schema.TypeSpec{Type: "string"}},
						},
					},
				},
			},
		}),
	})

	require.NoError(t, engine.Run(t.Context()))

	var moduleComponent *run.RegisterResourceRequest
	for i := range mock.RegisteredResources {
		if strings.HasPrefix(mock.RegisteredResources[i].Type, "components:index:") {
			moduleComponent = &mock.RegisteredResources[i]
			break
		}
	}
	require.NotNil(t, moduleComponent, "expected module component to be registered")

	assert.Equal(t, "vpc.primary", moduleComponent.Name)

	var vpcResource *run.RegisterResourceRequest
	for i := range mock.RegisteredResources {
		if mock.RegisteredResources[i].Type == "aws:index:Vpc" {
			vpcResource = &mock.RegisteredResources[i]
			break
		}
	}
	require.NotNil(t, vpcResource, "expected aws:index:Vpc resource to be registered")
	assert.Equal(t, "vpc.primary.main", vpcResource.Name)
}

// TestEngine_ResourceTypeWithDot verifies that a resource whose HCL type label
// contains dots (e.g. "kubernetes_networking.k8s.io_v1_ingress", resolving to
// "kubernetes:networking.k8s.io/v1:Ingress") still produces a Pulumi resource
// name equal to the second block label, rather than leaking part of the type.
// Regression test for #140.
func TestEngine_ResourceTypeWithDot(t *testing.T) {
	t.Parallel()

	src := []byte(`
resource "kubernetes_networking.k8s.io_v1_ingress" "ingress" {
  metadata = {
    name = "minimal-ingress"
  }
}
`)

	p := parser.NewParser()
	config, diags := p.ParseSource("test.hcl", src)
	require.False(t, diags.HasErrors(), "parse error: %s", diags.Error())

	mock := &testutil.MockResourceMonitor{}
	engine := newTestEngine(t, config, &run.EngineOptions{
		ModuleLoader:    testModuleLoader(t),
		ProjectName:     "test-project",
		StackName:       "dev",
		ResourceMonitor: mock,
		WorkDir:         t.TempDir(),
		RootDir:         t.TempDir(),
		SchemaLoader: schemaloader.New(t, schema.PackageSpec{
			Name: "kubernetes",
			Resources: map[string]schema.ResourceSpec{
				"kubernetes:networking.k8s.io/v1:Ingress": {
					InputProperties: map[string]schema.PropertySpec{
						"metadata": {TypeSpec: schema.TypeSpec{Ref: "#/types/kubernetes:meta/v1:ObjectMeta"}},
					},
					ObjectTypeSpec: schema.ObjectTypeSpec{
						Properties: map[string]schema.PropertySpec{
							"metadata": {TypeSpec: schema.TypeSpec{Ref: "#/types/kubernetes:meta/v1:ObjectMeta"}},
						},
					},
				},
			},
			Types: map[string]schema.ComplexTypeSpec{
				"kubernetes:meta/v1:ObjectMeta": {
					ObjectTypeSpec: schema.ObjectTypeSpec{
						Type: "object",
						Properties: map[string]schema.PropertySpec{
							"name": {TypeSpec: schema.TypeSpec{Type: "string"}},
						},
					},
				},
			},
		}),
	})

	require.NoError(t, engine.Run(t.Context()))

	var ingress *run.RegisterResourceRequest
	for i := range mock.RegisteredResources {
		if mock.RegisteredResources[i].Type == "kubernetes:networking.k8s.io/v1:Ingress" {
			ingress = &mock.RegisteredResources[i]
			break
		}
	}
	require.NotNil(t, ingress, "expected kubernetes:networking.k8s.io/v1:Ingress resource to be registered")
	assert.Equal(t, "ingress", ingress.Name)
}

// dottedModuleSchema models the pulumi/kubernetes types from #646, whose
// modules contain dots.
func dottedModuleSchema(t *testing.T) schema.ReferenceLoader {
	str := schema.PropertySpec{TypeSpec: schema.TypeSpec{Type: "string"}}
	props := func(names ...string) map[string]schema.PropertySpec {
		m := map[string]schema.PropertySpec{}
		for _, n := range names {
			m[n] = str
		}
		return m
	}
	resource := func(names ...string) schema.ResourceSpec {
		return schema.ResourceSpec{
			InputProperties: props(names...),
			ObjectTypeSpec:  schema.ObjectTypeSpec{Properties: props(names...)},
		}
	}
	return schemaloader.New(t, schema.PackageSpec{
		Name:    "kubernetes",
		Version: "4.30.0",
		Resources: map[string]schema.ResourceSpec{
			"kubernetes:helm.sh/v3:Release":           resource("chart"),
			"kubernetes:helm.sh/v4:Chart":             resource("chart", "resources"),
			"kubernetes:networking.k8s.io/v1:Ingress": resource("host"),
		},
		Functions: map[string]schema.FunctionSpec{
			"kubernetes:helm.sh/v3:getRelease": {
				Inputs:  &schema.ObjectTypeSpec{Properties: props("name")},
				Outputs: &schema.ObjectTypeSpec{Properties: props("id")},
			},
		},
	})
}

// runDottedModuleProgram runs src against dottedModuleSchema.
func runDottedModuleProgram(t *testing.T, src string) (*testutil.MockResourceMonitor, error) {
	t.Helper()
	config, diags := parser.NewParser().ParseSource("main.tf", []byte(src))
	require.False(t, diags.HasErrors(), "parse error: %s", diags.Error())
	mock := &testutil.MockResourceMonitor{}
	engine := newTestEngine(t, config, &run.EngineOptions{
		ModuleLoader:    testModuleLoader(t),
		ProjectName:     "test-project",
		StackName:       "dev",
		ResourceMonitor: mock,
		WorkDir:         t.TempDir(),
		RootDir:         t.TempDir(),
		SchemaLoader:    dottedModuleSchema(t),
	})
	return mock, engine.Run(t.Context())
}

// TestEngine_DottedModuleTypes covers Pulumi types whose module contains a dot,
// such as "kubernetes:helm.sh/v3:Release". They are written with underscores,
// "kubernetes_helm_sh_v3_release", so they can be referenced; the legacy dotted
// spelling still declares the same type, silently. Regression test for #646.
func TestEngine_DottedModuleTypes(t *testing.T) {
	t.Parallel()

	// The header shared by the programs in #646.
	const header = `terraform {
  required_providers {
    kubernetes = {
      source  = "pulumi/kubernetes"
      version = "4.30.0"
    }
  }
}
`
	const releaseURN = "urn:pulumi:test::project::kubernetes:helm.sh/v3:Release::"

	tests := []struct {
		name        string
		src         string
		wantErr     string              // substring; "" means Run succeeds
		wantTypes   map[string]string   // resource name -> registered type
		wantDeps    map[string][]string // resource name -> Dependencies
		wantOutputs map[string]string
	}{
		{
			name: "#646 dotted reference",
			src: header + `
resource "kubernetes_helm.sh_v4_chart" "nginx" {
  chart = "oci://registry-1.docker.io/bitnamicharts/nginx"
}

output "chart_resources" {
  value = kubernetes_helm.sh_v4_chart.nginx.resources
}
`,
			// A reference splits at every dot, so it can never reach the
			// dotted type; only the underscore spelling can.
			wantErr: `unknown node "kubernetes_helm.sh_v4_chart"`,
		},
		{
			name: "#646 dotted declaration",
			src: header + `
resource "kubernetes_helm.sh_v4_chart" "nginx" {
  chart = "oci://registry-1.docker.io/bitnamicharts/nginx"
}
`,
			wantTypes: map[string]string{"nginx": "kubernetes:helm.sh/v4:Chart"},
		},
		{
			name: "#646 underscore spelling",
			src: header + `
resource "kubernetes_helm_sh_v4_chart" "nginx" {
  chart = "oci://registry-1.docker.io/bitnamicharts/nginx"
}

output "chart_resources" {
  value = kubernetes_helm_sh_v4_chart.nginx.resources
}
`,
			wantTypes: map[string]string{"nginx": "kubernetes:helm.sh/v4:Chart"},
		},
		{
			name: "underscore spelling references",
			src: `
resource "kubernetes_helm_sh_v3_release" "crds" {
  count = 1
  chart = "crds"
}

resource "kubernetes_helm_sh_v3_release" "app" {
  chart      = "app-${kubernetes_helm_sh_v3_release.crds[0].chart}"
  depends_on = [kubernetes_helm_sh_v3_release.crds]
}

resource "kubernetes_networking_k8s_io_v1_ingress" "web" {
  for_each = toset(["a"])
  host     = "${each.key}.${kubernetes_helm_sh_v3_release.app.chart}"
}

data "kubernetes_helm_sh_v3_release" "info" {
  name = kubernetes_helm_sh_v3_release.app.chart
}

output "app"  { value = kubernetes_helm_sh_v3_release.app.chart }
output "host" { value = kubernetes_networking_k8s_io_v1_ingress.web["a"].host }
output "info" { value = data.kubernetes_helm_sh_v3_release.info.id }
`,
			wantTypes: map[string]string{
				"crds[0]":  "kubernetes:helm.sh/v3:Release",
				"app":      "kubernetes:helm.sh/v3:Release",
				`web["a"]`: "kubernetes:networking.k8s.io/v1:Ingress",
			},
			wantDeps: map[string][]string{
				"app":      {releaseURN + "crds[0]"},
				`web["a"]`: {releaseURN + "app"},
			},
			wantOutputs: map[string]string{"app": "app-crds", "host": "a.app-crds", "info": "mock-id"},
		},
		{
			name: "dotted declaration, underscore reference",
			src: `
resource "kubernetes_helm.sh_v3_release" "crds" {
  chart = "crds"
}

output "chart" { value = kubernetes_helm_sh_v3_release.crds.chart }
`,
			wantErr: `unknown node "kubernetes_helm_sh_v3_release.crds"`,
		},
		{
			// Unresolvable labels get the unknown-type error's suggestion.
			name: "dotted typo",
			src: `
resource "kubernetes_helm.sh_v3_relase" "crds" {
  chart = "crds"
}
`,
			wantErr: `unknown resource type "kubernetes_helm.sh_v3_relase"; did you mean "kubernetes_helm_sh_v3_release"?`,
		},
		{
			name: "dotted data source",
			src: `
data "kubernetes_helm.sh_v3_release" "info" {
  name = "crds"
}
`,
			wantTypes: map[string]string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			mock, err := runDottedModuleProgram(t, tt.src)
			assert.Empty(t, mock.Warnings, "dotted labels resolve silently")
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)

			gotTypes, gotDeps := map[string]string{}, map[string][]string{}
			for _, r := range mock.RegisteredResources {
				if !r.Custom {
					continue
				}
				gotTypes[r.Name] = r.Type
				if len(r.Dependencies) > 0 {
					gotDeps[r.Name] = r.Dependencies
				}
			}
			assert.Equal(t, tt.wantTypes, gotTypes)
			if tt.wantDeps != nil {
				assert.Equal(t, tt.wantDeps, gotDeps)
			}
			for k, v := range tt.wantOutputs {
				assert.Equal(t, property.New(v), mock.StackOutputs.Get(k), "output %q", k)
			}
		})
	}
}

// TestEngine_DottedModuleTypeModulesAndChecks checks that a legacy dotted type
// label resolves silently through both a module source that several module
// blocks inline and a check-scoped data source. Regression test for #646.
func TestEngine_DottedModuleTypeModulesAndChecks(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeFile := func(name, content string) {
		path := filepath.Join(dir, name)
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	}
	writeFile("modules/child/main.tf", `resource "kubernetes_helm.sh_v3_release" "crds" {
  chart = "crds"
}
`)
	writeFile("main.tf", `module "a" {
  source = "./modules/child"
}

module "b" {
  source = "./modules/child"
}

check "c" {
  data "kubernetes_helm.sh_v3_release" "info" {
    name = "x"
  }
  assert {
    condition     = true
    error_message = "unreachable"
  }
}
`)

	config, diags := parser.NewParser().ParseDirectory(dir)
	require.False(t, diags.HasErrors(), "parse error: %s", diags.Error())
	mock := &testutil.MockResourceMonitor{}
	engine := newTestEngine(t, config, &run.EngineOptions{
		ModuleLoader:    testLiveModuleLoader(t),
		ProjectName:     "test-project",
		StackName:       "dev",
		ResourceMonitor: mock,
		WorkDir:         dir,
		RootDir:         dir,
		SchemaLoader:    dottedModuleSchema(t),
	})
	require.NoError(t, engine.Run(t.Context()))

	var releases int
	for _, r := range mock.RegisteredResources {
		if r.Type == "kubernetes:helm.sh/v3:Release" {
			releases++
		}
	}
	assert.Equal(t, 2, releases)
	assert.Empty(t, mock.Warnings, "dotted labels resolve silently")
}

// TestEngine_DottedModuleTypeURN checks that switching a type from its dotted
// spelling to its underscore spelling registers the same resources, so their
// URNs do not change.
func TestEngine_DottedModuleTypeURN(t *testing.T) {
	t.Parallel()

	program := func(typ string) string {
		return fmt.Sprintf(`
provider "kubernetes" {}

resource %q "crds" {
  count = 2
  chart = "crds-${count.index}"
}
`, typ)
	}
	// registered drops the fields that legitimately differ between runs, such
	// as hook names, which embed the HCL type.
	type registered struct {
		URN, Parent, Provider string
		Aliases               []run.Alias
	}
	register := func(typ string) []registered {
		mock, err := runDottedModuleProgram(t, program(typ))
		require.NoError(t, err)
		var out []registered
		for _, r := range mock.RegisteredResources {
			if !r.Custom {
				continue
			}
			urn, _ := mock.ResolveURN(r.Parent, r.Type, r.Name)
			out = append(out, registered{string(urn), string(r.Parent), r.Provider, r.Aliases})
		}
		sort.Slice(out, func(i, j int) bool { return out[i].URN < out[j].URN })
		return out
	}

	dotted := register("kubernetes_helm.sh_v3_release")
	underscore := register("kubernetes_helm_sh_v3_release")
	require.Len(t, underscore, 3) // two releases and the provider
	assert.Equal(t, "urn:pulumi:test::project::kubernetes:helm.sh/v3:Release::crds[0]", underscore[0].URN)
	assert.NotEmpty(t, underscore[0].Provider)
	assert.Equal(t, dotted, underscore)
}

// runSensitiveMetaArgTest is a shared driver for the four "sensitive value
// rejected by count/for_each" tests. It writes the given root.hcl into a
// temp dir, runs the engine, and returns the resulting error.
func runSensitiveMetaArgTest(t *testing.T, rootMain string) error {
	t.Helper()
	tmpDir := t.TempDir()

	moduleDir := tmpDir + "/modules/m"
	require.NoError(t, os.MkdirAll(moduleDir, 0o755))
	require.NoError(t, os.WriteFile(moduleDir+"/main.tf", []byte(`
output "name" { value = "leaf" }
`), 0o644))
	require.NoError(t, os.WriteFile(tmpDir+"/main.tf", []byte(rootMain), 0o644))

	p := parser.NewParser()
	config, diags := p.ParseDirectory(tmpDir)
	require.False(t, diags.HasErrors(), "parse error: %s", diags.Error())

	mock := &testutil.MockResourceMonitor{}
	engine := newTestEngine(t, config, &run.EngineOptions{
		ModuleLoader:    testLiveModuleLoader(t),
		ProjectName:     "test-project",
		StackName:       "dev",
		ResourceMonitor: mock,
		WorkDir:         tmpDir,
		RootDir:         tmpDir,
		SchemaLoader: schemaloader.New(t, schema.PackageSpec{
			Name: "aws",
			Resources: map[string]schema.ResourceSpec{
				"aws:index:Vpc": {
					InputProperties: map[string]schema.PropertySpec{
						"cidrBlock": {TypeSpec: schema.TypeSpec{Type: "string"}},
					},
					ObjectTypeSpec: schema.ObjectTypeSpec{
						Properties: map[string]schema.PropertySpec{
							"cidrBlock": {TypeSpec: schema.TypeSpec{Type: "string"}},
						},
					},
				},
			},
		}),
	})

	return engine.Run(t.Context())
}

// assertSensitiveArgumentError asserts that err is the specific diagnostic
// emitted when a sensitive value is supplied to a meta-argument.
func assertSensitiveArgumentError(t *testing.T, argName string, err error) {
	t.Helper()
	require.Error(t, err)
	msg := err.Error()
	assert.Contains(t, msg, "Invalid "+argName+" argument")
	assert.Contains(t, msg,
		"Sensitive values, or values derived from sensitive values, "+
			"cannot be used as "+argName+" arguments.")
}

// TestEngine_ModuleForEachSensitive verifies that supplying a sensitive
// value to a module's `for_each` produces a clean Terraform-style error
// rather than expanding the module with a leaked secret in the URN.
func TestEngine_ModuleForEachSensitive(t *testing.T) {
	t.Parallel()

	err := runSensitiveMetaArgTest(t, `
module "m" {
  source   = "./modules/m"
  for_each = sensitive(toset(["a", "b"]))
}
`)
	assertSensitiveArgumentError(t, "for_each", err)
}

// TestEngine_ModuleCountSensitive verifies the same behavior for module
// `count`.
func TestEngine_ModuleCountSensitive(t *testing.T) {
	t.Parallel()

	err := runSensitiveMetaArgTest(t, `
module "m" {
  source = "./modules/m"
  count  = sensitive(2)
}
`)
	assertSensitiveArgumentError(t, "count", err)
}

// TestEngine_ResourceForEachSensitive verifies the same behavior for a
// resource's `for_each` (handled via [eval.EvaluateForEach]).
func TestEngine_ResourceForEachSensitive(t *testing.T) {
	t.Parallel()

	err := runSensitiveMetaArgTest(t, `
resource "aws_vpc" "v" {
  for_each   = sensitive(toset(["a"]))
  cidr_block = "10.0.0.0/16"
}
`)
	assertSensitiveArgumentError(t, "for_each", err)
}

// TestEngine_ResourceCountSensitive verifies the same behavior for a
// resource's `count`.
func TestEngine_ResourceCountSensitive(t *testing.T) {
	t.Parallel()

	err := runSensitiveMetaArgTest(t, `
resource "aws_vpc" "v" {
  count      = sensitive(1)
  cidr_block = "10.0.0.0/16"
}
`)
	assertSensitiveArgumentError(t, "count", err)
}

// A known count carrying a DepMark (here, from an unknown attribute mixed
// into the count expression) must not panic AsBigFloat.
func TestEngine_ModuleCountMarkedKnown(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	moduleDir := tmpDir + "/modules/m"
	require.NoError(t, os.MkdirAll(moduleDir, 0o755))
	require.NoError(t, os.WriteFile(moduleDir+"/main.tf", []byte(`
output "name" { value = "leaf" }
`), 0o644))
	require.NoError(t, os.WriteFile(tmpDir+"/main.tf", []byte(`
variable "n" {
  type    = number
  default = 1
}

resource "test_resource" "upstream" {
  field = var.n > 0 ? "v" : ""
}

module "m" {
  source = "./modules/m"
  count  = var.n + (test_resource.upstream.id != "" ? 0 : 0)
}
`), 0o644))

	p := parser.NewParser()
	config, diags := p.ParseDirectory(tmpDir)
	require.False(t, diags.HasErrors(), "parse error: %s", diags.Error())

	mock := &testutil.MockResourceMonitor{
		DryRun: true,
		RegisterResourceHandler: func(ctx context.Context, req run.RegisterResourceRequest) (*run.RegisterResourceResponse, error) {
			return &run.RegisterResourceResponse{
				URN:     urn.URN("urn:pulumi:test::project::" + req.Type + "::" + req.Name),
				ID:      "",
				Outputs: req.Inputs,
			}, nil
		},
	}
	engine := newTestEngine(t, config, &run.EngineOptions{
		ModuleLoader:    testLiveModuleLoader(t),
		ProjectName:     "test-project",
		StackName:       "dev",
		ResourceMonitor: mock,
		WorkDir:         tmpDir,
		RootDir:         tmpDir,
		DryRun:          true,
		SchemaLoader:    schemaloader.New(t, testSchema()),
	})
	require.NoError(t, engine.Run(t.Context()))
}

// A nested module call runs once per instance of its count/for_each-expanded
// parent, with each instance evaluating in its own parent instance's scope
// and parented to that instance's component.
func TestEngine_NestedModuleUnderExpandedParent(t *testing.T) {
	t.Parallel()

	deploy := func(t *testing.T, rootMain string) *testutil.MockResourceMonitor {
		tmpDir := t.TempDir()
		require.NoError(t, os.MkdirAll(tmpDir+"/outer/inner", 0o755))
		require.NoError(t, os.WriteFile(tmpDir+"/outer/main.tf", []byte(`
variable "base" {
  type = string
}

module "inner" {
  source = "./inner"
  v      = var.base
}

output "combined" {
  value = module.inner.doubled
}
`), 0o644))
		require.NoError(t, os.WriteFile(tmpDir+"/outer/inner/main.tf", []byte(`
variable "v" {
  type = string
}

output "doubled" {
  value = "${var.v}-${var.v}"
}
`), 0o644))
		require.NoError(t, os.WriteFile(tmpDir+"/main.tf", []byte(rootMain), 0o644))

		p := parser.NewParser()
		config, diags := p.ParseDirectory(tmpDir)
		require.False(t, diags.HasErrors(), "parse error: %s", diags.Error())

		mock := &testutil.MockResourceMonitor{}
		engine := newTestEngine(t, config, &run.EngineOptions{
			ModuleLoader:    testLiveModuleLoader(t),
			ProjectName:     "test-project",
			StackName:       "dev",
			ResourceMonitor: mock,
			WorkDir:         tmpDir,
			RootDir:         tmpDir,
			SchemaLoader:    schemaloader.New(t, testSchema()),
		})
		require.NoError(t, engine.Run(t.Context()))
		return mock
	}

	componentParents := func(mock *testutil.MockResourceMonitor) map[string]urn.URN {
		parents := make(map[string]urn.URN)
		for _, req := range mock.RegisteredResources {
			if strings.HasPrefix(req.Type, "components:index:") {
				parents[req.Name] = req.Parent
			}
		}
		return parents
	}

	stackURN := urn.URN("urn:pulumi:test::project::pulumi:pulumi:Stack::test-project-dev")

	t.Run("count", func(t *testing.T) {
		t.Parallel()

		mock := deploy(t, `
module "outer" {
  source = "./outer"
  count  = 2
  base   = "b${count.index}"
}

output "all" {
  value = [for m in module.outer : m.combined]
}
`)
		assert.Equal(t, property.New([]property.Value{
			property.New("b0-b0"),
			property.New("b1-b1"),
		}), mock.StackOutputs.Get("all"))

		assert.Equal(t, map[string]urn.URN{
			"outer[0]":       stackURN,
			"outer[1]":       stackURN,
			"outer[0].inner": "urn:pulumi:test::project::components:index:Outer::outer[0]",
			"outer[1].inner": "urn:pulumi:test::project::components:index:Outer::outer[1]",
		}, componentParents(mock))
	})

	t.Run("for_each", func(t *testing.T) {
		t.Parallel()

		mock := deploy(t, `
module "outer" {
  source   = "./outer"
  for_each = { x = "b0", y = "b1" }
  base     = each.value
}

output "all" {
  value = { for k, m in module.outer : k => m.combined }
}
`)
		assert.Equal(t, property.New(map[string]property.Value{
			"x": property.New("b0-b0"),
			"y": property.New("b1-b1"),
		}), mock.StackOutputs.Get("all"))

		assert.Equal(t, map[string]urn.URN{
			`outer["x"]`:       stackURN,
			`outer["y"]`:       stackURN,
			`outer["x"].inner`: `urn:pulumi:test::project::components:index:Outer::outer["x"]`,
			`outer["y"].inner`: `urn:pulumi:test::project::components:index:Outer::outer["y"]`,
		}, componentParents(mock))
	})
}

// TestEngine_ModuleOutputRace verifies that concurrent processing of multiple
// module outputs does not trigger a data race on moduleInstance.Outputs.
// This is a regression test for https://github.com/pulumi/pulumi-hcl/issues/60.
func TestEngine_ModuleOutputRace(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()

	// Create a module with many outputs to maximize scheduling contention.
	moduleDir := tmpDir + "/modules/multi"
	if err := os.MkdirAll(moduleDir, 0o755); err != nil {
		t.Fatalf("creating module dir: %v", err)
	}

	moduleMain := `
variable "input" {
  type = string
}

output "out_a" { value = var.input }
output "out_b" { value = var.input }
output "out_c" { value = var.input }
output "out_d" { value = var.input }
output "out_e" { value = var.input }
output "out_f" { value = var.input }
output "out_g" { value = var.input }
output "out_h" { value = var.input }
`
	if err := os.WriteFile(moduleDir+"/main.tf", []byte(moduleMain), 0o644); err != nil {
		t.Fatalf("writing module file: %v", err)
	}

	rootMain := `
module "multi" {
  source = "./modules/multi"
  input  = "hello"
}
`
	if err := os.WriteFile(tmpDir+"/main.tf", []byte(rootMain), 0o644); err != nil {
		t.Fatalf("writing root file: %v", err)
	}

	p := parser.NewParser()
	config, diags := p.ParseDirectory(tmpDir)
	if diags.HasErrors() {
		t.Fatalf("parse error: %s", diags.Error())
	}

	mock := &testutil.MockResourceMonitor{}
	engine := newTestEngine(t, config, &run.EngineOptions{
		ModuleLoader:    testLiveModuleLoader(t),
		ProjectName:     "test-project",
		StackName:       "dev",
		ResourceMonitor: mock,
		WorkDir:         tmpDir,
		RootDir:         tmpDir,
		SchemaLoader:    schemaloader.New(t, schema.PackageSpec{Name: "empty"}),
	})

	// Under -race this would fail before the fix due to concurrent map writes
	// in processModuleOutput.
	if err := engine.Run(t.Context()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestEngine_Timeouts(t *testing.T) {
	t.Parallel()

	src := []byte(`
resource "aws_instance" "web" {
  ami           = "ami-12345"
  instance_type = "t3.micro"

  timeouts {
    create = "60m"
    update = "30m"
    delete = "2h"
  }
}
`)

	p := parser.NewParser()
	config, diags := p.ParseSource("test.hcl", src)
	if diags.HasErrors() {
		t.Fatalf("parse error: %s", diags.Error())
	}

	mock := &testutil.MockResourceMonitor{}
	engine := newTestEngine(t, config, &run.EngineOptions{
		ModuleLoader:    testModuleLoader(t),
		ProjectName:     "test-project",
		StackName:       "dev",
		ResourceMonitor: mock,
		WorkDir:         t.TempDir(),
		RootDir:         t.TempDir(),
		SchemaLoader: schemaloader.New(t, schema.PackageSpec{
			Name: "aws",
			Resources: map[string]schema.ResourceSpec{
				"aws:index:Instance": {
					InputProperties: map[string]schema.PropertySpec{
						"ami":          {TypeSpec: schema.TypeSpec{Type: "string"}},
						"instanceType": {TypeSpec: schema.TypeSpec{Type: "string"}},
					},
					ObjectTypeSpec: schema.ObjectTypeSpec{
						Properties: map[string]schema.PropertySpec{
							"ami":          {TypeSpec: schema.TypeSpec{Type: "string"}},
							"instanceType": {TypeSpec: schema.TypeSpec{Type: "string"}},
						},
					},
				},
			},
		}),
	})

	err := engine.Run(t.Context())
	if err != nil {
		t.Fatalf("run error: %v", err)
	}

	// Find the instance resource
	var instanceReq *run.RegisterResourceRequest
	for i := range mock.RegisteredResources {
		if mock.RegisteredResources[i].Type == "aws:index:Instance" {
			instanceReq = &mock.RegisteredResources[i]
			break
		}
	}

	require.NotNil(t, instanceReq, "expected aws:index:Instance resource to be registered")

	// Check that timeouts were set
	require.NotNil(t, instanceReq.CustomTimeouts, "expected CustomTimeouts to be set")

	// 60m = 3600 seconds
	if instanceReq.CustomTimeouts.Create != 3600 {
		t.Errorf("expected Create timeout 3600, got %f", instanceReq.CustomTimeouts.Create)
	}

	// 30m = 1800 seconds
	if instanceReq.CustomTimeouts.Update != 1800 {
		t.Errorf("expected Update timeout 1800, got %f", instanceReq.CustomTimeouts.Update)
	}

	// 2h = 7200 seconds
	if instanceReq.CustomTimeouts.Delete != 7200 {
		t.Errorf("expected Delete timeout 7200, got %f", instanceReq.CustomTimeouts.Delete)
	}
}

func TestEngine_TimeoutsDefault(t *testing.T) {
	t.Parallel()

	src := []byte(`
resource "aws_instance" "web" {
  ami           = "ami-12345"
  instance_type = "t3.micro"

  timeouts {
    create  = "60m"
    default = "5m"
  }
}
`)

	config, diags := parser.NewParser().ParseSource("test.hcl", src)
	require.False(t, diags.HasErrors(), diags.Error())

	mock := &testutil.MockResourceMonitor{}
	engine := newTestEngine(t, config, &run.EngineOptions{
		ModuleLoader:    testModuleLoader(t),
		ProjectName:     "test-project",
		StackName:       "dev",
		ResourceMonitor: mock,
		WorkDir:         t.TempDir(),
		RootDir:         t.TempDir(),
		SchemaLoader: schemaloader.New(t, schema.PackageSpec{
			Name: "aws",
			Resources: map[string]schema.ResourceSpec{
				"aws:index:Instance": {
					InputProperties: map[string]schema.PropertySpec{
						"ami":          {TypeSpec: schema.TypeSpec{Type: "string"}},
						"instanceType": {TypeSpec: schema.TypeSpec{Type: "string"}},
					},
					ObjectTypeSpec: schema.ObjectTypeSpec{
						Properties: map[string]schema.PropertySpec{
							"ami":          {TypeSpec: schema.TypeSpec{Type: "string"}},
							"instanceType": {TypeSpec: schema.TypeSpec{Type: "string"}},
						},
					},
				},
			},
		}),
	})

	require.NoError(t, engine.Run(t.Context()))

	var instanceReq *run.RegisterResourceRequest
	for i := range mock.RegisteredResources {
		if mock.RegisteredResources[i].Type == "aws:index:Instance" {
			instanceReq = &mock.RegisteredResources[i]
			break
		}
	}

	require.NotNil(t, instanceReq, "expected aws:index:Instance resource to be registered")

	assert.Equal(t, &run.CustomTimeouts{Create: 3600, Read: 300, Update: 300, Delete: 300}, instanceReq.CustomTimeouts)
}

func TestEngine_SensitiveResourceOptions(t *testing.T) {
	t.Parallel()

	src := []byte(`
variable "secret_timeout" {
  type      = string
  sensitive = true
  default   = "10m"
}

variable "secret_version" {
  type      = string
  sensitive = true
  default   = "1.2.3"
}

variable "secret_url" {
  type      = string
  sensitive = true
  default   = "https://example.com/plugins"
}

resource "aws_instance" "web" {
  ami           = "ami-12345"
  instance_type = "t3.micro"

  timeouts {
    create = var.secret_timeout
  }

  pulumi {
    retain_on_delete    = length(var.secret_timeout) > 0
    version             = var.secret_version
    plugin_download_url = var.secret_url
    env_var_mappings    = { AWS_REGION = var.secret_timeout }
  }
}
`)

	p := parser.NewParser()
	config, diags := p.ParseSource("test.hcl", src)
	require.False(t, diags.HasErrors(), "parse error: %s", diags.Error())

	mock := &testutil.MockResourceMonitor{}
	engine := newTestEngine(t, config, &run.EngineOptions{
		ModuleLoader:    testModuleLoader(t),
		ProjectName:     "test-project",
		StackName:       "dev",
		ResourceMonitor: mock,
		WorkDir:         t.TempDir(),
		RootDir:         t.TempDir(),
		SchemaLoader: schemaloader.New(t, schema.PackageSpec{
			Name: "aws",
			Resources: map[string]schema.ResourceSpec{
				"aws:index:Instance": {
					InputProperties: map[string]schema.PropertySpec{
						"ami":          {TypeSpec: schema.TypeSpec{Type: "string"}},
						"instanceType": {TypeSpec: schema.TypeSpec{Type: "string"}},
					},
					ObjectTypeSpec: schema.ObjectTypeSpec{
						Properties: map[string]schema.PropertySpec{
							"ami":          {TypeSpec: schema.TypeSpec{Type: "string"}},
							"instanceType": {TypeSpec: schema.TypeSpec{Type: "string"}},
						},
					},
				},
			},
		}),
	})

	require.NoError(t, engine.Run(t.Context()))

	var instanceReq *run.RegisterResourceRequest
	for i := range mock.RegisteredResources {
		if mock.RegisteredResources[i].Type == "aws:index:Instance" {
			instanceReq = &mock.RegisteredResources[i]
			break
		}
	}
	require.NotNil(t, instanceReq, "expected aws:index:Instance resource to be registered")

	retain := true
	assert.Equal(t, &run.CustomTimeouts{Create: 600}, instanceReq.CustomTimeouts)
	assert.Equal(t, &retain, instanceReq.RetainOnDelete)
	assert.Equal(t, "1.2.3", instanceReq.Version)
	assert.Equal(t, "https://example.com/plugins", instanceReq.PluginDownloadURL)
	assert.Equal(t, map[string]string{"AWS_REGION": "10m"}, instanceReq.EnvVarMappings)
}

func TestEngine_MovedBlock(t *testing.T) {
	t.Parallel()

	src := []byte(`
moved {
  from = aws_instance.old_server
  to   = aws_instance.web
}

resource "aws_instance" "web" {
  ami           = "ami-12345"
  instance_type = "t3.micro"
}
`)

	p := parser.NewParser()
	config, diags := p.ParseSource("test.hcl", src)
	if diags.HasErrors() {
		t.Fatalf("parse error: %s", diags.Error())
	}

	mock := &testutil.MockResourceMonitor{}
	engine := newTestEngine(t, config, &run.EngineOptions{
		ModuleLoader:    testModuleLoader(t),
		ProjectName:     "test-project",
		StackName:       "dev",
		ResourceMonitor: mock,
		WorkDir:         t.TempDir(),
		RootDir:         t.TempDir(),
		SchemaLoader: schemaloader.New(t, schema.PackageSpec{
			Name: "aws",
			Resources: map[string]schema.ResourceSpec{
				"aws:index:Instance": {
					InputProperties: map[string]schema.PropertySpec{
						"ami":          {TypeSpec: schema.TypeSpec{Type: "string"}},
						"instanceType": {TypeSpec: schema.TypeSpec{Type: "string"}},
					},
					ObjectTypeSpec: schema.ObjectTypeSpec{
						Properties: map[string]schema.PropertySpec{
							"ami":          {TypeSpec: schema.TypeSpec{Type: "string"}},
							"instanceType": {TypeSpec: schema.TypeSpec{Type: "string"}},
						},
					},
				},
			},
		}),
	})

	err := engine.Run(t.Context())
	if err != nil {
		t.Fatalf("run error: %v", err)
	}

	// Find the instance resource
	var instanceReq *run.RegisterResourceRequest
	for i := range mock.RegisteredResources {
		if mock.RegisteredResources[i].Type == "aws:index:Instance" {
			instanceReq = &mock.RegisteredResources[i]
			break
		}
	}

	require.NotNil(t, instanceReq, "expected aws:index:Instance resource to be registered")

	// The alias names the resource by the bare name it had under the old
	// address ("old_server"), not the full "type.name" address, so the engine
	// recognizes the rename as a move rather than replacing the resource.
	if len(instanceReq.Aliases) == 0 {
		t.Fatal("expected Aliases to contain the moved 'from' name")
	}

	found := false
	for _, alias := range instanceReq.Aliases {
		if alias.Spec != nil && alias.Spec.Name == "old_server" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected alias with name 'old_server', got %v", instanceReq.Aliases)
	}
}

// TestEngine_MovedBlockDisableCount moves a counted instance back to a bare
// resource (`aws_instance.web[0]` -> `aws_instance.web`), so the alias must
// carry the prior instance key from the keyed `from` address.
func TestEngine_MovedBlockDisableCount(t *testing.T) {
	t.Parallel()

	src := []byte(`
moved {
  from = aws_instance.web[0]
  to   = aws_instance.web
}

resource "aws_instance" "web" {
  ami           = "ami-12345"
  instance_type = "t3.micro"
}
`)

	p := parser.NewParser()
	config, diags := p.ParseSource("test.hcl", src)
	require.False(t, diags.HasErrors(), "parse error: %s", diags.Error())

	mock := &testutil.MockResourceMonitor{}
	engine := newTestEngine(t, config, &run.EngineOptions{
		ModuleLoader:    testModuleLoader(t),
		ProjectName:     "test-project",
		StackName:       "dev",
		ResourceMonitor: mock,
		WorkDir:         t.TempDir(),
		RootDir:         t.TempDir(),
		SchemaLoader: schemaloader.New(t, schema.PackageSpec{
			Name: "aws",
			Resources: map[string]schema.ResourceSpec{
				"aws:index:Instance": {
					InputProperties: map[string]schema.PropertySpec{
						"ami":          {TypeSpec: schema.TypeSpec{Type: "string"}},
						"instanceType": {TypeSpec: schema.TypeSpec{Type: "string"}},
					},
					ObjectTypeSpec: schema.ObjectTypeSpec{
						Properties: map[string]schema.PropertySpec{
							"ami":          {TypeSpec: schema.TypeSpec{Type: "string"}},
							"instanceType": {TypeSpec: schema.TypeSpec{Type: "string"}},
						},
					},
				},
			},
		}),
	})

	require.NoError(t, engine.Run(t.Context()))

	var instanceReq *run.RegisterResourceRequest
	for i := range mock.RegisteredResources {
		if mock.RegisteredResources[i].Type == "aws:index:Instance" {
			instanceReq = &mock.RegisteredResources[i]
			break
		}
	}
	require.NotNil(t, instanceReq, "expected aws:index:Instance resource to be registered")

	assert.Equal(t, []run.Alias{{Spec: &run.AliasSpec{
		Name:     "web[0]",
		NoParent: false,
	}}}, instanceReq.Aliases)
}

func TestEngine_ImportBlock(t *testing.T) {
	t.Parallel()

	src := []byte(`
import {
  to = aws_instance.imported
  id = "i-1234567890abcdef0"
}

resource "aws_instance" "imported" {
  ami           = "ami-12345"
  instance_type = "t3.micro"
}
`)

	p := parser.NewParser()
	config, diags := p.ParseSource("test.hcl", src)
	if diags.HasErrors() {
		t.Fatalf("parse error: %s", diags.Error())
	}

	mock := &testutil.MockResourceMonitor{}
	engine := newTestEngine(t, config, &run.EngineOptions{
		ModuleLoader:    testModuleLoader(t),
		ProjectName:     "test-project",
		StackName:       "dev",
		ResourceMonitor: mock,
		WorkDir:         t.TempDir(),
		RootDir:         t.TempDir(),
		SchemaLoader: schemaloader.New(t, schema.PackageSpec{
			Name: "aws",
			Resources: map[string]schema.ResourceSpec{
				"aws:index:Instance": {
					InputProperties: map[string]schema.PropertySpec{
						"ami":          {TypeSpec: schema.TypeSpec{Type: "string"}},
						"instanceType": {TypeSpec: schema.TypeSpec{Type: "string"}},
					},
					ObjectTypeSpec: schema.ObjectTypeSpec{
						Properties: map[string]schema.PropertySpec{
							"ami":          {TypeSpec: schema.TypeSpec{Type: "string"}},
							"instanceType": {TypeSpec: schema.TypeSpec{Type: "string"}},
						},
					},
				},
			},
		}),
	})

	err := engine.Run(t.Context())
	if err != nil {
		t.Fatalf("run error: %v", err)
	}

	// Find the instance resource
	var instanceReq *run.RegisterResourceRequest
	for i := range mock.RegisteredResources {
		if mock.RegisteredResources[i].Type == "aws:index:Instance" {
			instanceReq = &mock.RegisteredResources[i]
			break
		}
	}

	require.NotNil(t, instanceReq, "expected aws:index:Instance resource to be registered")

	// Check that ImportId was set
	if instanceReq.ImportId != "i-1234567890abcdef0" {
		t.Errorf("expected ImportId 'i-1234567890abcdef0', got %q", instanceReq.ImportId)
	}
}

// TestEngine_ImportBlockInstanceKey covers import blocks whose `to` address
// names one instance of an expanded resource: each instance takes the ID of
// the import block keyed with its own key, and an import address never
// matches an instance with a different (or missing) key.
func TestEngine_ImportBlockInstanceKey(t *testing.T) {
	t.Parallel()

	awsSchema := schema.PackageSpec{
		Name: "aws",
		Resources: map[string]schema.ResourceSpec{
			"aws:index:Instance": {
				InputProperties: map[string]schema.PropertySpec{
					"ami": {TypeSpec: schema.TypeSpec{Type: "string"}},
				},
				ObjectTypeSpec: schema.ObjectTypeSpec{
					Properties: map[string]schema.PropertySpec{
						"ami": {TypeSpec: schema.TypeSpec{Type: "string"}},
					},
				},
			},
		},
	}

	// importIdsByName runs src and returns the ImportId of every registered
	// aws:index:Instance, keyed by resource name.
	importIdsByName := func(t *testing.T, tmpDir string) map[string]string {
		p := parser.NewParser()
		config, diags := p.ParseDirectory(tmpDir)
		require.False(t, diags.HasErrors(), "parse error: %s", diags.Error())

		mock := &testutil.MockResourceMonitor{}
		engine := newTestEngine(t, config, &run.EngineOptions{
			ModuleLoader:    testLiveModuleLoader(t),
			ProjectName:     "test-project",
			StackName:       "dev",
			ResourceMonitor: mock,
			WorkDir:         tmpDir,
			RootDir:         tmpDir,
			SchemaLoader:    schemaloader.New(t, awsSchema),
		})
		require.NoError(t, engine.Run(t.Context()))

		got := map[string]string{}
		for _, r := range mock.RegisteredResources {
			if r.Type == "aws:index:Instance" {
				got[r.Name] = r.ImportId
			}
		}
		return got
	}

	writeRoot := func(t *testing.T, src string) string {
		tmpDir := t.TempDir()
		require.NoError(t, os.WriteFile(tmpDir+"/main.tf", []byte(src), 0o644))
		return tmpDir
	}

	t.Run("for_each", func(t *testing.T) {
		t.Parallel()

		tmpDir := writeRoot(t, `
resource "aws_instance" "web" {
  for_each = toset(["a", "b"])
  ami      = "ami-12345"
}

import {
  to = aws_instance.web["a"]
  id = "id-a"
}

import {
  to = aws_instance.web["b"]
  id = "id-b"
}
`)
		assert.Equal(t, map[string]string{
			`web["a"]`: "id-a",
			`web["b"]`: "id-b",
		}, importIdsByName(t, tmpDir))
	})

	t.Run("tuple_for_each", func(t *testing.T) {
		t.Parallel()

		tmpDir := writeRoot(t, `
resource "aws_instance" "web" {
  count = 2
  ami   = "ami-12345"
}

import {
  for_each = ["id-a", "id-b"]
  to       = aws_instance.web[each.key]
  id       = each.value
}
`)
		assert.Equal(t, map[string]string{
			"web[0]": "id-a",
			"web[1]": "id-b",
		}, importIdsByName(t, tmpDir))
	})

	t.Run("unkeyed_import_does_not_match_expanded_resource", func(t *testing.T) {
		t.Parallel()

		tmpDir := writeRoot(t, `
resource "aws_instance" "web" {
  for_each = toset(["a", "b"])
  ami      = "ami-12345"
}

import {
  to = aws_instance.web
  id = "id-unkeyed"
}
`)
		assert.Equal(t, map[string]string{
			`web["a"]`: "",
			`web["b"]`: "",
		}, importIdsByName(t, tmpDir))
	})
}

// testSchema returns a minimal schema for a test_resource resource.
func testSchema() schema.PackageSpec {
	return schema.PackageSpec{
		Name: "test",
		Resources: map[string]schema.ResourceSpec{
			"test:index:Resource": {
				InputProperties: map[string]schema.PropertySpec{
					"field": {TypeSpec: schema.TypeSpec{Type: "string"}},
				},
				ObjectTypeSpec: schema.ObjectTypeSpec{
					Properties: map[string]schema.PropertySpec{
						"field": {TypeSpec: schema.TypeSpec{Type: "string"}},
					},
				},
			},
		},
	}
}

// hasRegisteredResource reports whether the mock has a registered resource with the given type.
func hasRegisteredResource(mock *testutil.MockResourceMonitor, typ string) bool {
	for _, r := range mock.RegisteredResources {
		if r.Type == typ {
			return true
		}
	}
	return false
}

// TestEngine_WholeResourceReferenceAsComponentInput covers passing a whole
// resource by value into a component (MLC) input typed as a resource reference,
func TestEngine_WholeResourceReferenceAsComponentInput(t *testing.T) {
	t.Parallel()

	componentSchema := schema.PackageSpec{
		Name: "test",
		Resources: map[string]schema.ResourceSpec{
			"test:index:Resource": {
				InputProperties: map[string]schema.PropertySpec{
					"field": {TypeSpec: schema.TypeSpec{Type: "string"}},
				},
				ObjectTypeSpec: schema.ObjectTypeSpec{
					Properties: map[string]schema.PropertySpec{
						"field": {TypeSpec: schema.TypeSpec{Type: "string"}},
					},
				},
			},
			"test:index:Component": {
				IsComponent: true,
				InputProperties: map[string]schema.PropertySpec{
					"handler": {TypeSpec: schema.TypeSpec{Ref: "#/resources/test:index:Resource"}},
				},
				ObjectTypeSpec: schema.ObjectTypeSpec{
					Properties: map[string]schema.PropertySpec{
						"url": {TypeSpec: schema.TypeSpec{Type: "string"}},
					},
				},
			},
		},
	}

	src := []byte(`
resource "test_resource" "fn" {
  field = "value"
}

resource "test_component" "comp" {
  handler = test_resource.fn
}
`)

	p := parser.NewParser()
	config, diags := p.ParseSource("test.hcl", src)
	require.False(t, diags.HasErrors(), diags.Error())

	mock := &testutil.MockResourceMonitor{}
	engine := newTestEngine(t, config, &run.EngineOptions{
		ModuleLoader:    testModuleLoader(t),
		ProjectName:     "test-project",
		StackName:       "dev",
		ResourceMonitor: mock,
		WorkDir:         t.TempDir(),
		RootDir:         t.TempDir(),
		SchemaLoader:    schemaloader.New(t, componentSchema),
	})
	require.NoError(t, engine.Run(t.Context()))

	var component *run.RegisterResourceRequest
	for i := range mock.RegisteredResources {
		if mock.RegisteredResources[i].Type == "test:index:Component" {
			component = &mock.RegisteredResources[i]
		}
	}
	require.NotNil(t, component, "component resource should be registered")

	handler, ok := component.Inputs.GetOk("handler")
	require.True(t, ok, "component should receive the handler input")
	require.True(t, handler.IsResourceReference(),
		"the whole-resource input should reach the component as a resource reference, not %s",
		handler.GoString())
	require.Equal(t, property.ResourceReference{
		URN: "urn:pulumi:test::project::test:index:Resource::fn",
		ID:  property.New("fn-id"),
	}, handler.AsResourceReference())
}

func TestEngine_FieldAccessOnComponentReferenceOutput(t *testing.T) {
	t.Parallel()

	componentSchema := schema.PackageSpec{
		Name: "test",
		Resources: map[string]schema.ResourceSpec{
			"test:index:Resource": {
				InputProperties: map[string]schema.PropertySpec{
					"value": {TypeSpec: schema.TypeSpec{Type: "string"}},
				},
				ObjectTypeSpec: schema.ObjectTypeSpec{
					Properties: map[string]schema.PropertySpec{
						"value": {TypeSpec: schema.TypeSpec{Type: "string"}},
					},
				},
			},
			"test:index:Component": {
				IsComponent: true,
				ObjectTypeSpec: schema.ObjectTypeSpec{
					Properties: map[string]schema.PropertySpec{
						"ref": {TypeSpec: schema.TypeSpec{Ref: "#/resources/test:index:Resource"}},
					},
				},
			},
		},
	}

	const innerURN = "urn:pulumi:test::project::test:index:Resource::inner"

	src := []byte(`
resource "test_component" "comp" {
}

resource "test_resource" "consumer" {
  value = test_component.comp.ref.value
}
`)

	p := parser.NewParser()
	config, diags := p.ParseSource("test.hcl", src)
	require.False(t, diags.HasErrors(), diags.Error())

	mock := &testutil.MockResourceMonitor{
		// The component returns a reference to a resource it created.
		RegisterResourceHandler: func(_ context.Context, req run.RegisterResourceRequest) (*run.RegisterResourceResponse, error) {
			resURN := urn.URN("urn:pulumi:test::project::" + req.Type + "::" + req.Name)
			outputs := req.Inputs
			if req.Type == "test:index:Component" {
				outputs = property.NewMap(map[string]property.Value{
					"ref": property.New(property.ResourceReference{
						URN: innerURN,
						ID:  property.New("inner-id"),
					}),
				})
			}
			return &run.RegisterResourceResponse{URN: resURN, ID: req.Name + "-id", Outputs: outputs}, nil
		},
		// getResource on the referenced resource returns its state.
		InvokeHandler: func(_ context.Context, req run.InvokeRequest) (*run.InvokeResponse, error) {
			if req.Token == "pulumi:pulumi:getResource" {
				return &run.InvokeResponse{Return: property.NewMap(map[string]property.Value{
					"state": property.New(property.NewMap(map[string]property.Value{
						"value": property.New("hello-from-ref"),
					})),
				})}, nil
			}
			return &run.InvokeResponse{Return: property.NewMap(nil)}, nil
		},
	}
	engine := newTestEngine(t, config, &run.EngineOptions{
		ModuleLoader:    testModuleLoader(t),
		ProjectName:     "test-project",
		StackName:       "dev",
		ResourceMonitor: mock,
		WorkDir:         t.TempDir(),
		RootDir:         t.TempDir(),
		SchemaLoader:    schemaloader.New(t, componentSchema),
	})
	require.NoError(t, engine.Run(t.Context()))

	var consumer *run.RegisterResourceRequest
	for i := range mock.RegisteredResources {
		if mock.RegisteredResources[i].Name == "consumer" {
			consumer = &mock.RegisteredResources[i]
		}
	}
	require.NotNil(t, consumer, "consumer resource should be registered")

	value, ok := consumer.Inputs.GetOk("value")
	require.True(t, ok, "consumer should receive the value input")
	require.Equal(t, "hello-from-ref", value.AsString(),
		"a field read off a component's reference output should resolve to the referenced resource's state")
}

// TestEngine_SecretResourceReferenceConfig locks in that a secret resource
// reference keeps its mark across the component boundary: when a secret
// reference is supplied as config and its state is fetched, the resolved outputs
// are secret too, so a value derived from one of its fields stays secret.
func TestEngine_SecretResourceReferenceConfig(t *testing.T) {
	t.Parallel()

	src := []byte(`
variable "handler" {
}

resource "test_resource" "consumer" {
  value = var.handler.value
}
`)

	p := parser.NewParser()
	config, diags := p.ParseSource("test.hcl", src)
	require.False(t, diags.HasErrors(), diags.Error())

	ref := property.New(property.ResourceReference{
		URN: "urn:pulumi:test::project::test:index:Resource::inner",
		ID:  property.New("inner-id"),
	}).WithSecret(true)

	mock := &testutil.MockResourceMonitor{
		InvokeHandler: func(_ context.Context, req run.InvokeRequest) (*run.InvokeResponse, error) {
			if req.Token == "pulumi:pulumi:getResource" {
				return &run.InvokeResponse{Return: property.NewMap(map[string]property.Value{
					"state": property.New(property.NewMap(map[string]property.Value{
						"value": property.New("secret-val"),
					})),
				})}, nil
			}
			return &run.InvokeResponse{Return: property.NewMap(nil)}, nil
		},
	}
	engine := newTestEngine(t, config, &run.EngineOptions{
		ModuleLoader:    testModuleLoader(t),
		ProjectName:     "test-project",
		StackName:       "dev",
		ResourceMonitor: mock,
		WorkDir:         t.TempDir(),
		RootDir:         t.TempDir(),
		SchemaLoader: schemaloader.New(t, schema.PackageSpec{
			Name: "test",
			Resources: map[string]schema.ResourceSpec{
				"test:index:Resource": {
					InputProperties: map[string]schema.PropertySpec{
						"value": {TypeSpec: schema.TypeSpec{Type: "string"}},
					},
					ObjectTypeSpec: schema.ObjectTypeSpec{
						Properties: map[string]schema.PropertySpec{
							"value": {TypeSpec: schema.TypeSpec{Type: "string"}},
						},
					},
				},
			},
		}),
		Config: map[string]run.ConfigValue{
			"test-project:handler": run.TypedConfigValue(transform.PropertyValueToCty(ref)),
		},
	})
	require.NoError(t, engine.Run(t.Context()))

	var consumer *run.RegisterResourceRequest
	for i := range mock.RegisteredResources {
		if mock.RegisteredResources[i].Name == "consumer" {
			consumer = &mock.RegisteredResources[i]
		}
	}
	require.NotNil(t, consumer, "consumer resource should be registered")

	value, ok := consumer.Inputs.GetOk("value")
	require.True(t, ok, "consumer should receive the value input")
	require.Equal(t, "secret-val", value.AsString(),
		"the field should resolve to the referenced resource's fetched state")
	require.True(t, value.Secret(),
		"a value derived from a secret resource reference must stay secret")
}

// TestEngine_WholeResourceModuleOutput covers returning a resource the program
// created as a whole: a module/component output `value = test_resource.r`
// exposes the resource's fields to the caller. (This is the run-engine view of
// what a component returns; the construct boundary only wraps these outputs.)
func TestEngine_WholeResourceModuleOutput(t *testing.T) {
	t.Parallel()

	src := []byte(`
resource "test_resource" "r" {
  value = "exported"
}

output "exported" {
  value = test_resource.r
}
`)

	p := parser.NewParser()
	config, diags := p.ParseSource("test.hcl", src)
	require.False(t, diags.HasErrors(), diags.Error())

	mock := &testutil.MockResourceMonitor{}
	engine := newTestEngine(t, config, &run.EngineOptions{
		ModuleLoader:    testModuleLoader(t),
		ProjectName:     "test-project",
		StackName:       "dev",
		ResourceMonitor: mock,
		WorkDir:         t.TempDir(),
		RootDir:         t.TempDir(),
		SchemaLoader: schemaloader.New(t, schema.PackageSpec{
			Name: "test",
			Resources: map[string]schema.ResourceSpec{
				"test:index:Resource": {
					InputProperties: map[string]schema.PropertySpec{
						"value": {TypeSpec: schema.TypeSpec{Type: "string"}},
					},
					ObjectTypeSpec: schema.ObjectTypeSpec{
						Properties: map[string]schema.PropertySpec{
							"value": {TypeSpec: schema.TypeSpec{Type: "string"}},
						},
					},
				},
			},
		}),
	})
	require.NoError(t, engine.Run(t.Context()))

	exported, ok := mock.StackOutputs.GetOk("exported")
	require.True(t, ok, "expected the exported output")
	out := exported.AsMap()

	value, ok := out.GetOk("value")
	require.True(t, ok, "the whole-resource output should expose the resource's value field")
	require.Equal(t, "exported", value.AsString())

	_, hasURN := out.GetOk("urn")
	require.False(t, hasURN, "the synthetic urn attribute must not leak into the output")
}

func TestEngine_ReplaceTriggeredBy(t *testing.T) {
	t.Parallel()

	src := []byte(`
resource "test_resource" "res" {
  field = "value"

  lifecycle {
    replace_triggered_by = ["sentinel-a", "sentinel-b"]
  }
}
`)

	p := parser.NewParser()
	config, diags := p.ParseSource("test.hcl", src)
	if diags.HasErrors() {
		t.Fatalf("parse error: %s", diags.Error())
	}

	mock := &testutil.MockResourceMonitor{}
	engine := newTestEngine(t, config, &run.EngineOptions{
		ModuleLoader:    testModuleLoader(t),
		ProjectName:     "test-project",
		StackName:       "dev",
		ResourceMonitor: mock,
		WorkDir:         t.TempDir(),
		RootDir:         t.TempDir(),
		SchemaLoader:    schemaloader.New(t, testSchema()),
	})

	require.NoError(t, engine.Run(t.Context()))

	require.Len(t, mock.RegisteredResources, 2) // stack + resource
	req := mock.RegisteredResources[1]
	require.False(t, req.ReplacementTrigger.IsNull(),
		"expected replacement_trigger to be set from lifecycle.replace_triggered_by")
}

// TestEngine_ReplaceTriggeredByWholeResource covers the action-based half of a
// whole-resource replace_triggered_by element: the referenced instance's URN
// must be recorded in ReplaceWith so the engine replaces the dependent whenever
// the referenced resource is replaced, even if its value is unchanged. The
// detection is value-based, so a local aliasing the resource behaves the same,
// while an attribute of the resource stays purely value-triggered.
func TestEngine_ReplaceTriggeredByWholeResource(t *testing.T) {
	t.Parallel()

	src := []byte(`
resource "test_resource" "middle" {
  field = "const"
}

locals {
  indirect = test_resource.middle
}

resource "test_resource" "byWhole" {
  field = "dep"

  lifecycle {
    replace_triggered_by = [test_resource.middle]
  }
}

resource "test_resource" "byLocal" {
  field = "dep"

  lifecycle {
    replace_triggered_by = [local.indirect]
  }
}

resource "test_resource" "byAttr" {
  field = "dep"

  lifecycle {
    replace_triggered_by = [test_resource.middle.field]
  }
}
`)

	p := parser.NewParser()
	config, diags := p.ParseSource("test.hcl", src)
	require.False(t, diags.HasErrors(), diags.Error())

	mock := &testutil.MockResourceMonitor{}
	engine := newTestEngine(t, config, &run.EngineOptions{
		ModuleLoader:    testModuleLoader(t),
		ProjectName:     "test-project",
		StackName:       "dev",
		ResourceMonitor: mock,
		WorkDir:         t.TempDir(),
		RootDir:         t.TempDir(),
		SchemaLoader:    schemaloader.New(t, testSchema()),
	})

	require.NoError(t, engine.Run(t.Context()))

	byName := make(map[string]run.RegisterResourceRequest)
	for _, req := range mock.RegisteredResources {
		byName[req.Name] = req
	}

	middleURN := "urn:pulumi:test::project::test:index:Resource::middle"

	byWhole, ok := byName["byWhole"]
	require.True(t, ok, "expected byWhole to be registered")
	assert.Equal(t, []string{middleURN}, byWhole.ReplaceWith)
	assert.False(t, byWhole.ReplacementTrigger.IsNull(),
		"a whole-resource reference keeps its value trigger to cover in-place updates")

	byLocal, ok := byName["byLocal"]
	require.True(t, ok, "expected byLocal to be registered")
	assert.Equal(t, []string{middleURN}, byLocal.ReplaceWith,
		"a local aliasing a whole resource behaves like the resource itself")

	byAttr, ok := byName["byAttr"]
	require.True(t, ok, "expected byAttr to be registered")
	assert.Empty(t, byAttr.ReplaceWith, "an attribute reference is value-based only")
	assert.False(t, byAttr.ReplacementTrigger.IsNull())
}

// TestEngine_HetListOutputRoundTrip drives the engine against a mock provider whose
// resource output is a list of objects with a nested optional object populated in some
// elements and absent in others.
func TestEngine_HetListOutputRoundTrip(t *testing.T) {
	t.Parallel()

	src := []byte(`resource "test_het_list" "het" {}`)
	p := parser.NewParser()
	config, diags := p.ParseSource("test.hcl", src)
	require.False(t, diags.HasErrors(), diags.Error())

	hetListSchema := schema.PackageSpec{
		Name: "test",
		Types: map[string]schema.ComplexTypeSpec{
			"test:index:Token": {
				ObjectTypeSpec: schema.ObjectTypeSpec{
					Type: "object",
					Properties: map[string]schema.PropertySpec{
						"audience": {TypeSpec: schema.TypeSpec{Type: "string"}},
					},
				},
			},
			"test:index:Source": {
				ObjectTypeSpec: schema.ObjectTypeSpec{
					Type: "object",
					Properties: map[string]schema.PropertySpec{
						"token": {TypeSpec: schema.TypeSpec{Ref: "#/types/test:index:Token"}},
					},
				},
			},
		},
		Resources: map[string]schema.ResourceSpec{
			"test:index:HetList": {
				ObjectTypeSpec: schema.ObjectTypeSpec{
					Properties: map[string]schema.PropertySpec{
						"sources": {
							TypeSpec: schema.TypeSpec{
								Type:  "array",
								Items: &schema.TypeSpec{Ref: "#/types/test:index:Source"},
							},
						},
					},
				},
			},
		},
	}

	monitor := &testutil.MockResourceMonitor{
		RegisterResourceHandler: func(ctx context.Context, req run.RegisterResourceRequest) (*run.RegisterResourceResponse, error) {
			urn := urn.URN("urn:pulumi:test::project::" + req.Type + "::" + req.Name)
			if req.Type == "test:index:HetList" {
				return &run.RegisterResourceResponse{
					URN: urn,
					ID:  req.Name + "-id",
					Outputs: property.NewMap(map[string]property.Value{
						"sources": property.New([]property.Value{
							property.New(property.NewMap(map[string]property.Value{
								"token": property.New(property.NewMap(map[string]property.Value{})),
							})),
							property.New(property.NewMap(map[string]property.Value{})),
						}),
					}),
				}, nil
			}
			return &run.RegisterResourceResponse{
				URN:     urn,
				ID:      req.Name + "-id",
				Outputs: req.Inputs,
			}, nil
		},
	}
	engine := newTestEngine(t, config, &run.EngineOptions{
		ModuleLoader:    testModuleLoader(t),
		ProjectName:     "test-project",
		StackName:       "dev",
		ResourceMonitor: monitor,
		WorkDir:         t.TempDir(),
		RootDir:         t.TempDir(),
		SchemaLoader:    schemaloader.New(t, hetListSchema),
	})

	assert.NotPanics(t, func() {
		err := engine.Run(t.Context())
		require.NoError(t, err)
	})
}

// TestEngine_SchemaConstValues verifies that the engine applies "const" values
// declared in the Pulumi schema.
func TestEngine_SchemaConstValues(t *testing.T) {
	t.Parallel()

	// PartA and PartB differ only in the *type* of their `field`
	// property — an object{fooBar} vs map(string) — so renaming
	// `foo_bar` → `fooBar` is correct for one branch and wrong for the
	// other. Picking the right rule per element requires the engine to
	// consult the const-pinned `kind` discriminator first.
	constSchema := func() schema.PackageSpec {
		return schema.PackageSpec{
			Name: "test",
			Types: map[string]schema.ComplexTypeSpec{
				"test:index:PartAField": {
					ObjectTypeSpec: schema.ObjectTypeSpec{
						Type: "object",
						Properties: map[string]schema.PropertySpec{
							"fooBar": {TypeSpec: schema.TypeSpec{Type: "string"}},
						},
					},
				},
				"test:index:PartA": {
					ObjectTypeSpec: schema.ObjectTypeSpec{
						Type: "object",
						Properties: map[string]schema.PropertySpec{
							"kind":  {TypeSpec: schema.TypeSpec{Type: "string"}, Const: "a"},
							"field": {TypeSpec: schema.TypeSpec{Ref: "#/types/test:index:PartAField"}},
						},
						Required: []string{"kind"},
					},
				},
				"test:index:PartB": {
					ObjectTypeSpec: schema.ObjectTypeSpec{
						Type: "object",
						Properties: map[string]schema.PropertySpec{
							"kind": {TypeSpec: schema.TypeSpec{Type: "string"}, Const: "b"},
							"field": {TypeSpec: schema.TypeSpec{
								Type:                 "object",
								AdditionalProperties: &schema.TypeSpec{Type: "string"},
							}},
						},
						Required: []string{"kind"},
					},
				},
			},
			Resources: map[string]schema.ResourceSpec{
				"test:index:Widget": {
					InputProperties: map[string]schema.PropertySpec{
						"kind": {TypeSpec: schema.TypeSpec{Type: "string"}, Const: "widget"},
						"name": {TypeSpec: schema.TypeSpec{Type: "string"}},
						"parts": {
							TypeSpec: schema.TypeSpec{
								Type: "array",
								Items: &schema.TypeSpec{
									OneOf: []schema.TypeSpec{
										{Ref: "#/types/test:index:PartA"},
										{Ref: "#/types/test:index:PartB"},
									},
								},
							},
						},
					},
					RequiredInputs: []string{"kind", "name"},
					ObjectTypeSpec: schema.ObjectTypeSpec{
						Properties: map[string]schema.PropertySpec{
							"kind": {TypeSpec: schema.TypeSpec{Type: "string"}, Const: "widget"},
							"name": {TypeSpec: schema.TypeSpec{Type: "string"}},
						},
						Required: []string{"kind", "name"},
					},
				},
			},
		}
	}

	runEngine := func(t *testing.T, src string) (*testutil.MockResourceMonitor, error) {
		t.Helper()
		p := parser.NewParser()
		config, diags := p.ParseSource("test.hcl", []byte(src))
		require.False(t, diags.HasErrors(), "parse error: %s", diags.Error())

		mock := &testutil.MockResourceMonitor{}
		engine := newTestEngine(t, config, &run.EngineOptions{
			ModuleLoader:    testModuleLoader(t),
			ProjectName:     "test-project",
			StackName:       "dev",
			ResourceMonitor: mock,
			WorkDir:         t.TempDir(),
			RootDir:         t.TempDir(),
			SchemaLoader:    schemaloader.New(t, constSchema()),
		})
		return mock, engine.Run(t.Context())
	}

	findWidget := func(t *testing.T, mock *testutil.MockResourceMonitor) run.RegisterResourceRequest {
		t.Helper()
		for _, r := range mock.RegisteredResources {
			if r.Type == "test:index:Widget" {
				return r
			}
		}
		t.Fatal("expected a test:index:Widget registration")
		return run.RegisterResourceRequest{}
	}

	t.Run("applied when omitted", func(t *testing.T) {
		t.Parallel()

		mock, err := runEngine(t, `
resource "test_widget" "w" {
  name = "hello"
}
`)
		require.NoError(t, err)

		req := findWidget(t, mock)
		assert.Equal(t, property.New("widget"), req.Inputs.Get("kind"))
		assert.Equal(t, property.New("hello"), req.Inputs.Get("name"))
	})

	t.Run("distinguishes union by const", func(t *testing.T) {
		t.Parallel()

		mock, err := runEngine(t, `
resource "test_widget" "w" {
  name = "hello"
  parts = [
    { kind = "a", field = { foo_bar = "object-value" } },
    { kind = "b", field = { foo_bar = "map-value" } },
  ]
}
`)
		require.NoError(t, err)

		req := findWidget(t, mock)
		assert.Equal(t, property.New("widget"), req.Inputs.Get("kind"))

		parts := req.Inputs.Get("parts").AsArray().AsSlice()
		require.Len(t, parts, 2)

		// PartA: object property — `foo_bar` is renamed to `fooBar`.
		assert.Equal(t, property.New(property.NewMap(map[string]property.Value{
			"kind": property.New("a"),
			"field": property.New(property.NewMap(map[string]property.Value{
				"fooBar": property.New("object-value"),
			})),
		})), parts[0])

		// PartB: map key — `foo_bar` is preserved verbatim.
		assert.Equal(t, property.New(property.NewMap(map[string]property.Value{
			"kind": property.New("b"),
			"field": property.New(property.NewMap(map[string]property.Value{
				"foo_bar": property.New("map-value"),
			})),
		})), parts[1])
	})

	t.Run("missing union discriminator errors clearly", func(t *testing.T) {
		t.Parallel()

		_, err := runEngine(t, `
resource "test_widget" "w" {
  name = "hello"
  parts = [
    { field = { foo_bar = "ambiguous" } },
  ]
}
`)
		require.Error(t, err)
		assert.EqualError(t, err,
			`registering test_widget.w: test.hcl:5,5-42: `+
				`cannot determine union variant for "parts[0]"; `+
				`missing discriminator "kind" (expected one of "a", "b")`)
	})
}

func TestEngine_UnknownResourceSuggestsAlternative(t *testing.T) {
	t.Parallel()

	src := []byte(`
terraform {
  required_providers {
    aws = {
      source  = "pulumi/aws"
      version = "1.0.0"
    }
  }
}

resource "aws_ec2_vpd" "example" {}
`)

	p := parser.NewParser()
	config, diags := p.ParseSource("test.hcl", src)
	require.Empty(t, diags)

	mock := &testutil.MockResourceMonitor{}
	engine := newTestEngine(t, config, &run.EngineOptions{
		ModuleLoader:    testModuleLoader(t),
		ProjectName:     "test-project",
		StackName:       "dev",
		ResourceMonitor: mock,
		WorkDir:         t.TempDir(),
		RootDir:         t.TempDir(),
		SchemaLoader: schemaloader.New(t, schema.PackageSpec{
			Name: "aws",
			Meta: &schema.MetadataSpec{
				ModuleFormat: `(.*)(?:/[^/]*)`,
			},
			Resources: map[string]schema.ResourceSpec{
				"aws:ec2/vpc:Vpc": {},
			},
		}),
	})

	err := engine.Run(t.Context())
	require.Error(t, err)
	assert.EqualError(t, err,
		`test.hcl:11,10-23: unknown resource type "aws_ec2_vpd"; did you mean "aws_ec2_vpc"?`)
}

func TestEngine_UnknownDataSourceSuggestsAlternative(t *testing.T) {
	t.Parallel()

	src := []byte(`
terraform {
  required_providers {
    aws = {
      source  = "pulumi/aws"
      version = "1.0.0"
    }
  }
}

data "aws_ec2_vpd" "example" {}
`)

	p := parser.NewParser()
	config, diags := p.ParseSource("test.hcl", src)
	require.False(t, diags.HasErrors(), diags.Error())

	mock := &testutil.MockResourceMonitor{}
	engine := newTestEngine(t, config, &run.EngineOptions{
		ModuleLoader:    testModuleLoader(t),
		ProjectName:     "test-project",
		StackName:       "dev",
		ResourceMonitor: mock,
		WorkDir:         t.TempDir(),
		RootDir:         t.TempDir(),
		SchemaLoader: schemaloader.New(t, schema.PackageSpec{
			Name: "aws",
			Meta: &schema.MetadataSpec{
				ModuleFormat: `(.*)(?:/[^/]*)`,
			},
			Functions: map[string]schema.FunctionSpec{
				"aws:ec2/getVpc:getVpc": {},
			},
		}),
	})

	err := engine.Run(t.Context())
	require.Error(t, err)
	assert.EqualError(t, err,
		`test.hcl:11,6-19: unknown data source type "aws_ec2_vpd"; did you mean "aws_ec2_vpc"?`)
}

// TestEngine_ChildModuleResourceDependencies covers two cases that used to
// register child-module resources with empty Dependencies:
//   - cross-module: a child resource consuming var.X, where the parent bound
//     X to test_resource.upstream.id.
//   - intra-module: a child resource referencing a count-expanded sibling
//     (test_resource.first[0].id) declared in the same module.
func TestEngine_ChildModuleResourceDependencies(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()

	moduleDir := tmpDir + "/modules/child"
	require.NoError(t, os.MkdirAll(moduleDir, 0o755))

	moduleMain := `
variable "parent" { type = string }

resource "test_resource" "first" {
  count        = 1
  parent_field = var.parent
}

resource "test_resource" "second" {
  sibling_field = test_resource.first[0].id
}
`
	require.NoError(t, os.WriteFile(moduleDir+"/main.tf", []byte(moduleMain), 0o644))

	rootMain := `
resource "test_resource" "upstream" {
  field = "root-value"
}

module "child" {
  source = "./modules/child"
  parent = test_resource.upstream.id
}
`
	require.NoError(t, os.WriteFile(tmpDir+"/main.tf", []byte(rootMain), 0o644))

	p := parser.NewParser()
	config, diags := p.ParseDirectory(tmpDir)
	require.False(t, diags.HasErrors(), diags.Error())

	mock := &testutil.MockResourceMonitor{}
	engine := newTestEngine(t, config, &run.EngineOptions{
		ModuleLoader:    testLiveModuleLoader(t),
		ProjectName:     "test-project",
		StackName:       "dev",
		ResourceMonitor: mock,
		WorkDir:         tmpDir,
		RootDir:         tmpDir,
		SchemaLoader: schemaloader.New(t, schema.PackageSpec{
			Name: "test",
			Resources: map[string]schema.ResourceSpec{
				"test:index:Resource": {
					InputProperties: map[string]schema.PropertySpec{
						"field":        {TypeSpec: schema.TypeSpec{Type: "string"}},
						"parentField":  {TypeSpec: schema.TypeSpec{Type: "string"}},
						"siblingField": {TypeSpec: schema.TypeSpec{Type: "string"}},
					},
					ObjectTypeSpec: schema.ObjectTypeSpec{
						Properties: map[string]schema.PropertySpec{
							"field":        {TypeSpec: schema.TypeSpec{Type: "string"}},
							"parentField":  {TypeSpec: schema.TypeSpec{Type: "string"}},
							"siblingField": {TypeSpec: schema.TypeSpec{Type: "string"}},
						},
					},
				},
			},
		}),
	})

	require.NoError(t, engine.Run(t.Context()))

	find := func(name string) *run.RegisterResourceRequest {
		for i := range mock.RegisteredResources {
			r := &mock.RegisteredResources[i]
			if r.Type == "test:index:Resource" && r.Name == name {
				return r
			}
		}
		return nil
	}

	upstream := find("upstream")
	first := find("child.first[0]")
	second := find("child.second")
	require.NotNil(t, upstream, "upstream resource should be registered")
	require.NotNil(t, first, "child-first resource should be registered")
	require.NotNil(t, second, "child-second resource should be registered")

	urnOf := func(r *run.RegisterResourceRequest) string {
		return "urn:pulumi:test::project::" + r.Type + "::" + r.Name
	}

	assert.Equal(t,
		map[string][]string{"parentField": {urnOf(upstream)}},
		first.PropertyDependencies)
	assert.Equal(t, []string{urnOf(upstream)}, first.Dependencies)

	assert.Equal(t,
		map[string][]string{"siblingField": {urnOf(first)}},
		second.PropertyDependencies)
	assert.Equal(t, []string{urnOf(first)}, second.Dependencies)
}

// When both the root and a child module declare a default `provider "simple"`
// config, a resource in the child binds to the child's own block, not the
// inherited root default.
// A provider block with for_each registers one provider per key, each
// configured with its own each.value, and a resource's
// `provider = simple.by_key["a"]` binds to the matching instance.
func TestEngine_ProviderForEach(t *testing.T) {
	t.Parallel()

	src := []byte(`
variable "prefixes" {
  type = map(string)
  default = {
    a = "alpha"
    b = "beta"
  }
}

provider "simple" {
  alias    = "by_key"
  for_each = var.prefixes
  prefix   = each.value
}

resource "simple_resource" "r" {
  provider = simple.by_key["a"]
  input    = "world"
}
`)

	p := parser.NewParser()
	config, diags := p.ParseSource("test.hcl", src)
	require.False(t, diags.HasErrors(), "parse error: %s", diags.Error())

	mock := &testutil.MockResourceMonitor{}
	engine := newTestEngine(t, config, &run.EngineOptions{
		ModuleLoader:    testModuleLoader(t),
		ProjectName:     "test-project",
		StackName:       "dev",
		ResourceMonitor: mock,
		WorkDir:         t.TempDir(),
		RootDir:         t.TempDir(),
		SchemaLoader: schemaloader.New(t, schema.PackageSpec{
			Name: "simple",
			Provider: &schema.ResourceSpec{
				InputProperties: map[string]schema.PropertySpec{
					"prefix": {TypeSpec: schema.TypeSpec{Type: "string"}},
				},
			},
			Resources: map[string]schema.ResourceSpec{
				"simple:index:Resource": {
					InputProperties: map[string]schema.PropertySpec{
						"input": {TypeSpec: schema.TypeSpec{Type: "string"}},
					},
					ObjectTypeSpec: schema.ObjectTypeSpec{
						Properties: map[string]schema.PropertySpec{
							"input": {TypeSpec: schema.TypeSpec{Type: "string"}},
						},
					},
				},
			},
		}),
	})

	require.NoError(t, engine.Run(t.Context()))

	var stackURN urn.URN
	var providerRegs []run.RegisterResourceRequest
	var resourceReg *run.RegisterResourceRequest
	for i, r := range mock.RegisteredResources {
		switch r.Type {
		case "pulumi:pulumi:Stack":
			stackURN = urn.URN("urn:pulumi:test::project::" + r.Type + "::" + r.Name)
		case "pulumi:providers:simple":
			providerRegs = append(providerRegs, r)
		case "simple:index:Resource":
			resourceReg = &mock.RegisteredResources[i]
		}
	}
	sort.Slice(providerRegs, func(i, j int) bool { return providerRegs[i].Name < providerRegs[j].Name })

	assert.Equal(t, []run.RegisterResourceRequest{
		{
			Type:                 "pulumi:providers:simple",
			Name:                 `by_key["a"]`,
			Inputs:               property.NewMap(map[string]property.Value{"prefix": property.New("alpha")}),
			PropertyDependencies: map[string][]string{"prefix": nil},
			Custom:               true,
			Parent:               stackURN,
		},
		{
			Type:                 "pulumi:providers:simple",
			Name:                 `by_key["b"]`,
			Inputs:               property.NewMap(map[string]property.Value{"prefix": property.New("beta")}),
			PropertyDependencies: map[string][]string{"prefix": nil},
			Custom:               true,
			Parent:               stackURN,
		},
	}, providerRegs)

	require.NotNil(t, resourceReg, "resource should register")
	assert.Equal(t,
		`urn:pulumi:test::project::pulumi:providers:simple::by_key["a"]::by_key["a"]-id`,
		resourceReg.Provider,
		"the resource must bind to the provider instance selected by its key")
}

// A provider configuration that consumes a resource output must carry that
// dependency on its provider-resource registration. The program graph already
// orders config before the provider during this run; the registration metadata
// is what preserves the reverse ordering for a later state-driven destroy.
func TestEngine_ProviderConfigDependencies(t *testing.T) {
	t.Parallel()

	src := []byte(`
resource "simple_resource" "config" {
  input = "token"
}

provider "simple" {
  alias  = "configured"
  prefix = simple_resource.config.input
}

resource "simple_resource" "consumer" {
  provider = simple.configured
  input    = "value"
}
`)

	p := parser.NewParser()
	config, diags := p.ParseSource("test.hcl", src)
	require.False(t, diags.HasErrors(), "parse error: %s", diags.Error())

	mock := &testutil.MockResourceMonitor{}
	engine := newTestEngine(t, config, &run.EngineOptions{
		ModuleLoader:    testModuleLoader(t),
		ProjectName:     "test-project",
		StackName:       "dev",
		ResourceMonitor: mock,
		WorkDir:         t.TempDir(),
		RootDir:         t.TempDir(),
		SchemaLoader: schemaloader.New(t, schema.PackageSpec{
			Name: "simple",
			Provider: &schema.ResourceSpec{
				InputProperties: map[string]schema.PropertySpec{
					"prefix": {TypeSpec: schema.TypeSpec{Type: "string"}},
				},
			},
			Resources: map[string]schema.ResourceSpec{
				"simple:index:Resource": {
					InputProperties: map[string]schema.PropertySpec{
						"input": {TypeSpec: schema.TypeSpec{Type: "string"}},
					},
					ObjectTypeSpec: schema.ObjectTypeSpec{
						Properties: map[string]schema.PropertySpec{
							"input": {TypeSpec: schema.TypeSpec{Type: "string"}},
						},
					},
				},
			},
		}),
	})

	require.NoError(t, engine.Run(t.Context()))

	var provider *run.RegisterResourceRequest
	for i := range mock.RegisteredResources {
		if mock.RegisteredResources[i].Type == "pulumi:providers:simple" {
			provider = &mock.RegisteredResources[i]
			break
		}
	}
	require.NotNil(t, provider, "configured provider should register")

	configURN := "urn:pulumi:test::project::simple:index:Resource::config"
	assert.Equal(t, []string{configURN}, provider.Dependencies)
	assert.Equal(t, map[string][]string{"prefix": {configURN}}, provider.PropertyDependencies)
}

// An aliased provider passed into a module via `providers` must survive a
// second hop into a nested module, whether re-passed explicitly or inherited
// implicitly as the nested call's default.
func TestEngine_ProviderResolution_AliasedPassThroughSecondHop(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	explicitInnerDir := tmpDir + "/modules/outer/inner"
	implicitInnerDir := tmpDir + "/modules/outer_implicit/inner"
	require.NoError(t, os.MkdirAll(explicitInnerDir, 0o755))
	require.NoError(t, os.MkdirAll(implicitInnerDir, 0o755))

	require.NoError(t, os.WriteFile(tmpDir+"/main.tf", []byte(`
provider "simple" {
  prefix = "default"
}

provider "simple" {
  alias  = "special"
  prefix = "special"
}

module "outer" {
  source = "./modules/outer"
  providers = {
    simple = simple.special
  }
}

module "outer_implicit" {
  source = "./modules/outer_implicit"
  providers = {
    simple = simple.special
  }
}
`), 0o644))

	require.NoError(t, os.WriteFile(tmpDir+"/modules/outer/main.tf", []byte(`
module "inner" {
  source = "./inner"
  providers = {
    simple = simple
  }
}
`), 0o644))

	require.NoError(t, os.WriteFile(explicitInnerDir+"/main.tf", []byte(`
resource "simple_resource" "r" {
  input = "explicit"
}
`), 0o644))

	require.NoError(t, os.WriteFile(tmpDir+"/modules/outer_implicit/main.tf", []byte(`
terraform {
  required_providers {
    simple = {
      source = "hashicorp/simple"
    }
  }
}

module "inner" {
  source = "./inner"
}
`), 0o644))

	require.NoError(t, os.WriteFile(implicitInnerDir+"/main.tf", []byte(`
terraform {
  required_providers {
    simple = {
      source = "hashicorp/simple"
    }
  }
}

resource "simple_resource" "r" {
  input = "implicit"
}
`), 0o644))

	p := parser.NewParser()
	config, diags := p.ParseDirectory(tmpDir)
	require.False(t, diags.HasErrors(), "parse error: %s", diags.Error())

	mock := &testutil.MockResourceMonitor{}
	engine := newTestEngine(t, config, &run.EngineOptions{
		ModuleLoader:    testLiveModuleLoader(t),
		ProjectName:     "test-project",
		StackName:       "dev",
		ResourceMonitor: mock,
		WorkDir:         tmpDir,
		RootDir:         tmpDir,
		SchemaLoader: schemaloader.New(t, schema.PackageSpec{
			Name: "simple",
			Provider: &schema.ResourceSpec{
				InputProperties: map[string]schema.PropertySpec{
					"prefix": {TypeSpec: schema.TypeSpec{Type: "string"}},
				},
			},
			Resources: map[string]schema.ResourceSpec{
				"simple:index:Resource": {
					InputProperties: map[string]schema.PropertySpec{
						"input": {TypeSpec: schema.TypeSpec{Type: "string"}},
					},
					ObjectTypeSpec: schema.ObjectTypeSpec{
						Properties: map[string]schema.PropertySpec{
							"input": {TypeSpec: schema.TypeSpec{Type: "string"}},
						},
					},
				},
			},
		}),
	})

	require.NoError(t, engine.Run(t.Context()))

	var specialProvider *run.RegisterResourceRequest
	resources := map[string]*run.RegisterResourceRequest{}
	for i := range mock.RegisteredResources {
		r := &mock.RegisteredResources[i]
		switch r.Type {
		case "pulumi:providers:simple":
			if v, ok := r.Inputs.GetOk("prefix"); ok && v.IsString() && v.AsString() == "special" {
				specialProvider = r
			}
		case "simple:index:Resource":
			if v, ok := r.Inputs.GetOk("input"); ok && v.IsString() {
				resources[v.AsString()] = r
			}
		}
	}
	require.NotNil(t, specialProvider, "the aliased provider block should register")
	require.NotNil(t, resources["explicit"], "the explicitly re-passed resource should register")
	require.NotNil(t, resources["implicit"], "the implicitly inheriting resource should register")

	specialRef := "urn:pulumi:test::project::" + specialProvider.Type + "::" + specialProvider.Name +
		"::" + specialProvider.Name + "-id"
	assert.Equal(t, specialRef, resources["explicit"].Provider,
		"an explicit `providers = { simple = simple }` re-pass must carry the aliased config")
	assert.Equal(t, specialRef, resources["implicit"].Provider,
		"an implicit nested call must inherit the middle module's passed-in default")

	componentProviders := map[string]map[string]string{}
	for i := range mock.RegisteredResources {
		r := &mock.RegisteredResources[i]
		if strings.HasPrefix(r.Type, "components:index:") {
			componentProviders[r.Name] = r.Providers
		}
	}
	assert.Equal(t, map[string]map[string]string{
		"outer":                {"simple": specialRef},
		"outer_implicit":       {"simple": specialRef},
		"outer.inner":          {"simple": specialRef},
		"outer_implicit.inner": nil,
	}, componentProviders, "module components must carry the providers passed to them")
}

func TestEngine_ProviderResolution_ChildModuleBlockWins(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	childDir := tmpDir + "/modules/child"
	require.NoError(t, os.MkdirAll(childDir, 0o755))

	require.NoError(t, os.WriteFile(childDir+"/main.tf", []byte(`
provider "simple" {
  prefix = "child-prefix"
}

resource "simple_resource" "r" {
  input = "p7"
}
`), 0o644))

	require.NoError(t, os.WriteFile(tmpDir+"/main.tf", []byte(`
provider "simple" {
  prefix = "root-prefix"
}

module "child" {
  source = "./modules/child"
}
`), 0o644))

	p := parser.NewParser()
	config, diags := p.ParseDirectory(tmpDir)
	require.False(t, diags.HasErrors(), "parse error: %s", diags.Error())

	mock := &testutil.MockResourceMonitor{}
	engine := newTestEngine(t, config, &run.EngineOptions{
		ModuleLoader:    testLiveModuleLoader(t),
		ProjectName:     "test-project",
		StackName:       "dev",
		ResourceMonitor: mock,
		WorkDir:         tmpDir,
		RootDir:         tmpDir,

		SchemaLoader: schemaloader.New(t, schema.PackageSpec{
			Name: "simple",
			Provider: &schema.ResourceSpec{
				InputProperties: map[string]schema.PropertySpec{
					"prefix": {TypeSpec: schema.TypeSpec{Type: "string"}},
				},
			},
			Resources: map[string]schema.ResourceSpec{
				"simple:index:Resource": {
					InputProperties: map[string]schema.PropertySpec{
						"input": {TypeSpec: schema.TypeSpec{Type: "string"}},
					},
					ObjectTypeSpec: schema.ObjectTypeSpec{
						Properties: map[string]schema.PropertySpec{
							"input": {TypeSpec: schema.TypeSpec{Type: "string"}},
						},
					},
				},
			},
		}),
	})

	require.NoError(t, engine.Run(t.Context()))

	// Locate a registered provider block by its `prefix` config value.
	findProvider := func(prefix string) (run.RegisterResourceRequest, bool) {
		for _, r := range mock.RegisteredResources {
			if r.Type != "pulumi:providers:simple" {
				continue
			}
			if v, ok := r.Inputs.GetOk("prefix"); ok && v.IsString() && v.AsString() == prefix {
				return r, true
			}
		}
		return run.RegisterResourceRequest{}, false
	}

	// The child resource depends on the child's own block, so that block
	// registers on its own (no AlwaysRegisterProviders).
	childProvider, ok := findProvider("child-prefix")
	require.True(t, ok, "child module's provider block should register")

	// MockResourceMonitor mints URN urn:pulumi:test::project::<type>::<name>
	// and ID <name>-id; a provider ref is "<urn>::<id>".
	providerRef := func(r run.RegisterResourceRequest) string {
		urn := "urn:pulumi:test::project::" + r.Type + "::" + r.Name
		return urn + "::" + r.Name + "-id"
	}

	var childResource *run.RegisterResourceRequest
	for i := range mock.RegisteredResources {
		if mock.RegisteredResources[i].Type == "simple:index:Resource" {
			childResource = &mock.RegisteredResources[i]
			break
		}
	}
	require.NotNil(t, childResource, "child resource should register")

	assert.Equal(t, providerRef(childProvider), childResource.Provider,
		"a resource in a child module with its own provider block must bind to that block")

	// The root block is unused (the child binds to its own block), so it is not
	// configured — TF only configures providers something actually uses.
	_, rootRegistered := findProvider("root-prefix")
	assert.False(t, rootRegistered, "the unused root provider block must not register")
}

// A resource in a child module with no provider block and no `providers = {}`
// mapping inherits the root module's default provider config. The graph edge
// also registers the otherwise-unused root block (no AlwaysRegisterProviders).
func TestEngine_ProviderResolution_ChildInheritsRootDefault(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	childDir := tmpDir + "/modules/child"
	require.NoError(t, os.MkdirAll(childDir, 0o755))

	require.NoError(t, os.WriteFile(childDir+"/main.tf", []byte(`
resource "simple_resource" "r" {
  input = "p2"
}
`), 0o644))

	require.NoError(t, os.WriteFile(tmpDir+"/main.tf", []byte(`
provider "simple" {
  prefix = "root-prefix"
}

module "child" {
  source = "./modules/child"
}
`), 0o644))

	p := parser.NewParser()
	config, diags := p.ParseDirectory(tmpDir)
	require.False(t, diags.HasErrors(), "parse error: %s", diags.Error())

	mock := &testutil.MockResourceMonitor{}
	engine := newTestEngine(t, config, &run.EngineOptions{
		ModuleLoader:    testLiveModuleLoader(t),
		ProjectName:     "test-project",
		StackName:       "dev",
		ResourceMonitor: mock,
		WorkDir:         tmpDir,
		RootDir:         tmpDir,
		SchemaLoader: schemaloader.New(t, schema.PackageSpec{
			Name: "simple",
			Provider: &schema.ResourceSpec{
				InputProperties: map[string]schema.PropertySpec{
					"prefix": {TypeSpec: schema.TypeSpec{Type: "string"}},
				},
			},
			Resources: map[string]schema.ResourceSpec{
				"simple:index:Resource": {
					InputProperties: map[string]schema.PropertySpec{
						"input": {TypeSpec: schema.TypeSpec{Type: "string"}},
					},
					ObjectTypeSpec: schema.ObjectTypeSpec{
						Properties: map[string]schema.PropertySpec{
							"input": {TypeSpec: schema.TypeSpec{Type: "string"}},
						},
					},
				},
			},
		}),
	})

	require.NoError(t, engine.Run(t.Context()))

	var rootProvider, childResource *run.RegisterResourceRequest
	for i := range mock.RegisteredResources {
		switch mock.RegisteredResources[i].Type {
		case "pulumi:providers:simple":
			rootProvider = &mock.RegisteredResources[i]
		case "simple:index:Resource":
			childResource = &mock.RegisteredResources[i]
		}
	}
	require.NotNil(t, rootProvider, "the root provider block should register via the inherited edge")
	require.NotNil(t, childResource, "child resource should register")

	rootRef := "urn:pulumi:test::project::" + rootProvider.Type + "::" + rootProvider.Name +
		"::" + rootProvider.Name + "-id"
	assert.Equal(t, rootRef, childResource.Provider,
		"a child resource with no provider block must inherit the root default provider")
}

// runExpansion executes src against a monitor whose provider-computed
// attributes are unknown during preview and concrete during up: a resource's
// id is unknown / "<name>-id", and its `num` attribute (output-only in the
// schema) is unknown / 2.
func runExpansion(t *testing.T, src string, dryRun bool) *testutil.MockResourceMonitor {
	t.Helper()
	mock, err := tryExpansion(t, src, dryRun, dryRun)
	require.NoError(t, err)
	return mock
}

// tryExpansion is runExpansion with the error surfaced and provider-computed
// attributes controlled independently of dryRun: unknownOutputs=true with
// dryRun=false models an apply whose dependency outputs never resolved (e.g.
// a --target update that skipped the dependency).
func tryExpansion(t *testing.T, src string, dryRun, unknownOutputs bool) (*testutil.MockResourceMonitor, error) {
	t.Helper()

	p := parser.NewParser()
	config, diags := p.ParseSource("test.hcl", []byte(src))
	require.Empty(t, diags)

	num := property.New(property.Computed)
	if !unknownOutputs {
		num = property.New(2.0)
	}
	mock := &testutil.MockResourceMonitor{
		DryRun: dryRun,
		RegisterResourceHandler: func(
			_ context.Context, req run.RegisterResourceRequest,
		) (*run.RegisterResourceResponse, error) {
			id := "" // unknown id
			if !unknownOutputs {
				id = req.Name + "-id"
			}
			return &run.RegisterResourceResponse{
				URN:     urn.URN("urn:pulumi:test::project::" + req.Type + "::" + req.Name),
				ID:      id,
				Outputs: req.Inputs.Set("num", num),
			}, nil
		},
		InvokeHandler: func(_ context.Context, req run.InvokeRequest) (*run.InvokeResponse, error) {
			return &run.InvokeResponse{Return: property.NewMap(map[string]property.Value{
				"result": property.New("result-" + req.Args.Get("filter").AsString()),
			})}, nil
		},
	}
	engine := newTestEngine(t, config, &run.EngineOptions{
		ModuleLoader:    testModuleLoader(t),
		ProjectName:     "test-project",
		StackName:       "dev",
		ResourceMonitor: mock,
		WorkDir:         t.TempDir(),
		RootDir:         t.TempDir(),
		DryRun:          dryRun,
		SchemaLoader: schemaloader.New(t, schema.PackageSpec{
			Name: "aws",
			Resources: map[string]schema.ResourceSpec{
				"aws:index:Instance": {
					InputProperties: map[string]schema.PropertySpec{
						"ami": {TypeSpec: schema.TypeSpec{Type: "string"}},
					},
					ObjectTypeSpec: schema.ObjectTypeSpec{
						Properties: map[string]schema.PropertySpec{
							"ami": {TypeSpec: schema.TypeSpec{Type: "string"}},
							"num": {TypeSpec: schema.TypeSpec{Type: "number"}},
						},
					},
				},
			},
			Functions: map[string]schema.FunctionSpec{
				"aws:index:getInstance": {
					Inputs: &schema.ObjectTypeSpec{
						Properties: map[string]schema.PropertySpec{
							"filter": {TypeSpec: schema.TypeSpec{Type: "string"}},
						},
					},
					Outputs: &schema.ObjectTypeSpec{
						Properties: map[string]schema.PropertySpec{
							"result": {TypeSpec: schema.TypeSpec{Type: "string"}},
						},
					},
				},
			},
		}),
	})
	return mock, engine.Run(t.Context())
}

// instanceInputs maps every registered aws:index:Instance name to its ami
// input, giving one comparable view of which instances an operation produced.
func instanceInputs(mock *testutil.MockResourceMonitor) map[string]string {
	got := map[string]string{}
	for _, r := range mock.RegisteredResources {
		if r.Type == "aws:index:Instance" {
			got[r.Name] = r.Inputs.Get("ami").AsString()
		}
	}
	return got
}

// requireComputedOutput asserts that a stack output resolved to unknown
// during preview — present and computed, rather than an error, an empty
// collection, or missing.
func requireComputedOutput(t *testing.T, mock *testutil.MockResourceMonitor, name string) {
	t.Helper()
	out, ok := mock.StackOutputs.GetOk(name)
	require.Truef(t, ok, "expected %q stack output to be registered", name)
	assert.Truef(t, out.IsComputed(), "expected %q stack output to be unknown, got %v", name, out)
}

func TestEngine_ForEachFromResourceOutput(t *testing.T) {
	t.Parallel()

	// The dependent resource's instance keys derive from ids the provider
	// generates at create time, so the for_each collection is unknown during
	// preview. Terraform rejects this program at plan time; we instead defer:
	// preview omits the instances it cannot enumerate yet and up creates them.
	src := `
resource "aws_instance" "base" {
  count = 2
  ami   = "ami-${count.index}"
}

resource "aws_instance" "dependent" {
  for_each = toset(aws_instance.base[*].id)
  ami      = each.value
}

output "amis" {
  value = [for k, r in aws_instance.dependent : r.ami]
}
`

	t.Run("preview", func(t *testing.T) {
		t.Parallel()
		mock := runExpansion(t, src, true)

		assert.Equal(t, map[string]string{
			"base[0]": "ami-0",
			"base[1]": "ami-1",
		}, instanceInputs(mock))

		requireComputedOutput(t, mock, "amis")
	})

	t.Run("up", func(t *testing.T) {
		t.Parallel()
		mock := runExpansion(t, src, false)

		assert.Equal(t, map[string]string{
			"base[0]":                 "ami-0",
			"base[1]":                 "ami-1",
			`dependent["base[0]-id"]`: "base[0]-id",
			`dependent["base[1]-id"]`: "base[1]-id",
		}, instanceInputs(mock))

		assert.Equal(t, property.New([]property.Value{
			property.New("base[0]-id"),
			property.New("base[1]-id"),
		}), mock.StackOutputs.Get("amis"))
	})
}

func TestEngine_CountFromResourceOutput(t *testing.T) {
	t.Parallel()

	// The dependent resource's count reads a provider-computed number, so it
	// is unknown during preview.
	src := `
resource "aws_instance" "base" {
  ami = "ami-base"
}

resource "aws_instance" "dependent" {
  count = aws_instance.base.num
  ami   = "ami-${count.index}"
}

output "amis" {
  value = aws_instance.dependent[*].ami
}
`

	t.Run("preview", func(t *testing.T) {
		t.Parallel()
		mock := runExpansion(t, src, true)

		assert.Equal(t, map[string]string{
			"base": "ami-base",
		}, instanceInputs(mock))

		requireComputedOutput(t, mock, "amis")
	})

	t.Run("up", func(t *testing.T) {
		t.Parallel()
		mock := runExpansion(t, src, false)

		assert.Equal(t, map[string]string{
			"base":         "ami-base",
			"dependent[0]": "ami-0",
			"dependent[1]": "ami-1",
		}, instanceInputs(mock))

		assert.Equal(t, property.New([]property.Value{
			property.New("ami-0"),
			property.New("ami-1"),
		}), mock.StackOutputs.Get("amis"))
	})
}

func TestEngine_ChainedExpansionFromResourceOutput(t *testing.T) {
	t.Parallel()

	// Unknowns chain through successive expansions: second's count reads a
	// computed attribute of a for_each instance of first, and third's for_each
	// reads the generated ids of the count instances of second. During preview
	// nothing past first can be enumerated; up must expand the whole chain.
	src := `
resource "aws_instance" "first" {
  for_each = toset(["a", "b"])
  ami      = "ami-${each.key}"
}

resource "aws_instance" "second" {
  count = aws_instance.first["a"].num
  ami   = aws_instance.first["a"].id
}

resource "aws_instance" "third" {
  for_each = toset(aws_instance.second[*].id)
  ami      = each.value
}

output "amis" {
  value = [for k, r in aws_instance.third : r.ami]
}
`

	t.Run("preview", func(t *testing.T) {
		t.Parallel()
		mock := runExpansion(t, src, true)

		assert.Equal(t, map[string]string{
			`first["a"]`: "ami-a",
			`first["b"]`: "ami-b",
		}, instanceInputs(mock))

		requireComputedOutput(t, mock, "amis")
	})

	t.Run("up", func(t *testing.T) {
		t.Parallel()
		mock := runExpansion(t, src, false)

		assert.Equal(t, map[string]string{
			`first["a"]`:            "ami-a",
			`first["b"]`:            "ami-b",
			"second[0]":             `first["a"]-id`,
			"second[1]":             `first["a"]-id`,
			`third["second[0]-id"]`: "second[0]-id",
			`third["second[1]-id"]`: "second[1]-id",
		}, instanceInputs(mock))

		assert.Equal(t, property.New([]property.Value{
			property.New("second[0]-id"),
			property.New("second[1]-id"),
		}), mock.StackOutputs.Get("amis"))
	})
}

func TestEngine_DataSourceForEachFromResourceOutput(t *testing.T) {
	t.Parallel()

	// A data source whose for_each reads a generated id must not be invoked
	// during preview; its aggregate value resolves to unknown instead.
	src := `
resource "aws_instance" "base" {
  ami = "ami-base"
}

data "aws_instance" "dependent" {
  for_each = toset([aws_instance.base.id])
  filter   = each.value
}

output "results" {
  value = [for k, d in data.aws_instance.dependent : d.result]
}
`

	t.Run("preview", func(t *testing.T) {
		t.Parallel()
		mock := runExpansion(t, src, true)

		assert.Empty(t, mock.InvokedFunctions)
		requireComputedOutput(t, mock, "results")
	})

	t.Run("up", func(t *testing.T) {
		t.Parallel()
		mock := runExpansion(t, src, false)

		require.Len(t, mock.InvokedFunctions, 1)
		assert.Equal(t, "base-id", mock.InvokedFunctions[0].Args.Get("filter").AsString())

		assert.Equal(t, property.New([]property.Value{
			property.New("result-base-id"),
		}), mock.StackOutputs.Get("results"))
	})
}

func TestEngine_ExpansionUnknownDuringApply(t *testing.T) {
	t.Parallel()

	// During apply an unknown count/for_each can only mean a dependency's
	// outputs never resolved (e.g. a --target update that skipped it); that
	// must be an error, not a silent expansion to zero instances.
	t.Run("count", func(t *testing.T) {
		t.Parallel()
		_, err := tryExpansion(t, `
resource "aws_instance" "base" {
  ami = "ami-base"
}

resource "aws_instance" "dependent" {
  count = aws_instance.base.num
  ami   = "ami-${count.index}"
}
`, false, true)
		require.ErrorContains(t, err, "the count value depends on values that are not yet known")
	})

	t.Run("for_each", func(t *testing.T) {
		t.Parallel()
		_, err := tryExpansion(t, `
resource "aws_instance" "base" {
  ami = "ami-base"
}

resource "aws_instance" "dependent" {
  for_each = toset([tostring(aws_instance.base.num)])
  ami      = each.value
}
`, false, true)
		require.ErrorContains(t, err, "the for_each value depends on values that are not yet known")
	})

	t.Run("data source", func(t *testing.T) {
		t.Parallel()
		_, err := tryExpansion(t, `
resource "aws_instance" "base" {
  ami = "ami-base"
}

data "aws_instance" "dependent" {
  for_each = toset([tostring(aws_instance.base.num)])
  filter   = each.value
}
`, false, true)
		require.ErrorContains(t, err, "the for_each value depends on values that are not yet known")
	})
}

func TestEngine_DataSourceConditions(t *testing.T) {
	t.Parallel()

	t.Run("precondition fail blocks the read", func(t *testing.T) {
		t.Parallel()
		mock, err := tryExpansion(t, `
variable "enabled" {
  type    = bool
  default = false
}

data "aws_instance" "d" {
  filter = "x"

  lifecycle {
    precondition {
      condition     = var.enabled
      error_message = "DATA_PRECONDITION_MSG"
    }
  }
}
`, false, false)
		require.ErrorContains(t, err, "precondition for data.aws_instance.d: DATA_PRECONDITION_MSG")
		assert.Empty(t, mock.InvokedFunctions, "a failed precondition must prevent the read")
	})

	t.Run("postcondition fail after the read", func(t *testing.T) {
		t.Parallel()
		mock, err := tryExpansion(t, `
data "aws_instance" "d" {
  filter = "x"

  lifecycle {
    postcondition {
      condition     = self.result == "nope"
      error_message = "DATA_POSTCONDITION_MSG"
    }
  }
}
`, false, false)
		require.ErrorContains(t, err, "postcondition for data.aws_instance.d: DATA_POSTCONDITION_MSG")
		require.Len(t, mock.InvokedFunctions, 1, "the postcondition must be evaluated against a completed read")
	})

	t.Run("passing conditions", func(t *testing.T) {
		t.Parallel()
		mock, err := tryExpansion(t, `
variable "enabled" {
  type    = bool
  default = true
}

data "aws_instance" "d" {
  filter = "x"

  lifecycle {
    precondition {
      condition     = var.enabled
      error_message = "unreachable"
    }
    postcondition {
      condition     = self.result == "result-x"
      error_message = "unreachable"
    }
  }
}

output "result" {
  value = data.aws_instance.d.result
}
`, false, false)
		require.NoError(t, err)
		assert.Equal(t, property.New("result-x"), mock.StackOutputs.Get("result"))
	})

	t.Run("unknown postcondition defers during preview", func(t *testing.T) {
		t.Parallel()
		_, err := tryExpansion(t, `
data "aws_instance" "d" {
  filter = tostring(aws_instance.base.num)

  lifecycle {
    postcondition {
      condition     = self.result == "nope"
      error_message = "unreachable during preview"
    }
  }
}

resource "aws_instance" "base" {
  ami = "ami-base"
}
`, true, true)
		require.NoError(t, err)
	})
}

// TestEngine_LifecycleRefDependencies: a resource referenced only from a
// lifecycle precondition/postcondition/replace_triggered_by establishes a
// dependency, as in TF — directly, through a local, and through a data
// source's check rule — while body properties carry explicit empty
// PropertyDependencies entries (the reference is ordering-only; an absent
// map would make the engine assume every property depends on it). `self`
// in a postcondition is not a reference and must not break dep collection.
func TestEngine_LifecycleRefDependencies(t *testing.T) {
	t.Parallel()

	mock, err := tryExpansion(t, `
resource "aws_instance" "first" {
  ami = "ami-first"
}

locals {
  first_id = aws_instance.first.id
}

resource "aws_instance" "pre" {
  ami = "ami-pre"
  lifecycle {
    precondition {
      condition     = aws_instance.first.id != ""
      error_message = "first must exist"
    }
  }
}

resource "aws_instance" "post" {
  ami = "ami-post"
  lifecycle {
    postcondition {
      condition     = self.ami != "" && local.first_id != ""
      error_message = "self and first must exist"
    }
  }
}

resource "aws_instance" "replace" {
  ami = "ami-replace"
  lifecycle {
    replace_triggered_by = [aws_instance.first.id]
  }
}

data "aws_instance" "d" {
  filter = "static"
  lifecycle {
    precondition {
      condition     = aws_instance.first.id != ""
      error_message = "first must exist"
    }
  }
}

resource "aws_instance" "reader" {
  ami = data.aws_instance.d.result
}
`, false, false)
	require.NoError(t, err)

	find := func(name string) *run.RegisterResourceRequest {
		for i := range mock.RegisteredResources {
			r := &mock.RegisteredResources[i]
			if r.Type == "aws:index:Instance" && r.Name == name {
				return r
			}
		}
		return nil
	}
	firstURN := "urn:pulumi:test::project::aws:index:Instance::first"

	for _, name := range []string{"pre", "post", "replace"} {
		r := find(name)
		require.NotNilf(t, r, "%s resource should be registered", name)
		assert.Equal(t, []string{firstURN}, r.Dependencies)
		assert.Equal(t, map[string][]string{"ami": nil}, r.PropertyDependencies)
	}

	reader := find("reader")
	require.NotNil(t, reader, "reader resource should be registered")
	assert.Equal(t, []string{firstURN}, reader.Dependencies)
	assert.Equal(t, map[string][]string{"ami": {firstURN}}, reader.PropertyDependencies)
}

// TestEngine_ProvisionerRefDependencies: a resource referenced only from a
// provisioner command, a resource-level connection block, or a provisioner's
// connection override establishes a dependency — directly and through a local
// — while body properties carry explicit empty PropertyDependencies entries
// (the reference is ordering-only).
// The referent is declared last so the edge, not source order, must sequence
// its evaluation before the referencing resources register. `self` in a
// create-time provisioner is not a reference and must not break dep
// collection; a destroy-time provisioner may only reference the resource
// itself and contributes no dependency.
func TestEngine_ProvisionerRefDependencies(t *testing.T) {
	t.Parallel()

	mock, err := tryExpansion(t, `
resource "aws_instance" "prov" {
  ami = "ami-prov"
  provisioner "local-exec" {
    command = "echo ${aws_instance.first.ami} ${self.id}"
  }
}

resource "aws_instance" "conn" {
  ami = "ami-conn"
  connection {
    host = aws_instance.first.ami
  }
}

resource "aws_instance" "provconn" {
  ami = "ami-provconn"
  provisioner "local-exec" {
    command = "echo provconn"
    connection {
      host = local.first_ami
    }
  }
}

resource "aws_instance" "dprov" {
  ami = "ami-dprov"
  provisioner "local-exec" {
    when    = destroy
    command = "echo ${aws_instance.first.ami}"
  }
}

locals {
  first_ami = aws_instance.first.ami
}

resource "aws_instance" "first" {
  ami = "ami-first"
}
`, false, false)
	require.NoError(t, err)

	find := func(name string) *run.RegisterResourceRequest {
		for i := range mock.RegisteredResources {
			r := &mock.RegisteredResources[i]
			if r.Type == "aws:index:Instance" && r.Name == name {
				return r
			}
		}
		return nil
	}
	firstURN := "urn:pulumi:test::project::aws:index:Instance::first"

	for _, name := range []string{"prov", "conn", "provconn"} {
		r := find(name)
		require.NotNilf(t, r, "%s resource should be registered", name)
		assert.Equal(t, []string{firstURN}, r.Dependencies)
		assert.Equal(t, map[string][]string{"ami": nil}, r.PropertyDependencies)
	}

	dprov := find("dprov")
	require.NotNil(t, dprov, "dprov resource should be registered")
	assert.Empty(t, dprov.Dependencies)
}
