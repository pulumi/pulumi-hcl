terraform {
  required_providers {
    renamed = {
      source  = "pulumi/renamed"
      version = "0.0.1"
    }
  }
}

data "renamed_lookup" "invoke_0" {
  query = "lambda"
  filter {
    kind = "trigger"
  }
}
data "renamed_lookup" "invoke_1" {
  query = "lambda"
  filter {
    kind = "trigger"
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
output "set_len" {
  value = length(one(renamed_thing.set.settings) == null ? [] : [one(renamed_thing.set.settings)])
}
output "unset_len" {
  value = length(one(renamed_thing.unset.settings) == null ? [] : [one(renamed_thing.unset.settings)])
}
output "unset_is_null" {
  value = (one(renamed_thing.unset.settings) == null ? [] : [one(renamed_thing.unset.settings)]) == null
}
output "data_len" {
  value = length(one(data.renamed_lookup.invoke_0.filter) == null ? [] : [one(data.renamed_lookup.invoke_1.filter)])
}
