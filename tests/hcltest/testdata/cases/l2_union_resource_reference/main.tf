terraform {
  required_providers {
    unionref = {
      source = "pulumi/unionref"
    }
  }
}

resource "unionref_github" "gh" {}

resource "unionref_gitlab" "gl" {}

resource "unionref_pipeline" "p" {
  source = unionref_github.gh
  program = {
    source = unionref_gitlab.gl
  }
  sources = [unionref_github.gh, unionref_gitlab.gl]
}
