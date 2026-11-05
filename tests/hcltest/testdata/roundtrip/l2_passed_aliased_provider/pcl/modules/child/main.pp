resource "r" "simple:index/resource:Resource" {
  inputOne = "world"
  inputTwo = true
  options {
    deleteBeforeReplace = true
  }
}

output "result" {
  value = r.prefixResult
}

