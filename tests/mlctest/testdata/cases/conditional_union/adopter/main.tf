variable "create" {
  type = bool
}

resource "timeoutable_resource" "this" {
  count     = var.create ? 1 : 0
  input_one = "hello"
}

data "timeoutable_data" "this" {
  count = var.create ? 0 : 1
}

locals {
  thing = var.create ? timeoutable_resource.this[0] : data.timeoutable_data.this[0]
}

output "result" {
  value = local.thing.result
}

output "thing" {
  value = local.thing
}
