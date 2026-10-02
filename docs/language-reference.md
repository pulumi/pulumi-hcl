# Pulumi HCL Language Reference

Pulumi HCL lets you write Pulumi programs using Terraform-compatible HCL syntax. You get familiar HCL blocks, expressions, and functions while using Pulumi's state management, secrets, and deployment engine.

## Table of Contents

- [Overview](#overview)
- [Top-Level Blocks](#top-level-blocks)
- [Variables](#variables)
- [Resources](#resources)
- [Resource Options](#resource-options)
- [Data Sources](#data-sources)
- [Providers](#providers)
- [Outputs](#outputs)
- [Locals](#locals)
- [Modules](#modules)
- [Call Blocks](#call-blocks)
- [Moved and Import Blocks](#moved-and-import-blocks)
- [Check Blocks](#check-blocks)
- [Expressions](#expressions)
- [Built-in Functions](#built-in-functions)
- [Stack References](#stack-references)
- [Terraform Block](#terraform-block)
- [Terraform Compatibility](#terraform-compatibility)

## Overview

A Pulumi HCL program consists of one or more `.tf` files in a directory with a `Pulumi.yaml` specifying `runtime: hcl`:

```yaml
name: my-project
runtime: hcl
```

HCL files declare infrastructure using blocks for [resources](#resources), [data sources](#data-sources), [variables](#variables), [locals](#locals), [outputs](#outputs), [providers](#providers), [modules](#modules), and Pulumi-specific constructs like [call blocks](#call-blocks). Programs also support [moved and import blocks](#moved-and-import-blocks) for resource lifecycle management, [stack references](#stack-references) for cross-stack access, and a [terraform block](#terraform-block) for version constraints and multi-language component declarations.

The full set of [expressions](#expressions) and [built-in functions](#built-in-functions) from Terraform's HCL is available, along with Pulumi-specific asset/archive functions. See [Terraform Compatibility](#terraform-compatibility) for the small number of differences.

## Top-Level Blocks

| Block       | Purpose                                           |
|-------------|---------------------------------------------------|
| `variable`  | Declare input variables                           |
| `resource`  | Manage cloud resources                            |
| `data`      | Read external data via provider invocations       |
| `provider`  | Configure provider instances                      |
| `output`    | Export values from the stack                      |
| `locals`    | Define reusable intermediate values               |
| `module`    | Invoke local or remote modules as components      |
| `call`      | Invoke methods on resources                       |
| `moved`     | Rename resources without recreation               |
| `import`    | Import existing cloud resources                   |
| `check`     | Non-blocking assertions about infrastructure      |
| `terraform` | Version constraints and component declarations    |

## Variables

Variables declare input values for the program. Values are set via Pulumi's configuration system.

```hcl
variable "region" {
  type        = string
  default     = "us-west-2"
  description = "AWS region to deploy into"
}

variable "instance_count" {
  type    = number
  default = 1
}

variable "enable_monitoring" {
  type      = bool
  sensitive = true
}

variable "name" {
  type = string

  validation {
    condition     = length(var.name) > 0
    error_message = "Name must not be empty."
  }
}
```

### Attributes

| Attribute     | Type       | Required | Description                                                                                        |
|---------------|------------|----------|----------------------------------------------------------------------------------------------------|
| `type`        | type expr  | No       | Type constraint (e.g., `string`, `number`, `bool`, `list(string)`, `map(number)`, `object({...})`) |
| `default`     | expression | No       | Default value when not configured                                                                  |
| `description` | string     | No       | Human-readable description                                                                         |
| `sensitive`   | bool       | No       | When `true`, the value becomes a Pulumi secret                                                     |
| `ephemeral`   | bool       | No       | Ephemeral value; see [Ephemeral Values](#ephemeral-values)                                         |
| `nullable`    | bool       | No       | When `false`, rejects null values (default: `true`)                                                |
| `validation`  | block      | No       | One or more validation rules (see below)                                                           |

### Validation Blocks

Each `validation` block has:

| Attribute       | Type       | Required | Description                                   |
|-----------------|------------|----------|-----------------------------------------------|
| `condition`     | expression | Yes      | Expression that must evaluate to `true`       |
| `error_message` | expression | Yes      | Error message shown when condition is `false` |

### Setting Variable Values

Variables are set from the following sources, in priority order:

1. **Stack config**: `pulumi config set <project>:<varName> <value>` (highest
   priority), which takes the place of Terraform's `-var`
2. **Variable-value files** in the program directory, applied in this order,
   with later files winning: `terraform.tfvars`, `terraform.tfvars.json`, then
   every `*.auto.tfvars` and `*.auto.tfvars.json` in lexical order by file name
3. **Environment variables**: `TF_VAR_name=value`. A variable set to the empty
   string is set, and still outranks the default
4. **Default values** in `variable` blocks (lowest priority)

A variable-value file assigns values to variable names, and its values are
literals — they may not refer to anything else in the program:

```hcl
# terraform.tfvars
instance_type = "t3.micro"
tags          = { Name = "web-server" }
```

Only the root module loads these files. A file shipped inside a module is
never read, and a value for a name the root module does not declare reaches
nothing and is reported as a warning.

Reference variables in expressions as `var.<name>`:

```hcl
resource "aws_instance" "web" {
  instance_type = var.instance_type
}
```

## Resources

Resources declare managed infrastructure.

```hcl
resource "aws_instance" "web" {
  ami           = "ami-12345678"
  instance_type = "t3.micro"

  tags = {
    Name = "web-server"
  }
}
```

The first label is the resource type (Terraform-style, e.g. `aws_instance`). The second label is the logical name. The body contains the resource's input properties.

For a Pulumi provider, the type is `<package>_<module>_<member>`, built from the resource's Pulumi type token. The module
is the one the package schema defines: for bridged providers such as `pulumi/aws`, `aws:ec2/vpc:Vpc` has module `ec2`.
The module and member become `snake_case`, `/` and `.` in the module become `_`, and the `index` module is omitted. So
`aws:ec2/vpc:Vpc` is `aws_ec2_vpc`, `kubernetes:core/v1:ConfigMap` is `kubernetes_core_v1_config_map` and
`kubernetes:helm.sh/v3:Release` is `kubernetes_helm_sh_v3_release`. Data sources also drop the function's `get` prefix. A
type written with a `.`, such as `kubernetes_helm.sh_v3_release`, still declares the resource but cannot be referenced,
because a reference splits at every `.`. Both spellings name the same Pulumi type, so switching to the underscore spelling
keeps the resource's URN.

### Meta-Arguments

| Argument     | Type       | Description                                                |
|--------------|------------|------------------------------------------------------------|
| `count`      | number     | Create multiple instances indexed by `count.index`         |
| `for_each`   | map or set | Create instances keyed by `each.key` with `each.value`     |
| `depends_on` | list       | Explicit dependencies on other resources                   |
| `provider`   | reference  | Specific provider configuration to use                     |
| `providers`  | map        | Explicit provider configurations (modules/components only), e.g. `providers = { aws = aws.east }` |

### Lifecycle Block

```hcl
resource "aws_instance" "web" {
  # ...

  lifecycle {
    create_before_destroy = true
    prevent_destroy       = true
    ignore_changes        = [tags]
    replace_triggered_by  = [aws_instance.other]
  }
}
```

| Attribute               | Type | Description                                                              |
|-------------------------|------|--------------------------------------------------------------------------|
| `create_before_destroy` | bool | When `true`, creates the replacement before destroying the old resource. |
| `prevent_destroy`       | bool | Refuse plans that would destroy this resource                            |
| `ignore_changes`        | list | Property paths to exclude from diff detection                            |
| `replace_triggered_by`  | list | Replace this resource when any referenced resource or attribute changes  |

### Timeouts Block

```hcl
resource "aws_instance" "web" {
  # ...

  timeouts {
    create = "60m"
    update = "30m"
    delete = "2h"
  }
}
```

| Attribute | Type   | Description                   |
|-----------|--------|-------------------------------|
| `create`  | string | Timeout for create operations |
| `read`    | string | Timeout for read operations   |
| `update`  | string | Timeout for update operations |
| `delete`  | string | Timeout for delete operations |

### Preconditions and Postconditions

Preconditions and postconditions are nested inside the `lifecycle` block:

```hcl
resource "aws_instance" "web" {
  # ...

  lifecycle {
    precondition {
      condition     = var.instance_type != ""
      error_message = "Instance type must be specified."
    }

    postcondition {
      condition     = self.public_ip != ""
      error_message = "Instance must have a public IP."
    }
  }
}
```

### Provisioners

Provisioners run commands after resource creation. They map to the [Pulumi Command provider](https://www.pulumi.com/registry/packages/command/).

```hcl
resource "aws_instance" "web" {
  ami           = "ami-12345678"
  instance_type = "t3.micro"

  provisioner "local-exec" {
    command = "echo ${self.public_ip} >> hosts.txt"
  }

  provisioner "remote-exec" {
    inline = [
      "sudo apt-get update",
      "sudo apt-get install -y nginx",
    ]
  }

  provisioner "file" {
    source      = "config.txt"
    destination = "/tmp/config.txt"
  }

  connection {
    type        = "ssh"
    user        = "ubuntu"
    private_key = file("~/.ssh/id_rsa")
    host        = self.public_ip
  }
}
```

| Provisioner Type | Pulumi Equivalent             |
|------------------|-------------------------------|
| `local-exec`     | `command:local:Command`       |
| `remote-exec`    | `command:remote:Command`      |
| `file`           | `command:remote:CopyToRemote` |

Provisioner options:

| Attribute    | Values                  | Description               |
|--------------|-------------------------|---------------------------|
| `when`       | `"create"`, `"destroy"` | When the provisioner runs |
| `on_failure` | `"continue"`, `"fail"`  | Behavior on failure       |

Connection blocks support SSH only (WinRM is not supported). The `self` reference provides access to the parent resource's attributes.

### Dynamic Blocks

Dynamic blocks generate repeated nested blocks from a collection:

```hcl
resource "aws_security_group" "web" {
  name = "web-sg"

  dynamic "ingress" {
    for_each = var.ingress_rules
    content {
      from_port   = ingress.value.from_port
      to_port     = ingress.value.to_port
      protocol    = ingress.value.protocol
      cidr_blocks = ingress.value.cidr_blocks
    }
  }
}
```

The label on the `dynamic` block becomes the iterator variable name. Inside the `content` block, `<label>.value` refers to the current element and `<label>.key` refers to its key or index.

### Referencing Resources

Resources are referenced by `<type>.<name>`:

```hcl
output "instance_id" {
  value = aws_instance.web.id
}
```

When using `count`, instances are indexed: `aws_instance.web[0].id` or `aws_instance.web[*].id` (splat).

When using `for_each`, instances are keyed: `aws_instance.web["key"].id`.

## Resource Options

These are Pulumi-specific meta-arguments. They live in a nested `pulumi` block:

```hcl
resource "aws_instance" "web" {
  # ...

  pulumi {
    name                       = "web-primary"
    parent                     = module.my_component
    additional_secret_outputs  = [password]
    protect                    = true
    retain_on_delete           = true
    deleted_with               = aws_vpc.main
    replace_on_changes         = [ami]
    replace_with               = [aws_instance.replacement]
    hide_diffs                 = [user_data]
    import_id                  = "i-1234567890abcdef0"
    aliases                    = [{ name = "old-name" }]
    version                    = "6.0.0"
    plugin_download_url        = "https://example.com/plugins"
  }
}
```

| Attribute                   | Type         | Description                                                                   |
|-----------------------------|--------------|-------------------------------------------------------------------------------|
| `name`                      | string          | Override the Pulumi logical name                                              |
| `parent`                    | reference       | Parent resource for component hierarchy                                       |
| `additional_secret_outputs` | list(attribute) | Output properties to encrypt in state                                         |
| `protect`                   | bool            | Mark the resource protected in state; deleting it requires unprotecting first |
| `retain_on_delete`          | bool            | Keep the cloud resource when removed from the program                         |
| `deleted_with`              | reference       | Cascade deletion when the referenced resource is deleted                      |
| `replace_with`              | list            | Resources whose replacement triggers replacement of this one                  |
| `hide_diffs`                | list(attribute) | Attributes whose diffs should not be displayed                                |
| `replace_on_changes`        | list(attribute) | Attributes that force replacement when changed                                |
| `import_id`                 | string          | Cloud resource ID to import                                                   |
| `aliases`                   | list            | Previous identities for this resource (used during renames)                   |
| `version`                   | string          | Provider plugin version                                                       |
| `plugin_download_url`       | string          | URL to download the provider plugin from                                      |
| `env_var_mappings`          | expression      | Environment variable remappings for the provider                              |

`additional_secret_outputs`, `hide_diffs`, and `replace_on_changes` entries
are bare attribute names (like `lifecycle.ignore_changes`), not quoted
strings, and use the provider's `snake_case` attribute names. Only top-level
attribute names are supported.

Each `aliases` entry is either a full URN string or an object with any
combination of `name`, `type`, `stack`, `project`, `parent_urn`, and
`no_parent` — a plain string is always treated as a URN, so an old logical
name must use the object form `{ name = "old-name" }`.

To trigger replacement when another resource or attribute changes, use the
standard Terraform [`replace_triggered_by`](#lifecycle-block) lifecycle
argument.

## Data Sources

Data sources read information from providers via invocations.

```hcl
data "aws_ami" "ubuntu" {
  most_recent = true
  owners      = ["099720109477"]

  filter {
    name   = "name"
    values = ["ubuntu/images/hvm-ssd/ubuntu-*-amd64-server-*"]
  }
}

output "ami_id" {
  value = data.aws_ami.ubuntu.id
}
```

Data sources use the `data` block with the same type naming as resources. Results are referenced as `data.<type>.<name>.<attribute>`.

Data sources support the same meta-arguments as resources: `count`, `for_each`, `depends_on`, and `provider`.

## Providers

Providers supply the implementation for resources and data sources.

### Required Providers

Providers are resolved the same way as OpenTofu. By default they are looked up
in the [OpenTofu registry](https://opentofu.org/registry/), so an unqualified
`aws` resolves to `registry.opentofu.org/hashicorp/aws`. Declaring
`required_providers` is only necessary to pin a source or version. Requirements
go inside the `terraform` block:

```hcl
terraform {
  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = ">= 6.0"
    }
    random = {
      source  = "pulumi/random"
      version = "4.16.0"
    }
  }
}
```

Sources prefixed with `pulumi/` consume a native Pulumi provider; any other
source is bridged from its Terraform provider. See
[docs/providers.md](providers.md) for the full resolution rules.

### Provider Configuration

Configure providers with `provider` blocks:

```hcl
provider "aws" {
  region = "us-west-2"
}
```

### Multiple Provider Configurations

Use `alias` to create multiple configurations of the same provider:

```hcl
provider "aws" {
  region = "us-west-2"
}

provider "aws" {
  alias  = "east"
  region = "us-east-1"
}

resource "aws_instance" "web" {
  provider = aws.east
  # ...
}
```

### Explicit Providers

Pass an aliased provider configuration to a resource with the `provider`
meta-argument, or to a component (module) with `providers`:

```hcl
provider "aws" {
  alias  = "explicit"
  region = "us-west-2"
}

resource "aws_instance" "web" {
  provider = aws.explicit
  # ...
}
```

Pulumi-specific provider options (`version`, `plugin_download_url`,
`env_var_mappings`, `additional_secret_outputs`) go in a nested `pulumi` block
inside the `provider` block.

## Outputs

Outputs export values from the stack.

```hcl
output "instance_ip" {
  value       = aws_instance.web.public_ip
  description = "Public IP of the web server"
}

output "db_password" {
  value     = random_password.db.result
  sensitive = true
}

output "vpc_id" {
  value      = aws_vpc.main.id
  depends_on = [aws_internet_gateway.gw]

  precondition {
    condition     = aws_vpc.main.id != ""
    error_message = "VPC must have an ID."
  }
}
```

| Attribute      | Type       | Required | Description                                     |
|----------------|------------|----------|-------------------------------------------------|
| `value`        | expression | Yes      | The value to export                             |
| `description`  | string     | No       | Human-readable description                      |
| `sensitive`    | bool       | No       | When `true`, the output becomes a Pulumi secret |
| `ephemeral`    | bool       | No       | Ephemeral value; see [Ephemeral Values](#ephemeral-values)  |
| `depends_on`   | list       | No       | Explicit dependencies                           |
| `precondition` | block      | No       | Validation checks before export                 |

## Locals

Locals define reusable intermediate values.

```hcl
locals {
  common_tags = {
    Environment = "dev"
    Project     = "my-project"
  }

  name_prefix = "myapp-${var.environment}"

  user_data = <<-EOF
    #!/bin/bash
    echo "Hello, World!" > index.html
    nohup python3 -m http.server 80 &
  EOF
}
```

Multiple `locals` blocks are allowed. Reference locals as `local.<name>`:

```hcl
resource "aws_instance" "web" {
  tags      = local.common_tags
  user_data = local.user_data
}
```

## Modules

Modules invoke reusable configurations as Pulumi component resources.

```hcl
module "vpc" {
  source     = "./modules/vpc"
  cidr_block = "10.0.0.0/16"
}

output "vpc_id" {
  value = module.vpc.vpc_id
}
```

### Module Sources

| Source Type           | Example                                                        |
|-----------------------|----------------------------------------------------------------|
| Local path            | `./modules/vpc`                                                |
| Git                   | `git::https://github.com/org/repo.git?ref=v1.0.0`              |
| Git with subdirectory | `git::https://github.com/org/repo.git//modules/vpc?ref=v1.0.0` |
| GitHub shorthand      | `github.com/org/repo`                                          |
| BitBucket shorthand   | `bitbucket.org/org/repo`                                       |
| Terraform Registry    | `terraform-aws-modules/vpc/aws`                                |
| HTTP archive          | `https://example.com/module.zip`                               |

Remote modules are cached in `~/.pulumi/modules/`.

### Module Meta-Arguments

| Argument     | Type       | Description                                    |
|--------------|------------|------------------------------------------------|
| `source`     | string     | Module source (required)                       |
| `version`    | string     | Version constraint (for registry modules)      |
| `count`      | number     | Create multiple module instances               |
| `for_each`   | map or set | Create keyed module instances                  |
| `depends_on` | list       | Explicit dependencies                          |
| `providers`  | map        | Provider configuration mappings for the module |

Module outputs are referenced as `module.<name>.<output_name>`.

Like resources, a module block accepts a nested `pulumi` block; its `name`
attribute overrides the Pulumi logical name of each module instance
(evaluated per instance, with `count.index`/`each.key` in scope). The
overridden name also prefixes the derived names of everything inside the
instance, and is visible to the module source as `pulumi.module.name`. Its
`protect` attribute marks each instance's component resource protected in
state.

```hcl
module "vpc" {
  source = "./modules/vpc"

  pulumi {
    name = "vpc-primary"
  }
}
```

## Call Blocks

Call blocks invoke methods on existing resources. This is a Pulumi-specific extension with no Terraform equivalent.

```hcl
resource "call_custom" "my_resource" {
  value = "hello"
}

call "my_resource" "provider_value" {
}

output "result" {
  value = call.my_resource.provider_value.result
}
```

The first label is the resource's logical name (matching a declared resource). The second label is the method name. The body contains arguments to the method.

Results are referenced as `call.<resource_name>.<method_name>.<attribute>`.

## Moved and Import Blocks

### Moved Blocks

Rename resources without recreating them. Maps to Pulumi's `aliases` resource option.

```hcl
moved {
  from = aws_instance.old_name
  to   = aws_instance.new_name
}
```

| Attribute | Type      | Required | Description               |
|-----------|-----------|----------|---------------------------|
| `from`    | reference | Yes      | Original resource address |
| `to`      | reference | Yes      | New resource address      |

### Import Blocks

Import existing cloud resources into Pulumi state.

```hcl
import {
  to       = aws_instance.web
  id       = "i-1234567890abcdef0"
  provider = aws.east
}
```

| Attribute  | Type      | Required | Description                   |
|------------|-----------|----------|-------------------------------|
| `to`       | reference | Yes      | Target resource address       |
| `id`       | string    | Yes      | Cloud resource ID to import   |
| `provider` | reference | No       | Provider configuration to use |

## Check Blocks

Check blocks make non-blocking assertions about your infrastructure. Unlike a
resource precondition or postcondition, a failed `assert` reports a warning and
the operation continues rather than aborting.

```hcl
check "health" {
  data "http" "status" {
    url = "https://example.com"
  }

  assert {
    condition     = data.http.status.status_code == 200
    error_message = "${data.http.status.url} returned an unhealthy status."
  }
}
```

Each check block has a name (its label) and one or more `assert` blocks:

| Attribute       | Type       | Required | Description                             |
|-----------------|------------|----------|-----------------------------------------|
| `condition`     | expression | Yes      | Expression that must evaluate to `true` |
| `error_message` | expression | Yes      | Warning shown when condition is `false` |

A check may also declare a single nested `data` source (a *scoped data source*),
read fresh on every operation and visible only to that check's assertions.

## Expressions

Pulumi HCL supports the full HCL expression language.

### Literals

```hcl
"hello"           # string
42                # number
3.14              # number
true              # bool
null              # null
["a", "b", "c"]   # list
{key = "value"}   # map
```

### String Interpolation

```hcl
"Hello, ${var.name}!"
"prefix-${local.env}-suffix"
```

### Heredocs

```hcl
<<-EOF
  multi-line
  string content
EOF
```

### References

| Reference                | Description                        |
|--------------------------|------------------------------------|
| `var.<name>`             | Input variable                     |
| `local.<name>`           | Local value                        |
| `<type>.<name>`          | Resource attribute                 |
| `<type>.<name>[<index>]` | Counted resource instance          |
| `<type>.<name>["<key>"]` | For-each resource instance         |
| `data.<type>.<name>`     | Data source attribute              |
| `module.<name>`          | Module output                      |
| `call.<res>.<method>`    | Call block result                  |
| `self`                   | Current resource (in provisioners) |
| `count.index`            | Current count iteration index      |
| `each.key`               | Current for_each key               |
| `each.value`             | Current for_each value             |
| `path.module`            | Path to the current module         |
| `path.root`              | Path to the root module            |
| `path.cwd`               | Current working directory          |
| `pulumi.stack`           | Current stack name                 |
| `pulumi.project`         | Current project name               |
| `pulumi.organization`    | Current organization name          |
| `pulumi.module.name`     | Pulumi logical name of the enclosing module instance (null at the root) |

### Operators

| Category   | Operators                        |
|------------|----------------------------------|
| Arithmetic | `+`, `-`, `*`, `/`, `%`          |
| Comparison | `==`, `!=`, `<`, `<=`, `>`, `>=` |
| Logical    | `&&`, `\|\|`, `!`                |

### Conditional Expression

```hcl
condition ? true_value : false_value
```

### For Expressions

```hcl
# List comprehension
[for name in var.names : upper(name)]

# With index
[for i, name in var.names : "${i}-${name}"]

# Map comprehension
{for k, v in var.tags : k => upper(v)}

# With filter
[for name in var.names : name if name != ""]
```

### Splat Expressions

```hcl
# Equivalent to [for r in aws_instance.web : r.id]
aws_instance.web[*].id
```

### Property Access

```hcl
resource.name.property
resource.name["key"]
resource.name[0]
```

### `try` and `can`

```hcl
try(var.optional.nested.value, "default")
can(var.optional.nested.value)  # returns true or false
```

## Built-in Functions

Pulumi HCL supports nearly all Terraform built-in functions. Functions are grouped by category below.

### Numeric

`abs`, `ceil`, `floor`, `log`, `max`, `min`, `pow`, `signum`, `parseint`

### String

`chomp`, `endswith`, `format`, `formatlist`, `indent`, `join`, `lower`, `regex`, `regexall`, `replace`, `split`, `startswith`, `strcontains`, `strrev`, `substr`, `title`, `trim`, `trimprefix`, `trimsuffix`, `trimspace`, `upper`

### Collection

`alltrue`, `anytrue`, `chunklist`, `coalesce`, `coalescelist`, `compact`, `concat`, `contains`, `distinct`, `element`, `entries`, `flatten`, `index`, `keys`, `length`, `list`, `lookup`, `map`, `matchkeys`, `merge`, `one`, `range`, `reverse`, `setintersection`, `setproduct`, `setsubtract`, `setunion`, `slice`, `sort`, `sum`, `transpose`, `values`, `zipmap`

### Encoding

`base64decode`, `base64encode`, `base64gzip`, `base64gunzip`, `csvdecode`, `jsondecode`, `jsonencode`, `textdecodebase64`, `textencodebase64`, `urlencode`, `urldecode`, `yamldecode`, `yamlencode`

### Filesystem

`abspath`, `basename`, `dirname`, `file`, `filebase64`, `fileexists`, `fileset`, `pathexpand`, `templatefile`, `templatestring`

### Date and Time

`formatdate`, `timeadd`, `timecmp`, `timestamp`

### Hash and Crypto

`base64sha256`, `base64sha512`, `bcrypt`, `filebase64sha256`, `filebase64sha512`, `filemd5`, `filesha1`, `filesha256`, `filesha512`, `md5`, `rsadecrypt`, `sha1`, `sha256`, `sha512`, `uuid`, `uuidv5`

### IP Network

`cidrhost`, `cidrnetmask`, `cidrsubnet`, `cidrsubnets`, `cidrcontains`

### Type Conversion

`can`, `ephemeralasnull`, `issensitive`, `nonsensitive`, `sensitive`, `tobool`, `tolist`, `tomap`, `tonumber`, `toset`, `tostring`, `try`, `type`

### Pulumi-Specific Functions

| Function                       | Description                                                  |
|--------------------------------|--------------------------------------------------------------|
| `fileasset(path)`              | Create a Pulumi `FileAsset` from a local file path           |
| `stringasset(text)`            | Create a Pulumi `StringAsset` from a string value            |
| `remoteasset(uri)`             | Create a Pulumi `RemoteAsset` from a URL                     |
| `filearchive(path)`            | Create a Pulumi `FileArchive` from a local path              |
| `remotearchive(uri)`           | Create a Pulumi `RemoteArchive` from a URL                   |
| `assetarchive(map)`            | Create a Pulumi `AssetArchive` from a map of assets/archives |
| `pulumiresourcename(resource)` | Get the logical name from a resource's URN                   |
| `pulumiresourcetype(resource)` | Get the type token from a resource's URN                     |
| `pulumiresourceurn(resource)`  | Get a resource's URN                                         |

## Stack References

Access outputs from other Pulumi stacks using the `pulumi_stack_reference` resource:

```hcl
resource "pulumi_stack_reference" "network" {
  name = "myorg/networking/prod"
}

output "vpc_id" {
  value = pulumi_stack_reference.network.outputs["vpc_id"]
}
```

## Terraform Block

The `terraform` block declares provider requirements (see
[Required Providers](#required-providers)) and other program-level settings.

### Version Constraints

```hcl
terraform {
  required_version_range = ">= 3.0.0"
}
```

The Pulumi CLI version can also be constrained with OpenTofu's extensible
[`language` block](https://opentofu.org/docs/language/settings/#declaring-implementation-compatibility). Only the
`pulumi` argument of a `compatible_with` block is interpreted; arguments addressed to other software (such as
`opentofu`) are ignored without validation, so a single module can declare compatibility with several implementations
at once:

```hcl
language {
  compatible_with {
    opentofu = ">= 1.12"
    pulumi   = ">= 3.0.0"
  }
}
```

### Multi-Language Components

The `component` and `package` blocks declare an HCL module as a reusable component consumable from any Pulumi
language. See [Multi-Language Components](mlc.md) for full details.

```hcl
terraform {
  component {
    name   = "VpcNetwork"
    module = "index"
  }
  package {
    name    = "my-networking"
    version = "1.0.0"
  }
}
```

## Terraform Compatibility

Pulumi HCL aims to run valid Terraform configuration unchanged. This section covers the behavioral differences and the
few unsupported features. If you find a case where `tofu` works and `pulumi` does not, please [open an issue](https://github.com/pulumi/pulumi-hcl/issues/new).

### Behavioral Differences

**Sensitive values** — Variables and outputs marked `sensitive = true` become Pulumi secrets, encrypted at rest in state.

**Ephemeral values** — see [Ephemeral Values](#ephemeral-values).

**Property names** — HCL uses `snake_case`. The plugin automatically converts to Pulumi's `camelCase` for the engine. Map keys are not translated.

### Ephemeral Values

Terraform's ephemeral values exist to keep a value out of state and plan files entirely. Pulumi's state model already
encrypts secrets at rest, so Pulumi HCL interprets `ephemeral = true` through that lens instead of reproducing
Terraform's persistence rules:

- **Persisted as secrets.** A value from an ephemeral variable or output is stored in state, but encrypted, exactly
  like `sensitive = true`. It is masked in CLI output and in the Pulumi Console.
- **Diffs are hidden.** An ephemeral value is free to differ on every run, so the resource property it flows into is
  registered with its diff hidden (the `hideDiffs` resource option), down to the exact property path — an ephemeral
  value inside one block of a repeated block hides only that block's attribute.
- **Not sensitive.** Ephemerality and sensitivity are tracked separately: `issensitive` returns `false` for a purely
  ephemeral value, matching Terraform.
- **`ephemeralasnull` is supported** and behaves as in Terraform: it replaces the ephemeral parts of a value with
  typed nulls (preserving sensitivity marks), making the result usable anywhere.

Differences from Terraform to be aware of:

- Terraform rejects an ephemeral value used in a non-ephemeral context (a regular resource argument, a non-ephemeral
  output). Pulumi HCL accepts it and persists the value encrypted; there is no flow validation.
- Terraform re-prompts for a required ephemeral variable on every run. Pulumi HCL reads it from stack configuration
  like any other variable, so set it once with `pulumi config set --secret`.
- A root-level ephemeral output becomes a secret stack output rather than being omitted.

### Feature Mappings

| Terraform Feature       | Pulumi Equivalent      | Notes                                     |
|-------------------------|------------------------|-------------------------------------------|
| `prevent_destroy`       | (native)               | Guard lifts when removed from the program |
| `ignore_changes`        | `ignoreChanges`        | Same behavior                             |
| `create_before_destroy` | `deleteBeforeReplace`  | Inverted; defaults to TF delete-first     |
| `replace_triggered_by`  | `replacementTrigger`   | Replaces when a referenced value changes  |
| `moved` blocks          | `aliases`              | Renames without recreation                |
| `import` blocks         | Import resource option | Imports existing resources                |
| `timeouts`              | `customTimeouts`       | Same duration format                      |
| Modules                 | Component resources    | All source types supported                |
| Provisioners            | Command provider       | `local-exec`, `remote-exec`, `file`       |

### Unsupported Features

- **`backend`, `required_version`, `provider_meta`, `experiments`** — Accepted inside the `terraform` block but ignored with a warning; Pulumi manages state independently and tracks its own version constraints via `required_version_range` or a `language` block's `compatible_with { pulumi = ... }` argument, and language experiments have no Pulumi HCL equivalent.
- **`cloud`** — Not accepted inside the `terraform` block at all. It is absent from the block's schema, so a `cloud` block is a parse error rather than a warning.
- **WinRM connections** — Only `type = "ssh"` is supported in `connection` blocks.
- **`List<Object>` empty vs null** — HCL block syntax cannot distinguish between an empty and null `List<Object>`, a known incompatibility with some Pulumi programs.

### CLI Equivalents

| Terraform           | Pulumi           |
|---------------------|------------------|
| `terraform plan`    | `pulumi preview` |
| `terraform apply`   | `pulumi up`      |
| `terraform destroy` | `pulumi destroy` |
| `terraform state`   | `pulumi state`   |
| `terraform import`  | `pulumi import`  |
| Workspaces          | Stacks           |
