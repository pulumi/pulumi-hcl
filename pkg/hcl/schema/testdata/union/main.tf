variable "flag" {
  type = bool
}

locals {
  either = var.flag ? { name = "a", zones = ["x"] } : { name = 1, nested = { deep = true } }
}

output "either" {
  value = local.either
}

output "name" {
  value = local.either.name
}

output "nested" {
  value = local.either.nested
}
