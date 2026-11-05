terraform {
  required_providers {
    simple = {
      source  = "pulumi/simple"
      version = "0.0.1"
    }
  }
}

resource "simple_resource" "r" {
  pulumi {
    name ="${pulumi.module.name}-r"
  }
  input_one = "world"
  input_two = true
}
output "result" {
  value = simple_resource.r.prefix_result
}
