terraform {
  required_providers {
    test = {
      source  = "pulumi/test"
      version = "1.0.0"
    }
  }
}

data "test_info" "info" {
}

resource "test_sink" "snakeSink" {
  lifecycle {
    create_before_destroy = true
  }
  value = data.test_info.info[0].snake_case_field
}
resource "test_sink" "tagSink" {
  lifecycle {
    create_before_destroy = true
  }
  value = data.test_info.info[0].tags_map["UserKey"]
}
