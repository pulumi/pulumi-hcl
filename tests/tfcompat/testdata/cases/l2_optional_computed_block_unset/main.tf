# `identity` is an Optional+Computed MaxItems=1 nested block that the provider
# leaves entirely unset at create time. In OpenTofu an unset optional+computed
# singular block materializes as null, so `== null` is true and jsonencode
# yields "null"; pulumi-hcl agrees.
resource "optcomp_thing" "t" {
  name = "probe"
}

output "identity_is_null" {
  value = optcomp_thing.t.identity == null
}

output "identity_json" {
  value = jsonencode(optcomp_thing.t.identity)
}

# The TypeSet twin stays an empty set when unset in OpenTofu: `== null` is
# false and jsonencode yields "[]". pulumi-hcl reads it as null instead — the
# divergence this case is skipped for.
output "identity_set_is_null" {
  value = optcomp_thing.t.identity_set == null
}

output "identity_set_json" {
  value = jsonencode(optcomp_thing.t.identity_set)
}
