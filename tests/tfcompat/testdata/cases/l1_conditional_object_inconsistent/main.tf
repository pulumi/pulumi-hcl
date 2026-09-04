# Branch objects whose attribute types do not unify are an error, even
# though only one branch is selected.
variable "flag" {
  type    = bool
  default = true
}

locals {
  inconsistent = var.flag ? { a = "x" } : { b = ["y"] }
}

output "inconsistent" {
  value = local.inconsistent
}
