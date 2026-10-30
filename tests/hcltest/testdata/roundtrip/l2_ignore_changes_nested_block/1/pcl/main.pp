# Stage 1: settings.mode changes "a" -> "b". Nothing else changes, and the only
# changed attribute is covered by ignore_changes, so OpenTofu plans no update and
# the stored mode stays "a". pulumi-hcl must ignore the same path.
resource "t" "blocky:index/thing:Thing" {
  name = "thing"
  settings = {
    mode    = "b"
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

