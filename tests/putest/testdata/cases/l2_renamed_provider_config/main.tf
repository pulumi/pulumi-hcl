provider "renamed" {
  endpoint = "https://example.test/api"

  default_options {
    retries = 3
  }
}

resource "renamed_thing" "r" {
  function_name = "alpha"
}

output "endpoint_seen" {
  # Resource surfaces back the provider's endpoint (TF `endpoint` →
  # Pulumi `host`). Exercises provider-config scalar input rename.
  value = renamed_thing.r.provider_endpoint
}

output "retries_seen" {
  # Provider's `default_options { retries }` (TF) is renamed to
  # `defaults.retryCount` on the Pulumi side. Exercises provider-config
  # nested-block input rename.
  value = renamed_thing.r.provider_retries
}
