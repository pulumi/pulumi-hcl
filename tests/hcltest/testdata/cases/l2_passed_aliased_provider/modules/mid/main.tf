terraform {
  required_providers {
    simple = {
      source                = "pulumi/simple"
      configuration_aliases = [simple.east]
    }
  }
}

module "child" {
  source = "../child"
  providers = {
    simple.east = simple.east
  }
}

output "result" {
  value = module.child.result
}
