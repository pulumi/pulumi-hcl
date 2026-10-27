terraform {
  required_providers {
    collections = {
      source  = "pulumi/collections"
      version = "0.0.1"
    }
  }
}

# Stage 1: change only the keys that `replace_on_changes` does not name. The
# resource is updated in place.
resource "collections_thing" "t" {
  pulumi {
    hide_diffs         = [tags["b"]]
    replace_on_changes = [tags["a"], metadata[0].labels["x"]]
  }
  tags = {
    "a" = "1"
    "b" = "2"
  }
  metadata {
    labels = {
      "x" = "1"
      "y" = "2"
    }
  }
}
output "summary" {
  value = collections_thing.t.summary
}
