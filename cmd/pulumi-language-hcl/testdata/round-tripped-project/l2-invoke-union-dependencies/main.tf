terraform {
  required_providers {
    simple = {
      source  = "pulumi/simple"
      version = "2.0.0"
    }
    simple-invoke = {
      source  = "pulumi/simple-invoke"
      version = "10.0.0"
    }
  }
}

data "simple-invoke_secret_invoke" "invoke_0" {
  value           = simple-invoke_string_resource.a.text
  secret_response = simple_resource.b.value
}

// Baseline for invoke dependency propagation: an invoke that reads properties from two different
// resources produces a return value whose consumer must depend on the union of both.
resource "simple-invoke_string_resource" "a" {
  lifecycle {
    create_before_destroy = true
  }
  text = "hello"
}
resource "simple_resource" "b" {
  lifecycle {
    create_before_destroy = true
  }
  value = true
}
resource "simple-invoke_string_resource" "d" {
  lifecycle {
    create_before_destroy = true
  }
  text = data.simple-invoke_secret_invoke.invoke_0.response
}
