package {
  baseProviderName    = "borrowbase"
  baseProviderVersion = "56.0.0"
  parameterization {
    name    = "myborrow"
    version = "3.0.0"
    value   = "V2lkZ2V0"
  }
}

// An extension resource whose property type is defined by the base provider
// rather than by the extension itself.
resource "widget" "myborrow:index:Widget" {
}

output "metadataName" {
  value = widget.metadata.name
}

