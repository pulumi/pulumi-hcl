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

package parser

import (
	"maps"
	"slices"
	"testing"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/pulumi/pulumi-hcl/pkg/hcl/ast"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zclconf/go-cty/cty"
)

func TestParseBasicConfig(t *testing.T) {
	t.Parallel()
	src := []byte(`
terraform {
  required_providers {
    aws = {
      source  = "pulumi/aws"
      version = "6.0.0"
    }
  }
}

provider "aws" {
  region = "us-west-2"
}

variable "bucket_name" {
  type        = string
  description = "Name of the S3 bucket"
  default     = "my-bucket"
}

locals {
  common_tags = {
    Environment = "dev"
    ManagedBy   = "Pulumi"
  }
}

resource "aws_s3_bucket" "example" {
  bucket = var.bucket_name
  tags   = local.common_tags

  lifecycle {
    prevent_destroy = true
  }
}

data "aws_ami" "ubuntu" {
  most_recent = true
}

output "bucket_arn" {
  value       = aws_s3_bucket.example.arn
  description = "The ARN of the bucket"
  sensitive   = false
}

module "vpc" {
  source  = "./modules/vpc"
  version = "1.0.0"

  cidr_block = "10.0.0.0/16"
}
`)

	p := NewParser()
	config, diags := p.ParseSource("test.hcl", src)

	if diags.HasErrors() {
		for _, d := range diags {
			t.Errorf("Diagnostic: %s: %s", d.Summary, d.Detail)
		}
		t.FailNow()
	}

	// Verify terraform block
	if config.Terraform == nil {
		t.Error("Expected terraform block")
	} else {
		if len(config.Terraform.RequiredProviders) != 1 {
			t.Errorf("Expected 1 required provider, got %d", len(config.Terraform.RequiredProviders))
		}
		if rp, ok := config.Terraform.RequiredProviders["aws"]; ok {
			if rp.Source != "pulumi/aws" {
				t.Errorf("Expected source 'pulumi/aws', got %q", rp.Source)
			}
		} else {
			t.Error("Expected 'aws' in required providers")
		}
	}

	// Verify provider
	if len(config.Providers) != 1 {
		t.Errorf("Expected 1 provider, got %d", len(config.Providers))
	}
	if _, ok := config.Providers["aws"]; !ok {
		t.Error("Expected 'aws' provider")
	}

	// Verify variable
	if len(config.Variables) != 1 {
		t.Errorf("Expected 1 variable, got %d", len(config.Variables))
	}
	if v, ok := config.Variables["bucket_name"]; ok {
		if v.Description != "Name of the S3 bucket" {
			t.Errorf("Unexpected variable description: %q", v.Description)
		}
	} else {
		t.Error("Expected 'bucket_name' variable")
	}

	// Verify locals
	if len(config.Locals) != 1 {
		t.Errorf("Expected 1 local, got %d", len(config.Locals))
	}
	if _, ok := config.Locals["common_tags"]; !ok {
		t.Error("Expected 'common_tags' local")
	}

	// Verify resource
	if len(config.Resources) != 1 {
		t.Errorf("Expected 1 resource, got %d", len(config.Resources))
	}
	if r, ok := config.Resources["aws_s3_bucket.example"]; ok {
		if r.Type != "aws_s3_bucket" {
			t.Errorf("Unexpected resource type: %q", r.Type)
		}
		if r.Name != "example" {
			t.Errorf("Unexpected resource name: %q", r.Name)
		}
		if r.Lifecycle == nil {
			t.Error("Expected lifecycle block")
		} else if r.Lifecycle.PreventDestroy == nil {
			t.Error("Expected prevent_destroy to be set")
		} else if v, _ := r.Lifecycle.PreventDestroy.Value(nil); !cty.True.RawEquals(v) {
			t.Error("Expected prevent_destroy to be true")
		}
	} else {
		t.Error("Expected 'aws_s3_bucket.example' resource")
	}

	// Verify data source
	if len(config.DataSources) != 1 {
		t.Errorf("Expected 1 data source, got %d", len(config.DataSources))
	}
	if _, ok := config.DataSources["aws_ami.ubuntu"]; !ok {
		t.Error("Expected 'aws_ami.ubuntu' data source")
	}

	// Verify output
	if len(config.Outputs) != 1 {
		t.Errorf("Expected 1 output, got %d", len(config.Outputs))
	}
	if o, ok := config.Outputs["bucket_arn"]; ok {
		if o.Description != "The ARN of the bucket" {
			t.Errorf("Unexpected output description: %q", o.Description)
		}
	} else {
		t.Error("Expected 'bucket_arn' output")
	}

	// Verify module
	if len(config.Modules) != 1 {
		t.Errorf("Expected 1 module, got %d", len(config.Modules))
	}
	if m, ok := config.Modules["vpc"]; ok {
		if m.Source != "./modules/vpc" {
			t.Errorf("Unexpected module source: %q", m.Source)
		}
	} else {
		t.Error("Expected 'vpc' module")
	}
}

func TestParseTimeouts(t *testing.T) {
	t.Parallel()
	src := []byte(`
resource "aws_instance" "web" {
  timeouts {
    create  = "1h"
    default = "10m"
  }
}
`)
	config, diags := NewParser().ParseSource("test.hcl", src)
	require.False(t, diags.HasErrors(), diags.Error())

	timeouts := config.Resources["aws_instance.web"].Timeouts
	require.NotNil(t, timeouts)
	assert.NotNil(t, timeouts.Create)
	assert.NotNil(t, timeouts.Default)
	assert.Nil(t, timeouts.Read)
	assert.Nil(t, timeouts.Update)
	assert.Nil(t, timeouts.Delete)
}

