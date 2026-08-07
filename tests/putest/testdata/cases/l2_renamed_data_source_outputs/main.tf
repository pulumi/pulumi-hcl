data "renamed_lookup" "d" {
  query = "lambda"
}

output "matched" {
  # Unrenamed scalar output, sanity check.
  value = data.renamed_lookup.d.matched
}

output "from" {
  # `upstream` (TF) — bridge renames it to `source` on the Pulumi side.
  # Engine must surface it as `upstream` (the TF name) in the eval context.
  value = data.renamed_lookup.d.upstream
}
