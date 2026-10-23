terraform {
  required_providers {
    renamed = {
      source  = "pulumi/renamed"
      version = "0.0.1"
    }
  }
}

data "renamed_lookup" "invoke_0" {
  for_each = {
    "lambda" = true
  }
  query = each.key
  filter {
    kind = "trigger"
  }
}

# The data block has two consumers, so its invoke is hoisted to a local with
# one result for each element of the range.
resource "renamed_thing" "each" {
  for_each = {
    "lambda" = true
  }
  pulumi {
    name ="each-${each.key}"
  }
  function_name = local.each_[each.key].filter[0].kind
}
locals {
  each_ = {for key, value in {
    "lambda" = true
  } : key => data.renamed_lookup.invoke_0[key]}
}
output "kind" {
  value = local.each_["lambda"].filter[0].kind
}
output "arn" {
  value = renamed_thing.each["lambda"].arn
}
