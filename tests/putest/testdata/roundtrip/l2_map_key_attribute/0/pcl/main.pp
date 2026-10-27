# Stage 0: create. A map key is written as an attribute, in `ignore_changes`
# and in a reference. The key is data, so `foo_bar` keeps its name.
resource "t" "collections:index/thing:Thing" {
  tags = {
    foo_bar = "x"
  }
  options {
    ignoreChanges       = [tags["foo_bar"]]
    deleteBeforeReplace = true
  }
}

output "foo_bar" {
  value = t.tags["foo_bar"]
}

