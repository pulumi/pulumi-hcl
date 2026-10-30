# `settings` is a MaxItems=1 block, which the bridge flattens to one object.
# A splat reads it as a list of one element.
resource "r" "renamed:index/thing:Thing" {
  name = "alpha"
  config = {
    sizeWindow = 5
  }
  options {
    deleteBeforeReplace = true
  }
}

output "splat" {
  value = r.config[*].sizeWindow
}

