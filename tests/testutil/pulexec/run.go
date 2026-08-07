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

package pulexec

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	goplugin "github.com/hashicorp/go-plugin"
	"github.com/pulumi/providertest/providers"
	"github.com/pulumi/providertest/pulumitest"
	"github.com/pulumi/providertest/pulumitest/optnewstack"
	"github.com/pulumi/providertest/pulumitest/opttest"
	"github.com/pulumi/pulumi-hcl/pkg/converter"
	"github.com/pulumi/pulumi-hcl/pkg/server"
	"github.com/pulumi/pulumi-hcl/tests/testutil/tfexec"
	"github.com/pulumi/pulumi/pkg/v3/resource/plugin"
	"github.com/pulumi/pulumi/sdk/v3/go/auto"
	"github.com/pulumi/pulumi/sdk/v3/go/auto/optdestroy"
	"github.com/pulumi/pulumi/sdk/v3/go/common/apitype"
	"github.com/pulumi/pulumi/sdk/v3/go/common/util/contract"
	"github.com/pulumi/pulumi/sdk/v3/go/common/util/rpcutil"
	pulumirpc "github.com/pulumi/pulumi/sdk/v3/proto/go"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

// serveLanguageHost serves the pulumi-language-hcl runtime in-process on an
// ephemeral port and returns that port. The engine attaches to it via
// PULUMI_DEBUG_LANGUAGES instead of spawning the plugin binary, so the runtime
// under test runs in the test process (no build step, debuggable, covered).
//
// The engine address is unknown at construction; it arrives per `pulumi`
// invocation through the host's Handshake RPC. Each Driver gets its own host
// so concurrent tests never share an engine connection.
func serveLanguageHost(t *testing.T) int {
	t.Helper()
	cancel := make(chan bool)
	t.Cleanup(func() { close(cancel) })
	handle, err := rpcutil.ServeWithOptions(rpcutil.ServeOptions{
		Cancel: cancel,
		Init: func(srv *grpc.Server) error {
			host, err := server.NewLanguageHost("")
			if err != nil {
				return err
			}
			t.Cleanup(func() { contract.IgnoreClose(host) })
			pulumirpc.RegisterLanguageRuntimeServer(srv, host)
			return nil
		},
	})
	require.NoError(t, err)
	return handle.Port
}

// languageHostGuard writes a pulumi-language-hcl executable into a fresh temp
// dir that fails loudly when run, and returns that dir.
func languageHostGuard(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	stub := "#!/bin/sh\necho 'should not use HCL language binary' >&2\nexit 1\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "pulumi-language-hcl"), []byte(stub), 0o755))
	return dir
}

// Provider pairs a provider name with a way for the engine to reach it.
// Exactly one of Start and Reattach is set.
//
// Start serves an in-process Pulumi provider server the engine attaches to
// directly (SDKv2Provider, PFProvider). Reattach describes an in-process TF
// provider server the engine reaches through the real terraform-provider
// plugin via PULUMI_BRIDGE_REATTACH_PROVIDERS (SDKv2ProviderDynamic,
// PFProviderDynamic).
type Provider struct {
	Name     string
	Start    func(ctx context.Context) (pulumirpc.ResourceProviderServer, error)
	Reattach *goplugin.ReattachConfig
}

// Result holds the outputs and resource state from a Pulumi deployment.
type Result struct {
	Outputs   map[string]string
	Resources []apitype.ResourceV3
	Changes   map[string]int
	// Output is the combined stdout/stderr of the `pulumi up`, used by tests
	// that assert on user-visible diagnostics (e.g. check-block warnings).
	Output string
}

// Driver wraps a long-lived pulumitest project so callers can run multiple
// `pulumi up` cycles against the same stack — required for tests that verify
// behavior across changes (e.g. lifecycle.replace_triggered_by).
type Driver struct {
	pt        *pulumitest.PulumiTest
	dir       string
	providers []Provider
	// install is set when reattach providers are present: their SDKs are
	// written by the real `pulumi install` flow, run once per stage.
	install bool
	// extraEnv holds the same env pairs opttest.Env configures on the
	// workspace, for commands the harness must run itself (`pulumi install`,
	// which pulumitest runs without the workspace env).
	extraEnv []string
	// lastProgramFiles are the program-file paths written by the previous
	// writeFiles call, removed before the next stage so a stage that drops a
	// file doesn't inherit a stale copy.
	lastProgramFiles []string
}

