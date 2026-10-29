resource "order_resource" "producer" {
  name         = "producer"
  delay_create = true
}

output "id" {
  value = order_resource.producer.id
}
