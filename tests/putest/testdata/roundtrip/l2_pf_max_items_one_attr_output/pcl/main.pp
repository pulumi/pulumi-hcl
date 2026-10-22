resource "this" "pfx:index/flat:Flat" {
  settings = {
    enabled = true
  }
  options {
    deleteBeforeReplace = true
  }
}

output "settings" {
  value = this.settings
}

output "enabled" {
  value = this.settings[0].enabled
}

output "count" {
  value = length(this.settings)
}

