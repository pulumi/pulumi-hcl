terraform {
  required_providers {
    union = {
      source  = "pulumi/union"
      version = "0.0.1"
    }
  }
}

resource "union_thing" "a" {
  cfg = {
    type  = "a"
    value = "x"
  }
}
resource "union_thing" "b" {
  cfg = {
    "type"  = union_thing.a.out
    "value" = "y"
  }
}
