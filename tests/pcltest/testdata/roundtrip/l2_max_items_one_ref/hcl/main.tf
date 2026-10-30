terraform {
  required_providers {
    blocky = {
      source  = "pulumi/blocky"
      version = "0.0.1"
    }
  }
}

# `settings` is a TypeList MaxItems=1 block. The bridge flattens it to one
# object, but HCL reads a list.
resource "blocky_thing" "list" {
  lifecycle {
    create_before_destroy = true
  }
  name = "list"
  settings {
    mode = "a"
  }
}
output "mode" {
  value = blocky_thing.list.settings[0].mode
}
