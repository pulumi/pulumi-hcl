# The config now appends a second element. OpenTofu drops an ignore_changes
# entry whose path does not resolve in the prior state, so the new element is
# applied verbatim and `zones` becomes ["a", "z"].
resource "collections_thing" "grow" {
  zones = ["a", "z"]

  lifecycle {
    ignore_changes = [zones[1]]
  }
}

output "zones" { value = collections_thing.grow.zones }
