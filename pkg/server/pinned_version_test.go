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

package server

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/blang/semver"
	"github.com/pulumi/pulumi-hcl/tests/testutil/schemaloader"
	"github.com/pulumi/pulumi/pkg/v3/codegen/convert"
	"github.com/pulumi/pulumi/pkg/v3/codegen/schema"
	pulumirpc "github.com/pulumi/pulumi/sdk/v3/proto/go"
	codegenrpc "github.com/pulumi/pulumi/sdk/v3/proto/go/codegen"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/structpb"
	"pgregory.net/rapid"
)

// installedAt returns the schema that serves name at version from plugins,
// or the newest one when version is empty, as the engine's plugin loader
// does. ok is false when no such plugin is installed.
func installedAt(ctx context.Context, plugins schema.ReferenceLoader, name, version string) (schema.PackageReference, bool) {
	desc := &schema.PackageDescriptor{Name: name}
	if version != "" {
		v, err := semver.ParseTolerant(version)
		if err != nil {
			return nil, false
		}
		desc.Version = &v
	}
	pkg, err := plugins.LoadPackageReferenceV2(ctx, desc)
	return pkg, err == nil
}

// pinnedProviderMonitor stands in for the engine and the plugins behind it.
// It serves a request from the plugin version the request names, as the
// engine does, and rejects a resource or function that the schema at that
// version lacks, as the real plugin does after HCL has already accepted the
// program. A request for a package that is not installed at all, such as a
// component, is accepted as is.
type pinnedProviderMonitor struct {
	pulumirpc.UnimplementedResourceMonitorServer
	plugins schema.ReferenceLoader

	mu         sync.Mutex
	registered []*pulumirpc.RegisterResourceRequest
	invoked    []*pulumirpc.ResourceInvokeRequest
	rejected   []string
}

func (m *pinnedProviderMonitor) reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.registered, m.invoked, m.rejected = nil, nil, nil
}

// requests counts the accepted requests as "<kind> <token>@<version>", with
// the version the plugin that served the request runs at.
func (m *pinnedProviderMonitor) requests(ctx context.Context) map[string]int {
	m.mu.Lock()
	defer m.mu.Unlock()
	counts := map[string]int{}
	for _, req := range m.registered {
		if pkg, ok := installedAt(ctx, m.plugins, packageOf(req.Type), m.versionOf(req.PackageRef, req.Version)); ok {
			counts["resource "+req.Type+"@"+pkg.Version().String()]++
		}
	}
	for _, req := range m.invoked {
		if pkg, ok := installedAt(ctx, m.plugins, packageOf(req.Tok), m.versionOf(req.PackageRef, req.Version)); ok {
			counts["data "+req.Tok+"@"+pkg.Version().String()]++
		}
	}
	return counts
}

func packageOf(token string) string {
	name, _, _ := strings.Cut(token, ":")
	return name
}

// versionOf is the version a request runs at: the engine resolves a package
// ref before it reads the request's own version.
func (m *pinnedProviderMonitor) versionOf(packageRef, version string) string {
	if _, v, ok := strings.Cut(packageRef, "@"); ok {
		return v
	}
	return version
}

func (m *pinnedProviderMonitor) reject(kind, tok string, pkg schema.PackageReference) error {
	msg := fmt.Sprintf("unknown %s token %s in %s@%s", kind, tok, pkg.Name(), pkg.Version())
	m.mu.Lock()
	m.rejected = append(m.rejected, msg)
	m.mu.Unlock()
	return fmt.Errorf("%s", msg)
}

func (m *pinnedProviderMonitor) RegisterPackage(
	_ context.Context, req *pulumirpc.RegisterPackageRequest,
) (*pulumirpc.RegisterPackageResponse, error) {
	return &pulumirpc.RegisterPackageResponse{Ref: req.Name + "@" + req.Version}, nil
}

