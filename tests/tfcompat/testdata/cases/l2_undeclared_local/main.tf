locals {
  nopee = "x"
}

resource "order_resource" "consumer" {
  name       = "consumer"
  depends_on = [local.nope]
}
