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
  input_one = "passed"
}

output "value" {
  value = simple_resource.r.prefix_result
}
