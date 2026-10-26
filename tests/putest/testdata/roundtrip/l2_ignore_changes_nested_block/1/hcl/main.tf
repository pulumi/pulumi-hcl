terraform {
  required_providers {
    blocky = {
      source  = "pulumi/blocky"
      version = "0.0.1"
    }
  }
}

# Stage 1: settings.mode changes "a" -> "b". Nothing else changes, and the only
# changed attribute is covered by ignore_changes, so OpenTofu plans no update and
# the stored mode stays "a". pulumi-hcl must ignore the same path.
resource "blocky_thing" "t" {
  lifecycle {
    ignore_changes = [settings[0].mode]
  }
  name = "thing"
  settings {
    mode    = "b"
    verbose = false
  }
}
output "mode" {
  value = blocky_thing.t.settings[0].mode
}
