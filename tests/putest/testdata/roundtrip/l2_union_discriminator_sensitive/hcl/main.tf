terraform {
  required_providers {
    union = {
      source  = "pulumi/union"
      version = "0.0.1"
    }
  }
}

resource "union_thing" "t" {
  cfg = {
    "type"  = var.disc
    "value" = "x"
  }
}
variable "disc" {
  type    = string
  default = "a"
}
