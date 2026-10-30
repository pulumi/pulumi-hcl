# Each output needs its parentheses: without them, HCL precedence gives a
# different value.
output "sum_times" {
  value = (1 + 2) * 3
}

output "minus_minus" {
  value = 10 - (4 - 3)
}

output "not_and" {
  value = !(true && false)
}

output "negate_sum" {
  value = -(1 + 2)
}

output "eq_eq" {
  value = (1 == 2) == false
}

output "conditional_index" {
  value = (true ? [1] : [2])[0]
}

output "conditional_cond" {
  value = (false ? true : false) ? "yes" : "no"
}

