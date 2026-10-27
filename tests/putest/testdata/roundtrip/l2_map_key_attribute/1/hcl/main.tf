terraform {
  required_providers {
    collections = {
      source  = "pulumi/collections"
      version = "0.0.1"
    }
  }
}

# Stage 1: change the ignored key. The stored value stays "x".
resource "collections_thing" "t" {
  lifecycle {
    ignore_changes = [tags["foo_bar"]]
  }
  tags = {
    "foo_bar" = "y"
  }
}
output "foo_bar" {
  value = collections_thing.t.tags["foo_bar"]
}
