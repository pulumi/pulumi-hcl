data "renamed_lookup" "d" {
  query = "lambda"

  filter {
    # TF `filter { kind }` → Pulumi `lookupFilter.kindLabel`. Exercises a
    # data-source nested-block input rename for both the block name and the
    # nested field name.
    kind = "trigger"
  }
}

output "matched" {
  # Provider echoes the inputs back into `matched`, so this asserts the
  # renamed nested input was forwarded correctly.
  value = data.renamed_lookup.d.matched
}
