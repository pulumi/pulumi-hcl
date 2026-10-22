resource "renamed" "pulumi:providers:renamed" {
  host = "https://example.test/api"
  defaults = [{
    retryCount = 3
  }]
}

resource "r" "renamed:index/thing:Thing" {
  name = "alpha"
  options {
    deleteBeforeReplace = true
  }
}

output "endpoint_seen" {
  # Resource surfaces back the provider's endpoint (TF `endpoint` →
  # Pulumi `host`). Exercises provider-config scalar input rename.
  value = r.providerEndpoint
}

output "retries_seen" {
  # Provider's `default_options { retries }` (TF) is renamed to
  # `defaults.retryCount` on the Pulumi side. Exercises provider-config
  # nested-block input rename.
  value = r.providerRetries
}

