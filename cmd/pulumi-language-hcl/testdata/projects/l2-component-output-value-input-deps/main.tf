terraform {
  required_providers {
    output-value-input-deps = {
      source  = "pulumi/output-value-input-deps"
      version = "61.0.0"
    }
  }
}

resource "output-value-input-deps_custom" "a" {
  lifecycle {
    create_before_destroy = true
  }
  value = "a"
}
resource "output-value-input-deps_custom" "b" {
  lifecycle {
    create_before_destroy = true
  }
  value = "b"
}
resource "output-value-input-deps_component" "comp" {
  lifecycle {
    create_before_destroy = true
  }
  refs = {
    x = output-value-input-deps_custom.a.value
    y = output-value-input-deps_custom.b.value
  }
}
