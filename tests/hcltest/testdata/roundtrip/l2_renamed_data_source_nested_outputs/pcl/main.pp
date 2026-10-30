output "tag" {
  # TF `result { tag }` (a computed nested block) is renamed by the bridge
  # to `outcome.label`. The engine must surface the output back under the
  # TF names so this traversal works.
  value = invoke("renamed:index/getLookup:getLookup", {
    query = "lambda"
  }).outcome[0].label
}

