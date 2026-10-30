terraform {
  required_providers {
    pfx = {
      source  = "pulumi/pfx"
      version = "0.0.1"
    }
  }
}

resource "pfx_flat" "this" {
  settings = {
    enabled = true
  }
}
output "settings" {
  value = (pfx_flat.this.settings == null ? null : one(pfx_flat.this.settings)) == null ? null : [(pfx_flat.this.settings == null ? null : one(pfx_flat.this.settings))]
}
output "enabled" {
  value = pfx_flat.this.settings[0].enabled
}
output "count" {
  value = length((pfx_flat.this.settings == null ? null : one(pfx_flat.this.settings)) == null ? null : [(pfx_flat.this.settings == null ? null : one(pfx_flat.this.settings))])
}
