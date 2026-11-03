# HCL scopes names per kind, so a variable and a resource can share a name.
# PCL declares both in one namespace.
config "thing" "string" {
  default = "v"
}

resource "thing_" "simple:index/resource:Resource" {
  __logicalName = "thing"
  inputOne      = thing
  options {
    deleteBeforeReplace = true
  }
}

output "variable" {
  value = thing
}

output "resource" {
  value = thing_.result
}

