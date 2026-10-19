terraform {
  required_providers {
    config = {
      source  = "pulumi/config"
      version = "9.0.0"
    }
    multi-argument-invoke = {
      source  = "pulumi/multi-argument-invoke"
      version = "44.0.0"
    }
  }
}

data "config" "invoke_0" {
  text = local.greeting.result
}

// A multi-argument invoke passes its arguments positionally and omits the ones the program leaves
// out, so parenting it must not displace the options bag into an argument slot.
locals {
  greeting = provider::multi-argument-invoke::multi_argument_invoke("hello", null)
}
output "result" {
  value = data.config.invoke_0.text
}
