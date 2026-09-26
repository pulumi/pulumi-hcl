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
	"context"
	"strings"
	"testing"

	"github.com/blang/semver"
	"github.com/pulumi/pulumi-hcl/tests/testutil/schemaloader"
	"github.com/pulumi/pulumi/pkg/v3/codegen/schema"
	"github.com/pulumi/pulumi/sdk/v3/go/common/workspace"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInvalidToken_Error(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		token    string
		reason   string
		expected string
	}{
		{
			name:     "token and reason",
			token:    "aws",
			reason:   "must have at least 2 parts",
			expected: `invalid token "aws" must have at least 2 parts`,
		},
		{
			name:     "token only",
			token:    "foo",
			reason:   "",
			expected: `invalid token "foo"`,
		},
		{
			name:     "reason only",
			token:    "",
			reason:   "some reason",
			expected: "invalid token some reason",
		},
		{
			name:     "neither",
			token:    "",
			reason:   "",
			expected: "invalid token",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := InvalidToken{token: tt.token, reason: tt.reason}
			require.Equal(t, tt.expected, err.Error())
		})
	}
}

func TestResolveResource(t *testing.T) {
	t.Parallel()

	loader := schemaloader.New(t,
		schema.PackageSpec{
			Name: "aws",
			Resources: map[string]schema.ResourceSpec{
				"aws:s3:Bucket":      {},
				"aws:index:Instance": {},
				"aws:ec2:Vpc":        {},
			},
		},
		schema.PackageSpec{
			Name: "gcp",
			Resources: map[string]schema.ResourceSpec{
				"gcp:storage:Bucket": {},
			},
		},
		schema.PackageSpec{
			Name: "fail_on_create",
			Resources: map[string]schema.ResourceSpec{
				"fail_on_create:index:Resource": {},
			},
		},
		// Bridged-provider style: tokens use the `<mod>/<Member>:<Member>`
		// shape and the schema sets the ModuleFormat regex used by pulumi-aws
		// and other terraform-bridge providers. The resolver must match HCL
		// forms like "bridged_iam_role" against these tokens.
		schema.PackageSpec{
			Name: "bridged",
			Meta: &schema.MetadataSpec{
				ModuleFormat: `(.*)(?:/[^/]*)`,
			},
			Resources: map[string]schema.ResourceSpec{
				"bridged:iam/Role:Role": {},
			},
		},
		// Single-segment provider: the type name is the same word as the
		// provider, so HCL writes `resource "external" "x"` with no
		// underscore in the token. Models hashicorp/external and friends.
		schema.PackageSpec{
			Name: "external",
			Meta: &schema.MetadataSpec{
				ModuleFormat: `(.*)(?:/[^/]*)`,
			},
			Resources: map[string]schema.ResourceSpec{
				"external:index/external:External": {},
			},
		},
		// Kubernetes-style modules contain dots, which an HCL identifier
		// cannot, so the HCL form writes them as underscores.
		schema.PackageSpec{
			Name: "kubernetes",
			Resources: map[string]schema.ResourceSpec{
				"kubernetes:helm.sh/v3:Release":           {},
				"kubernetes:networking.k8s.io/v1:Ingress": {},
			},
		},
		// Two tokens whose search keys collide once separators are dropped.
		schema.PackageSpec{
			Name: "clash",
			Resources: map[string]schema.ResourceSpec{
				"clash:a.b:Foo": {},
				"clash:ab:Foo":  {},
			},
		},
	)

	ctx := t.Context()

	tests := []struct {
		name           string
		knownProviders []string
		token          string
		wantToken      string
		wantErr        error
		errAsInvalid   bool
		errContains    string
	}{
		{
			name:           "basic resource",
			knownProviders: []string{"aws"},
			token:          "aws_s3_bucket",
			wantToken:      "aws:s3:Bucket",
		},
		{
			name:           "bridged-style module embeds member name",
			knownProviders: []string{"bridged"},
			token:          "bridged_iam_role",
			wantToken:      "bridged:iam/Role:Role",
		},
		{
			name:           "index module",
			knownProviders: []string{"aws"},
			token:          "aws_instance",
			wantToken:      "aws:index:Instance",
		},
		{
			name:           "multi-part module",
			knownProviders: []string{"aws"},
			token:          "aws_ec2_vpc",
			wantToken:      "aws:ec2:Vpc",
		},
		{
			name:           "gcp provider",
			knownProviders: []string{"gcp"},
			token:          "gcp_storage_bucket",
			wantToken:      "gcp:storage:Bucket",
		},
		{
			name:           "single-segment token matches same-named provider",
			knownProviders: []string{"external"},
			token:          "external",
			wantToken:      "external:index/external:External",
		},
		{
			name:           "single-segment token with no same-named resource is not found",
			knownProviders: []string{"aws"},
			token:          "aws",
			wantErr:        ErrNotFound,
		},
		{
			name:         "empty token",
			token:        "",
			errAsInvalid: true,
			errContains:  "non-empty",
		},
		{
			name:           "resource not found",
			knownProviders: []string{"aws"},
			token:          "aws_nonexistent",
			wantErr:        ErrNotFound,
		},
		{
			name:    "package not found",
			token:   "fake_resource",
			wantErr: ErrNotFound,
		},
		{
			name:           "underscore package name",
			knownProviders: []string{"fail_on_create", "simple"},
			token:          "fail_on_create_resource",
			wantToken:      "fail_on_create:index:Resource",
		},
		{
			name:           "ambiguous token",
			knownProviders: []string{"foo", "foo_bar"},
			token:          "foo_bar_thing",
			errContains:    "ambiguous token",
		},
		{
			name:           "provider as resource (pulumi_providers_ prefix)",
			knownProviders: []string{"aws"},
			token:          "pulumi_providers_aws",
			errContains:    "is a provider type and cannot be declared with a resource block",
		},
		{
			name:           "dotted module written with underscores",
			knownProviders: []string{"kubernetes"},
			token:          "kubernetes_helm_sh_v3_release",
			wantToken:      "kubernetes:helm.sh/v3:Release",
		},
		{
			name:           "dotted module written with dots",
			knownProviders: []string{"kubernetes"},
			token:          "kubernetes_helm.sh_v3_release",
			wantToken:      "kubernetes:helm.sh/v3:Release",
		},
		{
			name:           "multi-dot module written with underscores",
			knownProviders: []string{"kubernetes"},
			token:          "kubernetes_networking_k8s_io_v1_ingress",
			wantToken:      "kubernetes:networking.k8s.io/v1:Ingress",
		},
		{
			name:           "type labels are case-sensitive",
			knownProviders: []string{"kubernetes"},
			token:          "kubernetes_Helm_sh_v3_release",
			wantErr:        ErrNotFound,
		},
		{
			name:           "ambiguous resource",
			knownProviders: []string{"clash"},
			token:          "clash_ab_foo",
			errContains:    `ambiguous token "clash_ab_foo": matches multiple resources [clash:a.b:Foo clash:ab:Foo]`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			res, err := ResolveResource(ctx, loader, tt.knownProviders, tt.token)

			if tt.errAsInvalid {
				require.Error(t, err)
				var invalidToken InvalidToken
				require.ErrorAs(t, err, &invalidToken)
				if tt.errContains != "" {
					require.Contains(t, err.Error(), tt.errContains)
				}
				return
			}

			if tt.wantErr != nil {
				require.ErrorIs(t, err, tt.wantErr)
				return
			}

			if tt.errContains != "" {
				require.Error(t, err)
				require.Contains(t, err.Error(), tt.errContains)
				return
			}

			require.NoError(t, err)
			require.NotNil(t, res)
			actualToken := res.Token
			require.Equal(t, tt.wantToken, actualToken)
		})
	}
}

