# The "pfx-beta" provider serves the "pfx_*" types, the same way
# hashicorp/google-beta serves "google_*" types. `join_str` is variadic.
terraform {
  required_providers {
    pfx-beta = {
      source = "hashicorp/pfx-beta"
    }
  }
}

output "joined" {
  value = provider::pfx-beta::join_str("-", "a", "b", "c")
}

output "joined_expanded" {
  value = provider::pfx-beta::join_str(".", ["x", "y"]...)
}
