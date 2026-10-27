terraform {
  required_providers {
    collections = { source = "pulumi/collections" }
  }
}

# Stage 1: change the ignored key. The stored value stays "x".
resource "collections_thing" "t" {
  tags = { foo_bar = "y" }
  lifecycle { ignore_changes = [tags.foo_bar] }
}

output "foo_bar" { value = collections_thing.t.tags.foo_bar }
