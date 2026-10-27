terraform {
  required_providers {
    roc = {
      source  = "pulumi/roc"
      version = "0.0.1"
    }
  }
}

# Stage 1: change `value`. The resource is updated in place.
resource "roc_thing" "t" {
  value   = "b"
  foo_bar = "a"
}
output "id" {
  value = roc_thing.t.id
}