func TestResolveFunction(t *testing.T) {
	t.Parallel()

	loader := schemaloader.New(t,
		schema.PackageSpec{
			Name: "aws",
			Functions: map[string]schema.FunctionSpec{
				"aws:s3:getBucket":      {},
				"aws:s3:listBuckets":    {},
				"aws:index:getInstance": {},
				"aws:ec2:getVpc":        {},
			},
		},
		schema.PackageSpec{
			Name: "gcp",
			Functions: map[string]schema.FunctionSpec{
				"gcp:storage:getBucket": {},
			},
		},
		schema.PackageSpec{
			Name: "mypkg",
			Meta: &schema.MetadataSpec{
				// Non-standard module format where the module is everything before the last _name segment.
				ModuleFormat: `(.*)(?:_[^_]*)`,
			},
			Functions: map[string]schema.FunctionSpec{
				"mypkg:mod_concatWorld:concatWorld":        {},
				"mypkg:mod/nested_concatWorld:concatWorld": {},
			},
		},
		// Bridged-provider style: tokens use a `<mod>/<Member>:<Member>` shape
		// and the schema sets the ModuleFormat regex used by pulumi-aws and
		// other terraform-bridge providers. The resolver must still match
		// against HCL data source names like "bridged_iam_role" (moduled) and
		// "bridged_availability_zone" (root/index module).
		schema.PackageSpec{
			Name: "bridged",
			Meta: &schema.MetadataSpec{
				ModuleFormat: `(.*)(?:/[^/]*)`,
			},
			Functions: map[string]schema.FunctionSpec{
				"bridged:iam/getRole:getRole":                           {},
				"bridged:index/getAvailabilityZone:getAvailabilityZone": {},
				// Dropping "get" from one name gives the other's full name.
				"bridged:compute/routerStatus:RouterStatus":       {},
				"bridged:compute/getRouterStatus:getRouterStatus": {},
			},
		},
		// Single-segment data source: HCL `data "external" "x"` resolves to
		// a function whose member name equals the provider name. Models
		// hashicorp/external and hashicorp/http.
		schema.PackageSpec{
			Name: "external",
			Meta: &schema.MetadataSpec{
				ModuleFormat: `(.*)(?:/[^/]*)`,
			},
			Functions: map[string]schema.FunctionSpec{
				"external:index/getExternal:getExternal": {},
			},
		},
		// Multi-segment modules: the HCL form omits "get" after the last
		// module segment, wherever that falls.
		schema.PackageSpec{
			Name: "kubernetes",
			Functions: map[string]schema.FunctionSpec{
				"kubernetes:helm.sh/v3:getRelease": {},
				"kubernetes:core/v1:getConfigMap":  {},
			},
		},
		schema.PackageSpec{
			Name: "grafana",
			Meta: &schema.MetadataSpec{
				ModuleFormat: `(.*)(?:/[^/]*)`,
			},
			Functions: map[string]schema.FunctionSpec{
				"grafana:onCall/getTeam:getTeam": {},
			},
		},
		// Two tokens whose search keys collide once separators are dropped.
		schema.PackageSpec{
			Name: "clash",
			Functions: map[string]schema.FunctionSpec{
				"clash:a.b:getFoo": {},
				"clash:ab:getFoo":  {},
			},
		},
		// A lowercase member suffix is not the `getName` convention used
		// for data sources, even when its module contains dots.
		schema.PackageSpec{
			Name: "lowerget",
			Functions: map[string]schema.FunctionSpec{
				"lowerget:helm.sh/v3:getrelease": {},
			},
		},
	)

	ctx := t.Context()

	tests := []struct {
		name           string
		knownProviders []string
		token          string
		wantToken      string
		wantErr        error
		errAsInvalid   bool
		errContains    string
	}{
		{
			name:           "direct function match",
			knownProviders: []string{"aws"},
			token:          "aws_s3_getbucket",
			wantToken:      "aws:s3:getBucket",
		},
		{
			name:           "index module function",
			knownProviders: []string{"aws"},
			token:          "aws_getinstance",
			wantToken:      "aws:index:getInstance",
		},
		{
			name:           "implicit get prefix",
			knownProviders: []string{"aws"},
			token:          "aws_s3_bucket",
			wantToken:      "aws:s3:getBucket",
		},
		{
			name:           "implicit get prefix multi-part",
			knownProviders: []string{"aws"},
			token:          "aws_ec2_vpc",
			wantToken:      "aws:ec2:getVpc",
		},
		{
			name:           "gcp implicit get",
			knownProviders: []string{"gcp"},
			token:          "gcp_storage_bucket",
			wantToken:      "gcp:storage:getBucket",
		},
		{
			name:           "list function",
			knownProviders: []string{"aws"},
			token:          "aws_s3_listbuckets",
			wantToken:      "aws:s3:listBuckets",
		},
		{
			name:           "single-segment data source matches same-named provider via implicit get",
			knownProviders: []string{"external"},
			token:          "external",
			wantToken:      "external:index/getExternal:getExternal",
		},
		{
			name:         "empty token",
			token:        "",
			errAsInvalid: true,
			errContains:  "non-empty",
		},
		{
			name:           "non-standard module format",
			knownProviders: []string{"mypkg"},
			token:          "mypkg_mod_concatworld",
			wantToken:      "mypkg:mod_concatWorld:concatWorld",
		},
		{
			name:           "non-standard module format with nested slash",
			knownProviders: []string{"mypkg"},
			token:          "mypkg_mod_nested_concatworld",
			wantToken:      "mypkg:mod/nested_concatWorld:concatWorld",
		},
		{
			name:           "bridged-style data source implicit get",
			knownProviders: []string{"bridged"},
			token:          "bridged_iam_role",
			wantToken:      "bridged:iam/getRole:getRole",
		},
		{
			name:           "bridged-style index module implicit get",
			knownProviders: []string{"bridged"},
			token:          "bridged_availability_zone",
			wantToken:      "bridged:index/getAvailabilityZone:getAvailabilityZone",
		},
		{
			name:           "full member name wins over get-less match",
			knownProviders: []string{"bridged"},
			token:          "bridged_compute_router_status",
			wantToken:      "bridged:compute/routerStatus:RouterStatus",
		},
		{
			name:           "explicit get selects the get-prefixed function",
			knownProviders: []string{"bridged"},
			token:          "bridged_compute_get_router_status",
			wantToken:      "bridged:compute/getRouterStatus:getRouterStatus",
		},
		{
			name:           "function not found",
			knownProviders: []string{"aws"},
			token:          "aws_nonexistent",
			wantErr:        ErrNotFound,
		},
		{
			name:    "package not found",
			token:   "fake_function",
			wantErr: ErrNotFound,
		},
		{
			name:           "dotted module implicit get",
			knownProviders: []string{"kubernetes"},
			token:          "kubernetes_helm_sh_v3_release",
			wantToken:      "kubernetes:helm.sh/v3:getRelease",
		},
		{
			name:           "dotted module explicit get",
			knownProviders: []string{"kubernetes"},
			token:          "kubernetes_helm_sh_v3_get_release",
			wantToken:      "kubernetes:helm.sh/v3:getRelease",
		},
		{
			name:           "dotted module written with dots",
			knownProviders: []string{"kubernetes"},
			token:          "kubernetes_helm.sh_v3_release",
			wantToken:      "kubernetes:helm.sh/v3:getRelease",
		},
		{
			name:           "lowercase get prefix stays part of the member name",
			knownProviders: []string{"lowerget"},
			token:          "lowerget_helm_sh_v3_getrelease",
			wantToken:      "lowerget:helm.sh/v3:getrelease",
		},
		{
			name:           "lowercase get prefix cannot be omitted",
			knownProviders: []string{"lowerget"},
			token:          "lowerget_helm_sh_v3_release",
			wantErr:        ErrNotFound,
		},
		{
			name:           "multi-segment module implicit get",
			knownProviders: []string{"kubernetes"},
			token:          "kubernetes_core_v1_config_map",
			wantToken:      "kubernetes:core/v1:getConfigMap",
		},
		{
			name:           "camelCase module implicit get",
			knownProviders: []string{"grafana"},
			token:          "grafana_on_call_team",
			wantToken:      "grafana:onCall/getTeam:getTeam",
		},
		{
			name:           "ambiguous function",
			knownProviders: []string{"clash"},
			token:          "clash_ab_foo",
			errContains:    `ambiguous token "clash_ab_foo": matches multiple functions [clash:a.b:getFoo clash:ab:getFoo]`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fn, err := ResolveFunction(ctx, loader, tt.knownProviders, tt.token)

			if tt.errAsInvalid {
				require.Error(t, err)
				var invalidToken InvalidToken
				require.ErrorAs(t, err, &invalidToken)
				if tt.errContains != "" {
					require.Contains(t, err.Error(), tt.errContains)
				}
				return
			}

			if tt.wantErr != nil {
				require.ErrorIs(t, err, tt.wantErr)
				return
			}

			if tt.errContains != "" {
				require.Error(t, err)
				require.Contains(t, err.Error(), tt.errContains)
				return
			}

			require.NoError(t, err)
			require.NotNil(t, fn)
			actualToken := fn.Token
			require.Equal(t, tt.wantToken, actualToken)
		})
	}
}

