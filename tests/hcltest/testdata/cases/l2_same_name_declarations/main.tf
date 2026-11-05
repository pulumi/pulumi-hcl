terraform {
  required_providers {
    simple = { source = "pulumi/simple" }
    blocky = { source = "pulumi/blocky" }
  }
}

# HCL scopes names by kind and type, so each declaration here is named `r`.
variable "r" {
  type    = string
  default = "v"
}

locals {
  r = "${var.r}-local"
}

provider "simple" {
  alias  = "r"
  prefix = "p"
}

resource "simple_resource" "r" {
  provider  = simple.r
  input_one = local.r
}

resource "blocky_single" "r" {}

data "simple_lookup" "r" {
  provider = simple.r
  query    = simple_resource.r.result
}

module "r" {
  source = "./modules/r"
  value  = data.simple_lookup.r.prefix_result
}

output "r" {
  value = module.r.value
}

output "variable" {
  value = var.r
}

output "local" {
  value = local.r
}

output "resource" {
  value = simple_resource.r.prefix_result
}

output "other_type" {
  value = blocky_single.r.id
}

output "data" {
  value = data.simple_lookup.r.prefix_result
}
