# The branches unify to a map, so an attribute that only the unselected
# branch declares is a missing map element, not an unsupported attribute.
variable "flag" {
  type    = bool
  default = true
}

locals {
  disjoint = var.flag ? { a = "x" } : { b = "y" }
}

output "disjoint_b" {
  value = local.disjoint.b
}
