terraform {
  required_providers {
    pick-call = {
      source  = "pulumi/pick-call"
      version = "57.0.0"
    }
    simple-invoke = {
      source  = "pulumi/simple-invoke"
      version = "10.0.0"
    }
  }
}

call "c" "get_both" {
  a = simple-invoke_string_resource.pa.text
  b = simple-invoke_string_resource.pb.text
}

// Exercises per-leaf OutputValue dependency propagation through a method Call.
//
// `getBoth` returns `{wrapper: {a, b}}` where `wrapper` is a plain field of a named object type and `a`,
// `b` are non-plain. The provider annotates `ReturnDependencies["wrapper"] = {pa, pb}` — the coarse
// answer, since ReturnDependencies cannot address nested leaves. Per-leaf precision only survives via
// OutputValues wrapping `a` and `b` individually.
//
// `g` consumes only `wrapper.a`:
//   - OutputValues path: per-leaf dep on `a` carries only pa; g depends on {pa, c}.
//   - Legacy path: the coarse {pa, pb} on `wrapper` propagates to any descent; g depends on {pa, pb, c}.
resource "pick-call_picker" "c" {
  lifecycle {
    create_before_destroy = true
  }
  value = "c"
}
resource "simple-invoke_string_resource" "pa" {
  lifecycle {
    create_before_destroy = true
  }
  text = "pa"
}
resource "simple-invoke_string_resource" "pb" {
  lifecycle {
    create_before_destroy = true
  }
  text = "pb"
}
resource "simple-invoke_string_resource" "g" {
  lifecycle {
    create_before_destroy = true
  }
  text = local.both.wrapper.a
}
locals {
  both = call.c.get_both
}
