# Stage 0: create with a `settings` block. `mode` is ForceNew, and it is listed
# in ignore_changes. `verbose` is an ordinary in-place attribute.
resource "r" "fnblock:index/resource:Resource" {
  note = "x"
  settings = {
    mode    = "a"
    verbose = false
  }
  options {
    ignoreChanges       = [settings[0].mode]
    deleteBeforeReplace = true
  }
}

output "settings" {
  value = r.settings
}

