terraform {
  required_providers {
    roc = { source = "pulumi/roc" }
  }
}

# Stage 1: change `value`. The resource is updated in place.
resource "roc_thing" "t" {
  value   = "b"
  foo_bar = "a"
}

output "id" { value = roc_thing.t.id }
