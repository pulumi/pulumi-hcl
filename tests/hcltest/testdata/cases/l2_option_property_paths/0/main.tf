terraform {
  required_providers {
    collections = { source = "pulumi/collections" }
  }
}

# Stage 0: create. `replace_on_changes` names one key of the `tags` map and
# one key of `labels`, a map inside the MaxItems=1 block `metadata`.
resource "collections_thing" "t" {
  tags = { a = "1", b = "1" }
  metadata {
    labels = { x = "1", y = "1" }
  }
  pulumi {
    replace_on_changes = [tags["a"], metadata[0].labels["x"]]
    hide_diffs         = [tags["b"]]
  }
}

output "summary" { value = collections_thing.t.summary }
