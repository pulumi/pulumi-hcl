resource "order_resource" "consumer" {
  name       = "consumer"
  depends_on = [module.nope.x]
}
