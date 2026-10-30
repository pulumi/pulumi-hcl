terraform {
  required_providers {
    odd = {
      source = "pulumi/odd"
    }
  }
}

resource "odd_dot_ted_v1_thing" "a" {
  dot_ted    = "a-dot_ted"
  sla_sh     = "a-sla_sh"
  at         = "a-at"
  bang       = "a-bang"
  dollar     = "a-dollar"
  colon_name = "a-colon_name"
  plus_name  = "a-plus_name"
  digits     = "a-digits"
  emoji      = "a-emoji"
  mid_tilde  = "a-mid_tilde"
}

resource "odd_dot_ted_v1_thing" "b" {
  dot_ted    = odd_dot_ted_v1_thing.a.dot_ted
  sla_sh     = odd_dot_ted_v1_thing.a.sla_sh
  at         = odd_dot_ted_v1_thing.a.at
  bang       = odd_dot_ted_v1_thing.a.bang
  dollar     = odd_dot_ted_v1_thing.a.dollar
  colon_name = odd_dot_ted_v1_thing.a.colon_name
  plus_name  = odd_dot_ted_v1_thing.a.plus_name
  digits     = odd_dot_ted_v1_thing.a.digits
  emoji      = odd_dot_ted_v1_thing.a.emoji
  mid_tilde  = odd_dot_ted_v1_thing.a.mid_tilde
}

data "odd_dot_ted_v1_thing" "c" {
  dot_ted    = odd_dot_ted_v1_thing.b.dot_ted
  sla_sh     = odd_dot_ted_v1_thing.b.sla_sh
  at         = odd_dot_ted_v1_thing.b.at
  bang       = odd_dot_ted_v1_thing.b.bang
  dollar     = odd_dot_ted_v1_thing.b.dollar
  colon_name = odd_dot_ted_v1_thing.b.colon_name
  plus_name  = odd_dot_ted_v1_thing.b.plus_name
  digits     = odd_dot_ted_v1_thing.b.digits
  emoji      = odd_dot_ted_v1_thing.b.emoji
  mid_tilde  = odd_dot_ted_v1_thing.b.mid_tilde
}

output "dot_ted" {
  value = data.odd_dot_ted_v1_thing.c.dot_ted
}

output "sla_sh" {
  value = data.odd_dot_ted_v1_thing.c.sla_sh
}

output "at" {
  value = data.odd_dot_ted_v1_thing.c.at
}

output "bang" {
  value = data.odd_dot_ted_v1_thing.c.bang
}

output "dollar" {
  value = data.odd_dot_ted_v1_thing.c.dollar
}

output "colon_name" {
  value = data.odd_dot_ted_v1_thing.c.colon_name
}

output "plus_name" {
  value = data.odd_dot_ted_v1_thing.c.plus_name
}

output "digits" {
  value = data.odd_dot_ted_v1_thing.c.digits
}

output "emoji" {
  value = data.odd_dot_ted_v1_thing.c.emoji
}

output "mid_tilde" {
  value = data.odd_dot_ted_v1_thing.c.mid_tilde
}
