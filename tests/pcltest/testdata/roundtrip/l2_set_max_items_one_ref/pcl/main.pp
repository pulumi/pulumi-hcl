# `opt` is a TypeSet MaxItems=1 block. The bridge flattens it to one object,
# but HCL reads a set, which HCL cannot index.
resource "set" "blocky:index/single:Single" {
  opt = {
    label = "b"
  }
}

output "label" {
  value = set.opt.label
}

