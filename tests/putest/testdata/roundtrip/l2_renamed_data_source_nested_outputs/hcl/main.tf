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
  # TF `result { tag }` (a computed nested block) is renamed by the bridge
  # to `outcome.label`. The engine must surface the output back under the
  # TF names so this traversal works.
  value = data.renamed_lookup.invoke_0.result[0].tag
}
