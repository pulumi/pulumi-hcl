terraform {
  required_providers {
    echo = {
      source = "pulumi/echo"
    }
  }
}

variable "name" {
  type = string
}

resource "echo_thing" "hello" {
  text = "hello ${var.name}"
}

output "greeting" {
  value = echo_thing.hello.out
}
