# `settings` is a TypeList MaxItems=1 block. The bridge flattens it to one
# object, but HCL reads a list.
resource "list" "blocky:index/thing:Thing" {
  name = "list"
  settings = {
    mode = "a"
  }
}

output "mode" {
  value = list.settings.mode
}