func TestParseProvisioners(t *testing.T) {
	t.Parallel()
	src := []byte(`
resource "aws_instance" "web" {
  ami           = "ami-123"
  instance_type = "t3.micro"

  connection {
    type        = "ssh"
    user        = "ubuntu"
    host        = self.public_ip
  }

  provisioner "local-exec" {
    command = "echo hello"
  }

  provisioner "remote-exec" {
    inline = ["apt update"]
    when   = create
  }

  provisioner "file" {
    source      = "local.txt"
    destination = "/tmp/remote.txt"
  }
}
`)

	p := NewParser()
	config, diags := p.ParseSource("test.hcl", src)

	if diags.HasErrors() {
		for _, d := range diags {
			t.Errorf("Diagnostic: %s: %s", d.Summary, d.Detail)
		}
		t.FailNow()
	}

	r, ok := config.Resources["aws_instance.web"]
	if !ok {
		t.Fatal("Expected 'aws_instance.web' resource")
	}

	if r.Connection == nil {
		t.Error("Expected connection block")
	} else if r.Connection.Type != "ssh" {
		t.Errorf("Expected connection type 'ssh', got %q", r.Connection.Type)
	}

	if len(r.Provisioners) != 3 {
		t.Errorf("Expected 3 provisioners, got %d", len(r.Provisioners))
	}

	if r.Provisioners[0].Type != "local-exec" {
		t.Errorf("Expected first provisioner to be 'local-exec', got %q", r.Provisioners[0].Type)
	}

	if r.Provisioners[1].Type != "remote-exec" {
		t.Errorf("Expected second provisioner to be 'remote-exec', got %q", r.Provisioners[1].Type)
	}
	if r.Provisioners[1].When != "create" {
		t.Errorf("Expected when='create', got %q", r.Provisioners[1].When)
	}

	if r.Provisioners[2].Type != "file" {
		t.Errorf("Expected third provisioner to be 'file', got %q", r.Provisioners[2].Type)
	}
}

// TestRequiredProvidersShapes pins down how each documented required_providers
// shape parses, and which of them route through the terraform-provider plugin.
// Sources non-prefixed by "pulumi/" (including a missing source) are TF-style
// and default to "hashicorp/<name>".
func TestRequiredProvidersShapes(t *testing.T) {
	t.Parallel()
	const src = `terraform {
  required_providers {
    ex1 = "~> 4.0"
    ex2 = { version = "~> 4.0" }
    ex3 = { source = "example/abc" }
    ex4 = { source = "example/def", version = "5.0.0" }
    ex5 = { source = "pulumi/ghi", version = "6.7.8" }
  }
}`
	cfg, diags := NewParser().ParseSource("test.hcl", []byte(src))
	require.False(t, diags.HasErrors(), "diags: %v", diags)
	require.NotNil(t, cfg.Terraform)

	got := cfg.Terraform.RequiredProviders
	for _, v := range got {
		v.DeclRange = hcl.Range{}
	}
	assert.Equal(t, map[string]*ast.RequiredProvider{
		"ex1": {
			Name:    "ex1",
			Version: "~> 4.0",
		},
		"ex2": {
			Name:    "ex2",
			Version: "~> 4.0",
		},
		"ex3": {
			Name:   "ex3",
			Source: "example/abc",
		},
		"ex4": {
			Name:    "ex4",
			Source:  "example/def",
			Version: "5.0.0",
		},
		"ex5": {
			Name:    "ex5",
			Source:  "pulumi/ghi",
			Version: "6.7.8",
		},
	}, got)
}

func TestRequiredProvidersVersionMustBeSemver(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		hcl     string
		wantErr bool
	}{
		{
			name: "pulumi_source_semver_ok",
			hcl: `terraform {
  required_providers {
    aws = { source = "pulumi/aws", version = "6.0.0" }
  }
}`,
		},
		{
			name: "pulumi_source_constraint_rejected",
			hcl: `terraform {
  required_providers {
    aws = { source = "pulumi/aws", version = "~> 6.0" }
  }
}`,
			wantErr: true,
		},
		{
			// Empty source now defaults to "hashicorp/<name>" (TF-style),
			// so a constraint-form version is fine.
			name: "no_source_constraint_ok",
			hcl: `terraform {
  required_providers {
    aws = { version = "~> 6.0" }
  }
}`,
		},
		{
			name: "tf_source_constraint_ok",
			hcl: `terraform {
  required_providers {
    random = { source = "hashicorp/random", version = "~> 4.0.0" }
  }
}`,
		},
		{
			name: "pulumi_source_no_version_ok",
			hcl: `terraform {
  required_providers {
    aws = { source = "pulumi/aws" }
  }
}`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, diags := NewParser().ParseSource("test.hcl", []byte(tc.hcl))
			if tc.wantErr {
				require.True(t, diags.HasErrors(), "expected parse errors, got %v", diags)
				require.Equal(t, "Invalid provider version", diags[0].Summary)
			} else {
				require.False(t, diags.HasErrors(), "unexpected parse errors: %v", diags)
			}
		})
	}
}

