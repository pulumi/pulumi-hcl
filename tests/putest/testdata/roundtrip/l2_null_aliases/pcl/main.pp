resource "res" "simple:index/resource:Resource" {
  options {
    aliases             = notImplemented("tolist(null)")
    deleteBeforeReplace = true
  }
}

