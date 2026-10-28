terraform {
  required_providers {
    optional-primitive-ref = {
      source  = "pulumi/optional-primitive-ref"
      version = "40.0.0"
    }
  }
}

resource "optional-primitive-ref_resource" "setRes" {
  lifecycle {
    create_before_destroy = true
  }
  data = {
    boolean      = true
    float        = 3.14
    integer      = 42
    string       = "hello"
    number_array = [-1, 0, 1]
    boolean_map = {
      "t" = true
      "f" = false
    }
  }
  optional_data = {
    string = "optional parent"
  }
}
resource "optional-primitive-ref_resource" "unsetRes" {
  lifecycle {
    create_before_destroy = true
  }
  data = {}
}
resource "optional-primitive-ref_resource" "fromNestedOptional" {
  lifecycle {
    create_before_destroy = true
  }
  data = {
    string = optional-primitive-ref_resource.setRes.optional_data.string
  }
}
resource "optional-primitive-ref_resource" "fromOptionalLists" {
  lifecycle {
    create_before_destroy = true
  }
  data = {
    number_array = local.optionalLists[0]
  }
}
output "setBoolean" {
  value = optional-primitive-ref_resource.setRes.data.boolean
}
output "setFloat" {
  value = optional-primitive-ref_resource.setRes.data.float
}
output "setInteger" {
  value = optional-primitive-ref_resource.setRes.data.integer
}
output "setString" {
  value = optional-primitive-ref_resource.setRes.data.string
}
output "setNumberArray" {
  value = optional-primitive-ref_resource.setRes.data.number_array
}
output "setBooleanMap" {
  value = optional-primitive-ref_resource.setRes.data.boolean_map
}
output "unsetBoolean" {
  value = optional-primitive-ref_resource.unsetRes.data.boolean == null ? "null" : "not null"
}
output "unsetFloat" {
  value = optional-primitive-ref_resource.unsetRes.data.float == null ? "null" : "not null"
}
output "unsetInteger" {
  value = optional-primitive-ref_resource.unsetRes.data.integer == null ? "null" : "not null"
}
output "unsetString" {
  value = optional-primitive-ref_resource.unsetRes.data.string == null ? "null" : "not null"
}
output "unsetNumberArray" {
  value = optional-primitive-ref_resource.unsetRes.data.number_array == null ? "null" : "not null"
}
output "unsetBooleanMap" {
  value = optional-primitive-ref_resource.unsetRes.data.boolean_map == null ? "null" : "not null"
}
output "setOptionalData" {
  value = optional-primitive-ref_resource.setRes.optional_data == null ? "null" : "not null"
}
output "unsetOptionalData" {
  value = optional-primitive-ref_resource.unsetRes.optional_data == null ? "null" : "not null"
}
output "tupleOfOptional" {
  value = [optional-primitive-ref_resource.setRes.data.string, optional-primitive-ref_resource.unsetRes.data.string]
}
locals {
  optionalLists = [optional-primitive-ref_resource.setRes.data.number_array, optional-primitive-ref_resource.unsetRes.data.number_array]
}
