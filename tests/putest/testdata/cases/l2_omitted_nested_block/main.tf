# `rule` (MaxItems=1) is an optional nested block, left out here: it reads as
# `[]`, matching OpenTofu. The plugin path reads null (https://github.com/pulumi/pulumi-hcl/issues/508).
resource "blocky_thing" "t" {
  name = "omit"

  policy {
    effect = "deny"
  }
}

output "rule" {
  value = blocky_thing.t.policy[0].rule
}

output "rule_len" {
  value = length(blocky_thing.t.policy[0].rule)
}
