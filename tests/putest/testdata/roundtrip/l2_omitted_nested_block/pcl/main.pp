# `rule` (MaxItems=1) is an optional nested block, left out here: it reads as
# `[]`, matching OpenTofu. The plugin path reads null (https://github.com/pulumi/pulumi-hcl/issues/508).
resource "t" "blocky:index/thing:Thing" {
  name = "omit"
  policy = [{
    effect = "deny"
  }]
  options {
    deleteBeforeReplace = true
  }
}

output "rule" {
  value = t.policy[0].rule
}

output "rule_len" {
  value = length(t.policy[0].rule)
}

