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
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/blang/semver"
	"github.com/pulumi/pulumi-hcl/pkg/hcl/run"
	"github.com/pulumi/pulumi/pkg/v3/codegen/schema"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource/urn"
	"github.com/pulumi/pulumi/sdk/v3/go/common/workspace"
	"github.com/pulumi/pulumi/sdk/v3/go/property"
)

// MockResourceMonitor is a mock implementation of run.ResourceMonitor for testing.
type MockResourceMonitor struct {
	mu                  sync.Mutex
	RegisteredResources []run.RegisterResourceRequest
	// RegisteredPackages holds every RegisterPackage request in order. The
	// ref of the i-th registration is "package-<i>".
	RegisteredPackages []workspace.PackageDescriptor
	ReadResources      []run.ReadResourceRequest
	InvokedFunctions   []run.InvokeRequest
	Calls              []run.CallRequest
	StackOutputs       property.Map
	Warnings           []string
	stackURN           urn.URN
	hooks              map[string]registeredHook

	// DryRun mirrors engine preview mode: hooks with OnDryRun=false are skipped.
	DryRun bool

	// Schemas, when set, is the plugin set the monitor serves requests from.
	// A custom resource, a read, an invoke, or a call must name a token that
	// the schema at the request's version holds, as the real plugin requires;
	// otherwise the request fails. A request that names no version is served
	// by the newest package, as the engine's default provider is. The
	// engine's own tokens (pulumi:*, terraform:*) are not checked, except
	// that a provider resource's package must exist at the request's version.
	Schemas schema.ReferenceLoader

	// InvokeHandler, if set, is called for each Invoke instead of the default behavior.
	InvokeHandler func(ctx context.Context, req run.InvokeRequest) (*run.InvokeResponse, error)

	// RegisterResourceHandler, if  set, is  called for each  RegisterResource instead of the default behavior.
	RegisterResourceHandler func(ctx context.Context, req run.RegisterResourceRequest) (*run.RegisterResourceResponse, error)

	// CheckPulumiVersionHandler, if set, is called for each CheckPulumiVersion instead of the default behavior.
	CheckPulumiVersionHandler func(ctx context.Context, versionRange string) error
}

type registeredHook struct {
	callback run.ResourceHookFunction
	opts     run.ResourceHookOptions
}

// ResolveURN mirrors RegisterResource's URN scheme.
func (m *MockResourceMonitor) ResolveURN(_ urn.URN, token, name string) (urn.URN, string) {
	return urn.URN("urn:pulumi:test::project::" + token + "::" + name), name
}

func (m *MockResourceMonitor) RegisterResource(ctx context.Context, req run.RegisterResourceRequest) (*run.RegisterResourceResponse, error) {
	if req.Custom {
		if err := m.checkResource(ctx, req.Type, req.Version); err != nil {
			return nil, err
		}
	}
	m.mu.Lock()
	resURN := urn.URN("urn:pulumi:test::project::" + req.Type + "::" + req.Name)
	if req.Type == "pulumi:pulumi:Stack" {
		m.stackURN = resURN
	}
	m.mu.Unlock()

	// The mock has no state, so every registration is treated as a create.
	args := &run.ResourceHookArgs{
		URN:       string(resURN),
		Name:      req.Name,
		Type:      req.Type,
		NewInputs: req.Inputs,
	}
	if req.Hooks != nil {
		if err := m.runHooks(ctx, req.Hooks.BeforeCreate, args); err != nil {
			return nil, err
		}
	}

	m.mu.Lock()
	m.RegisteredResources = append(m.RegisteredResources, req)
	handler := m.RegisterResourceHandler
	m.mu.Unlock()

	var resp *run.RegisterResourceResponse
	var err error
	if req.Type != "pulumi:pulumi:Stack" && handler != nil {
		resp, err = handler(ctx, req)
	} else {
		resp = &run.RegisterResourceResponse{
			URN:     urn.URN(resURN),
			ID:      req.Name + "-id",
			Outputs: req.Inputs,
		}
	}
	if err != nil {
		return resp, err
	}

	if req.Hooks != nil {
		args.NewOutputs = resp.Outputs
		args.ID = resp.ID
		if err := m.runHooks(ctx, req.Hooks.AfterCreate, args); err != nil {
			return resp, err
		}
	}

	return resp, nil
}

func (m *MockResourceMonitor) ReadResource(ctx context.Context, req run.ReadResourceRequest) (*run.ReadResourceResponse, error) {
	if err := m.checkResource(ctx, req.Type, req.Version); err != nil {
		return nil, err
	}
	m.mu.Lock()
	m.ReadResources = append(m.ReadResources, req)
	m.mu.Unlock()

	resURN := urn.URN("urn:pulumi:test::project::" + req.Type + "::" + req.Name)
	return &run.ReadResourceResponse{
		URN:     resURN,
		ID:      req.ID,
		Outputs: req.Inputs,
	}, nil
}

// getHook looks up a registered hook under the lock. Hook callbacks must run
// outside the lock because they can re-enter the monitor.
func (m *MockResourceMonitor) getHook(name string) (registeredHook, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	h, ok := m.hooks[name]
	return h, ok
}

