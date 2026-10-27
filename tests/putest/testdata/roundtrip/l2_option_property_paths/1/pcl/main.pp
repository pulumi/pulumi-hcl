# Stage 1: change only the keys that `replace_on_changes` does not name. The
# resource is updated in place.
resource "t" "collections:index/thing:Thing" {
  tags = {
    a = "1"
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

