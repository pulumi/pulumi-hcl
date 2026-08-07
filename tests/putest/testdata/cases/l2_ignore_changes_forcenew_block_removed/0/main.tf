# Stage 0: create with a `settings` block. `mode` is ForceNew, and it is listed
# in ignore_changes. `verbose` is an ordinary in-place attribute.
resource "fnblock_resource" "r" {
  note = "x"
  settings {
    mode    = "a"
    verbose = false
  }
  lifecycle { ignore_changes = [settings[0].mode] }
}

output "settings" { value = fnblock_resource.r.settings }
