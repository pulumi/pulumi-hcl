resource "r" "single:index/single:Single" {
  inputValue = "world"
  options {
    deleteBeforeReplace = true
  }
}

output "data_answer" {
  value = invoke("single:index/getSingle:getSingle", {
    query    = "hi"
    tagValue = "v"
  }).answer
}

output "resource_result" {
  value = r.result
}

