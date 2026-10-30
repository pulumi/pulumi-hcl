terraform {
  required_providers {
    single = {
      source  = "pulumi/single"
      version = "0.0.1"
    }
  }
}

data "single" "invoke_0" {
  query     = "hi"
  tag_value = "v"
}

resource "single" "r" {
  input_value = "world"
}
output "data_answer" {
  value = data.single.invoke_0.answer
}
output "resource_result" {
  value = single.r.result
}