// NewDriver builds the project dir, attaches the bridged providers, and sets
// any stack config. Call Driver.Apply once per stage.
func NewDriver(t *testing.T, provs []Provider, config map[string]string) *Driver {
	t.Helper()

	hostPort := serveLanguageHost(t)
	dir := t.TempDir()

	reattach := map[string]*goplugin.ReattachConfig{}
	for _, p := range provs {
		switch {
		case p.Start != nil && p.Reattach == nil:
		case p.Reattach != nil && p.Start == nil:
			// Keyed by the source the language host requests for a provider
			// with no required_providers entry (see tfProviderSource); an
			// explicit `source = "hashicorp/<name>"` matches the same key.
			reattach["hashicorp/"+p.Name] = p.Reattach
		default:
			t.Fatalf("provider %q: exactly one of Start or Reattach must be set", p.Name)
		}
	}

	// The project name is used as the default namespace for user config. It
	// must not collide with any attached provider name, or user config like
	// "<project>:foo" would be misrouted to the provider.
	pulumiYAML := `name: tfcompat
runtime: hcl
backend:
  url: file://` + filepath.Join(dir, "state") + "\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "Pulumi.yaml"), []byte(pulumiYAML), 0o600))

	env := [][2]string{
		{"PULUMI_DISABLE_AUTOMATIC_PLUGIN_ACQUISITION", "true"},
		{"PULUMI_HOME", isolatedPulumiHome(t)},
		{"PULUMI_DEBUG_LANGUAGES", fmt.Sprintf("hcl:%d", hostPort)},
		// The runtime under test is served in-process via PULUMI_DEBUG_LANGUAGES, so
		// the engine must never spawn (or download) the pulumi-language-hcl binary.
		{"PATH", languageHostGuard(t) + string(os.PathListSeparator) + os.Getenv("PATH")},
	}
	if len(reattach) > 0 {
		env = append(env, [2]string{
			"PULUMI_BRIDGE_REATTACH_PROVIDERS", tfexec.FormatReattachProviders(reattach),
		})
	}

	opts := append(
		make([]opttest.Option, 0, 4+len(env)+len(provs)),
		opttest.TestInPlace(),
		opttest.SkipInstall(),
		// Cleanup destroy fails on prevent_destroy cases; temp dir is
		// discarded by t.TempDir anyway.
		opttest.NewStackOptions(optnewstack.DisableAutoDestroy()),
	)
	extraEnv := make([]string, 0, len(env))
	for _, kv := range env {
		opts = append(opts, opttest.Env(kv[0], kv[1]))
		extraEnv = append(extraEnv, kv[0]+"="+kv[1])
	}
	for _, p := range provs {
		if p.Start == nil {
			continue
		}
		opts = append(opts, opttest.AttachProvider(
			p.Name,
			func(ctx context.Context, pt providers.PulumiTest) (providers.Port, error) {
				handle, err := startProvider(ctx, p.Start)
				if err != nil {
					return 0, err
				}
				return providers.Port(handle.Port), nil
			},
		))
	}
	pt := pulumitest.NewPulumiTest(t, dir, opts...)
	for k, v := range config {
		pt.SetConfig(t, k, v)
	}
	return &Driver{
		pt:        pt,
		dir:       dir,
		providers: provs,
		install:   len(reattach) > 0,
		extraEnv:  extraEnv,
	}
}

// Dir returns the program directory where pulumi runs and program files are
// written. Tests use this to scrub the temp path out of values that bake it
// in (e.g. path.cwd) before cross-driver comparison.
func (d *Driver) Dir() string { return d.dir }

