terraform {
  required_providers {
    borrowbase = {
      source  = "pulumi/borrowbase"
      version = "56.0.0"
    }
    myborrow = {
      source  = "pulumi/myborrow"
      version = "3.0.0"
    }
  }
}

// An extension resource whose property type is defined by the base provider
// rather than by the extension itself.
resource "myborrow_widget" "widget" {
  lifecycle {
    create_before_destroy = true
  }
}
output "metadataName" {
  value = myborrow_widget.widget.metadata.name
}
