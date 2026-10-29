data "order_data" "d" {
  name = "d"
}

output "o" {
  value = order_data.d.result
}
