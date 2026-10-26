terraform {
  required_providers {
    blocky = {
      source  = "pulumi/blocky"
      version = "0.0.1"
    }
  }
}

# Stage 0: create with settings.mode = "a". `settings` is a MaxItems=1 nested
# block, so in TF it is addressed with list-index notation (settings[0].mode).
resource "blocky_thing" "t" {
  lifecycle {
    ignore_changes = [settings[0].mode]
  }
  name = "thing"
  settings {
    mode    = "a"
    verbose = false
  }
}
output "mode" {
  value = blocky_thing.t.settings[0].mode
}
