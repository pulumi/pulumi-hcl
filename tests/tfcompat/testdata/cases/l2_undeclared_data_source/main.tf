resource "order_resource" "consumer" {
  name       = "consumer"
  depends_on = [data.order_data.nope]
}