func TestTerraformBlockCompat(t *testing.T) {
	t.Parallel()

	t.Run("multiple_terraform_blocks_merge", func(t *testing.T) {
		t.Parallel()
		src := `
terraform {
  backend "remote" {
    organization = "pulumi"
  }
}

terraform {
  required_providers {
    aws = { source = "hashicorp/aws", version = "6.19.0" }
  }
}`
		cfg, diags := NewParser().ParseSource("test.hcl", []byte(src))
		require.False(t, diags.HasErrors(), "unexpected errors: %v", diags)
		require.NotNil(t, cfg.Terraform)
		require.Contains(t, cfg.Terraform.RequiredProviders, "aws")
	})

	t.Run("backend_warns_continues", func(t *testing.T) {
		t.Parallel()
		src := `
terraform {
  backend "remote" {
    organization = "pulumi"
  }
  required_providers {
    aws = { source = "hashicorp/aws", version = "6.19.0" }
  }
}`
		cfg, diags := NewParser().ParseSource("test.hcl", []byte(src))
		require.False(t, diags.HasErrors(), "unexpected errors: %v", diags)
		require.Contains(t, cfg.Terraform.RequiredProviders, "aws")
		var found bool
		for _, d := range diags {
			if d.Severity == hcl.DiagWarning && d.Summary == "Ignoring terraform backend block" {
				found = true
				break
			}
		}
		require.True(t, found, "expected warning for backend block, got %v", diags)
	})

	t.Run("provider_meta_warns_continues", func(t *testing.T) {
		t.Parallel()
		src := `
terraform {
  required_providers {
    aws = { source = "hashicorp/aws", version = ">= 6.0" }
  }
  provider_meta "aws" {
    user_agent = ["github.com/terraform-aws-modules/terraform-aws-sqs"]
  }
}`
		cfg, diags := NewParser().ParseSource("test.hcl", []byte(src))
		require.False(t, diags.HasErrors(), "unexpected errors: %v", diags)
		require.Contains(t, cfg.Terraform.RequiredProviders, "aws")
		var found bool
		for _, d := range diags {
			if d.Severity == hcl.DiagWarning && d.Summary == "Ignoring terraform provider_meta block" {
				found = true
				break
			}
		}
		require.True(t, found, "expected warning for provider_meta block, got %v", diags)
	})

	t.Run("duplicate_required_provider_across_blocks", func(t *testing.T) {
		t.Parallel()
		src := `
terraform {
  required_providers {
    aws = { source = "hashicorp/aws", version = "6.19.0" }
  }
}

terraform {
  required_providers {
    aws = { source = "hashicorp/aws", version = "7.0.0" }
  }
}`
		_, diags := NewParser().ParseSource("test.hcl", []byte(src))
		require.True(t, diags.HasErrors(), "expected duplicate-provider error, got %v", diags)
		require.Equal(t, "Duplicate required_providers entry", diags[0].Summary)
	})

	t.Run("duplicate_required_version_range_across_blocks", func(t *testing.T) {
		t.Parallel()
		src := `
terraform {
  required_version_range = ">= 1.0.0"
}

terraform {
  required_version_range = ">= 2.0.0"
}`
		_, diags := NewParser().ParseSource("test.hcl", []byte(src))
		require.True(t, diags.HasErrors(), "expected duplicate-required_version_range error, got %v", diags)
		require.Equal(t, "Duplicate required_version_range attribute", diags[0].Summary)
	})

	t.Run("required_version_warns_continues", func(t *testing.T) {
		t.Parallel()
		src := `
terraform {
  required_version = ">= 1.0.0"
  required_providers {
    aws = { source = "hashicorp/aws", version = "6.19.0" }
  }
}`
		cfg, diags := NewParser().ParseSource("test.hcl", []byte(src))
		require.False(t, diags.HasErrors(), "unexpected errors: %v", diags)
		require.Contains(t, cfg.Terraform.RequiredProviders, "aws")
		var found bool
		for _, d := range diags {
			if d.Severity == hcl.DiagWarning && d.Summary == "Unsupported attribute: required_version" {
				found = true
				break
			}
		}
		require.True(t, found, "expected warning for required_version, got %v", diags)
	})

	t.Run("experiments_warns_continues", func(t *testing.T) {
		t.Parallel()
		src := `
terraform {
  experiments = [module_variable_optional_attrs]
  required_providers {
    aws = { source = "hashicorp/aws", version = "6.19.0" }
  }
}`
		cfg, diags := NewParser().ParseSource("test.hcl", []byte(src))
		require.False(t, diags.HasErrors(), "unexpected errors: %v", diags)
		require.Contains(t, cfg.Terraform.RequiredProviders, "aws")
		var found bool
		for _, d := range diags {
			if d.Severity == hcl.DiagWarning && d.Summary == "Ignoring terraform experiments argument" {
				found = true
				break
			}
		}
		require.True(t, found, "expected warning for experiments, got %v", diags)
	})
}

