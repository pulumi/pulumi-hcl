variable "input" {
  type = string
}

resource "simple_resource" "res" {
  input_one = var.input
  input_two = true
}

output "result" {
  value = simple_resource.res.result
}
