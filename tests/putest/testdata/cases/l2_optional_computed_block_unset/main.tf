# `identity` is an Optional+Computed MaxItems=1 nested block the provider
# leaves unset; `identity_set` is its TypeSet twin. Matching OpenTofu, the
# list variant reads null and the set variant stays an empty set. The plugin
# path reads the set variant as null too (https://github.com/pulumi/pulumi-hcl/issues/508).
resource "optcomp_thing" "t" {
  name = "probe"
}

output "identity_is_null" {
  value = optcomp_thing.t.identity == null
}

output "identity_json" {
  value = jsonencode(optcomp_thing.t.identity)
}

output "identity_set_is_null" {
  value = optcomp_thing.t.identity_set == null
}

output "identity_set_json" {
  value = jsonencode(optcomp_thing.t.identity_set)
}
