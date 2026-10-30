terraform {
  required_providers {
    blocky = { source = "pulumi/blocky" }
  }
}

# Stage 1: settings.mode changes "a" -> "b". Nothing else changes, and the only
# changed attribute is covered by ignore_changes, so OpenTofu plans no update and
# the stored mode stays "a". pulumi-hcl must ignore the same path.
resource "blocky_thing" "t" {
  name = "thing"
  settings {
    mode    = "b"
    verbose = false
  }

  lifecycle {
    ignore_changes = [settings[0].mode]
  }
}

output "mode" {
  value = blocky_thing.t.settings[0].mode
}
