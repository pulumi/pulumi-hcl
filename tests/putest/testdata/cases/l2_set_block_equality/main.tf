# `filter` is a repeating TypeSet nested block: a cty set, so `== toset(...)`
# of the same elements is true in either order, matching OpenTofu. The plugin
# path materializes an ordered tuple and yields false (https://github.com/pulumi/pulumi-hcl/issues/509).
resource "blocky_thing" "t" {
  name = "seteq"
  filter {
    name   = "zebra"
    values = "z"
  }
  filter {
    name   = "apple"
    values = "a"
  }
}

output "eq_same" {
  value = blocky_thing.t.filter == toset([
    { name = "zebra", values = "z" },
    { name = "apple", values = "a" },
  ])
}

output "eq_reordered" {
  value = blocky_thing.t.filter == toset([
    { name = "apple", values = "a" },
    { name = "zebra", values = "z" },
  ])
}
