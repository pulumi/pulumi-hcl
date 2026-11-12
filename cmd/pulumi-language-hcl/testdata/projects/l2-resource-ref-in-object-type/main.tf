terraform {
  required_providers {
    objectref = {
      source  = "pulumi/objectref"
      version = "62.0.0"
    }
  }
}

resource "objectref_referencing_first_widget" "widget" {
  lifecycle {
    create_before_destroy = true
  }
  settings = {
    name = "widget"
  }
}
resource "objectref_referencing_first_gadget" "gadget" {
  lifecycle {
    create_before_destroy = true
  }
  widget_ref = {
    widget = objectref_referencing_first_widget.widget
  }
}
resource "objectref_referenced_first_alpha" "alpha" {
  lifecycle {
    create_before_destroy = true
  }
  settings = {
    name = "alpha"
  }
}
resource "objectref_referenced_first_beta" "beta" {
  lifecycle {
    create_before_destroy = true
  }
  alpha_ref = {
    alpha = objectref_referenced_first_alpha.alpha
  }
}
