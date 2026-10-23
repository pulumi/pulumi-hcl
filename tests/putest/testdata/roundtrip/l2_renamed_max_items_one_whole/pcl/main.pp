# `settings` and `filter` are MaxItems=1 blocks: the bridge flattens each to
# one object, or to null when the block is absent, but HCL reads a list.
resource "set" "renamed:index/thing:Thing" {
  name = "set"
  config = {
    sizeWindow = 5
  }
  options {
    deleteBeforeReplace = true
  }
}

resource "unset" "renamed:index/thing:Thing" {
  name = "unset"
  options {
    deleteBeforeReplace = true
  }
}

d = invoke("renamed:index/getLookup:getLookup", {
  query = "lambda"
  lookupFilter = {
    kindLabel = "trigger"
  }
})

each_ = {for key, value in {
  lambda = true
  } : key => invoke("renamed:index/getLookup:getLookup", {
    query = key
    lookupFilter = {
      kindLabel = "trigger"
    }
})}

resource "each" "renamed:index/thing:Thing" {
  name = "len-${length((each_[range.key].lookupFilter == null ? [] : [each_[range.key].lookupFilter]))}"
  options {
    range = {
      lambda = true
    }
    deleteBeforeReplace = true
  }
}

output "set_len" {
  value = length((set.config == null ? [] : [set.config]))
}

output "unset_len" {
  value = length((unset.config == null ? [] : [unset.config]))
}

output "unset_is_null" {
  value = (unset.config == null ? [] : [unset.config]) == null
}

output "data_len" {
  value = length((d.lookupFilter == null ? [] : [d.lookupFilter]))
}

output "each_len" {
  value = each["lambda"].arn
}

