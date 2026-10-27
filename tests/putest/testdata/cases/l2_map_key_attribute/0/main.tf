terraform {
  required_providers {
    collections = { source = "pulumi/collections" }
  }
}

# Stage 0: create. A map key is written as an attribute, in `ignore_changes`
# and in a reference. The key is data, so `foo_bar` keeps its name.
resource "collections_thing" "t" {
  tags = { foo_bar = "x" }
  lifecycle { ignore_changes = [tags.foo_bar] }
}

output "foo_bar" { value = collections_thing.t.tags.foo_bar }
