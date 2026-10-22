terraform {
  required_providers {
    renamed = { source = "pulumi/renamed" }
  }
}

# `settings` is a MaxItems=1 block, which the bridge flattens to one object,
# or to null when the block is absent. A splat reads an absent block as an
# empty list.
resource "renamed_thing" "r" {
  function_name = "alpha"
}

output "splat" {
  value = renamed_thing.r.settings[*].window_size
}
