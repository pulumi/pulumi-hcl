terraform {
  required_providers {
    simple = {
      source  = "pulumi/simple"
      version = "2.0.0"
    }
  }
}

variable "input" {
  type        = bool
  description = "An input passed to the outer component"
}
module "innerComponent" {
  source = "../innerComponent"
  pulumi {
    name ="${pulumi.module.name}-innerComponent"
  }
  input = ! var.input
}
output "output" {
  value = module.innerComponent.output
}
