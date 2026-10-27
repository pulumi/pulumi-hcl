# Stage 0: create. `replace_on_changes` names one key of the `tags` map and
# one key of `labels`, a map inside the MaxItems=1 block `metadata`.
resource "t" "collections:index/thing:Thing" {
  tags = {
    a = "1"
    b = "1"
  }
  metadata = {
    labels = {
      x = "1"
      y = "1"
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