// TestCanonicalHCLNameRoundtrip checks that the HCL name generated for a token
// resolves back to that token, including for modules containing dots.
func TestCanonicalHCLNameRoundtrip(t *testing.T) {
	t.Parallel()

	loader := schemaloader.New(t,
		schema.PackageSpec{
			Name: "kubernetes",
			Resources: map[string]schema.ResourceSpec{
				"kubernetes:core/v1:ConfigMap":              {},
				"kubernetes:helm.sh/v3:Release":             {},
				"kubernetes:networking.k8s.io/v1:Ingress":   {},
				"kubernetes:storage.k8s.io/v1:StorageClass": {},
			},
			Functions: map[string]schema.FunctionSpec{
				"kubernetes:core/v1:getConfigMap":  {},
				"kubernetes:helm.sh/v3:getRelease": {},
			},
		},
		schema.PackageSpec{
			Name: "grafana",
			Meta: &schema.MetadataSpec{
				ModuleFormat: `(.*)(?:/[^/]*)`,
			},
			Resources: map[string]schema.ResourceSpec{
				"grafana:onCall/team:Team": {},
			},
			Functions: map[string]schema.FunctionSpec{
				"grafana:onCall/getTeam:getTeam": {},
			},
		},
	)

	tests := []struct {
		token      string
		isFunction bool
		want       string
	}{
		{"kubernetes:core/v1:ConfigMap", false, "kubernetes_core_v1_config_map"},
		{"kubernetes:helm.sh/v3:Release", false, "kubernetes_helm_sh_v3_release"},
		{"kubernetes:networking.k8s.io/v1:Ingress", false, "kubernetes_networking_k8s_io_v1_ingress"},
		{"kubernetes:storage.k8s.io/v1:StorageClass", false, "kubernetes_storage_k8s_io_v1_storage_class"},
		{"kubernetes:core/v1:getConfigMap", true, "kubernetes_core_v1_config_map"},
		{"kubernetes:helm.sh/v3:getRelease", true, "kubernetes_helm_sh_v3_release"},
		{"grafana:onCall/team:Team", false, "grafana_on_call_team"},
		{"grafana:onCall/getTeam:getTeam", true, "grafana_on_call_team"},
	}

	for _, tt := range tests {
		t.Run(tt.token, func(t *testing.T) {
			t.Parallel()
			pkgName, _, _ := strings.Cut(tt.token, ":")
			pkg, err := loader.LoadPackageReferenceV2(t.Context(), &schema.PackageDescriptor{Name: pkgName})
			require.NoError(t, err)

			if tt.isFunction {
				hclName, diags := PulumiFunctionTokenToHCL(pkg, tt.token)
				require.False(t, diags.HasErrors())
				require.Equal(t, tt.want, hclName)
				fn, err := ResolveFunction(t.Context(), loader, []string{pkgName}, hclName)
				require.NoError(t, err)
				assert.Equal(t, tt.token, fn.Token)
				return
			}
			hclName, diags := PulumiResourceTokenToHCL(pkg, tt.token)
			require.False(t, diags.HasErrors())
			require.Equal(t, tt.want, hclName)
			res, err := ResolveResource(t.Context(), loader, []string{pkgName}, hclName)
			require.NoError(t, err)
			assert.Equal(t, tt.token, res.Token)
		})
	}
}

