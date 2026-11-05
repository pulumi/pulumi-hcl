terraform {
  required_providers {
    simple = {
      source                = "pulumi/simple"
      configuration_aliases = [simple.east]
    }
  }
}

resource "simple_resource" "r" {
  provider  = simple.east
  input_one = "world"
  input_two = true
}

output "result" {
  value = simple_resource.r.prefix_result
}
