terraform {
  required_providers {
    pfx = { source = "pulumi/pfx" }
  }
}

provider "pfx" {
  registries = [
    { url = "oci://a.example" },
    { url = "oci://b.example" },
  ]
}

resource "pfx_flat" "this" {
  settings = [
    { enabled = true },
    { enabled = false },
  ]
}

data "pfx_set_lookup" "this" {
  items = [
    { name = "beta" },
    { name = "alpha" },
  ]
}

output "settings" {
  value = pfx_flat.this.settings
}

output "names" {
  value = data.pfx_set_lookup.this.names
}

output "registries" {
  value = data.pfx_set_lookup.this.registries
}
