resource "gh" "unionref:index:Github" {
  options {
    deleteBeforeReplace = true
  }
}

resource "gl" "unionref:index:Gitlab" {
  options {
    deleteBeforeReplace = true
  }
}

resource "p" "unionref:index:Pipeline" {
  source = gh
  program = {
    source = gl
  }
  sources = [gh, gl]
  options {
    deleteBeforeReplace = true
  }
}

