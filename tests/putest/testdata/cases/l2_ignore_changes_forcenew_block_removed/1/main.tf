# Stage 1: remove the whole `settings` block. ignore_changes can no longer
# reset the ForceNew `mode` (its index is gone), so the resource is REPLACED
# and `settings` reports the replacement's empty list, matching OpenTofu. The
# plugin path also replaces but reports the removed block's old value
# (https://github.com/pulumi/pulumi-hcl/issues/508).
resource "fnblock_resource" "r" {
  note = "y"
  lifecycle { ignore_changes = [settings[0].mode] }
}

output "settings" { value = fnblock_resource.r.settings }
