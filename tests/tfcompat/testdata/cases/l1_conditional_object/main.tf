# A conditional whose branches are objects with different attribute sets
# unifies to a map when the attribute types unify, and to the unified
# object type when the attribute names match.
variable "flag" {
  type    = bool
  default = true
}

locals {
  disjoint = var.flag ? { a = "x" } : { b = "y" }
  superset = var.flag ? { a = "x" } : { a = "y", b = "z" }
  mixed    = !var.flag ? { a = "x" } : { b = 1 }
  nested   = var.flag ? { shared = "s", inner = { p = "1" } } : { shared = "t", inner = { q = "2" } }
}

output "disjoint" {
  value = local.disjoint
}

output "disjoint_a" {
  value = local.disjoint.a
}

output "superset" {
  value = local.superset
}

output "mixed" {
  value = local.mixed
}

output "mixed_b" {
  value = local.mixed.b
}

output "nested" {
  value = local.nested
}

output "nested_inner" {
  value = local.nested.inner
}

output "lookup_missing" {
  value = lookup(local.disjoint, "b", "dflt")
}

output "can_missing" {
  value = can(local.disjoint.b)
}

output "try_missing" {
  value = try(local.disjoint.b, "fallback")
}
