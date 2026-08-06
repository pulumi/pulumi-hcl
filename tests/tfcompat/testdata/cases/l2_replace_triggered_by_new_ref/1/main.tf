resource "simple_resource" "trigger" {
  input_one = "a"
}

resource "simple_resource" "dependent" {
  input_one = "constant"
  lifecycle {
    replace_triggered_by = [simple_resource.trigger.result]
  }
}
