terraform {
  required_providers {
    roc = { source = "pulumi/roc" }
  }
}

# Stage 2: change `foo_bar`. The resource is replaced.
resource "roc_thing" "t" {
  value   = "b"
  foo_bar = "b"
}

output "id" { value = roc_thing.t.id }
