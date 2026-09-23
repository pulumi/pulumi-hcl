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

package testutil

import (
	"testing"

	"github.com/pulumi/pulumi-hcl/pkg/hcl/run"
	"github.com/pulumi/pulumi-hcl/tests/testutil/schemaloader"
	"github.com/pulumi/pulumi/pkg/v3/codegen/schema"
	"github.com/stretchr/testify/assert"
)

// With Schemas set, the monitor serves a request from the package version
// the request names and rejects a token that version lacks, as the real
// plugin does.
func TestMockResourceMonitor_ChecksRequestsAgainstSchemas(t *testing.T) {
	t.Parallel()

	m := &MockResourceMonitor{Schemas: schemaloader.New(t,
		schema.PackageSpec{
			Name:      "random",
			Version:   "1.0.0",
			Meta:      &schema.MetadataSpec{ModuleFormat: `(.*)(?:/[^/]*)`},
			Resources: map[string]schema.ResourceSpec{"random:index/uuid:Uuid": {}},
			Functions: map[string]schema.FunctionSpec{"random:index/getUuid:getUuid": {}},
		},
		schema.PackageSpec{
			Name:      "random",
			Version:   "2.0.0",
			Meta:      &schema.MetadataSpec{ModuleFormat: `(.*)(?:/[^/]*)`},
			Resources: map[string]schema.ResourceSpec{"random:index/uuid:Uuid": {}, "random:index/uuid4:Uuid4": {}},
			Functions: map[string]schema.FunctionSpec{"random:index/getUuid:getUuid": {}, "random:index/getUuid4:getUuid4": {}},
		},
	)}
	ctx := t.Context()

	register := func(typ, version string) error {
		_, err := m.RegisterResource(ctx, run.RegisterResourceRequest{Type: typ, Name: "x", Custom: true, Version: version})
		return err
	}
	assert.NoError(t, register("random:index/uuid4:Uuid4", "2.0.0"))
	assert.NoError(t, register("random:index/uuid4:Uuid4", ""), "no version is served by the newest package")
	assert.EqualError(t, register("random:index/uuid4:Uuid4", "1.0.0"),
		"unknown resource type random:index/uuid4:Uuid4: not in the random schema at version 1.0.0")
	assert.EqualError(t, register("random:index/uuid:Uuid", "3.0.0"),
		`no random plugin at version "3.0.0": no resource plugin 'pulumi-resource-random' found in the workspace at version v3.0.0`)
	assert.EqualError(t, register("pulumi:providers:random", "3.0.0"),
		`no random plugin at version "3.0.0": no resource plugin 'pulumi-resource-random' found in the workspace at version v3.0.0`)
	assert.NoError(t, register("pulumi:providers:random", "1.0.0"))
	assert.NoError(t, register("pulumi:pulumi:StackReference", ""), "the engine's own tokens are not checked")

	_, err := m.Invoke(ctx, run.InvokeRequest{Token: "random:index/getUuid4:getUuid4", Version: "1.0.0"})
	assert.EqualError(t, err, "unknown function random:index/getUuid4:getUuid4: not in the random schema at version 1.0.0")
	_, err = m.Invoke(ctx, run.InvokeRequest{Token: "random:index/getUuid4:getUuid4", Version: "2.0.0"})
	assert.NoError(t, err)
	_, err = m.ReadResource(ctx, run.ReadResourceRequest{Type: "random:index/uuid4:Uuid4", Name: "x", Version: "1.0.0"})
	assert.EqualError(t, err, "unknown resource type random:index/uuid4:Uuid4: not in the random schema at version 1.0.0")
}
