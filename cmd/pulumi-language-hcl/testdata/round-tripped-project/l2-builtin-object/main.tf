terraform {
  required_providers {
    output = {
      source  = "pulumi/output"
      version = "23.0.0"
    }
  }
}

resource "output_complex_resource" "res" {
  lifecycle {
    create_before_destroy = true
  }
  value = 1
}
output "entriesOutput" {
  value = entries(output_complex_resource.res.output_map)
}
output "lookupOutput" {
  value = lookup(output_complex_resource.res.output_map, "x", "default")
}
output "lookupOutputDefault" {
  value = lookup(output_complex_resource.res.output_map, "y", "default")
}
output "entriesObjectOutput" {
  value = entries(output_complex_resource.res.output_object)
}
output "lookupObjectOutput" {
  value = lookup(output_complex_resource.res.output_object, "output", "default")
}
output "lookupObjectOutputDefault" {
  value = lookup(output_complex_resource.res.output_object, "missing", "default")
}
output "lengthOutput" {
  value = length(output_complex_resource.res.output_map)
}
output "lengthObjectOutput" {
  value = length(output_complex_resource.res.output_object)
}
