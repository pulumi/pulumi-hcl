terraform {
  required_providers {
    simple = {
      source  = "pulumi/simple"
      version = "0.0.1"
    }
  }
}

module "child" {
  source = "./child"
  pulumi {
    name ="${pulumi.module.name}-child"
  }
}
output "result" {
  value = module.child.result
}
