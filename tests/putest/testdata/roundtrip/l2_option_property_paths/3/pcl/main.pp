# Stage 3: change `metadata[0].labels["x"]`. The resource is replaced.
resource "t" "collections:index/thing:Thing" {
  tags = {
    a = "2"
    b = "2"
  }
  metadata = {
    labels = {
      x = "2"
      y = "2"
    }
  }
  options {
    replaceOnChanges    = [tags["a"], metadata.labels["x"]]
    hideDiffs           = [tags["b"]]
    deleteBeforeReplace = true
  }
}

output "summary" {
  value = t.summary
}

