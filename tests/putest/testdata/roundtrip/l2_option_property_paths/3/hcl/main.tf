terraform {
  required_providers {
    collections = {
      source  = "pulumi/collections"
      version = "0.0.1"
    }
  }
}

# Stage 3: change `metadata[0].labels["x"]`. The resource is replaced.
resource "collections_thing" "t" {
  pulumi {
    hide_diffs         = [tags["b"]]
    replace_on_changes = [tags["a"], metadata[0].labels["x"]]
  }
  tags = {
    "a" = "2"
    "b" = "2"
  }
  metadata {
    labels = {
      "x" = "2"
      "y" = "2"
    }
  }
}
output "summary" {
  value = collections_thing.t.summary
}
