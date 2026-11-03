terraform {
  required_providers {
    simple = {
      source = "hashicorp/simple-beta"
    }
  }
}

provider "simple" {
  prefix = "hello"
}

resource "simple_resource" "r" {
  input_one = "world"
  input_two = true
}

data "simple_lookup" "ds" {
  query = "world"
}

output "prefix_result" {
  value = simple_resource.r.prefix_result
}

output "data_prefix_result" {
  value = data.simple_lookup.ds.prefix_result
}
