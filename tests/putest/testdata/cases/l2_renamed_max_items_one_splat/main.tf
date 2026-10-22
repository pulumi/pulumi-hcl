terraform {
  required_providers {
    renamed = { source = "pulumi/renamed" }
  }
}

# `settings` is a MaxItems=1 block, which the bridge flattens to one object.
# A splat reads it as a list of one element.
resource "renamed_thing" "r" {
  function_name = "alpha"
  settings {
    window_size = 5
  }
}

output "splat" {
  value = renamed_thing.r.settings[*].window_size
}
