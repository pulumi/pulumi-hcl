terraform {
  required_providers {
    fnblock = {
      source  = "pulumi/fnblock"
      version = "0.0.1"
    }
  }
}

# Stage 0: create with a `settings` block. `mode` is ForceNew, and it is listed
# in ignore_changes. `verbose` is an ordinary in-place attribute.
resource "fnblock_resource" "r" {
  lifecycle {
    ignore_changes = [settings[0].mode]
  }
  note = "x"
  settings {
    mode    = "a"
    verbose = false
  }
}
output "settings" {
  value = fnblock_resource.r.settings
}
