# A `count` derived from a sensitive value is accepted on resources, data
# sources and module calls. Only `for_each` rejects sensitive values.
variable "secret" {
  type      = string
  default   = ""
  sensitive = true
}

resource "simple_resource" "r" {
  count     = var.secret == "" ? 1 : 0
  input_one = "x"
  input_two = false
}

data "simple_lookup" "d" {
  count = sensitive(2)
  query = "q${count.index}"
}

module "items" {
  source = "./modules/item"
  count  = sensitive(2)
  name   = "item-${count.index}"
}

output "resources" {
  value = length(simple_resource.r)
}

output "lookups" {
  value = [for d in data.simple_lookup.d : d.prefix_result]
}

output "names" {
  value = [for m in module.items : m.name]
}
