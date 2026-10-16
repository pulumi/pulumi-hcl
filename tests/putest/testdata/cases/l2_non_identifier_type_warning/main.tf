terraform {
  required_providers {
    odd = {
      source = "pulumi/odd"
    }
  }
}

resource "odd_dot.ted_v1_thing" "a" {
  value = "a"
}

output "value" {
  value = "ok"
}
