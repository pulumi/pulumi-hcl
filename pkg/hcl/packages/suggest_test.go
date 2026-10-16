// Copyright 2026, Pulumi Corporation.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package packages

import (
	"testing"

	"github.com/pulumi/pulumi-hcl/tests/testutil/schemaloader"
	"github.com/pulumi/pulumi/pkg/v3/codegen/schema"
	"github.com/pulumi/pulumi/pkg/v3/codegen/testing/utils/rapidschema"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"
)

func TestNearestHCLToken(t *testing.T) {
	t.Parallel()

	loader := schemaloader.New(t, schema.PackageSpec{
		Name: "aws",
		Meta: &schema.MetadataSpec{
			ModuleFormat: `(.*)(?:/[^/]*)`,
		},
		Resources: map[string]schema.ResourceSpec{
			"aws:ec2/vpc:Vpc":                  {},
			"aws:s3/bucket:Bucket":             {},
			"aws:lb/loadBalancer:LoadBalancer": {},
		},
		Functions: map[string]schema.FunctionSpec{
			"aws:ec2/getVpc:getVpc":                             {},
			"aws:index/getAvailabilityZone:getAvailabilityZone": {},
		},
	}, schema.PackageSpec{
		Name: "kubernetes",
		Resources: map[string]schema.ResourceSpec{
			"kubernetes:helm.sh/v3:Release":                       {},
			"kubernetes:networking.k8s.io/v1:Ingress":             {},
			"kubernetes:rbac.authorization.k8s.io/v1:ClusterRole": {},
		},
	})

	tests := []struct {
		name       string
		pkg        string
		hclToken   string
		isFunction bool
		want       string
	}{
		{"one-character typo on resource", "aws", "aws_ec2_vpd", false, "aws_ec2_vpc"},
		{"camelCase expansion", "aws", "aws_lb_load_balancr", false, "aws_lb_load_balancer"},
		{"unrelated returns empty", "aws", "aws_completely_unrelated_thing_xyz", false, ""},
		{"function typo strips get", "aws", "aws_availability_zonee", true, "aws_availability_zone"},
		{"dotted module suggests underscores", "kubernetes", "kubernetes_helm_sh_v3_relase", false, "kubernetes_helm_sh_v3_release"},
		{"dotted typo suggests underscores", "kubernetes", "kubernetes_helm.sh_v3_relase", false, "kubernetes_helm_sh_v3_release"},
		{
			"multi-dot typo suggests underscores", "kubernetes", "kubernetes_rbac.authorization.k8s.io_v1_cluster_rol", false,
			"kubernetes_rbac_authorization_k8s_io_v1_cluster_role",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			pkg, err := loader.LoadPackageReferenceV2(t.Context(), &schema.PackageDescriptor{Name: tt.pkg})
			require.NoError(t, err)
			got := nearestHCLToken(pkg, tt.hclToken, tt.isFunction)
			require.Equal(t, tt.want, got)
		})
	}
}

func TestPulumiTokenToHCLForm(t *testing.T) {
	t.Parallel()

	loader := schemaloader.New(t,
		schema.PackageSpec{
			Name: "aws",
			Meta: &schema.MetadataSpec{
				ModuleFormat: `(.*)(?:/[^/]*)`,
			},
			Resources: map[string]schema.ResourceSpec{
				"aws:ec2/vpc:Vpc":                  {},
				"aws:lb/loadBalancer:LoadBalancer": {},
				"aws:index/instance:Instance":      {},
			},
		},
		schema.PackageSpec{
			Name: "kubernetes",
			Resources: map[string]schema.ResourceSpec{
				"kubernetes:helm.sh/v3:Release":             {},
				"kubernetes:storage.k8s.io/v1:StorageClass": {},
			},
			Functions: map[string]schema.FunctionSpec{
				"kubernetes:helm.sh/v3:getRelease": {},
			},
		},
	)

	tests := []struct {
		name       string
		pkg        string
		token      string
		isFunction bool
		want       string
	}{
		{"resource with snake-case name", "aws", "aws:ec2/vpc:Vpc", false, "aws_ec2_vpc"},
		{"resource with camelCase name", "aws", "aws:lb/loadBalancer:LoadBalancer", false, "aws_lb_load_balancer"},
		{"index module omitted", "aws", "aws:index/instance:Instance", false, "aws_instance"},
		{"function strips get prefix", "aws", "aws:ec2/getVpc:getVpc", true, "aws_ec2_vpc"},
		{"dotted module", "kubernetes", "kubernetes:helm.sh/v3:Release", false, "kubernetes_helm_sh_v3_release"},
		{"multi-dot module", "kubernetes", "kubernetes:storage.k8s.io/v1:StorageClass", false, "kubernetes_storage_k8s_io_v1_storage_class"},
		{"dotted module function", "kubernetes", "kubernetes:helm.sh/v3:getRelease", true, "kubernetes_helm_sh_v3_release"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			pkg, err := loader.LoadPackageReferenceV2(t.Context(), &schema.PackageDescriptor{Name: tt.pkg})
			require.NoError(t, err)
			got := pulumiTokenToHCLForm(pkg, tt.token, tt.isFunction)
			require.Equal(t, tt.want, got)
		})
	}
}

