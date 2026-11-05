resource "r" "simple:index/resource:Resource" {
  inputOne = "passed"
  options {
    deleteBeforeReplace = true
  }
}

output "value" {
  value = r.prefixResult
}

