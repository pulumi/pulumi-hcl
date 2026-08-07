resource "pfx_flat" "this" {
  settings = [{ enabled = true }]
}

output "id" {
  value = pfx_flat.this.id
}
