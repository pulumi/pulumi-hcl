terraform {
  required_providers {
    simple = { source = "pulumi/simple" }
  }
}

provider "simple" {
  prefix = "default"
}

provider "simple" {
  alias  = "east"
  prefix = "east"
}

module "mid" {
  source = "./modules/mid"
  providers = {
    simple.east = simple.east
  }
}

output "result" {
  value = module.mid.result
}
