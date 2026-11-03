terraform {
  required_providers {
    simple-beta = {
      source = "hashicorp/simple-beta"
    }
  }
}

resource "simple_resource" "r" {
  provider  = simple-beta
  input_one = "declared"
  input_two = true
}

output "prefix_result" {
  value = simple_resource.r.prefix_result
}
