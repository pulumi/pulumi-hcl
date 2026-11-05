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
    providers           = [simple_]
    deleteBeforeReplace = true
  }
}

# The value names the default provider of this module, which the round trip
# renames.
component "passed" "../passed" {
  options {
    providers = {
      simple = simple_
    }
  }
}

output "passed" {
  value = passed.value
}

output "value" {
  value = "${r.prefixResult}|${r_.prefixResult}"
}

