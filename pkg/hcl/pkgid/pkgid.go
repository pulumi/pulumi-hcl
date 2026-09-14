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

// Package pkgid owns package identity: which plugin, at which version and
// download URL, with which parameterization, serves a block. The engine
// resolves a workspace.PackageDescriptor for a block and obtains an opaque
// Identity from a Registrar. Only this package spells an Identity onto the
// wire, so no other package needs to know how the engine routes a request to
// a provider.
package pkgid

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"

	"github.com/pulumi/pulumi/sdk/v3/go/common/workspace"
	pulumirpc "github.com/pulumi/pulumi/sdk/v3/proto/go"
	"google.golang.org/grpc"
)

// Identity is the package a block registers or invokes against. The zero
// Identity names no package and leaves a request untouched.
type Identity struct {
	desc workspace.PackageDescriptor
	ref  string
}

// String renders the identity for diagnostics and test assertions:
// "random@4.18.5", "tls@0.1.0 git://example.com/tls", or
// "terraform-provider@0.9.0 [aws@5.0.0]" for a parameterized package.
func (id Identity) String() string {
	var b strings.Builder
	b.WriteString(id.desc.Name)
	if id.desc.Version != nil {
		b.WriteString("@" + id.desc.Version.String())
	}
	if id.desc.PluginDownloadURL != "" {
		b.WriteString(" " + id.desc.PluginDownloadURL)
	}
	if p := id.desc.Parameterization; p != nil {
		fmt.Fprintf(&b, " [%s@%s]", p.Name, p.Version)
	}
	if p := id.desc.ExtensionParameterization; p != nil {
		fmt.Fprintf(&b, " +[%s@%s]", p.Name, p.Version)
	}
	return b.String()
}

// Client is the part of the resource monitor a Registrar needs.
type Client interface {
	RegisterPackage(
		ctx context.Context, in *pulumirpc.RegisterPackageRequest, opts ...grpc.CallOption,
	) (*pulumirpc.RegisterPackageResponse, error)
}

// Registrar turns descriptors into identities. It registers each distinct
// descriptor with the engine once. The engine itself dedups a repeated
// registration to the same ref, so the memo only saves round trips.
type Registrar struct {
	client Client

	// mu is held across the RegisterPackage call so that concurrent requests
	// for the same descriptor wait for one registration instead of racing.
	// Distinct descriptors are few, so serializing them costs little.
	mu   sync.Mutex
	refs map[string]string
}

// NewRegistrar returns a Registrar that registers packages through client.
func NewRegistrar(client Client) *Registrar {
	return &Registrar{client: client, refs: map[string]string{}}
}

// Identity registers desc with the engine, or returns the identity of an
// earlier registration of the same descriptor.
func (r *Registrar) Identity(ctx context.Context, desc workspace.PackageDescriptor) (Identity, error) {
	req := registerRequest(desc)
	key := requestKey(req)

	r.mu.Lock()
	defer r.mu.Unlock()
	if ref, ok := r.refs[key]; ok {
		return Identity{desc: desc, ref: ref}, nil
	}
	resp, err := r.client.RegisterPackage(ctx, req)
	if err != nil {
		return Identity{}, fmt.Errorf("registering package %s: %w", desc.Name, err)
	}
	r.refs[key] = resp.Ref
	return Identity{desc: desc, ref: resp.Ref}, nil
}

func registerRequest(desc workspace.PackageDescriptor) *pulumirpc.RegisterPackageRequest {
	req := &pulumirpc.RegisterPackageRequest{
		Name:        desc.Name,
		Version:     versionString(desc),
		DownloadUrl: desc.PluginDownloadURL,
		Checksums:   desc.Checksums,
	}
	if p := desc.Parameterization; p != nil {
		req.Parameterization = &pulumirpc.Parameterization{Name: p.Name, Version: p.Version.String(), Value: p.Value}
	}
	if p := desc.ExtensionParameterization; p != nil {
		req.Extension = &pulumirpc.Parameterization{Name: p.Name, Version: p.Version.String(), Value: p.Value}
	}
	return req
}

// requestKey identifies a registration by content. Parameterization values
// can be large, so they enter the key as a digest.
func requestKey(req *pulumirpc.RegisterPackageRequest) string {
	param := func(p *pulumirpc.Parameterization) string {
		if p == nil {
			return ""
		}
		sum := sha256.Sum256(p.Value)
		return p.Name + "@" + p.Version + ":" + hex.EncodeToString(sum[:])
	}
	return strings.Join([]string{
		req.Name, req.Version, req.DownloadUrl, param(req.Parameterization), param(req.Extension),
	}, "\x00")
}

func versionString(desc workspace.PackageDescriptor) string {
	if desc.Version == nil {
		return ""
	}
	return desc.Version.String()
}

// The Apply methods write id onto a request. Each sets the ref together with
// the version and download URL. For an ordinary resource or an invoke the ref
// alone decides the provider: the engine resolves the ref to a provider
// request and ignores the other two fields. For a provider resource
// (pulumi:providers:*) the engine copies the ref's version and URL into the
// provider's inputs, then overwrites them with the default provider's plugin
// whenever the request's own Version and PluginDownloadURL are empty. Setting
// all three keeps an explicit provider block on its pinned plugin.

// ApplyRegisterResource writes id onto a RegisterResource request.
func (id Identity) ApplyRegisterResource(req *pulumirpc.RegisterResourceRequest) {
	req.PackageRef, req.Version, req.PluginDownloadURL = id.ref, versionString(id.desc), id.desc.PluginDownloadURL
}

// ApplyReadResource writes id onto a ReadResource request.
func (id Identity) ApplyReadResource(req *pulumirpc.ReadResourceRequest) {
	req.PackageRef, req.Version, req.PluginDownloadURL = id.ref, versionString(id.desc), id.desc.PluginDownloadURL
}

// ApplyInvoke writes id onto an Invoke request.
func (id Identity) ApplyInvoke(req *pulumirpc.ResourceInvokeRequest) {
	req.PackageRef, req.Version, req.PluginDownloadURL = id.ref, versionString(id.desc), id.desc.PluginDownloadURL
}

// ApplyCall writes id onto a Call request.
func (id Identity) ApplyCall(req *pulumirpc.ResourceCallRequest) {
	req.PackageRef, req.Version, req.PluginDownloadURL = id.ref, versionString(id.desc), id.desc.PluginDownloadURL
}
