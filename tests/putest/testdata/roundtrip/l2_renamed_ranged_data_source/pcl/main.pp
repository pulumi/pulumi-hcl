each_ = {for key, value in {
  lambda = true
  } : key => invoke("renamed:index/getLookup:getLookup", {
    query = key
    lookupFilter = {
      kindLabel = "trigger"
    }
})}

# The data block has two consumers, so its invoke is hoisted to a local with
# one result for each element of the range.
resource "each" "renamed:index/thing:Thing" {
  name = each_[range.key].lookupFilter.kindLabel
  options {
    range = {
      lambda = true
    }
    deleteBeforeReplace = true
  }
}

output "kind" {
  value = each_["lambda"].lookupFilter.kindLabel
}

output "arn" {
  value = each["lambda"].arn
}

