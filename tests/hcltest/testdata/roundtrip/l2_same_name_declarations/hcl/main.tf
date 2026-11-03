terraform {
  required_providers {
    simple = {
      source  = "pulumi/simple"
      version = "0.0.1"
    }
  }
}

resource "simple_resource" "thing" {
  input_one = var.thing
}
# HCL scopes names per kind, so a variable and a resource can share a name.
# PCL declares both in one namespace.
variable "thing" {
  type    = string
  default = "v"
}
output "variable" {
  value = var.thing
}
output "resource" {
  value = simple_resource.thing.result
}