func TestPulumiTokenToHCLFormRoundtrip(t *testing.T) {
	t.Parallel()

	rapid.Check(t, func(t *rapid.T) {
		pkg := rapidschema.Package().Draw(t, "pkg")
		if pkg.ExtensionParameterization != nil {
			// Extension-parameterized packages declare resources under the
			// extension's token namespace rather than the package's own name,
			// which the HCL resolver does not support yet.
			t.Skip("extension parameterization not supported")
		}
		spec, err := pkg.MarshalSpec()
		require.NoError(t, err)
		for _, r := range pkg.Resources {
			hclTk := pulumiTokenToHCLForm(pkg.Reference(), r.Token, false)
			resolved, err := ResolveResource(t.Context(), schemaloader.New(t, *spec), nil, hclTk)
			require.NoError(t, err)
			assert.Equal(t, r.Token, resolved.Token)
		}
	})
}

func TestLevenshtein(t *testing.T) {
	t.Parallel()

	tests := []struct {
		a, b string
		want int
	}{
		{"", "", 0},
		{"abc", "abc", 0},
		{"abc", "abd", 1},
		{"abc", "abcd", 1},
		{"abc", "", 3},
		{"kitten", "sitting", 3},
		{"aws_ec2_vpd", "aws_ec2_vpc", 1},
	}

	for _, tt := range tests {
		got := levenshtein(tt.a, tt.b)
		require.Equal(t, tt.want, got, "levenshtein(%q, %q)", tt.a, tt.b)
	}
}

func TestPulumiResourceTokenToHCL(t *testing.T) {
	t.Parallel()

	loader := schemaloader.New(t, schema.PackageSpec{
		Name: "aws",
		Meta: &schema.MetadataSpec{
			ModuleFormat: `(.*)(?:/[^/]*)`,
		},
		Resources: map[string]schema.ResourceSpec{
			"aws:s3/bucketWebsiteConfiguration:BucketWebsiteConfiguration": {},
			"aws:lb/loadBalancer:LoadBalancer":                             {},
			"aws:s3/bucket:Bucket":                                         {},
		},
		Functions: map[string]schema.FunctionSpec{
			"aws:s3/getBucket:getBucket": {},
		},
	})
	pkg, err := loader.LoadPackageReferenceV2(t.Context(), &schema.PackageDescriptor{Name: "aws"})
	require.NoError(t, err)

	t.Run("camelCase resource splits", func(t *testing.T) {
		t.Parallel()
		got, diags := PulumiResourceTokenToHCL(pkg, "aws:s3/bucketWebsiteConfiguration:BucketWebsiteConfiguration")
		require.False(t, diags.HasErrors())
		require.Equal(t, "aws_s3_bucket_website_configuration", got)
	})

	t.Run("function strips get prefix", func(t *testing.T) {
		t.Parallel()
		got, diags := PulumiFunctionTokenToHCL(pkg, "aws:s3/getBucket:getBucket")
		require.False(t, diags.HasErrors())
		require.Equal(t, "aws_s3_bucket", got)
	})

	t.Run("stack reference", func(t *testing.T) {
		t.Parallel()
		got, diags := PulumiResourceTokenToHCL(nil, "pulumi:pulumi:StackReference")
		require.False(t, diags.HasErrors())
		require.Equal(t, "pulumi_stack_reference", got)
	})

	t.Run("providers token preserves providers segment", func(t *testing.T) {
		t.Parallel()
		got, diags := PulumiResourceTokenToHCL(pkg, "pulumi:providers:aws")
		require.False(t, diags.HasErrors())
		require.Equal(t, "pulumi_providers_aws", got)
	})

	t.Run("nil package falls back to source-form module", func(t *testing.T) {
		t.Parallel()
		got, diags := PulumiResourceTokenToHCL(nil, "aws:s3/bucketWebsiteConfiguration:BucketWebsiteConfiguration")
		require.False(t, diags.HasErrors())
		require.Equal(t, "aws_s3_bucket_website_configuration_bucket_website_configuration", got)
	})
}
