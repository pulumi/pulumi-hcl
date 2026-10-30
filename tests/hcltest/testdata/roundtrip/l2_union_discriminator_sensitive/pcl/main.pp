config "disc" "string" {
  sensitive = true
  default   = "a"
}

resource "t" "union:index:Thing" {
  cfg = {
    type  = disc
    value = "x"
  }
  options {
    deleteBeforeReplace = true
  }
}

