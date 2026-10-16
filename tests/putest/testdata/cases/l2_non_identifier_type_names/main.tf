terraform {
  required_providers {
    odd = {
      source = "pulumi/odd"
    }
  }
}

resource "odd_dot_ted_v1_thing" "a" {
  value = "a"
}

# The dotted spelling names the same type, but a reference cannot use it.
resource "odd_dot.ted_v1_thing" "b" {
  value = odd_dot_ted_v1_thing.a.value
}

resource "odd_mod_dot_member" "c" {
  value = "c"
}

resource "odd_mod_dot.member" "d" {
  value      = odd_mod_dot_member.c.value
  depends_on = [odd_dot_ted_v1_thing.a]
}

data "odd_dot_ted_v1_thing" "e" {
  value = odd_mod_dot_member.c.value
}

data "odd_dot.ted_v1_thing" "f" {
  value = data.odd_dot_ted_v1_thing.e.value
}

output "value" {
  value = data.odd_dot_ted_v1_thing.e.value
}
