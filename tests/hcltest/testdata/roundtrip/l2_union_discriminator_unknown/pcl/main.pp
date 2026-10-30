resource "a" "union:index:Thing" {
  cfg = {
    type  = "a"
    value = "x"
  }
  options {
    deleteBeforeReplace = true
  }
}

resource "b" "union:index:Thing" {
  cfg = {
    type  = a.out
    value = "y"
  }
  options {
    deleteBeforeReplace = true
  }
}

