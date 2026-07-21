# A `timeouts` block is part of the resource's schema, so its values round-trip
# into state and are readable as an ordinary attribute of the resource.
resource "timeoutable_resource" "test" {
  input_one = "hello"

  timeouts {
    create = "5m"
  }
}

output "create_timeout" {
  value = timeoutable_resource.test.timeouts.create
}
