terraform {
  required_providers {
    pfx = {
      source  = "pulumi/pfx"
      version = "0.0.1"
    }
  }
}

data "pfx_set_lookup" "this_" {
  items = [{
    name = "beta"
    }, {
    name = "alpha"
  }]
  provider = pfx
}

provider "pfx" {
  registries = [{
    url = "oci://a.example"
    }, {
    url = "oci://b.example"
  }]
}
resource "pfx_flat" "this" {
  provider = pfx
  settings = [{
    enabled = true
    }, {
    enabled = false
  }]
}
output "settings" {
  value = pfx_flat.this.settings
}
output "names" {
  value = data.pfx_set_lookup.this_.names
}
output "registries" {
  value = data.pfx_set_lookup.this_.registries
}
