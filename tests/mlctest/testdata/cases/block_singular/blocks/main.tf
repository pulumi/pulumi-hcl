resource "blocky_thing" "t" {
  name = "alpha"
  settings {
    mode    = "fast"
    verbose = true
  }
}

output "summary" {
  value = blocky_thing.t.summary
}
