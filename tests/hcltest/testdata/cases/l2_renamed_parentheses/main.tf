terraform {
  required_providers {
    renamed = { source = "pulumi/renamed" }
  }
}

resource "renamed_thing" "r" {
  function_name = "alpha"
  settings {
    window_size = 5
  }
}

data "renamed_lookup" "d" {
  query = "lambda"
}

# Each reference is inside parentheses, so the conversion must rename the
# expression inside them.
output "fn_name" {
  value = (renamed_thing.r.function_name)
}

output "window" {
  value = (renamed_thing.r.settings[0].window_size) + 1
}

output "nested" {
  value = ((renamed_thing.r.function_name))
}

output "from" {
  value = (data.renamed_lookup.d.upstream)
}
