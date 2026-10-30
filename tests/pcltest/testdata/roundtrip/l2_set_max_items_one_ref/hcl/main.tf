terraform {
  required_providers {
    blocky = {
      source  = "pulumi/blocky"
      version = "0.0.1"
    }
  }
}

# `opt` is a TypeSet MaxItems=1 block. The bridge flattens it to one object,
# but HCL reads a set, which HCL cannot index.
resource "blocky_single" "set" {
  lifecycle {
    create_before_destroy = true
  }
  opt {
    label = "b"
  }
}
output "label" {
  value = one(blocky_single.set.opt).label
}
