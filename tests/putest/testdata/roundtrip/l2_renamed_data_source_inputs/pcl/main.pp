output "matched" {
  # Provider echoes the inputs back into `matched`, so this asserts the
  # renamed nested input was forwarded correctly.
  value = invoke("renamed:index/getLookup:getLookup", {
    query = "lambda"
    lookupFilter = {
      kindLabel = "trigger"
    }
  }).matched
}

