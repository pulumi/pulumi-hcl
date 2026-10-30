terraform {
  required_providers {
    blocky = { source = "pulumi/blocky" }
  }
}

# Stage 0: create with settings.mode = "a". `settings` is a MaxItems=1 nested
# block, so in TF it is addressed with list-index notation (settings[0].mode).
resource "blocky_thing" "t" {
  name = "thing"
  settings {
    mode    = "a"
    verbose = false
  }

  lifecycle {
    ignore_changes = [settings[0].mode]
  }
}

output "mode" {
  value = blocky_thing.t.settings[0].mode
}
