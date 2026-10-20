terraform {
  required_providers {
    pfx = {
      source  = "pulumi/pfx"
      version = "0.0.1"
    }
  }
}

data "pfx_set_lookup" "invoke_0" {
  items = [{
    name = "beta"
    }, {
    name = "alpha"
  }]
}
data "pfx_set_lookup" "invoke_1" {
  items = [{
    name = "beta"
    }, {
    name = "alpha"
  }]
}

provider "pfx" {
  registries = [{
    url = "oci://a.example"
    }, {
    url = "oci://b.example"
  }]
}
resource "pfx_flat" "this" {
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
  value = data.pfx_set_lookup.invoke_0.names
}
output "registries" {
  value = data.pfx_set_lookup.invoke_1.registries
}
