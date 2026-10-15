terraform {
  required_providers {
    renamed = {
      source  = "pulumi/renamed"
      version = "0.0.1"
    }
  }
}

data "renamed_lookup" "invoke_0" {
  query = "lambda"
}

output "tag" {
  value = data.renamed_lookup.invoke_0.result[0].tag
}
