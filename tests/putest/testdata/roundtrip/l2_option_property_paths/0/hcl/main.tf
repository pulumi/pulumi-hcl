terraform {
  required_providers {
    collections = {
      source  = "pulumi/collections"
      version = "0.0.1"
    }
  }
}

# Stage 0: create. `replace_on_changes` names one key of the `tags` map and
# one key of `labels`, a map inside the MaxItems=1 block `metadata`.
resource "collections_thing" "t" {
  pulumi {
    hide_diffs         = [tags["b"]]
    replace_on_changes = [tags["a"], metadata[0].labels["x"]]
  }
  tags = {
    "a" = "1"
    "b" = "1"
  }
  metadata {
    labels = {
      "x" = "1"
      "y" = "1"
    }
  }
}
output "summary" {
  value = collections_thing.t.summary
}
