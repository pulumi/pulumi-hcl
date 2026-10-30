terraform {
  required_providers {
    collections = {
      source  = "pulumi/collections"
      version = "0.0.1"
    }
  }
}

# Stage 0: create. A map key is written as an attribute, in `ignore_changes`
# and in a reference. The key is data, so `foo_bar` keeps its name.
resource "collections_thing" "t" {
  lifecycle {
    ignore_changes = [tags["foo_bar"]]
  }
  tags = {
    "foo_bar" = "x"
  }
}
output "foo_bar" {
  value = collections_thing.t.tags["foo_bar"]
}
