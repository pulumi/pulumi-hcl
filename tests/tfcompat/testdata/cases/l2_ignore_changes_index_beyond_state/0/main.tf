# `zones[1]` is ignored, but at creation the list has a single element, so
# index 1 does not exist in the resource's prior state.
resource "collections_thing" "grow" {
  zones = ["a"]

  lifecycle {
    ignore_changes = [zones[1]]
  }
}

output "zones" { value = collections_thing.grow.zones }
