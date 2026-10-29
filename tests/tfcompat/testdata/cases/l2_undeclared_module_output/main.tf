module "m" {
  source = "./m"
}

output "o" {
  value = module.m.nope
}
