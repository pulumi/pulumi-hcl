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

output "settings" {
  value = this.settings
}

output "names" {
  value = invoke("pfx:index/getSetLookup:getSetLookup", {
    items = [{
      name = "beta"
      }, {
      name = "alpha"
    }]
  }).names
}

output "registries" {
  value = invoke("pfx:index/getSetLookup:getSetLookup", {
    items = [{
      name = "beta"
      }, {
      name = "alpha"
    }]
  }).registries
}

