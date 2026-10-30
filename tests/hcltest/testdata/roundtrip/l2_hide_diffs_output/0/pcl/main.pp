# `out_val` is an output that is not an input. The display diffs outputs, so
# hide_diffs can name it.
resource "t" "hd:index:Thing" {
  value = "a"
  options {
    hideDiffs           = [outVal]
    deleteBeforeReplace = true
  }
}

output "out_val" {
  value = t.outVal
}

