terraform {
  required_providers {
    collections = { source = "pulumi/collections" }
  }
}

# Stage 3: change `metadata[0].labels["x"]`. The resource is replaced.
resource "collections_thing" "t" {
  tags = { a = "2", b = "2" }
  metadata {
    labels = { x = "2", y = "2" }
  }
  pulumi {
    replace_on_changes = [tags["a"], metadata[0].labels["x"]]
    hide_diffs         = [tags["b"]]
  }
}

output "summary" { value = collections_thing.t.summary }
