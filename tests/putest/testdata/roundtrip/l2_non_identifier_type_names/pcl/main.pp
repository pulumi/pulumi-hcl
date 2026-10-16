resource "a" "odd:dot.ted/v1:Thing" {
  value = "a"
  options {
    deleteBeforeReplace = true
  }
}

# The dotted spelling names the same type, but a reference cannot use it.
resource "b" "odd:dot.ted/v1:Thing" {
  value = a.value
  options {
    deleteBeforeReplace = true
  }
}

resource "c" "odd:mod:Dot.Member" {
  value = "c"
  options {
    deleteBeforeReplace = true
  }
}

resource "d" "odd:mod:Dot.Member" {
  value = c.value
  options {
    dependsOn           = [a]
    deleteBeforeReplace = true
  }
}

output "value" {
  value = invoke("odd:dot.ted/v1:getThing", {
    value = c.value
  }).value
}

