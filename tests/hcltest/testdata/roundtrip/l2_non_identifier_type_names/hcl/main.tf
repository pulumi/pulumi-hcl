terraform {
  required_providers {
    odd = {
      source  = "pulumi/odd"
      version = "0.0.1"
    }
  }
}

data "odd_dot_ted_v1_thing" "e" {
  value = odd_mod_dot_member.c.value
}
data "odd_dot_ted_v1_thing" "f" {
  value = data.odd_dot_ted_v1_thing.e.value
}

resource "odd_dot_ted_v1_thing" "a" {
  value = "a"
}
# The dotted spelling names the same type, but a reference cannot use it.
resource "odd_dot_ted_v1_thing" "b" {
  value = odd_dot_ted_v1_thing.a.value
}
resource "odd_mod_dot_member" "c" {
  value = "c"
}
resource "odd_mod_dot_member" "d" {
  depends_on = [odd_dot_ted_v1_thing.a]
  value      = odd_mod_dot_member.c.value
}
output "value" {
  value = data.odd_dot_ted_v1_thing.e.value
}
