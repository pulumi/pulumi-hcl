module "child" {
  source = "./modules/child"
}

output "out" {
  value = module.child.out
}
