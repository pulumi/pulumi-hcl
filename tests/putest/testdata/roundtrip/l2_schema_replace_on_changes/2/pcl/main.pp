# Stage 2: change `foo_bar`. The resource is replaced.
resource "t" "roc:index:Thing" {
  value  = "b"
  fooBar = "b"
  options {
    deleteBeforeReplace = true
  }
}

output "id" {
  value = t.id
}