func TestLanguageBlock(t *testing.T) {
	t.Parallel()

	t.Run("pulumi_constraint_parsed", func(t *testing.T) {
		t.Parallel()
		src := `
language {
  compatible_with {
    opentofu = ">= 1.12"
    pulumi   = ">= 3.0.0"
  }
}`
		cfg, diags := NewParser().ParseSource("test.hcl", []byte(src))
		require.False(t, diags.HasErrors(), "unexpected errors: %v", diags)
		require.NotNil(t, cfg.Language)
		val, valDiags := cfg.Language.CompatibleWithPulumi.Value(nil)
		require.False(t, valDiags.HasErrors(), valDiags.Error())
		assert.Equal(t, cty.StringVal(">= 3.0.0"), val)
	})

	t.Run("other_software_ignored", func(t *testing.T) {
		t.Parallel()
		src := `
language {
  compatible_with {
    opentofu       = ">= 1.12"
    other_software = ["not", "a", "version"]
  }
}`
		cfg, diags := NewParser().ParseSource("test.hcl", []byte(src))
		require.False(t, diags.HasErrors(), "unexpected errors: %v", diags)
		assert.Nil(t, cfg.Language)
	})

	t.Run("duplicate_pulumi_across_blocks", func(t *testing.T) {
		t.Parallel()
		src := `
language {
  compatible_with {
    pulumi = ">= 3.0.0"
  }
}

language {
  compatible_with {
    pulumi = ">= 4.0.0"
  }
}`
		_, diags := NewParser().ParseSource("test.hcl", []byte(src))
		require.True(t, diags.HasErrors(), "expected duplicate-pulumi error, got %v", diags)
		require.Equal(t, "Duplicate compatible_with pulumi argument", diags[0].Summary)
	})

	t.Run("current_edition_accepted", func(t *testing.T) {
		t.Parallel()
		src := `
language {
  edition = tofu2024
}`
		_, diags := NewParser().ParseSource("test.hcl", []byte(src))
		require.False(t, diags.HasErrors(), "unexpected errors: %v", diags)
	})

	t.Run("future_edition_rejected", func(t *testing.T) {
		t.Parallel()
		src := `
language {
  edition = tofu2030
}`
		_, diags := NewParser().ParseSource("test.hcl", []byte(src))
		require.True(t, diags.HasErrors(), "expected unsupported-edition error, got %v", diags)
		require.Equal(t, "Unsupported language edition", diags[0].Summary)
	})

	t.Run("experiments_rejected", func(t *testing.T) {
		t.Parallel()
		src := `
language {
  experiments = [some_experiment]
}`
		_, diags := NewParser().ParseSource("test.hcl", []byte(src))
		require.True(t, diags.HasErrors(), "expected unknown-experiment error, got %v", diags)
		require.Equal(t, "Unknown experiment keyword", diags[0].Summary)
	})
}

func TestParseUnknownBlockType(t *testing.T) {
	t.Parallel()
	src := []byte(`
widget "x" {
}
`)
	p := NewParser()
	_, diags := p.ParseSource("test.hcl", src)
	require.True(t, diags.HasErrors())
	require.Equal(t, "Unsupported block type", diags[0].Summary)
	require.Equal(t, `Blocks of type "widget" are not expected here.`, diags[0].Detail)
}

func TestParseProvisionerInvalidWhen(t *testing.T) {
	t.Parallel()
	src := []byte(`
resource "aws_instance" "web" {
  provisioner "local-exec" {
    command = "true"
    when    = "sometimes"
  }
}
`)
	p := NewParser()
	_, diags := p.ParseSource("test.hcl", src)
	require.True(t, diags.HasErrors())
	require.Equal(t, "Invalid \"when\" value", diags[0].Summary)
}

func TestParseProvisionerInvalidOnFailure(t *testing.T) {
	t.Parallel()
	src := []byte(`
resource "aws_instance" "web" {
  provisioner "local-exec" {
    command    = "true"
    on_failure = "panic"
  }
}
`)
	p := NewParser()
	_, diags := p.ParseSource("test.hcl", src)
	require.True(t, diags.HasErrors())
	require.Equal(t, "Invalid \"on_failure\" value", diags[0].Summary)
}

// justAttrNames returns the sorted attribute names visible via JustAttributes.
func justAttrNames(t *testing.T, body hcl.Body) []string {
	t.Helper()
	attrs, _ := body.JustAttributes()
	return slices.Sorted(maps.Keys(attrs))
}

