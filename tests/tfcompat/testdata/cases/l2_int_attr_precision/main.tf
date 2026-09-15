# An integer literal that fits in int64 but exceeds the exact-integer range of
# a float64 (2^53 + 1 = 9007199254740993). OpenTofu carries resource-attribute
# numbers as big.Float and hands the provider the exact value; a resource input
# that round-trips through a float64 loses the low bit and the provider instead
# receives 9007199254740992.
resource "collections_thing" "big" {
  ports = [9007199254740993]
}

output "summary" {
  value = collections_thing.big.summary
}
