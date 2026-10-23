terraform {
  required_providers {
    primitive = {
      source  = "pulumi/primitive"
      version = "7.0.0"
    }
  }
}

data "primitive_invoke" "invokeResult" {
  boolean      = var.plainString
  float        = var.plainInteger
  integer      = var.plainNumericString
  string       = var.plainBool
  number_array = [var.plainInteger, var.plainNumericString, var.plainNumber]
  boolean_map = {
    "fromBool"   = var.plainBool
    "fromString" = var.plainString
  }
}

resource "primitive_resource" "plainValues" {
  lifecycle {
    create_before_destroy = true
  }
  boolean      = var.plainString
  float        = var.plainInteger
  integer      = var.plainNumericString
  string       = var.plainNumber
  number_array = [var.plainInteger, var.plainNumericString, var.plainNumber]
  boolean_map = {
    "fromBool"   = var.plainBool
    "fromString" = var.plainString
  }
}
resource "primitive_resource" "secretValues" {
  lifecycle {
    create_before_destroy = true
  }
  boolean      = var.secretString
  float        = var.secretInteger
  integer      = var.secretNumericString
  string       = var.secretNumber
  number_array = [var.plainInteger, var.plainNumericString, var.plainNumber]
  boolean_map = {
    "fromBool"   = var.plainBool
    "fromString" = var.plainString
  }
}
resource "primitive_resource" "invokeValues" {
  lifecycle {
    create_before_destroy = true
  }
  boolean      = data.primitive_invoke.invokeResult.boolean
  float        = data.primitive_invoke.invokeResult.float
  integer      = data.primitive_invoke.invokeResult.integer
  string       = data.primitive_invoke.invokeResult.string
  number_array = data.primitive_invoke.invokeResult.number_array
  boolean_map  = data.primitive_invoke.invokeResult.boolean_map
}
variable "plainBool" {
  type = bool
}
variable "plainNumber" {
  type = number
}
variable "plainInteger" {
  type = number
}
variable "plainString" {
  type = string
}
variable "plainNumericString" {
  type = string
}
variable "secretNumber" {
  type = number
}
variable "secretInteger" {
  type = number
}
variable "secretString" {
  type = string
}
variable "secretNumericString" {
  type = string
}
