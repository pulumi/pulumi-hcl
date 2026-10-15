terraform {
  required_providers {
    simple = {
      source  = "pulumi/simple"
      version = "0.0.1"
    }
  }
}

resource "simple_resource" "res" {
  pulumi {
    aliases = tolist(null)
  }
}
