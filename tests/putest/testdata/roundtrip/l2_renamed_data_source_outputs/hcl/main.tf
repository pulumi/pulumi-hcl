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
  # Unrenamed scalar output, sanity check.
  value = data.renamed_lookup.invoke_0.matched
}
output "from" {
  # `upstream` (TF) — bridge renames it to `source` on the Pulumi side.
  # Engine must surface it as `upstream` (the TF name) in the eval context.
  value = data.renamed_lookup.invoke_1.upstream
}
