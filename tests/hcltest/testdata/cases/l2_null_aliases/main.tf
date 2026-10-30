terraform {
  required_providers {
    simple = { source = "pulumi/simple" }
  }
}

resource "simple_resource" "res" {
  pulumi {
    aliases = tolist(null)
  }
}
