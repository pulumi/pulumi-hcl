# Stage 2: change `tags["a"]`. The resource is replaced.
resource "t" "collections:index/thing:Thing" {
  tags = {
    a = "2"
    b = "2"
  }
  metadata = {
    labels = {
      x = "1"
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