func TestParseEscapingBlocks(t *testing.T) {
	t.Parallel()
	src := []byte(`
provider "aws" {
  region = "us-west-2"
  _ {
    alias = "not-an-alias"
  }
}

resource "aws_instance" "web" {
  count = 2
  ami   = "ami-123"

  lifecycle {
    prevent_destroy = true
  }

  _ {
    # Set both as a meta-argument above and as a resource-type-specific
    # argument here; both must be populated.
    count = "not-a-count"

    # A literal block, not the lifecycle meta-argument block.
    lifecycle {
      prevent_destroy = "not-a-bool"
    }
  }

  provisioner "local-exec" {
    when = destroy
    _ {
      command = "true"
      when    = "not-a-when"
    }
  }
}

data "aws_ami" "ubuntu" {
  _ {
    provider = "not-a-provider"
  }
}

module "vpc" {
  source = "./modules/vpc"
  _ {
    version = "not-a-version"
  }
}
`)
	p := NewParser()
	config, diags := p.ParseSource("test.hcl", src)
	require.False(t, diags.HasErrors(), "%v", diags)

	provider := config.Providers["aws"]
	require.NotNil(t, provider)
	assert.Equal(t, "", provider.Alias)
	assert.Equal(t, []string{"alias", "region"}, justAttrNames(t, provider.Config))

	res := config.Resources["aws_instance.web"]
	require.NotNil(t, res)
	// The meta-argument and the escaped argument of the same name are both
	// populated: count = 2 drives expansion, the escaped count is config.
	require.NotNil(t, res.Count)
	countVal, valDiags := res.Count.Value(nil)
	require.False(t, valDiags.HasErrors())
	assert.True(t, cty.NumberIntVal(2).RawEquals(countVal))
	require.NotNil(t, res.Lifecycle)
	require.NotNil(t, res.Lifecycle.PreventDestroy)
	pdVal, valDiags := res.Lifecycle.PreventDestroy.Value(nil)
	require.False(t, valDiags.HasErrors())
	assert.True(t, cty.True.RawEquals(pdVal))

	content, _, contentDiags := res.Config.PartialContent(&hcl.BodySchema{
		Attributes: []hcl.AttributeSchema{{Name: "ami"}, {Name: "count"}},
		Blocks:     []hcl.BlockHeaderSchema{{Type: "lifecycle"}},
	})
	require.False(t, contentDiags.HasErrors(), "%v", contentDiags)
	escapedCount, valDiags := content.Attributes["count"].Expr.Value(nil)
	require.False(t, valDiags.HasErrors())
	assert.Equal(t, cty.StringVal("not-a-count"), escapedCount)
	// The lifecycle block written inside `_` is literal resource config, not
	// the meta-argument block.
	require.Len(t, content.Blocks, 1)

	require.Len(t, res.Provisioners, 1)
	prov := res.Provisioners[0]
	assert.Equal(t, "destroy", prov.When)
	assert.Equal(t, []string{"command", "when"}, justAttrNames(t, prov.Config))

	ds := config.DataSources["aws_ami.ubuntu"]
	require.NotNil(t, ds)
	assert.Nil(t, ds.Provider)
	assert.Equal(t, []string{"provider"}, justAttrNames(t, ds.Config))

	mod := config.Modules["vpc"]
	require.NotNil(t, mod)
	assert.Equal(t, "", mod.Version)
	assert.Equal(t, []string{"version"}, justAttrNames(t, mod.Config))
}

func TestParseDuplicateEscapingBlock(t *testing.T) {
	t.Parallel()
	src := []byte(`
resource "aws_instance" "web" {
  _ {
    count = "a"
  }
  _ {
    provider = "b"
  }
}
`)
	p := NewParser()
	_, diags := p.ParseSource("test.hcl", src)
	require.True(t, diags.HasErrors())
	require.Equal(t, "Duplicate escaping block", diags[0].Summary)
}

func TestParseMetaArguments(t *testing.T) {
	t.Parallel()
	src := []byte(`
resource "aws_instance" "web" {
  count = 3

  ami           = "ami-123"
  instance_type = "t3.micro"

  depends_on = [aws_vpc.main, aws_subnet.primary]

  lifecycle {
    create_before_destroy = true
    ignore_changes        = [tags, ami]
  }
}

resource "aws_instance" "app" {
  for_each = toset(["a", "b", "c"])

  ami           = "ami-456"
  instance_type = "t3.small"

  lifecycle {
    ignore_changes = all
  }
}
`)

	p := NewParser()
	config, diags := p.ParseSource("test.hcl", src)

	if diags.HasErrors() {
		for _, d := range diags {
			t.Errorf("Diagnostic: %s: %s", d.Summary, d.Detail)
		}
		t.FailNow()
	}

	// Check count resource
	r1, ok := config.Resources["aws_instance.web"]
	if !ok {
		t.Fatal("Expected 'aws_instance.web' resource")
	}

	if r1.Count == nil {
		t.Error("Expected count expression")
	}

	if len(r1.DependsOn) != 2 {
		t.Errorf("Expected 2 depends_on entries, got %d", len(r1.DependsOn))
	}

	if r1.Lifecycle == nil {
		t.Error("Expected lifecycle block")
	} else {
		if r1.Lifecycle.CreateBeforeDestroy == nil || !*r1.Lifecycle.CreateBeforeDestroy {
			t.Error("Expected create_before_destroy to be true")
		}
		if len(r1.Lifecycle.IgnoreChanges) != 2 {
			t.Errorf("Expected 2 ignore_changes, got %d", len(r1.Lifecycle.IgnoreChanges))
		}
	}

	// Check for_each resource
	r2, ok := config.Resources["aws_instance.app"]
	if !ok {
		t.Fatal("Expected 'aws_instance.app' resource")
	}

	if r2.ForEach == nil {
		t.Error("Expected for_each expression")
	}

	if r2.Lifecycle == nil {
		t.Error("Expected lifecycle block")
	} else if !r2.Lifecycle.IgnoreAllChanges {
		t.Error("Expected ignore_changes = all")
	}
}

func TestParseCheckBlock(t *testing.T) {
	t.Parallel()
	src := []byte(`
check "health" {
  assert {
    condition     = 1 == 1
    error_message = "first"
  }

  assert {
    condition     = 2 == 2
    error_message = "second"
  }
}
`)
	p := NewParser()
	config, diags := p.ParseSource("test.tf", src)
	require.False(t, diags.HasErrors(), diags.Error())

	check, ok := config.Checks["health"]
	require.True(t, ok, "expected check %q", "health")
	assert.Equal(t, "health", check.Name)
	require.Len(t, check.Asserts, 2)
	for _, a := range check.Asserts {
		assert.NotNil(t, a.Condition)
		assert.NotNil(t, a.ErrorMessage)
	}
}

