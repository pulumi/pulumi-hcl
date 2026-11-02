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

package bridge_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/pulumi/pulumi-hcl/pkg/hcl/bridge"
	"github.com/pulumi/pulumi/pkg/v3/codegen/convert"
	"github.com/pulumi/pulumi/sdk/v3/go/common/workspace"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// pluginMapper serves a mapping from each plugin under the TF provider name
// that the plugin maps under, and records each request.
type pluginMapper struct {
	// mappings maps "<plugin>/<TF provider>" to the Pulumi package name of
	// the mapping.
	mappings map[string]string
	requests []string
}

func (m *pluginMapper) GetMapping(
	_ context.Context, provider string, hint *convert.MapperPackageHint, _ string,
) ([]byte, error) {
	key := hint.PluginName + "/" + provider
	m.requests = append(m.requests, key)
	name, ok := m.mappings[key]
	if !ok {
		return nil, nil
	}
	return json.Marshal(map[string]any{"name": name})
}

// Two providers can map their types under the same prefix. A mapping is
// identified by that prefix and the package whose plugin serves it.
func TestMappingIsIdentifiedByPrefixAndPackage(t *testing.T) {
	t.Parallel()

	mapper := &pluginMapper{mappings: map[string]string{
		"prov/prov":      "prov",
		"prov-beta/prov": "prov-beta",
	}}
	source := bridge.NewCache(bridge.NewMapperSource(mapper))
	pkg := func(name string) *workspace.PackageDescriptor {
		return &workspace.PackageDescriptor{PluginDescriptor: workspace.PluginDescriptor{Name: name}}
	}

	names := map[string]string{}
	for _, name := range []string{"prov", "prov-beta", "prov"} {
		info, err := source.GetProviderInfo(t.Context(), "prov", pkg(name))
		require.NoError(t, err)
		names[name] = info.Name
	}

	assert.Equal(t, map[string]string{"prov": "prov", "prov-beta": "prov-beta"}, names)
	assert.Equal(t, []string{"prov/prov", "prov-beta/prov"}, mapper.requests)
}
