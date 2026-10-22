# `settings` is a MaxItems=1 block, which the bridge flattens to one object,
# or to null when the block is absent. A splat reads an absent block as an
# empty list.
resource "r" "renamed:index/thing:Thing" {
  name = "alpha"
  options {
    deleteBeforeReplace = true
  }
}

output "splat" {
  value = r.config[*].sizeWindow
}

