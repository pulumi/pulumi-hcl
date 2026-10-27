terraform {
  required_providers {
    roc = { source = "pulumi/roc" }
  }
}

# Stage 0: create. The schema marks `fooBar`, `dot.ted` and `nest.inNer` as
# replaceOnChanges.
resource "roc_thing" "t" {
  value   = "a"
  foo_bar = "a"
}

output "id" { value = roc_thing.t.id }