// TryApply runs `pulumi up` and returns the error instead of fataling. State
// is exported regardless of error so callers can inspect post-failure state.
func (d *Driver) TryApply(t *testing.T, programFiles map[string]string) (Result, error) {
	t.Helper()

	d.writeFiles(t, programFiles)

	var upResult auto.UpResult
	cap := newCaptureT(t)
	done := make(chan struct{})
	go func() {
		// pt.Up's fatal calls runtime.Goexit; isolate it in this goroutine.
		defer close(done)
		upResult = d.pt.Up(cap)
	}()
	<-done
	var upErr error
	if cap.Failed() {
		upErr = fmt.Errorf("pulumi up: %s", cap.Logs())
	}

	outputs := make(map[string]string, len(upResult.Outputs))
	for k, v := range upResult.Outputs {
		if s, ok := v.Value.(string); ok {
			outputs[k] = s
		} else {
			raw, err := json.Marshal(v.Value)
			require.NoError(t, err)
			outputs[k] = string(raw)
		}
	}

	exported := d.pt.ExportStack(t)
	var deployment apitype.DeploymentV3
	require.NoError(t, json.Unmarshal(exported.Deployment, &deployment))

	var changes map[string]int
	if upResult.Summary.ResourceChanges != nil {
		changes = *upResult.Summary.ResourceChanges
	}
	return Result{
		Outputs:   outputs,
		Resources: deployment.Resources,
		Changes:   changes,
		Output:    upResult.StdOut + "\n" + upResult.StdErr + "\n" + cap.Logs(),
	}, upErr
}

// Preview runs `pulumi preview` and returns the error (nil on success).
// Same captureT indirection as TryApply.
func (d *Driver) Preview(t *testing.T, programFiles map[string]string) (map[apitype.OpType]int, error) {
	t.Helper()

	d.writeFiles(t, programFiles)

	var res auto.PreviewResult
	cap := newCaptureT(t)
	done := make(chan struct{})
	go func() {
		defer close(done)
		res = d.pt.Preview(cap)
	}()
	<-done
	if cap.Failed() {
		return nil, fmt.Errorf("pulumi preview: %s", cap.Logs())
	}
	return res.ChangeSummary, nil
}

// --run-program re-runs the language host during destroy so BeforeDelete
// hooks (destroy-time provisioners) can fire.
func (d *Driver) Destroy(t *testing.T, programFiles map[string]string) error {
	t.Helper()

	d.writeFiles(t, programFiles)

	cap := newCaptureT(t)
	done := make(chan struct{})
	go func() {
		defer close(done)
		d.pt.Destroy(cap, optdestroy.RunProgram(true))
	}()
	<-done
	if cap.Failed() {
		return fmt.Errorf("pulumi destroy: %s", cap.Logs())
	}
	return nil
}

func (d *Driver) writeFiles(t *testing.T, programFiles map[string]string) {
	t.Helper()
	for _, path := range d.lastProgramFiles {
		require.NoError(t, os.RemoveAll(filepath.Join(d.dir, path)))
	}
	d.lastProgramFiles = d.lastProgramFiles[:0]
	for path, content := range programFiles {
		fullPath := filepath.Join(d.dir, path)
		require.NoError(t, os.MkdirAll(filepath.Dir(fullPath), 0o755))
		require.NoError(t, os.WriteFile(fullPath, []byte(content), 0o600))
		d.lastProgramFiles = append(d.lastProgramFiles, path)
	}
	d.writeStubSDKs(t)
	if d.install {
		// Reattach providers go through the real parameterization flow: the
		// language host reports them as terraform-provider PackageSpecs and
		// `pulumi install` runs the plugin (which reattaches to the in-process
		// TF provider) to generate their SDK descriptors. pulumitest's Install
		// runs without the workspace env, so run the command directly.
		cmd := exec.Command("pulumi", "install")
		cmd.Dir = d.dir
		// Go child processes resolve duplicate env entries last-wins, so
		// appending overrides the test process env.
		cmd.Env = append(os.Environ(), d.extraEnv...)
		out, err := cmd.CombinedOutput()
		require.NoErrorf(t, err, "pulumi install failed: %s", out)
	}
}

// writeStubSDKs materializes a minimal sdks/<provider>/hcl.sdk.json for each
// attached provider. In production this file is written by `pulumi install`
// (via GeneratePackage), but the harness uses AttachProvider, so the
// terraform-provider plugin that `pulumi install` would normally invoke is
// never run. The descriptor is intentionally unparameterized — AttachProvider
// short-circuits plugin lookup to the in-process gRPC server.
func (d *Driver) writeStubSDKs(t *testing.T) {
	t.Helper()
	for _, p := range d.providers {
		if p.Start == nil {
			// Reattach providers get real SDKs from `pulumi install`.
			continue
		}
		sdkDir := filepath.Join(d.dir, "sdks", p.Name)
		require.NoError(t, os.MkdirAll(sdkDir, 0o755))
		desc := fmt.Appendf(nil, `{"name":%q,"kind":"resource"}`+"\n", p.Name)
		require.NoError(t, os.WriteFile(
			filepath.Join(sdkDir, "hcl.sdk.json"), desc, 0o600,
		))
	}
}

