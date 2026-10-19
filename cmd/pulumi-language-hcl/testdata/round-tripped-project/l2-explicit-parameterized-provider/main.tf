terraform {
  required_providers {
    goodbye = {
      source  = "pulumi/goodbye"
      version = "2.0.0"
    }
  }
}

provider "goodbye" {
  alias = "prov"
  text  = "World"
}
// The resource name is based on the parameter value
resource "goodbye" "res" {
  provider = goodbye.prov
  lifecycle {
    create_before_destroy = true
  }
}
// The resource name is based on the parameter value and the provider config
output "parameterValue" {
  value = goodbye.res.parameter_value
}
