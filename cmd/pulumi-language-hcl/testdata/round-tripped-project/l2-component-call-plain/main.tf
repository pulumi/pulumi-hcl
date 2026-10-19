terraform {
  required_providers {
    configurer = {
      source  = "pulumi/configurer"
      version = "38.0.0"
    }
  }
}

call "configurer" "plain_provider" {
}
call "configurer" "nested_plain_provider" {
}
call "configurer" "plain_value" {
}

resource "configurer" "configurer" {
  lifecycle {
    create_before_destroy = true
  }
  provider_config = "propagated"
}
resource "configurer_custom" "customFromPlainProvider" {
  provider = call.configurer.plain_provider
  lifecycle {
    create_before_destroy = true
  }
  value = "from-plain-provider"
}
resource "configurer_custom" "customFromNestedPlainProvider" {
  provider = call.configurer.nested_plain_provider.provider
  lifecycle {
    create_before_destroy = true
  }
  value = "from-nested-plain-provider"
}
output "plainValue" {
  value = call.configurer.plain_value
}
output "nestedPlainValue" {
  value = call.configurer.nested_plain_provider.value
}
