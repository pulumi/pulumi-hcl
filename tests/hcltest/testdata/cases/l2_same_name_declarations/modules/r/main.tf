terraform {
  required_providers {
    simple = { source = "pulumi/simple" }
  }
}

variable "value" {
  type = string
}

# The local and the default provider are both named `simple`.
locals {
  simple = "${var.value}-child"
}

provider "simple" {
  prefix = "c"
}

# The root module declares a data block with the same type and name.
data "simple_lookup" "r" {
  query = local.simple
}

resource "simple_resource" "r" {
  provider  = simple
  providers = [simple]
  input_one = data.simple_lookup.r.prefix_result
}

# The value names the default provider of this module, which the round trip
# renames.
module "passed" {
  source = "../passed"
  providers = {
    simple.east = simple
  }
}

output "passed" {
  value = module.passed.value
}

output "value" {
  value = "${simple_resource.r.prefix_result}|${data.simple_lookup.r.prefix_result}"
}
