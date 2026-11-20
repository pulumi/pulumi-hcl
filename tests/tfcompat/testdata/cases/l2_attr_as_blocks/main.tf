resource "pfx_group" "this" {
  rule {
    port        = 22
    cidr_blocks = ["10.0.0.0/8"]
  }
  rule {
    port        = 443
    cidr_blocks = ["0.0.0.0/0", "::/0"]
  }
  dynamic "rule" {
    for_each = [8080, 8443]
    content {
      port        = rule.value
      cidr_blocks = ["192.168.0.0/16"]
    }
  }
}

output "rules" {
  value = pfx_group.this.rule
}
