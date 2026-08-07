resource "renamed_thing" "r" {
  function_name = "alpha"

  settings {
    window_size = 5
  }
}

output "fn_name" {
  # `function_name` (TF) — bridge renames it to `name` on the Pulumi side.
  # Engine must surface the output under the TF name.
  value = renamed_thing.r.function_name
}

output "arn" {
  # `arn` is unrenamed; sanity check default-cased outputs still work alongside.
  value = renamed_thing.r.arn
}

output "window" {
  # Nested block field, also non-default rename (window_size → windowSize via
  # explicit Name, plus the block itself is renamed settings → config).
  value = renamed_thing.r.settings[0].window_size
}
