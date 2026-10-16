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

// Package ast defines the Abstract Syntax Tree types for HCL configurations.
package ast

import (
	"github.com/hashicorp/hcl/v2"
)

// Config represents the root of a parsed HCL configuration.
// It aggregates all blocks from all files in the configuration directory.
type Config struct {
	// Terraform contains the terraform block configuration (program-level options
	// and provider requirements).
	Terraform *Terraform

	// Language contains the language block's Pulumi compatibility
	// declaration, if any.
	Language *Language

	// Providers maps provider alias to provider configuration.
	// The key is the provider local name (e.g., "aws") or alias (e.g., "aws.west").
	Providers map[string]*Provider

	// Variables maps variable name to variable definition.
	Variables map[string]*Variable

	// Locals maps local value name to its expression.
	Locals map[string]*Local

	// Resources maps "type.name" to resource definition.
	Resources map[string]*Resource

	// DataSources maps "type.name" to data source definition.
	DataSources map[string]*DataSource

	// Outputs maps output name to output definition.
	Outputs map[string]*Output

	// Modules maps module name to module call definition.
	Modules map[string]*Module

	// Calls maps "resourceName.methodName" to call definitions.
	Calls map[string]*Call

	// Moved contains moved blocks for resource renaming.
	Moved []*Moved

	// Removed contains removed blocks for resource deletion.
	Removed []*Removed

	// Imports contains import blocks for importing existing resources.
	Imports []*Import

	// Checks maps check name to its top-level check block.
	Checks map[string]*Check

	// Files contains the parsed HCL files.
	Files map[string]*hcl.File

	// ProviderFunctionCalls lists the TF provider local names referenced by
	// provider-defined function calls (provider::<name>::<fn>) anywhere in
	// the configuration's files, deduplicated and sorted.
	ProviderFunctionCalls []string

	// ProviderFunctionCallsIncomplete is true when a file could not be
	// scanned for provider-defined function calls (JSON syntax), so
	// ProviderFunctionCalls may be missing providers. Consumers fall back
	// to the declared required providers.
	ProviderFunctionCallsIncomplete bool
}

// NewConfig creates a new empty configuration.
func NewConfig() *Config {
	return &Config{
		Providers:   make(map[string]*Provider),
		Variables:   make(map[string]*Variable),
		Locals:      make(map[string]*Local),
		Resources:   make(map[string]*Resource),
		DataSources: make(map[string]*DataSource),
		Outputs:     make(map[string]*Output),
		Modules:     make(map[string]*Module),
		Calls:       make(map[string]*Call),
		Checks:      make(map[string]*Check),
		Files:       make(map[string]*hcl.File),
	}
}

// ResourceKey returns the key for a resource in the Resources/DataSources maps.
func ResourceKey(resourceType, name string) string {
	return resourceType + "." + name
}
