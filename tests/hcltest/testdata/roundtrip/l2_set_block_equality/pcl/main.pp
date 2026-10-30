# `filter` is a repeating TypeSet nested block: a cty set, so `== toset(...)`
# of the same elements is true in either order, matching OpenTofu. The plugin
# path materializes an ordered tuple and yields false (https://github.com/pulumi/pulumi-hcl/issues/509).
resource "t" "blocky:index/thing:Thing" {
  name = "seteq"
  filters = [{
    name   = "zebra"
    values = "z"
    }, {
    name   = "apple"
    values = "a"
  }]
  options {
    deleteBeforeReplace = true
  }
}

output "eq_same" {
  value = t.filters == notImplemented("toset([\n    { name = \"zebra\", values = \"z\" },\n    { name = \"apple\", values = \"a\" },\n  ])")
}

output "eq_reordered" {
  value = t.filters == notImplemented("toset([\n    { name = \"apple\", values = \"a\" },\n    { name = \"zebra\", values = \"z\" },\n  ])")
}

