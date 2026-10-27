terraform {
  required_providers {
    hd = {
      source  = "pulumi/hd"
      version = "0.0.1"
    }
  }
}

# `out_val` is an output that is not an input. The display diffs outputs, so
# hide_diffs can name it.
resource "hd_thing" "t" {
  pulumi {
    hide_diffs = [out_val]
  }
  value = "a"
}
output "out_val" {
  value = hd_thing.t.out_val
}
