terraform {
  required_providers {
    renamed = { source = "pulumi/renamed" }
  }
}

# The data block has two consumers, so its invoke is hoisted to a local with
# one result for each element of the range.
data "renamed_lookup" "each" {
  for_each = { lambda = true }
  query    = each.key
  filter {
    kind = "trigger"
  }
}

resource "renamed_thing" "each" {
  for_each      = { lambda = true }
  function_name = data.renamed_lookup.each[each.key].filter[0].kind
}

output "kind" {
  value = data.renamed_lookup.each["lambda"].filter[0].kind
}

output "arn" {
  value = renamed_thing.each["lambda"].arn
}
