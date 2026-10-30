resource "r" "renamed:index/thing:Thing" {
  name = "alpha"
  config = {
    sizeWindow = 5
  }
  options {
    deleteBeforeReplace = true
  }
}

output "fn_name" {
  # `function_name` (TF) — bridge renames it to `name` on the Pulumi side.
  # Engine must surface the output under the TF name.
  value = r.name
}

output "arn" {
  # `arn` is unrenamed; sanity check default-cased outputs still work alongside.
  value = r.arn
}

output "window" {
  # Nested block field, also non-default rename (window_size → windowSize via
  # explicit Name, plus the block itself is renamed settings → config).
  value = r.config.sizeWindow
}

