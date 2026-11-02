# The "simple-beta" provider serves the "simple_*" types, the same way
# hashicorp/google-beta serves "google_*" types. No provider named "simple" is
# declared, so each block gets its schema from the provider that its
# `provider` meta-argument selects.
terraform {
  required_providers {
    simple-beta = {
      source = "hashicorp/simple-beta"
    }
  }
}

provider "simple-beta" {
  prefix = "hello"
}

resource "simple_resource" "r" {
  provider  = simple-beta
  input_one = "world"
  input_two = true
}

data "simple_lookup" "ds" {
  provider = simple-beta
  query    = "world"
}

# The child module declares "simple-beta" in its own required_providers.
module "declared" {
  source = "./modules/declared"
  providers = {
    simple-beta = simple-beta
  }
}

# The child module does not declare "simple-beta", so the local name stands
# for hashicorp/simple-beta and the child inherits the root's configuration.
module "undeclared" {
  source = "./modules/undeclared"
}

output "prefix_result" {
  value = simple_resource.r.prefix_result
}

output "data_prefix_result" {
  value = data.simple_lookup.ds.prefix_result
}

output "declared" {
  value = module.declared.prefix_result
}

output "undeclared" {
  value = module.undeclared.prefix_result
}
