resource "blocky_thing" "t" {
  name = "alpha"
  settings {
    mode    = "fast"
    verbose = true
  }
}

output "mode" {
  value = blocky_thing.t.settings[0].mode
}
