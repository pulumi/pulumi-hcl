resource "r" "single:index/single:Single" {
  myInput = "world"
  options {
    deleteBeforeReplace = true
  }
}

output "data_answer" {
  value = invoke("single:index/getSingle:getSingle", {
    query       = "hi"
    taggedThing = "v"
  }).answer
}

output "resource_result" {
  value = r.result
}

