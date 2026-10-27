# Stage 0: create. The schema marks `fooBar`, `dot.ted` and `nest.inNer` as
# replaceOnChanges.
resource "t" "roc:index:Thing" {
  value  = "a"
  fooBar = "a"
  options {
    deleteBeforeReplace = true
  }
}

output "id" {
  value = t.id
}

