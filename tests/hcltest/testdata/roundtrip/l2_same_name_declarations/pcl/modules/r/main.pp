config "value" "string" {
}

# The local and the default provider are both named `simple`.
simple = "${value}-child"

resource "simple_" "pulumi:providers:simple" {
  __logicalName = "simple"
  prefix        = "c"
}

r_ = invoke("simple:index/getLookup:getLookup", {
  query = simple
  }, {
  provider = simple_
})

# The root module declares a data block with the same type and name.
resource "r" "simple:index/resource:Resource" {
  inputOne = r_.prefixResult
  options {
    provider            = simple_
    deleteBeforeReplace = true
  }
}

output "value" {
  value = "${r.prefixResult}|${r_.prefixResult}"
}

