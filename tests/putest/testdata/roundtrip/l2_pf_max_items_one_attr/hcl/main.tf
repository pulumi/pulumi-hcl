terraform {
  required_providers {
    pfx = {
      source  = "pulumi/pfx"
      version = "0.0.1"
    }
  }
}

resource "pfx_flat" "this" {
  settings = [{
    "enabled" = true
  }]
}
output "id" {
  value = pfx_flat.this.id
}
