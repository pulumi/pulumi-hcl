terraform {
  required_providers {
    nestedobject = {
      source  = "pulumi/nestedobject"
      version = "1.42.0"
    }
  }
}

data "nestedobject_values" "values" {
  names = nestedobject_container.source.inputs
}

# A resource whose computed output feeds the invoke, forcing the invoke into its
# output-versioned form so that `values` is an Output.
resource "nestedobject_container" "source" {
  lifecycle {
    create_before_destroy = true
  }
  inputs = ["alpha", "bravo", "charlie"]
}
# Ranges over the length of the invoke's computed list and indexes that same
# Output-typed list by the loop counter inside the body. This is the shape from
# https://github.com/pulumi/pulumi/issues/12507.
resource "nestedobject_target" "routes" {
  count = length(data.nestedobject_values.values.results)
  pulumi {
    name ="routes-${count.index}"
  }
  lifecycle {
    create_before_destroy = true
  }
  name = data.nestedobject_values.values.results[count.index]
}
