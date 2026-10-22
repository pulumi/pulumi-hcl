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
  filter {
    kind = "trigger"
  }
}

output "matched" {
  # Provider echoes the inputs back into `matched`, so this asserts the
  # renamed nested input was forwarded correctly.
  value = data.renamed_lookup.invoke_0.matched
}
