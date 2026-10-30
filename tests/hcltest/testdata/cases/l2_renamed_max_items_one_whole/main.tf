terraform {
  required_providers {
    renamed = { source = "pulumi/renamed" }
  }
}

# `settings` and `filter` are MaxItems=1 blocks: the bridge flattens each to
# one object, or to null when the block is absent, but HCL reads a list.
resource "renamed_thing" "set" {
  function_name = "set"
  settings {
    window_size = 5
  }
}

resource "renamed_thing" "unset" {
  function_name = "unset"
}

data "renamed_lookup" "d" {
  query = "lambda"
  filter {
    kind = "trigger"
  }
}

data "renamed_lookup" "each" {
  for_each = { lambda = true }
  query    = each.key
  filter {
    kind = "trigger"
  }
}

resource "renamed_thing" "each" {
  for_each      = { lambda = true }
  function_name = "len-${length(data.renamed_lookup.each[each.key].filter)}"
}

output "set_len" {
  value = length(renamed_thing.set.settings)
}

output "unset_len" {
  value = length(renamed_thing.unset.settings)
}

output "unset_is_null" {
  value = renamed_thing.unset.settings == null
}

output "data_len" {
  value = length(data.renamed_lookup.d.filter)
}

output "each_len" {
  value = renamed_thing.each["lambda"].arn
}