type recordingLoader struct {
	schema.ReferenceLoader
	got *schema.PackageDescriptor
}

func (r *recordingLoader) LoadPackageReferenceV2(
	_ context.Context, d *schema.PackageDescriptor,
) (schema.PackageReference, error) {
	r.got = d
	return nil, nil
}

func TestParameterizationAwareLoader_PlainPackageVersionAndURL(t *testing.T) {
	t.Parallel()

	v := semver.MustParse("0.0.0-x52a8a71555d964542b308da197755c64dbe63352")
	inner := &recordingLoader{}
	loader := NewParameterizationAwareLoader(inner, map[string]workspace.PackageDescriptor{
		"tls-self-signed-cert": {PluginDescriptor: workspace.PluginDescriptor{
			Name:              "tls-self-signed-cert",
			Version:           &v,
			PluginDownloadURL: "git://github.com/pulumi/component-test-providers/test-provider",
		}},
	})
	_, err := loader.LoadPackageReferenceV2(t.Context(), &schema.PackageDescriptor{Name: "tls-self-signed-cert"})
	require.NoError(t, err)
	require.Equal(t, &schema.PackageDescriptor{
		Name:        "tls-self-signed-cert",
		Version:     &v,
		DownloadURL: "git://github.com/pulumi/component-test-providers/test-provider",
	}, inner.got)
}
