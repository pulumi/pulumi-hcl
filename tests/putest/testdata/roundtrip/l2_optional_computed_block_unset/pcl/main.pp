# `identity` is an Optional+Computed MaxItems=1 nested block the provider
# leaves unset; `identity_set` is its TypeSet twin. Matching OpenTofu, the
# list variant reads null and the set variant stays an empty set. The plugin
# path reads the set variant as null too (https://github.com/pulumi/pulumi-hcl/issues/508).
resource "t" "optcomp:index/thing:Thing" {
  name = "probe"
  options {
    deleteBeforeReplace = true
  }
}

output "identity_is_null" {
  value = t.identity == null
}

output "identity_json" {
  value = toJSON(t.identity)
}

output "identity_set_is_null" {
  value = t.identitySet == null
}

output "identity_set_json" {
  value = toJSON(t.identitySet)
}

