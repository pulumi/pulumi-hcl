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
	"testing"

	"github.com/blang/semver"
	"github.com/pulumi/pulumi-hcl/pkg/hcl/ast"
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
	)

	ctx := t.Context()

	tests := []struct {
		name         string
		pkgName      string
		token        string
		wantToken    string
		wantErr      error
		errAsInvalid bool
		errContains  string
	}{
		{
			name:      "basic resource",
			pkgName:   "aws",
			token:     "aws_s3_bucket",
			wantToken: "aws:s3:Bucket",
		},
		{
			name:      "bridged-style module embeds member name",
			pkgName:   "bridged",
			token:     "bridged_iam_role",
			wantToken: "bridged:iam/Role:Role",
		},
		{
			name:      "index module",
			pkgName:   "aws",
			token:     "aws_instance",
			wantToken: "aws:index:Instance",
		},
		{
			name:      "multi-part module",
			pkgName:   "aws",
			token:     "aws_ec2_vpc",
			wantToken: "aws:ec2:Vpc",
		},
		{
			name:      "gcp provider",
			pkgName:   "gcp",
			token:     "gcp_storage_bucket",
			wantToken: "gcp:storage:Bucket",
		},
		{
			name:      "single-segment token matches same-named provider",
			pkgName:   "external",
			token:     "external",
			wantToken: "external:index/external:External",
		},
		{
			name:    "single-segment token with no same-named resource is not found",
			pkgName: "aws",
			token:   "aws",
			wantErr: ErrNotFound,
		},
		{
			name:         "empty token",
			token:        "",
			errAsInvalid: true,
			errContains:  "non-empty",
		},
		{
			name:    "resource not found",
			pkgName: "aws",
			token:   "aws_nonexistent",
			wantErr: ErrNotFound,
		},
		{
			name:    "package not found",
			pkgName: "fake",
			token:   "fake_resource",
			wantErr: ErrNotFound,
		},
		{
			name:      "underscore package name",
			pkgName:   "fail_on_create",
			token:     "fail_on_create_resource",
			wantToken: "fail_on_create:index:Resource",
		},
		{
			name:    "token without the package prefix is not found",
			pkgName: "gcp",
			token:   "aws_s3_bucket",
			wantErr: ErrNotFound,
		},
		{
			name:        "provider as resource (pulumi_providers_ prefix)",
			pkgName:     "aws",
			token:       "pulumi_providers_aws",
			errContains: "is a provider type and cannot be declared with a resource block",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			res, err := ResolveResource(ctx, loader, tt.pkgName, tt.token)

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
	)

	ctx := t.Context()

	tests := []struct {
		name         string
		pkgName      string
		token        string
		wantToken    string
		wantErr      error
		errAsInvalid bool
		errContains  string
	}{
		{
			name:      "direct function match",
			pkgName:   "aws",
			token:     "aws_s3_getbucket",
			wantToken: "aws:s3:getBucket",
		},
		{
			name:      "index module function",
			pkgName:   "aws",
			token:     "aws_getinstance",
			wantToken: "aws:index:getInstance",
		},
		{
			name:      "implicit get prefix",
			pkgName:   "aws",
			token:     "aws_s3_bucket",
			wantToken: "aws:s3:getBucket",
		},
		{
			name:      "implicit get prefix multi-part",
			pkgName:   "aws",
			token:     "aws_ec2_vpc",
			wantToken: "aws:ec2:getVpc",
		},
		{
			name:      "gcp implicit get",
			pkgName:   "gcp",
			token:     "gcp_storage_bucket",
			wantToken: "gcp:storage:getBucket",
		},
		{
			name:      "list function",
			pkgName:   "aws",
			token:     "aws_s3_listbuckets",
			wantToken: "aws:s3:listBuckets",
		},
		{
			name:      "single-segment data source matches same-named provider via implicit get",
			pkgName:   "external",
			token:     "external",
			wantToken: "external:index/getExternal:getExternal",
		},
		{
			name:         "empty token",
			token:        "",
			errAsInvalid: true,
			errContains:  "non-empty",
		},
		{
			name:      "non-standard module format",
			pkgName:   "mypkg",
			token:     "mypkg_mod_concatworld",
			wantToken: "mypkg:mod_concatWorld:concatWorld",
		},
		{
			name:      "non-standard module format with nested slash",
			pkgName:   "mypkg",
			token:     "mypkg_mod_nested_concatworld",
			wantToken: "mypkg:mod/nested_concatWorld:concatWorld",
		},
		{
			name:      "bridged-style data source implicit get",
			pkgName:   "bridged",
			token:     "bridged_iam_role",
			wantToken: "bridged:iam/getRole:getRole",
		},
		{
			name:      "bridged-style index module implicit get",
			pkgName:   "bridged",
			token:     "bridged_availability_zone",
			wantToken: "bridged:index/getAvailabilityZone:getAvailabilityZone",
		},
		{
			name:    "function not found",
			pkgName: "aws",
			token:   "aws_nonexistent",
			wantErr: ErrNotFound,
		},
		{
			name:    "package not found",
			token:   "fake_function",
			wantErr: ErrNotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fn, err := ResolveFunction(ctx, loader, tt.pkgName, tt.token)

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

func TestTypePackage(t *testing.T) {
	t.Parallel()

	tf := &ast.Terraform{RequiredProviders: map[string]*ast.RequiredProvider{
		"myp":         {Source: "hashicorp/simple"},
		"snake_names": {Source: "pulumi/snake_names"},
	}}

	tests := []struct {
		name string
		typ  string
		want string
	}{
		{name: "implied provider", typ: "google_project", want: "google"},
		{name: "implied provider with underscores", typ: "snake_names_thing", want: "snake_names"},
		{name: "implied provider renamed by its source", typ: "myp_thing", want: "simple"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := TypePackage(tf, tt.typ)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}
