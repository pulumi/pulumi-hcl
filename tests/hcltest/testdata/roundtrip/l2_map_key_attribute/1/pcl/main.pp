# Stage 1: change the ignored key. The stored value stays "x".
resource "t" "collections:index/thing:Thing" {
  tags = {
    foo_bar = "y"
  }
  options {
    ignoreChanges       = [tags["foo_bar"]]
    deleteBeforeReplace = true
  }
}

output "foo_bar" {
  value = t.tags["foo_bar"]
}

