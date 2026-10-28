variable "x" {
  type = string
}

resource "terraform_data" "t" {
  input            = var.x
  triggers_replace = var.x
}