func (m *MockResourceMonitor) runHooks(
	ctx context.Context, names []string, args *run.ResourceHookArgs,
) error {
	for _, name := range names {
		h, ok := m.getHook(name)
		if !ok {
			return fmt.Errorf("hook %q not registered", name)
		}
		if m.DryRun && !h.opts.OnDryRun {
			continue
		}
		if err := h.callback(ctx, args); err != nil {
			return fmt.Errorf("hook %q failed: %w", name, err)
		}
	}
	return nil
}

func (m *MockResourceMonitor) Invoke(ctx context.Context, req run.InvokeRequest) (*run.InvokeResponse, error) {
	if err := m.checkFunction(ctx, req.Token, req.Version); err != nil {
		return nil, err
	}
	m.mu.Lock()
	m.InvokedFunctions = append(m.InvokedFunctions, req)
	handler := m.InvokeHandler
	m.mu.Unlock()

	if handler != nil {
		return handler(ctx, req)
	}
	return &run.InvokeResponse{
		Return: property.NewMap(map[string]property.Value{
			"id": property.New("mock-id"),
		}),
	}, nil
}

func (m *MockResourceMonitor) RegisterResourceOutputs(ctx context.Context, urn urn.URN, outputs property.Map) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if urn == m.stackURN {
		m.StackOutputs = outputs
	}
	return nil
}

// Call serves a method call. A call names no version, so it is checked
// against the newest package.
func (m *MockResourceMonitor) Call(ctx context.Context, req run.CallRequest) (*run.CallResponse, error) {
	if err := m.checkFunction(ctx, req.Token, ""); err != nil {
		return nil, err
	}
	m.mu.Lock()
	m.Calls = append(m.Calls, req)
	m.mu.Unlock()
	return &run.CallResponse{
		Return: property.NewMap(nil),
	}, nil
}

func (m *MockResourceMonitor) CheckPulumiVersion(ctx context.Context, versionRange string) error {
	if m.CheckPulumiVersionHandler != nil {
		return m.CheckPulumiVersionHandler(ctx, versionRange)
	}
	return nil
}

func (m *MockResourceMonitor) LogWarning(ctx context.Context, message string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Warnings = append(m.Warnings, message)
	return nil
}

// packageAt loads the schema that serves name at version from Schemas. ok is
// false when the monitor checks nothing for name.
func (m *MockResourceMonitor) packageAt(
	ctx context.Context, name, version string,
) (pkg schema.PackageReference, ok bool, err error) {
	if m.Schemas == nil || name == "pulumi" || name == "terraform" {
		return nil, false, nil
	}
	desc := &schema.PackageDescriptor{Name: name}
	if version != "" {
		v, err := semver.ParseTolerant(version)
		if err != nil {
			return nil, false, fmt.Errorf("package %s: invalid version %q: %w", name, version, err)
		}
		desc.Version = &v
	}
	pkg, err = m.Schemas.LoadPackageReferenceV2(ctx, desc)
	if err != nil {
		return nil, false, fmt.Errorf("no %s plugin at version %q: %w", name, version, err)
	}
	return pkg, true, nil
}

func packageOf(token string) string {
	name, _, _ := strings.Cut(token, ":")
	return name
}

// checkResource fails when the schema at version lacks the resource typ.
func (m *MockResourceMonitor) checkResource(ctx context.Context, typ, version string) error {
	if provider, ok := strings.CutPrefix(typ, "pulumi:providers:"); ok {
		_, _, err := m.packageAt(ctx, provider, version)
		return err
	}
	pkg, ok, err := m.packageAt(ctx, packageOf(typ), version)
	if !ok {
		return err
	}
	_, found, err := pkg.Resources().Get(typ)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("unknown resource type %s: not in the %s schema at version %s", typ, pkg.Name(), pkg.Version())
	}
	return nil
}

// checkFunction fails when the schema at version lacks the function tok.
func (m *MockResourceMonitor) checkFunction(ctx context.Context, tok, version string) error {
	pkg, ok, err := m.packageAt(ctx, packageOf(tok), version)
	if !ok {
		return err
	}
	_, found, err := pkg.Functions().Get(tok)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("unknown function %s: not in the %s schema at version %s", tok, pkg.Name(), pkg.Version())
	}
	return nil
}

// RegisterPackage records pkg and returns a ref that names its position in
// RegisteredPackages, so a test can tell which package a request carries.
func (m *MockResourceMonitor) RegisterPackage(ctx context.Context, pkg workspace.PackageDescriptor) (run.PackageRef, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.RegisteredPackages = append(m.RegisteredPackages, pkg)
	return run.PackageRef(fmt.Sprintf("package-%d", len(m.RegisteredPackages)-1)), nil
}

func (m *MockResourceMonitor) RegisterResourceHook(
	ctx context.Context, name string, callback run.ResourceHookFunction, opts run.ResourceHookOptions,
) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.hooks == nil {
		m.hooks = make(map[string]registeredHook)
	}
	if _, exists := m.hooks[name]; exists {
		return fmt.Errorf("hook %q already registered", name)
	}
	m.hooks[name] = registeredHook{callback: callback, opts: opts}
	return nil
}