func startProvider(
	ctx context.Context, start func(context.Context) (pulumirpc.ResourceProviderServer, error),
) (*rpcutil.ServeHandle, error) {
	// Fail fast on providers that cannot start at all; the router then builds
	// one instance server per engine connection.
	if _, err := start(ctx); err != nil {
		return nil, fmt.Errorf("starting provider server: %w", err)
	}

	handle, err := rpcutil.ServeWithOptions(rpcutil.ServeOptions{
		Init: func(srv *grpc.Server) error {
			pulumirpc.RegisterResourceProviderServer(srv, newConnRoutedProvider(start))
			return nil
		},
		Options: []grpc.ServerOption{grpc.StatsHandler(&tfexec.ConnTagger{})},
	})
	if err != nil {
		return nil, fmt.Errorf("rpcutil.ServeWithOptions failed: %w", err)
	}

	return &handle, nil
}

func (d *Driver) serveConverter(t *testing.T) int {
	t.Helper()
	cancel := make(chan bool)
	t.Cleanup(func() { close(cancel) })
	handle, err := rpcutil.ServeWithOptions(rpcutil.ServeOptions{
		Cancel: cancel,
		Init: func(srv *grpc.Server) error {
			pulumirpc.RegisterConverterServer(srv, plugin.NewConverterServer(converter.NewInDir(d.dir)))
			return nil
		},
	})
	require.NoError(t, err)
	return handle.Port
}

// converterShim writes a pulumi-converter-hcl executable into a fresh temp dir
// and returns that dir. Converter plugins have no PULUMI_DEBUG_* attach path,
// but the CLI prefers an ambient plugin found on PATH and its only handshake
// is the port printed on stdout — so a script that prints the in-process
// server's port and blocks stands in for the real binary. The CLI kills the
// shim when it closes the plugin; the in-process server outlives it.
func converterShim(t *testing.T, port int) string {
	t.Helper()
	dir := t.TempDir()
	shim := fmt.Sprintf("#!/bin/sh\necho %d\nexec tail -f /dev/null\n", port)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "pulumi-converter-hcl"), []byte(shim), 0o755))
	return dir
}

// Import writes programFiles and runs `pulumi import --from hcl` with the
// given converter arguments. The converter is served in-process behind a PATH
// shim, and the CLI's mapper resolves mappings through the providers the
// workspace env configures (the terraform-provider reattach path for dynamic
// providers); the values themselves come from the state, so no provider Read
// runs. Imports are unprotected for the same reason as ImportFromFile.
//
// TODO[https://github.com/pulumi/pulumi/issues/24144]: This is a raw CLI invocation: the
// Automation API's ImportResources appends `--stack` after the `--` separator, which the
// CLI then hands to the converter as arguments.
func (d *Driver) Import(t *testing.T, programFiles map[string]string, converterArgs ...string) (string, error) {
	t.Helper()
	d.writeFiles(t, programFiles)
	stack := d.pt.CurrentStack()
	require.NotNil(t, stack, "driver has no stack")

	args := append([]string{
		"import", "--from", "hcl",
		"--yes", "--skip-preview", "--protect=false", "--generate-code=false",
		"--stack", stack.Name(), "--non-interactive", "--",
	}, converterArgs...)
	cmd := exec.Command("pulumi", args...)
	cmd.Dir = d.dir
	env := os.Environ()
	envPath := os.Getenv("PATH")
	for k, v := range stack.Workspace().GetEnvVars() {
		env = append(env, k+"="+v)
		if k == "PATH" {
			envPath = v
		}
	}
	shimDir := converterShim(t, d.serveConverter(t))
	env = append(env, "PATH="+shimDir+string(os.PathListSeparator)+envPath)
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	return string(out), err
}
