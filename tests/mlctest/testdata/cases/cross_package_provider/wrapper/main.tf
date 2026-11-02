# The "simple-beta" provider serves the "simple_resource" type. No provider
# named "simple" is declared, so the type of simple_resource.res comes from
# the provider that the `provider` meta-argument selects.
terraform {
  required_providers {
    simple-beta = {
      source = "hashicorp/simple-beta"
    }
  }
}

variable "input" {
  type = string
}

resource "simple_resource" "res" {
  provider  = simple-beta
  input_one = var.input
  input_two = true
}

output "result" {
  value = simple_resource.res.result
}
