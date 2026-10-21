output "matched" {
  # Unrenamed scalar output, sanity check.
  value = invoke("renamed:index/getLookup:getLookup", {
    query = "lambda"
  }).matched
}

output "from" {
  # `upstream` (TF) — bridge renames it to `source` on the Pulumi side.
  # Engine must surface it as `upstream` (the TF name) in the eval context.
  value = invoke("renamed:index/getLookup:getLookup", {
    query = "lambda"
  }).source
}

