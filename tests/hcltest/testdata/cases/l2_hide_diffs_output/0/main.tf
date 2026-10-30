terraform {
  required_providers {
    hd = { source = "pulumi/hd" }
  }
}

# `out_val` is an output that is not an input. The display diffs outputs, so
# hide_diffs can name it.
resource "hd_thing" "t" {
  value = "a"
  pulumi {
    hide_diffs = [out_val]
  }
}

output "out_val" { value = hd_thing.t.out_val }
