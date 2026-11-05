# HCL scopes names by kind and type, so each declaration here is named `r`.
config "r" "string" {
  default = "v"
}

r_ = "${r}-local"

resource "r__" "pulumi:providers:simple" {
  __logicalName = "r"
  prefix        = "p"
}

resource "r___" "simple:index/resource:Resource" {
  __logicalName = "r"
  inputOne      = r_
  options {
    provider            = r__
    deleteBeforeReplace = true
  }
}

resource "r____" "blocky:index/single:Single" {
  __logicalName = "r"
  options {
    deleteBeforeReplace = true
  }
}

r______ = invoke("simple:index/getLookup:getLookup", {
  query = r___.result
  }, {
  provider = r__
})

component "r_____" "./modules/r" {
  __logicalName = "r"
  value         = r______.prefixResult
}

# The key names the aliased provider of the child module. The value names the
# provider `r` of this module, which the round trip renames.
component "passed" "./modules/passed" {
  options {
    providers = {
      simple = r__
    }
  }
}

output "r" {
  value = r_____.value
}

output "passed" {
  value = passed.value
}

output "nested_passed" {
  value = r_____.passed
}

output "variable" {
  value = r
}

output "local" {
  value = r_
}

output "resource" {
  value = r___.prefixResult
}

output "other_type" {
  value = r____.id
}

output "data" {
  value = r______.prefixResult
}

