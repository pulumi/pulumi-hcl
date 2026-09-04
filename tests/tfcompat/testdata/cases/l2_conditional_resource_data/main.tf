# The "create or adopt" idiom: a conditional selects between a counted
# resource and its matching data source. The two declare different timeout
# operations, so their `timeouts` attribute types differ.
variable "create" {
  type    = bool
  default = false
}

resource "timeoutable_resource" "this" {
  count     = var.create ? 1 : 0
  input_one = "hello"
}

data "timeoutable_data" "this" {
  count = var.create ? 0 : 1
}

locals {
  selected = var.create ? timeoutable_resource.this[0] : data.timeoutable_data.this[0]
}

output "selected_id" {
  value = local.selected.id
}

output "selected_result" {
  value = local.selected.result
}

output "selected_timeouts" {
  value = local.selected.timeouts
}
