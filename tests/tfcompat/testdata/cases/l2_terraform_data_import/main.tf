resource "terraform_data" "a" {
  input = "from-config"
}

import {
  to = terraform_data.a
  id = "existing-id"
}

output "id" {
  value = terraform_data.a.id
}

output "out" {
  value = terraform_data.a.output
}
