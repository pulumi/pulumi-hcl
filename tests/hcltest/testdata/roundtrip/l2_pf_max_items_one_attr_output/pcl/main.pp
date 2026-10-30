resource "this" "pfx:index/flat:Flat" {
  settings = {
    enabled = true
  }
  options {
    deleteBeforeReplace = true
  }
}

output "settings" {
  value = (this.settings == null ? null : [this.settings])
}

output "enabled" {
  value = this.settings.enabled
}

output "count" {
  value = length((this.settings == null ? null : [this.settings]))
}