func TestParseCheckBlockMissingAssert(t *testing.T) {
	t.Parallel()
	src := []byte(`
check "empty" {
}
`)
	p := NewParser()
	config, diags := p.ParseSource("test.tf", src)
	require.True(t, diags.HasErrors())
	assert.Equal(t, "Missing assert block", diags[0].Summary)
	_, ok := config.Checks["empty"]
	assert.False(t, ok, "a check with no assert block must not be recorded")
}

func TestParseCheckBlockDuplicate(t *testing.T) {
	t.Parallel()
	src := []byte(`
check "dup" {
  assert {
    condition     = true
    error_message = "a"
  }
}

check "dup" {
  assert {
    condition     = true
    error_message = "b"
  }
}
`)
	p := NewParser()
	_, diags := p.ParseSource("test.tf", src)
	require.True(t, diags.HasErrors())
	assert.Equal(t, "Duplicate check", diags[0].Summary)
}

func TestParseCheckBlockScopedDataSource(t *testing.T) {
	t.Parallel()
	src := []byte(`
check "with_data" {
  data "http" "example" {
    url = "https://example.com"
  }

  assert {
    condition     = data.http.example.status_code == 200
    error_message = "a"
  }
}
`)
	p := NewParser()
	config, diags := p.ParseSource("test.tf", src)
	require.False(t, diags.HasErrors(), diags.Error())

	check, ok := config.Checks["with_data"]
	require.True(t, ok, "expected check %q", "with_data")
	require.NotNil(t, check.DataResource)
	assert.Equal(t, "http", check.DataResource.Type)
	assert.Equal(t, "example", check.DataResource.Name)

	// A check's scoped data source must not leak into the global data sources.
	_, leaked := config.DataSources["http.example"]
	assert.False(t, leaked, "scoped data source must not be registered globally")
}

func TestParseCheckBlockMultipleDataSources(t *testing.T) {
	t.Parallel()
	src := []byte(`
check "two_data" {
  data "http" "a" {
    url = "https://example.com"
  }

  data "http" "b" {
    url = "https://example.org"
  }

  assert {
    condition     = data.http.a.status_code == 200
    error_message = "a"
  }
}
`)
	p := NewParser()
	_, diags := p.ParseSource("test.tf", src)
	require.True(t, diags.HasErrors())
	assert.Equal(t, "Multiple data resource blocks", diags[0].Summary)
}

func TestParseProviderCallReserved(t *testing.T) {
	t.Parallel()
	src := []byte(`
provider "call" {
  value = "x"
}
`)

	p := NewParser()
	_, diags := p.ParseSource("test.tf", src)
	if !diags.HasErrors() {
		t.Fatal("expected parse error for provider named \"call\"")
	}
	if got := diags.Error(); !contains(got, `"call" is reserved`) {
		t.Errorf("parser error missing expected substring; got: %q", got)
	}
}

// TF 1.3+ allows `optional(<type>, <default>)` for object attributes.
// HCL's typeexpr.TypeConstraint rejects the 2-arg form; we use the
// TypeConstraintWithDefaults variant. aws-ia/vpc, aws-ia/ipam, and many
// other registry modules use this idiom.
func TestVariableTypeOptionalWithDefault(t *testing.T) {
	t.Parallel()
	src := []byte(`
variable "subnets" {
  type = object({
    name    = string
    cidr    = optional(string, "10.0.0.0/16")
    enabled = optional(bool, true)
  })
}
`)
	_, diags := NewParser().ParseSource("test.tf", src)
	if diags.HasErrors() {
		t.Fatalf("unexpected errors: %v", diags.Errs())
	}
}

// The pre-0.12 bare keywords `list` and `map` are shorthand for `list(any)`
// and `map(any)`; typeexpr itself rejects them.
func TestVariableTypeBareListMap(t *testing.T) {
	t.Parallel()
	src := []byte(`
variable "l" {
  type = list
}

variable "m" {
  type = map
}
`)
	config, diags := NewParser().ParseSource("test.tf", src)
	require.False(t, diags.HasErrors(), "unexpected errors: %v", diags.Errs())
	assert.Equal(t, cty.List(cty.DynamicPseudoType), config.Variables["l"].TypeConstraint)
	assert.Equal(t, cty.Map(cty.DynamicPseudoType), config.Variables["m"].TypeConstraint)
}

func TestParseEphemeral(t *testing.T) {
	t.Parallel()
	src := []byte(`
variable "token" {
  type      = string
  ephemeral = true
}

variable "plain" {
  type      = string
  ephemeral = false
}

output "token_out" {
  value     = var.token
  ephemeral = true
}
`)
	config, diags := NewParser().ParseSource("test.tf", src)
	require.False(t, diags.HasErrors(), "unexpected errors: %v", diags.Errs())
	assert.True(t, config.Variables["token"].Ephemeral)
	assert.False(t, config.Variables["token"].Sensitive)
	assert.False(t, config.Variables["plain"].Ephemeral)
	assert.True(t, config.Outputs["token_out"].Ephemeral)
	assert.False(t, config.Outputs["token_out"].Sensitive)
}

