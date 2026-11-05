terraform {
  required_providers {
    simple = {
      source  = "pulumi/simple"
      version = "0.0.1"
    }
  }
}

data "simple_lookup" "r_" {
  query    = local.simple
  provider = simple
}

provider "simple" {
  prefix = "c"
}
# The root module declares a data block with the same type and name.
resource "simple_resource" "r" {
  pulumi {
    name ="${pulumi.module.name}-r"
  }
  provider  = simple
  input_one = data.simple_lookup.r_.prefix_result
}
variable "value" {
  type = string
}
# The local and the default provider are both named `simple`.
locals {
  simple ="${var.value}-child"
}
output "value" {
  value ="${simple_resource.r.prefix_result}|${data.simple_lookup.r_.prefix_result}"
}
