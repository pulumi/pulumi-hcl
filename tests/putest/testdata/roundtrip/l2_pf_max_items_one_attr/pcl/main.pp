resource "this" "pfx:index/flat:Flat" {
  settings = [{
    enabled = true
  }]
  options {
    deleteBeforeReplace = true
  }
}

output "id" {
  value = this.id
}

