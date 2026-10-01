terraform {
  required_providers {
    extbase = {
      source  = "pulumi/extbase"
      version = "45.0.0"
    }
    myext = {
      source  = "pulumi/myext"
      version = "2.0.0"
    }
  }
}

// Tests extension parameterization on an explicit provider.
// The extension resource's provider lookup must resolve to the base provider.
provider "extbase" {
  alias = "prov"
}
resource "myext_greeting" "greeting" {
  provider = extbase.prov
  lifecycle {
    create_before_destroy = true
  }
}
resource "extbase_base" "base" {
  provider = extbase.prov
  lifecycle {
    create_before_destroy = true
  }
}
output "parameterValue" {
  value = myext_greeting.greeting.parameter_value
}
output "baseValue" {
  value = extbase_base.base.base_value
}
