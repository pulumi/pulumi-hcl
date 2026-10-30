# Stage 1: change `value`. The resource is updated in place.
resource "t" "roc:index:Thing" {
  value  = "b"
  fooBar = "a"
  options {
    deleteBeforeReplace = true
  }
}

output "id" {
  value = t.id
}

