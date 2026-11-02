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

// Package bridge resolves Terraform provider names to bridged
// tfbridge.ProviderInfo instances via the Pulumi convert.Mapper, so the engine
// can use original TF block/attribute layout when interpreting HCL.
package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/pulumi/pulumi-terraform-bridge/v3/pkg/tfbridge"
	"github.com/pulumi/pulumi/pkg/v3/codegen/convert"
	"github.com/pulumi/pulumi/sdk/v3/go/common/workspace"
)

// ProviderInfoSource resolves a TF provider name to its bridged
// tfbridge.ProviderInfo. The TF provider name is the prefix of the provider's
// resource types, which the bridge maps the provider under. desc, when
// non-nil, names the Pulumi package that serves the provider, and routes the
// mapper request to that package's plugin and parameterization.
type ProviderInfoSource interface {
	GetProviderInfo(ctx context.Context, tfProvider string, desc *workspace.PackageDescriptor) (*tfbridge.ProviderInfo, error)
}

// TypeProvider returns the TF provider name that a bridged provider maps a
// resource or data source type under: the type's first underscore-delimited
// segment, or the whole type when it has no underscore.
func TypeProvider(tfType string) string {
	before, _, _ := strings.Cut(tfType, "_")
	return before
}

// Cache wraps another ProviderInfoSource with per-process memoisation,
// keyed by the TF provider name and the package that serves it. Negative
// results are cached as nil entries.
type Cache struct {
	inner ProviderInfoSource

	mu      sync.RWMutex
	entries map[cacheKey]cacheEntry
}

type cacheKey struct {
	tfProvider, pkgName, version string
}

type cacheEntry struct {
	info *tfbridge.ProviderInfo
	err  error
}

// NewCache wraps source with read-through caching.
func NewCache(source ProviderInfoSource) *Cache {
	return &Cache{
		inner:   source,
		entries: map[cacheKey]cacheEntry{},
	}
}

// GetProviderInfo returns the bridged info for tfProvider served by desc,
// reusing the first result (including errors) for the same name and package.
func (c *Cache) GetProviderInfo(
	ctx context.Context, tfProvider string, desc *workspace.PackageDescriptor,
) (*tfbridge.ProviderInfo, error) {
	key := cacheKey{tfProvider: tfProvider}
	if desc != nil {
		key.pkgName = desc.PackageName()
		if v := desc.PackageVersion(); v != nil {
			key.version = v.String()
		}
	}

	c.mu.RLock()
	if e, ok := c.entries[key]; ok {
		c.mu.RUnlock()
		return e.info, e.err
	}
	c.mu.RUnlock()

	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.entries[key]; ok {
		return e.info, e.err
	}
	info, err := c.inner.GetProviderInfo(ctx, tfProvider, desc)
	c.entries[key] = cacheEntry{info: info, err: err}
	return info, err
}

// MapperSource queries a convert.Mapper for the bridge mapping JSON and
// decodes it into a tfbridge.ProviderInfo.
type MapperSource struct {
	mapper convert.Mapper
}

// NewMapperSource wraps mapper as a ProviderInfoSource.
func NewMapperSource(mapper convert.Mapper) *MapperSource {
	return &MapperSource{mapper: mapper}
}

// GetProviderInfo asks the mapper for the bridge mapping of tfProvider from
// the plugin that desc names, with desc's parameterization. Without desc, the
// plugin named tfProvider is asked.
func (s *MapperSource) GetProviderInfo(
	ctx context.Context, tfProvider string, desc *workspace.PackageDescriptor,
) (*tfbridge.ProviderInfo, error) {
	if s.mapper == nil {
		return nil, errors.New("no mapper available")
	}

	mapperHint := &convert.MapperPackageHint{PluginName: tfProvider}
	if desc != nil {
		mapperHint = &convert.MapperPackageHint{
			PluginName:       desc.Name,
			Parameterization: desc.Parameterization,
		}
	}

	// The HCL runtime executes Terraform programs, so it always consumes
	// "terraform" mappings regardless of the conversion key the engine
	// configured the mapper with (`pulumi import --from hcl` keys it "hcl").
	data, err := s.mapper.GetMapping(ctx, tfProvider, mapperHint, "terraform")
	if err != nil {
		return nil, fmt.Errorf("mapper GetMapping(%q): %w", tfProvider, err)
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("no mapping available for terraform provider %q", tfProvider)
	}

	var marshalled tfbridge.MarshallableProviderInfo
	if err := json.Unmarshal(data, &marshalled); err != nil {
		return nil, fmt.Errorf("decoding mapping for %q: %w", tfProvider, err)
	}
	return marshalled.Unmarshal(), nil
}
