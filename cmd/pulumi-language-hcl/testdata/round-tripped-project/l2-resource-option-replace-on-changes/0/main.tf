terraform {
  required_providers {
    replaceonchanges = {
      source  = "pulumi/replaceonchanges"
      version = "25.0.0"
    }
  }
}

// Stage 0: Initial resource creation
// Scenario 1: Schema-based replaceOnChanges on replaceProp
resource "replaceonchanges_resource_a" "schemaReplace" {
  lifecycle {
    create_before_destroy = true
  }
  value        = true
  replace_prop = true
}
// Scenario 2: Option-based replaceOnChanges on value
resource "replaceonchanges_resource_b" "optionReplace" {
  pulumi {
    replace_on_changes = [value]
  }
  lifecycle {
    create_before_destroy = true
  }
  value = true
}
// Scenario 3: Both schema and option - will change value
resource "replaceonchanges_resource_a" "bothReplaceValue" {
  pulumi {
    replace_on_changes = [value]
  }
  lifecycle {
    create_before_destroy = true
  }
  value        = true
  replace_prop = true
}
// Scenario 4: Both schema and option - will change replaceProp
resource "replaceonchanges_resource_a" "bothReplaceProp" {
  pulumi {
    replace_on_changes = [value]
  }
  lifecycle {
    create_before_destroy = true
  }
  value        = true
  replace_prop = true
}
// Scenario 5: No replaceOnChanges - baseline update
resource "replaceonchanges_resource_b" "regularUpdate" {
  lifecycle {
    create_before_destroy = true
  }
  value = true
}
// Scenario 6: replaceOnChanges set but no change
resource "replaceonchanges_resource_b" "noChange" {
  pulumi {
    replace_on_changes = [value]
  }
  lifecycle {
    create_before_destroy = true
  }
  value = true
}
// Scenario 7: replaceOnChanges on value, but only replaceProp changes
resource "replaceonchanges_resource_a" "wrongPropChange" {
  pulumi {
    replace_on_changes = [value]
  }
  lifecycle {
    create_before_destroy = true
  }
  value        = true
  replace_prop = true
}
// Scenario 8: Multiple properties in replaceOnChanges array
resource "replaceonchanges_resource_a" "multiplePropReplace" {
  pulumi {
    replace_on_changes = [value, replace_prop]
  }
  lifecycle {
    create_before_destroy = true
  }
  value        = true
  replace_prop = true
}
