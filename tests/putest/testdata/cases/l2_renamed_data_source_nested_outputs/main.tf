data "renamed_lookup" "d" {
  query = "lambda"
}

output "tag" {
  # TF `result { tag }` (a computed nested block) is renamed by the bridge
  # to `outcome.label`. The engine must surface the output back under the
  # TF names so this traversal works.
  value = data.renamed_lookup.d.result[0].tag
}
