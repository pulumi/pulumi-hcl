# The selected branch is the data source, so an attribute that only the
# resource declares is unsupported.
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

output "selected_input_one" {
  value = local.selected.input_one
}
