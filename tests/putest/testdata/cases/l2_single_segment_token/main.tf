data "single" "ds" {
  query     = "hi"
  tag_value = "v"
}

resource "single" "r" {
  input_value = "world"
}

output "data_answer" {
  value = data.single.ds.answer
}

output "resource_result" {
  value = single.r.result
}
