terraform {
  required_providers {
    blocky = {
      source  = "pulumi/blocky"
      version = "0.0.1"
    }
    simple = {
      source  = "pulumi/simple"
      version = "0.0.1"
    }
  }
}

data "simple_lookup" "r______" {
  query    = simple_resource.r.result
  provider = simple.r
}

provider "simple" {
  alias  = "r"
  prefix = "p"
}
resource "simple_resource" "r" {
  provider  = simple.r
  input_one = local.r_
}
resource "blocky_single" "r" {
}
# HCL scopes names by kind and type, so each declaration here is named `r`.
variable "r" {
  type    = string
  default = "v"
}
locals {
  r_ ="${var.r}-local"
}
module "r" {
  source = "./r"
  value  = data.simple_lookup.r______.prefix_result
}
# The key names the aliased provider of the child module. The value names the
# provider `r` of this module, which the round trip renames.
module "passed" {
  source = "./passed"
  providers = {
    simple = simple.r
  }
}
output "r" {
  value = module.r.value
}
output "passed" {
  value = module.passed.value
}
output "nested_passed" {
  value = module.r.passed
}
output "variable" {
  value = var.r
}
output "local" {
  value = local.r_
}
output "resource" {
  value = simple_resource.r.prefix_result
}
output "other_type" {
  value = blocky_single.r.id
}
output "data" {
  value = data.simple_lookup.r______.prefix_result
}
