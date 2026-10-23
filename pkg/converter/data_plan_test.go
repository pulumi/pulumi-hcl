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

package converter

import (
	"testing"

	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/pulumi/pulumi-hcl/tests/testutil/schemaloader"
	"github.com/pulumi/pulumi/pkg/v3/codegen/schema"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestEjectDataBlockHoisting checks that the invoke of a data block is inlined
// only when it then runs exactly as often as Terraform reads the data block.
// In all other cases, the invoke is hoisted to a local.
func TestEjectDataBlockHoisting(t *testing.T) {
	t.Parallel()

	loader := schemaloader.New(t, schema.PackageSpec{
		Name:    "test",
		Version: "1.0.0",
		Resources: map[string]schema.ResourceSpec{
			"test:index:Item": {
				InputProperties: map[string]schema.PropertySpec{
					"value": {TypeSpec: schema.TypeSpec{Type: "string"}},
				},
			},
		},
		Functions: map[string]schema.FunctionSpec{
			"test:index:echo": {
				Inputs: &schema.ObjectTypeSpec{
					Properties: map[string]schema.PropertySpec{
						"input": {TypeSpec: schema.TypeSpec{Type: "string"}},
					},
				},
				Outputs: &schema.ObjectTypeSpec{
					Properties: map[string]schema.PropertySpec{
						"result": {TypeSpec: schema.TypeSpec{Type: "string"}},
					},
				},
			},
		},
	})

	const header = `terraform {
  required_providers {
    test = {
      source  = "pulumi/test"
      version = "1.0.0"
    }
  }
}
`

	tests := []struct {
		name     string
		src      string
		expected string
	}{
		{
			name: "unreferenced",
			src: `
data "test_echo" "unused" {
  input = "a"
}
`,
			expected: `unused = invoke("test:index:echo", {
  input = "a"
})

`,
		},
		{
			name: "reference in a for expression",
			src: `
data "test_echo" "lookup" {
  input = "a"
}

output "results" {
  value = [for v in ["x", "y"] : "${v}-${data.test_echo.lookup.result}"]
}
`,
			expected: `lookup = invoke("test:index:echo", {
  input = "a"
})

output "results" {
  value = [for v in ["x", "y"] : "${v}-${lookup.result}"]
}

`,
		},
		{
			name: "for_each with a literal index",
			src: `
data "test_echo" "lookup" {
  for_each = {
    us = "alpha"
    eu = "bravo"
  }
  input = each.value
}

output "us" {
  value = data.test_echo.lookup["us"].result
}
`,
			expected: `lookup = {for key, value in {
  us = "alpha"
  eu = "bravo"
  } : key => invoke("test:index:echo", {
    input = value
})}

output "us" {
  value = lookup["us"].result
}

`,
		},
		{
			name: "for_each indexed from a different range",
			src: `
data "test_echo" "lookup" {
  for_each = {
    us = "alpha"
  }
  input = each.value
}

resource "test_item" "item" {
  for_each = {
    us = "zulu"
  }
  value = data.test_echo.lookup[each.key].result
}
`,
			expected: `lookup = {for key, value in {
  us = "alpha"
  } : key => invoke("test:index:echo", {
    input = value
})}

resource "item" "test:index:Item" {
  value = lookup[range.key].result
  options {
    range = {
      us = "zulu"
    }
    deleteBeforeReplace = true
  }
}

`,
		},
		{
			name: "count with two references",
			src: `
data "test_echo" "lookup" {
  count = 2
  input = "item-${count.index}"
}

output "first" {
  value = data.test_echo.lookup[0].result
}

output "second" {
  value = data.test_echo.lookup[1].result
}
`,
			expected: `lookup = [for index in range(2) : invoke("test:index:echo", {
  input = "item-${index}"
})]

output "first" {
  value = lookup[0].result
}

output "second" {
  value = lookup[1].result
}

`,
		},
		{
			name: "unranged data block in the body of an inlined ranged data block",
			src: `
data "test_echo" "main" {
  input = "prod"
}

data "test_echo" "zone" {
  for_each = {
    a = "us-a"
  }
  input = "${data.test_echo.main.result}-${each.value}"
}

resource "test_item" "item" {
  for_each = {
    a = "us-a"
  }
  value = data.test_echo.zone[each.key].result
}
`,
			expected: `main = invoke("test:index:echo", {
  input = "prod"
})

resource "item" "test:index:Item" {
  value = invoke("test:index:echo", {
    input = "${main.result}-${range.value}"
  }).result
  options {
    range = {
      a = "us-a"
    }
    deleteBeforeReplace = true
  }
}

`,
		},
		{
			name: "local name collides with another name",
			src: `
variable "lookup" {
  type = string
}

data "test_echo" "lookup" {
  input = var.lookup
}

output "a" {
  value = data.test_echo.lookup.result
}

output "b" {
  value = data.test_echo.lookup.result
}
`,
			expected: `config "lookup" "string" {
}

lookup_ = invoke("test:index:echo", {
  input = lookup
})

output "a" {
  value = lookup_.result
}

output "b" {
  value = lookup_.result
}

`,
		},
		{
			name: "for variable captures a name in the inlined body",
			src: `
variable "v" {
  type = string
}

data "test_echo" "lookup" {
  for_each = {
    a = "alpha"
  }
  input = "${var.v}-${each.value}"
}

output "results" {
  value = [for k, v in {
    a = "alpha"
  } : data.test_echo.lookup[k].result]
}
`,
			expected: `config "v" "string" {
}

lookup = {for key, value in {
  a = "alpha"
  } : key => invoke("test:index:echo", {
    input = "${v}-${value}"
})}

output "results" {
  value = [for k, v in {
    a = "alpha"
  } : lookup[k].result]
}

`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			out := hclwrite.NewEmptyFile()
			diags := transformSingleFile(t, []byte(header+tt.src), "main.tf", out.Body(), loader, nil)
			require.False(t, diags.HasErrors(), diags.Error())
			assert.Equal(t, tt.expected, string(out.Bytes()))
		})
	}
}
