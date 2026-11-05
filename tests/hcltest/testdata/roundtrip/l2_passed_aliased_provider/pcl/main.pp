resource "simple" "pulumi:providers:simple" {
  prefix = "default"
}

resource "east" "pulumi:providers:simple" {
  prefix = "east"
}

component "mid" "./modules/mid" {
  options {
    providers = {
      simple = east
    }
  }
}

output "result" {
  value = mid.result
}