// A non-bool ephemeral value is a hard error; a string that converts to bool
// ("true") is accepted. Both mirror OpenTofu's bool decoding.
func TestParseEphemeralNonBool(t *testing.T) {
	t.Parallel()

	_, diags := NewParser().ParseSource("test.tf", []byte(`
variable "token" {
  type      = string
  ephemeral = "yes"
}
`))
	require.True(t, diags.HasErrors())
	assert.Equal(t, "Unsuitable value type", diags[0].Summary)

	_, diags = NewParser().ParseSource("test.tf", []byte(`
output "token_out" {
  value     = "v"
  ephemeral = 1
}
`))
	require.True(t, diags.HasErrors())
	assert.Equal(t, "Unsuitable value type", diags[0].Summary)

	config, diags := NewParser().ParseSource("test.tf", []byte(`
variable "token" {
  type      = string
  ephemeral = "true"
}
`))
	require.False(t, diags.HasErrors(), "unexpected errors: %v", diags.Errs())
	assert.True(t, config.Variables["token"].Ephemeral)
}

// TestParseConstantAttributeTypes pins that constant-valued block attributes
// are decoded strictly: a value of the wrong type is a hard error rather than
// being silently ignored, and a value convertible to the target type (e.g.
// "true" for a bool) is accepted.
func TestParseConstantAttributeTypes(t *testing.T) {
	t.Parallel()

	errCases := map[string]string{
		"variable sensitive":       `variable "v" { sensitive = "yes" }`,
		"variable nullable":        `variable "v" { nullable = "yes" }`,
		"variable description":     `variable "v" { description = ["x"] }`,
		"output sensitive":         `output "o" { value = "v", sensitive = 3 }`,
		"output description":       `output "o" { value = "v", description = {} }`,
		"lifecycle cbd":            `resource "r" "r" { lifecycle { create_before_destroy = "x" } }`,
		"provider alias":           `provider "p" { alias = ["a"] }`,
		"module source":            `module "m" { source = ["./m"] }`,
		"required_providers src":   `terraform { required_providers { p = { source = ["x"] } } }`,
		"connection type":          `resource "r" "r" { connection { type = ["ssh"] } }`,
		"component name":           `terraform { component { name = ["C"] } }`,
		"package version non-str":  `terraform { package { name = "p", version = ["1"] } }`,
		"resource pulumi importId": `resource "r" "r" { pulumi { import_id = ["i"] } }`,
	}
	for name, src := range errCases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, diags := NewParser().ParseSource("test.tf", []byte(src))
			require.True(t, diags.HasErrors(), "expected a type error for: %s", src)
		})
	}

	// Values convertible to the target type are accepted, and site-specific
	// validation still runs on the decoded value.
	config, diags := NewParser().ParseSource("test.tf", []byte(`
variable "v" {
  sensitive = "true"
  nullable  = "false"
}
`))
	require.False(t, diags.HasErrors(), "unexpected errors: %v", diags.Errs())
	assert.True(t, config.Variables["v"].Sensitive)
	assert.False(t, config.Variables["v"].Nullable)

	_, diags = NewParser().ParseSource("test.tf", []byte(`
terraform {
  package {
    name    = "pkg"
    version = "not-semver"
  }
}
`))
	require.True(t, diags.HasErrors())
	assert.Equal(t, "Invalid package version", diags[0].Summary)
}

func TestParseProviderForEach(t *testing.T) {
	t.Parallel()
	src := []byte(`
provider "simple" {
  alias    = "by_key"
  for_each = { a = "alpha" }
  prefix   = each.value
}
`)
	config, diags := NewParser().ParseSource("test.tf", src)
	require.False(t, diags.HasErrors(), "unexpected errors: %v", diags.Errs())

	provider, ok := config.Providers["simple.by_key"]
	require.True(t, ok)
	assert.Equal(t, "simple", provider.Name)
	assert.Equal(t, "by_key", provider.Alias)
	assert.NotNil(t, provider.ForEach)
}

func TestParseProviderForEachRequiresAlias(t *testing.T) {
	t.Parallel()
	src := []byte(`
provider "simple" {
  for_each = { a = "alpha" }
  prefix   = each.value
}
`)
	_, diags := NewParser().ParseSource("test.tf", src)
	require.True(t, diags.HasErrors())
	assert.Equal(t,
		`test.tf:3,14-29: Alias required when using "for_each"; `+
			"The for_each argument is allowed only for provider configurations with an alias.",
		diags.Error())
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// Quoted references are the pre-0.12 form of `depends_on` and `ignore_changes`
// entries. They are still accepted, with a deprecation warning.
func TestParseQuotedReferences(t *testing.T) {
	t.Parallel()
	src := []byte(`
resource "aws_instance" "web" {
  depends_on = ["aws_vpc.main"]

  lifecycle {
    ignore_changes = ["tags"]
  }
}
`)

	p := NewParser()
	config, diags := p.ParseSource("test.hcl", src)
	require.False(t, diags.HasErrors(), "%s", diags)

	r, ok := config.Resources["aws_instance.web"]
	require.True(t, ok)
	assert.Equal(t, []string{"aws_vpc.main"}, traversalStrings(r.DependsOn))
	require.NotNil(t, r.Lifecycle)
	assert.Equal(t, []string{".tags"}, traversalStrings(r.Lifecycle.IgnoreChanges))

	require.Len(t, diags, 2)
	for _, d := range diags {
		assert.Equal(t, hcl.DiagWarning, d.Severity)
		assert.Equal(t, "Quoted references are deprecated", d.Summary)
	}
}

// traversalStrings renders each traversal as it would be written in source. A
// relative traversal keeps its leading dot (`.tags`).
func traversalStrings(traversals []hcl.Traversal) []string {
	var ret []string
	for _, t := range traversals {
		ret = append(ret, string(hclwrite.TokensForTraversal(t).Bytes()))
	}
	return ret
}

// TestParseDataRejectsLifecycleArgs: a data block's lifecycle carries only
// conditions; every lifecycle argument is an error.
func TestParseDataRejectsLifecycleArgs(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		attr string
	}{
		{"create_before_destroy", "create_before_destroy = true"},
		{"prevent_destroy", "prevent_destroy = true"},
		{"ignore_changes", "ignore_changes = [query]"},
		{"replace_triggered_by", "replace_triggered_by = [simple_resource.r]"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			src := `
data "simple_lookup" "d" {
  query = "x"
  lifecycle {
    ` + tc.attr + `
  }
}`
			_, diags := NewParser().ParseSource("test.hcl", []byte(src))
			require.True(t, diags.HasErrors(), "expected parse errors, got %v", diags)
			require.Equal(t, "Invalid data resource lifecycle argument", diags[0].Summary)
		})
	}
}

