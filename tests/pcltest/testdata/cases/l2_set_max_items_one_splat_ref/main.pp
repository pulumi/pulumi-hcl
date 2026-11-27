resource "set" "blocky:index/single:Single" {
  options {
    range = 2
  }
  opt = {
    label = "b"
  }
}

output "labels" {
  value = set[*].opt.label
}
