variable "x" {
  type = string
}

resource "terraform_data" "t" {
  input = var.x

  provisioner "local-exec" {
    command = "true"
  }

  lifecycle {
    precondition {
      condition     = var.x != ""
      error_message = "x must be set"
    }
    postcondition {
      condition     = self.output == var.x
      error_message = "output must equal x"
    }
  }
}
