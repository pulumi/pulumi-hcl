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
	"sync"

	"github.com/pulumi/pulumi-hcl/pkg/hcl/pkgid"
	"github.com/pulumi/pulumi-hcl/pkg/hcl/run"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource/urn"
	"github.com/pulumi/pulumi/sdk/v3/go/common/workspace"
	"github.com/pulumi/pulumi/sdk/v3/go/property"
	pulumirpc "github.com/pulumi/pulumi/sdk/v3/proto/go"
	"google.golang.org/grpc"
)

// MockResourceMonitor is a mock implementation of run.ResourceMonitor for testing.
type MockResourceMonitor struct {
	mu                  sync.Mutex
	RegisteredResources []run.RegisterResourceRequest
	// RegisteredPackages holds every RegisterPackage request the engine made,
	// one per distinct package identity, in registration order.
	RegisteredPackages []*pulumirpc.RegisterPackageRequest
	packages           *pkgid.Registrar
	ReadResources      []run.ReadResourceRequest
	InvokedFunctions   []run.InvokeRequest
	StackOutputs       property.Map
	Warnings           []string
	stackURN           urn.URN
	hooks              map[string]registeredHook

	// DryRun mirrors engine preview mode: hooks with OnDryRun=false are skipped.
	DryRun bool

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

func (m *MockResourceMonitor) Call(ctx context.Context, req run.CallRequest) (*run.CallResponse, error) {
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

// RegisterPackage registers pkg through a pkgid.Registrar backed by this mock,
// so identities dedup and render exactly as they do against the engine.
func (m *MockResourceMonitor) RegisterPackage(ctx context.Context, pkg workspace.PackageDescriptor) (pkgid.Identity, error) {
	m.mu.Lock()
	if m.packages == nil {
		m.packages = pkgid.NewRegistrar(packageClient{m})
	}
	registrar := m.packages
	m.mu.Unlock()
	return registrar.Identity(ctx, pkg)
}

// packageClient is the pkgid.Client the mock's registrar registers through.
// Refs name the request's position in RegisteredPackages.
type packageClient struct{ m *MockResourceMonitor }

func (c packageClient) RegisterPackage(
	_ context.Context, req *pulumirpc.RegisterPackageRequest, _ ...grpc.CallOption,
) (*pulumirpc.RegisterPackageResponse, error) {
	c.m.mu.Lock()
	defer c.m.mu.Unlock()
	c.m.RegisteredPackages = append(c.m.RegisteredPackages, req)
	return &pulumirpc.RegisterPackageResponse{Ref: fmt.Sprintf("package-%d", len(c.m.RegisteredPackages)-1)}, nil
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
