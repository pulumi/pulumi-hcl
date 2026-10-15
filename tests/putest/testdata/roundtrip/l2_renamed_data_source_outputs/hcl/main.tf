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
data "renamed_lookup" "invoke_1" {
  query = "lambda"
}

output "matched" {
  value = data.renamed_lookup.invoke_0.matched
}
output "from" {
  value = data.renamed_lookup.invoke_1.upstream
}
