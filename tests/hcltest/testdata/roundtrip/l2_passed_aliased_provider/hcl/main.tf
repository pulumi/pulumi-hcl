terraform {
  required_providers {
    simple = {
      source  = "pulumi/simple"
      version = "0.0.1"
    }
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
  source = "./mid"
  providers = {
    simple = simple.east
  }
}
output "result" {
  value = module.mid.result
}