// TestParseDataRejectsResourceSurface: pulumi options outside the data
// subset are schema-rejected, as are managed-resource blocks.
func TestParseDataRejectsResourceSurface(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		body    string
		summary string
	}{
		{"protect", "pulumi {\n  protect = true\n}", "Unsupported argument"},
		{"retain_on_delete", "pulumi {\n  retain_on_delete = true\n}", "Unsupported argument"},
		{"name", "pulumi {\n  name = \"n\"\n}", "Unsupported argument"},
		{"provisioner", "provisioner \"local-exec\" {\n  command = \"true\"\n}", "Unsupported block type"},
		{"connection", "connection {\n  host = \"h\"\n}", "Unsupported block type"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			src := `
data "simple_lookup" "d" {
  query = "x"
  ` + tc.body + `
}`
			_, diags := NewParser().ParseSource("test.hcl", []byte(src))
			require.True(t, diags.HasErrors(), "expected parse errors, got %v", diags)
			require.Equal(t, tc.summary, diags[0].Summary)
		})
	}
}

// TestParseDataPulumiOptions: the data subset of pulumi options parses.
func TestParseDataPulumiOptions(t *testing.T) {
	t.Parallel()
	src := `
resource "simple_resource" "r" {
  input_one = "x"
}

data "simple_lookup" "d" {
  query = "x"
  pulumi {
    parent              = simple_resource.r
    version             = "1.2.3"
    plugin_download_url = "https://example.com"
  }
}`
	cfg, diags := NewParser().ParseSource("test.hcl", []byte(src))
	require.False(t, diags.HasErrors(), "unexpected parse errors: %v", diags)
	ds := cfg.DataSources["simple_lookup.d"]
	require.NotNil(t, ds.ResourceParent)
	require.NotNil(t, ds.Version)
	require.NotNil(t, ds.PluginDownloadURL)
}

// TestParseDataLifecycleConditionsStillParse: precondition/postcondition
// blocks remain valid inside a data block's lifecycle.
func TestParseDataLifecycleConditionsStillParse(t *testing.T) {
	t.Parallel()
	src := `
data "simple_lookup" "d" {
  query = "x"
  lifecycle {
    precondition {
      condition     = true
      error_message = "never"
    }
  }
}`
	cfg, diags := NewParser().ParseSource("test.hcl", []byte(src))
	require.False(t, diags.HasErrors(), "unexpected parse errors: %v", diags)
	require.Len(t, cfg.DataSources["simple_lookup.d"].Preconditions, 1)
}

func TestTypeLabelNotIdentifierWarns(t *testing.T) {
	t.Parallel()

	labelRange := func(start, end int) *hcl.Range {
		return &hcl.Range{
			Filename: "test.hcl",
			Start:    hcl.Pos{Line: 1, Column: start + 1, Byte: start},
			End:      hcl.Pos{Line: 1, Column: end + 1, Byte: end},
		}
	}
	tests := []struct {
		src  string
		want hcl.Diagnostics
	}{
		{src: `resource "odd_dot_ted_v1_thing" "x" {}`},
		{
			src: `resource "odd_dot.ted_v1_thing" "x" {}`,
			want: hcl.Diagnostics{{
				Severity: hcl.DiagWarning,
				Summary:  "Type name is not a valid identifier",
				Detail: `The resource type "odd_dot.ted_v1_thing" is not a valid identifier, ` +
					`so references to this resource cannot work. Use "odd_dot_ted_v1_thing" instead.`,
				Subject: labelRange(9, 31),
			}},
		},
		{
			src: `data "odd_dot.ted_v1_thing" "x" {}`,
			want: hcl.Diagnostics{{
				Severity: hcl.DiagWarning,
				Summary:  "Type name is not a valid identifier",
				Detail: `The data source type "odd_dot.ted_v1_thing" is not a valid identifier, ` +
					`so references to this data source cannot work. Use "odd_dot_ted_v1_thing" instead.`,
				Subject: labelRange(5, 27),
			}},
		},
		{
			src: `resource "!!" "x" {}`,
			want: hcl.Diagnostics{{
				Severity: hcl.DiagWarning,
				Summary:  "Type name is not a valid identifier",
				Detail:   `The resource type "!!" is not a valid identifier, so references to this resource cannot work.`,
				Subject:  labelRange(9, 13),
			}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.src, func(t *testing.T) {
			t.Parallel()
			_, diags := NewParser().ParseSource("test.hcl", []byte(tt.src))
			assert.Equal(t, tt.want, diags)
		})
	}
}
