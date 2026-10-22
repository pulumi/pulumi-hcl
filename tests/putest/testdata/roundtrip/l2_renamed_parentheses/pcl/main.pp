resource "r" "renamed:index/thing:Thing" {
  name = "alpha"
  config = {
    sizeWindow = 5
  }
  options {
    deleteBeforeReplace = true
  }
}

# Each reference is inside parentheses, so the conversion must rename the
# expression inside them.
output "fn_name" {
  value = (r.name)
}

output "window" {
  value = (r.config.sizeWindow) + 1
}

output "nested" {
  value = ((r.name))
}

output "from" {
  value = (invoke("renamed:index/getLookup:getLookup", {
    query = "lambda"
  }).source)
}