func (m *pinnedProviderMonitor) RegisterResource(
	ctx context.Context, req *pulumirpc.RegisterResourceRequest,
) (*pulumirpc.RegisterResourceResponse, error) {
	name := packageOf(req.Type)
	if _, installed := installedAt(ctx, m.plugins, name, ""); installed {
		version := m.versionOf(req.PackageRef, req.Version)
		pkg, ok := installedAt(ctx, m.plugins, name, version)
		if !ok {
			return nil, fmt.Errorf("no %s plugin installed at version %q", name, version)
		}
		if _, found, err := pkg.Resources().Get(req.Type); err != nil || !found {
			return nil, m.reject("resource", req.Type, pkg)
		}
	}
	m.mu.Lock()
	m.registered = append(m.registered, req)
	m.mu.Unlock()
	return &pulumirpc.RegisterResourceResponse{
		Urn:    "urn:pulumi:dev::proj::" + req.Type + "::" + req.Name,
		Object: req.Object,
	}, nil
}

func (m *pinnedProviderMonitor) Invoke(
	ctx context.Context, req *pulumirpc.ResourceInvokeRequest,
) (*pulumirpc.ResourceInvokeResponse, error) {
	name := packageOf(req.Tok)
	version := m.versionOf(req.PackageRef, req.Version)
	pkg, ok := installedAt(ctx, m.plugins, name, version)
	if !ok {
		return nil, fmt.Errorf("no %s plugin installed at version %q", name, version)
	}
	if _, found, err := pkg.Functions().Get(req.Tok); err != nil || !found {
		return nil, m.reject("function", req.Tok, pkg)
	}
	m.mu.Lock()
	m.invoked = append(m.invoked, req)
	m.mu.Unlock()
	ret, err := structpb.NewStruct(map[string]any{"result": "ok"})
	if err != nil {
		return nil, err
	}
	return &pulumirpc.ResourceInvokeResponse{Return: ret}, nil
}

func (m *pinnedProviderMonitor) RegisterResourceOutputs(
	context.Context, *pulumirpc.RegisterResourceOutputsRequest,
) (*emptypb.Empty, error) {
	return &emptypb.Empty{}, nil
}

// noMapper has no bridge mapping for any provider, so every type resolves by
// the token naming rules alone.
type noMapper struct{}

func (noMapper) GetMapping(context.Context, string, *convert.MapperPackageHint, string) ([]byte, error) {
	return nil, nil
}

type quietEngine struct {
	pulumirpc.UnimplementedEngineServer
}

func (quietEngine) Log(context.Context, *pulumirpc.LogRequest) (*emptypb.Empty, error) {
	return &emptypb.Empty{}, nil
}

// pinnedWorkspace serves an installed plugin set to the language host as its
// schema loader and mapper, with a monitor that validates every request
// against the same set.
type pinnedWorkspace struct {
	monitor     *pinnedProviderMonitor
	host        *LanguageHost
	monitorAddr string
	loaderAddr  string
}

func newPinnedWorkspace(t *testing.T, plugins schema.ReferenceLoader) *pinnedWorkspace {
	t.Helper()
	monitor := &pinnedProviderMonitor{plugins: plugins}
	srv := grpc.NewServer()
	codegenrpc.RegisterLoaderServer(srv, schema.NewLoaderServer(plugins))
	codegenrpc.RegisterMapperServer(srv, convert.NewMapperServer(noMapper{}))
	pulumirpc.RegisterEngineServer(srv, quietEngine{})
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	host, err := NewLanguageHost(lis.Addr().String())
	require.NoError(t, err)
	return &pinnedWorkspace{
		monitor:     monitor,
		host:        host,
		monitorAddr: serveResourceMonitor(t, monitor),
		loaderAddr:  lis.Addr().String(),
	}
}

// run writes files under dir and runs dir as a root program. It returns the
// error the run reported, empty when the program ran.
func (w *pinnedWorkspace) run(ctx context.Context, dir string, files map[string]string) (string, error) {
	if err := writeFiles(dir, files); err != nil {
		return "", err
	}
	resp, err := w.host.Run(ctx, &pulumirpc.RunRequest{
		Project:        "proj",
		Stack:          "dev",
		MonitorAddress: w.monitorAddr,
		LoaderTarget:   w.loaderAddr,
		MapperTarget:   w.loaderAddr,
		Parallel:       1,
		Info:           &pulumirpc.ProgramInfo{ProgramDirectory: dir, RootDirectory: dir, EntryPoint: "."},
	})
	if err != nil {
		return "", err
	}
	return resp.Error, nil
}

func writeFiles(dir string, files map[string]string) error {
	for name, src := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
			return err
		}
	}
	return nil
}

