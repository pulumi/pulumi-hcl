# A numeric literal larger than the largest finite float64 (~1.8e308).
# OpenTofu keeps numbers as arbitrary-precision cty big.Float values, so this
# evaluates to the finite integer 10^400 and is stored as such. pulumi-hcl
# collapses every number to float64 when producing a stack output, so the same
# literal overflows to IEEE-754 +Infinity instead of the finite value.
output "overflow_literal" {
  value = 1e400
}

# The same divergence reached through the multiplication operator: 1e308 * 100
# is finite in big.Float (1e310) but overflows float64.
output "overflow_mul" {
  value = 1e308 * 100
}
