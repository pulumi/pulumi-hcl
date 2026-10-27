terraform {
  required_providers {
    collections = { source = "pulumi/collections" }
  }
}

# Stage 1: change only the keys that `replace_on_changes` does not name. The
# resource is updated in place.
resource "collections_thing" "t" {
  tags = { a = "1", b = "2" }
  metadata {
    labels = { x = "1", y = "2" }
  }
  pulumi {
    replace_on_changes = [tags["a"], metadata[0].labels["x"]]
    hide_diffs         = [tags["b"]]
  }
}

output "summary" { value = collections_thing.t.summary }