// randomSpec is the schema of pulumi-random at version. Each name is a
// resource with a data source of the same name.
func randomSpec(version string, names ...string) schema.PackageSpec {
	spec := schema.PackageSpec{
		Name:      "random",
		Version:   version,
		Meta:      &schema.MetadataSpec{ModuleFormat: `(.*)(?:/[^/]*)`},
		Resources: map[string]schema.ResourceSpec{},
		Functions: map[string]schema.FunctionSpec{},
	}
	for _, n := range names {
		spec.Resources[resourceToken(n)] = schema.ResourceSpec{}
		spec.Functions[functionToken(n)] = schema.FunctionSpec{
			ReturnType: &schema.ReturnTypeSpec{ObjectTypeSpec: &schema.ObjectTypeSpec{
				Type:       "object",
				Properties: map[string]schema.PropertySpec{"result": {TypeSpec: schema.TypeSpec{Type: "string"}}},
			}},
		}
	}
	return spec
}

func title(n string) string { return strings.ToUpper(n[:1]) + n[1:] }

func resourceToken(n string) string { return "random:index/" + n + ":" + title(n) }

func functionToken(n string) string { return "random:index/get" + title(n) + ":get" + title(n) }

func requiredRandom(version string) string {
	var b strings.Builder
	b.WriteString("terraform {\n  required_providers {\n    random = {\n      source  = \"pulumi/random\"\n")
	if version != "" {
		fmt.Fprintf(&b, "      version = %q\n", version)
	}
	b.WriteString("    }\n  }\n}\n")
	return b.String()
}

// The scenario of issue #627: two versions of pulumi-random are installed,
// the program pins the older one and uses a type that only the newer one
// has. HCL must reject the program against the pinned schema. The request
// must never reach the plugin, which rejects it at the pinned version.
func TestRun_PinnedProviderRejectsTypeOfNewerVersion(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		block string
		want  string
	}{
		{
			name:  "resource",
			block: `resource "random_uuid4" "id" {}`,
			want:  `main.tf:9,10-24: unknown resource type "random_uuid4"; did you mean "random_uuid"?`,
		},
		{
			name:  "data",
			block: `data "random_uuid4" "id" {}`,
			want:  `main.tf:9,6-20: unknown data source type "random_uuid4"; did you mean "random_uuid"?`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			w := newPinnedWorkspace(t, schemaloader.New(t,
				randomSpec("4.18.5", "uuid"), randomSpec("4.19.0", "uuid", "uuid4")))

			dir := t.TempDir()
			errMsg, err := w.run(t.Context(), dir, map[string]string{
				"main.tf": requiredRandom("4.18.5") + tt.block + "\n",
			})
			require.NoError(t, err)
			assert.Equal(t, filepath.Join(dir, tt.want), errMsg)
			assert.Empty(t, w.monitor.rejected)
		})
	}
}

// A source MLC pins its providers the same way a root program does, so its
// component schema is derived from the pinned provider schema.
func TestLocalProvider_PinnedProviderRejectsTypeOfNewerVersion(t *testing.T) {
	t.Parallel()

	w := newPinnedWorkspace(t, schemaloader.New(t,
		randomSpec("4.18.5", "uuid"), randomSpec("4.19.0", "uuid", "uuid4")))
	dir := filepath.Join(t.TempDir(), "pinned")
	require.NoError(t, writeFiles(dir, map[string]string{
		"main.tf": requiredRandom("4.18.5") + `
resource "random_uuid4" "id" {}
`,
	}))

	_, err := NewLocalProvider(t.Context(), dir, w.loaderAddr)
	assert.EqualError(t, err,
		`generating schema for the module root: resolving resource "random_uuid4.id": not found; did you mean "random_uuid"?`)
}

type pinnedBlock struct {
	kind string // "resource" or "data"
	typ  string
}

type pinnedProgram struct {
	pin    string // the required_providers version, "" for none
	blocks []pinnedBlock
	child  *pinnedProgram
}

func (p pinnedProgram) source(prefix string) string {
	var b strings.Builder
	b.WriteString(requiredRandom(p.pin))
	for i, blk := range p.blocks {
		fmt.Fprintf(&b, "\n%s \"random_%s\" \"%s%d\" {}\n", blk.kind, blk.typ, prefix, i)
	}
	return b.String()
}

