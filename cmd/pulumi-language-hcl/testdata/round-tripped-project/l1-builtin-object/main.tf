variable "aMap" {
  type = map(string)
}
output "entriesOutput" {
  value = entries(var.aMap)
}
output "lookupOutput" {
  value = lookup(var.aMap, "keyPresent", "default")
}
output "lookupOutputDefault" {
  value = lookup(var.aMap, "keyMissing", "default")
}
# An untyped (dynamic) config value. Pins iterating dynamic entries in generated programs
# (e.g. TypeScript's Object.entries over a value with no static type).
variable "alternativeNames" {
  type    = any
  default = {}
}
output "names" {
  value = [for entry in entries(var.alternativeNames) : entry.value]
}
output "lengthOutput" {
  value = length(var.aMap)
}
output "lengthDynamicOutput" {
  value = length(var.alternativeNames)
}
