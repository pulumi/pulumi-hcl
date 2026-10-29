# module.producer declares no `does_not_exist` output. A depends_on entry that
# names it is accepted but adds no dependency, so the consumer's create does
# not wait for the delayed producer: the recorded sequence must be
# [create consumer, create producer].
module "producer" {
  source = "./producer"
}

resource "order_resource" "consumer" {
  name       = "consumer"
  depends_on = [module.producer.does_not_exist]
}