func (p pinnedProgram) files() map[string]string {
	files := map[string]string{"main.tf": p.source("r")}
	if p.child != nil {
		files["main.tf"] += "\nmodule \"child\" {\n  source = \"./child\"\n}\n"
		files["child/main.tf"] = p.child.source("c")
	}
	return files
}

// expected models the program: one pin applies to the whole program, the
// root's when it names one and the child's otherwise, and no pin at all means
// the newest installed plugin. It returns the requests the program makes,
// counted as the monitor counts them, or false when a block's type is not in
// the schema of the version that serves it.
func (p pinnedProgram) expected(ctx context.Context, plugins schema.ReferenceLoader) (map[string]int, bool) {
	pin := p.pin
	if pin == "" && p.child != nil {
		pin = p.child.pin
	}
	want := map[string]int{}
	ok := true
	progs := []pinnedProgram{p}
	if p.child != nil {
		progs = append(progs, *p.child)
	}
	for _, prog := range progs {
		for _, blk := range prog.blocks {
			pkg, _ := installedAt(ctx, plugins, "random", pin)
			var tok string
			var exists bool
			if blk.kind == "resource" {
				tok = resourceToken(blk.typ)
				_, exists, _ = pkg.Resources().Get(tok)
			} else {
				tok = functionToken(blk.typ)
				_, exists, _ = pkg.Functions().Get(tok)
			}
			if !exists {
				ok = false
				continue
			}
			want[blk.kind+" "+tok+"@"+pkg.Version().String()]++
		}
	}
	return want, ok
}

// Type checking and registration must resolve the same plugin version for
// every block, whatever required_providers pins the root and a child module
// declare. Three versions of a package are installed with a type unique to
// each, so a disagreement surfaces as a request that the plugin at the
// registered version rejects.
func TestRun_TypeCheckAndRegistrationAgree(t *testing.T) {
	t.Parallel()

	plugins := schemaloader.New(t,
		randomSpec("1.0.0", "common", "one"),
		randomSpec("2.0.0", "common", "two"),
		randomSpec("3.0.0", "common", "three"),
	)
	w := newPinnedWorkspace(t, plugins)

	versions := rapid.SampledFrom([]string{"", "1.0.0", "2.0.0", "3.0.0"})
	block := rapid.Custom(func(t *rapid.T) pinnedBlock {
		return pinnedBlock{
			kind: rapid.SampledFrom([]string{"resource", "data"}).Draw(t, "kind"),
			typ:  rapid.SampledFrom([]string{"common", "one", "two", "three"}).Draw(t, "type"),
		}
	})

	rapid.Check(t, func(rt *rapid.T) {
		prog := pinnedProgram{
			pin:    versions.Draw(rt, "pin"),
			blocks: rapid.SliceOfN(block, 1, 4).Draw(rt, "blocks"),
		}
		if rapid.Bool().Draw(rt, "module") {
			prog.child = &pinnedProgram{
				pin:    versions.Draw(rt, "child pin"),
				blocks: rapid.SliceOfN(block, 1, 3).Draw(rt, "child blocks"),
			}
		}

		w.monitor.reset()
		errMsg, err := w.run(t.Context(), t.TempDir(), prog.files())
		require.NoError(rt, err)

		assert.Empty(rt, w.monitor.rejected, "a request ran at a version HCL did not type check against")
		want, ok := prog.expected(t.Context(), plugins)
		if !ok {
			assert.NotEmpty(rt, errMsg, "a block that the serving version lacks must fail type checking")
			return
		}
		require.Empty(rt, errMsg)
		assert.Equal(rt, want, w.monitor.requests(t.Context()))
	})
}

// A non-Pulumi provider with no local SDK fails the program the same way it
// did before the pins joined the package map.
func TestRun_MissingSDKForNonPulumiProvider(t *testing.T) {
	t.Parallel()

	w := newPinnedWorkspace(t, schemaloader.New(t, randomSpec("4.18.5", "uuid")))
	errMsg, err := w.run(t.Context(), t.TempDir(), map[string]string{
		"main.tf": `
terraform {
  required_providers {
    tls = {
      source = "hashicorp/tls"
    }
  }
}

resource "tls_private_key" "key" {}
`,
	})
	require.NoError(t, err)
	assert.Equal(t, "missing local SDK for non-Pulumi provider(s) [hashicorp/tls]; run `pulumi install` to fetch them", errMsg)
}
