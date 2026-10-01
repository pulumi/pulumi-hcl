package {
  baseProviderName    = "extbase"
  baseProviderVersion = "45.0.0"
  parameterization {
    name    = "myext"
    version = "2.0.0"
    value   = "SGVsbG8="
  }
}

// Tests extension parameterization on an explicit provider.
// The extension resource's provider lookup must resolve to the base provider.
resource "prov" "pulumi:providers:extbase" {
}

resource "greeting" "myext:index:Greeting" {
  options {
    provider = prov
  }
}

resource "base" "extbase:index:Base" {
  options {
    provider = prov
  }
}

output "parameterValue" {
  value = greeting.parameterValue
}

output "baseValue" {
  value = base.baseValue
}

