# Stage 0: create with settings.mode = "a". `settings` is a MaxItems=1 nested
# block, so in TF it is addressed with list-index notation (settings[0].mode).
resource "t" "blocky:index/thing:Thing" {
  name = "thing"
  settings = {
    mode    = "a"
    verbose = false
  }
  options {
    ignoreChanges       = [settings.mode]
    deleteBeforeReplace = true
  }
}

output "mode" {
  value = t.settings.mode
}

