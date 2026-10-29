# `settings` (TypeList) and `opt` (TypeSet) are MaxItems=1 blocks: the bridge
# flattens each to one object, or to null when the block is absent, but HCL
# reads a list or a set, so `one()` reads the block.
resource "list" "blocky:index/thing:Thing" {
  name = "list"
  settings = {
    mode = "a"
  }
  options {
    deleteBeforeReplace = true
  }
}

resource "list_unset" "blocky:index/thing:Thing" {
  name = "list_unset"
  options {
    deleteBeforeReplace = true
  }
}

resource "set" "blocky:index/single:Single" {
  opt = {
    label = "b"
  }
  options {
    deleteBeforeReplace = true
  }
}

resource "set_unset" "blocky:index/single:Single" {
  options {
    deleteBeforeReplace = true
  }
}

output "list_one" {
  value = list.settings.mode
}

output "list_one_unset" {
  value = list_unset.settings == null
}

output "set_one" {
  value = set.opt.label
}

output "set_one_unset" {
  value = set_unset.opt == null
}

