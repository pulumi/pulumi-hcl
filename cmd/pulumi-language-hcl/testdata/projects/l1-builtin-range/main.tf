variable "from" {
  type = number
}
variable "to" {
  type = number
}
output "rangeTo" {
  value = range(var.to)
}
output "rangeFromTo" {
  value = range(var.from, var.to)
}
output "literalTo" {
  value = range(3)
}
output "literalFromTo" {
  value = range(2, 5)
}
output "literalEmpty" {
  value = range(3, 3)
}
