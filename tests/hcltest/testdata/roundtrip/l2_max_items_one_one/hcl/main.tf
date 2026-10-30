terraform {
  required_providers {
    blocky = {
      source  = "pulumi/blocky"
      version = "0.0.1"
    }
  }
}

# `settings` (TypeList) and `opt` (TypeSet) are MaxItems=1 blocks: the bridge
# flattens each to one object, or to null when the block is absent, but HCL
# reads a list or a set, so `one()` reads the block.
resource "blocky_thing" "list" {
  name = "list"
  settings {
    mode = "a"
  }
}
resource "blocky_thing" "list_unset" {
  name = "list_unset"
}
resource "blocky_single" "set" {
  opt {
    label = "b"
  }
}
resource "blocky_single" "set_unset" {
}
output "list_one" {
  value = blocky_thing.list.settings[0].mode
}
output "list_one_unset" {
  value = one(blocky_thing.list_unset.settings) == null
}
output "set_one" {
  value = one(blocky_single.set.opt).label
}
output "set_one_unset" {
  value = one(blocky_single.set_unset.opt) == null
}
