terraform {
  required_providers {
    test = {
      source  = "pulumi/test"
      version = "1.0.0"
    }
  }
}

data "test_helm_sh_v3_release" "release" {
  name = test_helm_sh_v3_release.app.chart
}

resource "test_helm_sh_v3_release" "crds" {
  lifecycle {
    create_before_destroy = true
  }
  chart = "crds"
}
resource "test_helm_sh_v3_release" "app" {
  depends_on = [test_helm_sh_v3_release.crds]
  lifecycle {
    create_before_destroy = true
  }
  chart ="app-${test_helm_sh_v3_release.crds.chart}"
}
output "releaseId" {
  value = data.test_helm_sh_v3_release.release.id
}
