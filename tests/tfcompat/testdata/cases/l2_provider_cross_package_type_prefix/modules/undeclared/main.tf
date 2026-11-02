resource "simple_resource" "r" {
  provider  = simple-beta
  input_one = "undeclared"
  input_two = true
}

output "prefix_result" {
  value = simple_resource.r.prefix_result
}
