resource "pfx" "pulumi:providers:pfx" {
  registries = [{
    url = "oci://a.example"
    }, {
    url = "oci://b.example"
  }]
}

resource "this" "pfx:index/flat:Flat" {
  settings = [{
    enabled = true
    }, {
    enabled = false
  }]
  options {
    deleteBeforeReplace = true
  }
}

this_ = invoke("pfx:index/getSetLookup:getSetLookup", {
  items = [{
    name = "beta"
    }, {
    name = "alpha"
  }]
})

output "settings" {
  value = this.settings
}

output "names" {
  value = this_.names
}

output "registries" {
  value = this_.registries
}

