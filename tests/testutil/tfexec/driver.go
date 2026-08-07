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

// Package tfexec drives the Terraform/OpenTofu CLI against in-process TF
// providers (via reattach) so tests can exercise real Terraform behavior
// without installing remote provider binaries.
package tfexec

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/go-plugin"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-go/tfprotov5"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6/tf6server"
	"github.com/hashicorp/terraform-plugin-mux/tf5to6server"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

// Provider pairs a terraform provider name with a tfprotov6 server factory.
// The factory is invoked once per provider instance (per gRPC connection, see
// ConnRoutedServer), so it must return a fresh, independently configurable
// server on every call. Use SDKv2Provider or PFProvider to build one.
type Provider struct {
	Name   string
	Server func() tfprotov6.ProviderServer
}

// SDKv2Provider adapts an SDKv2 (helper/schema) provider factory into a
// Provider by upgrading it to protocol version 6, building a fresh provider
// per instance.
func SDKv2Provider(t *testing.T, name string, factory func() *schema.Provider) Provider {
	t.Helper()
	build := func() (tfprotov6.ProviderServer, error) {
		return tf5to6server.UpgradeServer(t.Context(),
			func() tfprotov5.ProviderServer { return factory().GRPCProvider() })
	}
	// Surface upgrade problems at construction time, where require works.
	_, err := build()
	require.NoError(t, err)
	return Provider{Name: name, Server: func() tfprotov6.ProviderServer {
		server, err := build()
		if err != nil {
			panic(fmt.Sprintf("upgrading provider %q to protocol v6: %v", name, err))
		}
		return server
	}}
}

// PFProvider adapts a terraform-plugin-framework provider factory into a
// Provider, recording its operations to rec at the protocol boundary (see
// WrapServer). A fresh provider is built per instance.
func PFProvider(name string, factory func() provider.Provider, rec *Recorder) Provider {
	return Provider{Name: name, Server: func() tfprotov6.ProviderServer {
		return WrapServer(providerserver.NewProtocol6(factory())(), rec)
	}}
}

// ServeProvider serves p in-process over go-plugin's test mode and returns
// the reattach config a client needs to connect. The server builds a fresh
// provider per client connection (identified by ConnTagger) so provider
// instances don't share state. go-plugin is invoked directly rather than via
// tf6server.Serve because the latter offers no way to attach the stats
// handler the router's connection identity comes from. Serving stops when the
// test finishes.
func ServeProvider(t *testing.T, p Provider) *plugin.ReattachConfig {
	t.Helper()

	reattachConfigCh := make(chan *plugin.ReattachConfig)
	name, router, tagger := p.Name, ConnRoutedServer(p.Server), &ConnTagger{}
	serveConfig := &plugin.ServeConfig{
		// The handshake tf6server.Serve performs: protocol major version
		// and the terraform plugin protocol's magic cookie.
		HandshakeConfig: plugin.HandshakeConfig{
			ProtocolVersion:  6,
			MagicCookieKey:   "TF_PLUGIN_MAGIC_COOKIE",
			MagicCookieValue: "d602bf8f470bc67ca7faa0386276bbdd4330efaf76d1a219cb4d6991ca9872b2",
		},
		Logger: hclog.FromStandardLogger(log.New(io.Discard, "", 0), hclog.DefaultOptions),
		Plugins: plugin.PluginSet{
			"provider": &tf6server.GRPCProviderPlugin{
				Name:         name,
				GRPCProvider: func() tfprotov6.ProviderServer { return router },
			},
		},
		GRPCServer: func(opts []grpc.ServerOption) *grpc.Server {
			// Message sizes tf6server.Serve would configure.
			opts = append(opts,
				grpc.MaxRecvMsgSize(256<<20),
				grpc.MaxSendMsgSize(256<<20),
				grpc.StatsHandler(tagger),
			)
			return grpc.NewServer(opts...)
		},
		Test: &plugin.ServeTestConfig{
			Context:          t.Context(),
			ReattachConfigCh: reattachConfigCh,
			CloseCh:          make(chan struct{}),
		},
	}
	go plugin.Serve(serveConfig)

	return <-reattachConfigCh
}

// Driver hosts TF providers in-process and runs the terraform CLI against them.
type Driver struct {
	cwd             string
	reattachConfigs map[string]*plugin.ReattachConfig
}

func init() {
	for k, v := range map[string]string{
		"TF_LOG_PROVIDER":  "off",
		"TF_LOG_SDK":       "off",
		"TF_LOG_SDK_PROTO": "off",
	} {
		if err := os.Setenv(k, v); err != nil {
			panic(fmt.Sprintf("setting %s: %v", k, err))
		}
	}
}

// NewDriver creates a Driver for the given providers. If no providers are
// given, the driver runs terraform without any reattach configuration.
func NewDriver(t *testing.T, providers []Provider) *Driver {
	t.Helper()

	reattachConfigs := make(map[string]*plugin.ReattachConfig, len(providers))
	for _, p := range providers {
		reattachConfigs[p.Name] = ServeProvider(t, p)
	}

	return &Driver{
		cwd:             t.TempDir(),
		reattachConfigs: reattachConfigs,
	}
}

// Dir returns the working directory where tofu runs and program files are
// written. Tests use this to scrub the temp path out of values that bake it
// in (e.g. path.cwd) before cross-driver comparison.
func (d *Driver) Dir() string { return d.cwd }

// TryApply writes the input files, runs terraform init + apply, and returns
// all outputs, the apply's combined output (so callers can assert on
// diagnostics, e.g. check-block warnings), and the error from `tofu apply`.
// Config values are passed as -var flags to terraform apply. The outputs map
// is still parsed from terraform.tfstate when the file exists, so callers can
// inspect post-failure state.
//
// TryApply may be called multiple times against the same Driver to drive a
// stack across stages; previous program files are removed before the new ones
// are written so a stage that drops a file doesn't leave the old one behind.
// State files (terraform.tfstate*, .terraform*) are kept across applies.
func (d *Driver) TryApply(
	t *testing.T, input map[string]string, config map[string]string,
) (map[string]string, string, error) {
	t.Helper()

	require.NoError(t, removeProgramFiles(d.cwd))

	for path, content := range input {
		fullPath := filepath.Join(d.cwd, path)
		require.NoError(t, os.MkdirAll(filepath.Dir(fullPath), 0o755))
		require.NoError(t, os.WriteFile(fullPath, []byte(content), 0o600))
	}

	if _, err := d.execTf(t, "init", "-backend=false"); err != nil {
		return nil, "", err
	}

	applyArgs := append(make([]string, 0, 4+2*len(config)), "apply", "-auto-approve", "-refresh=false")
	for k, v := range config {
		applyArgs = append(applyArgs, "-var", k+"="+v)
	}
	out, err := d.execTf(t, applyArgs...)
	if err != nil {
		return d.tryParseOutputs(), out, err
	}
	return d.parseOutputs(t), out, nil
}

// TF refuses to destroy a resource block whose destroy-time provisioner is
// no longer in configuration, so callers must pass the same files they
// applied.
func (d *Driver) Destroy(t *testing.T, input map[string]string, config map[string]string) error {
	t.Helper()
	require.NoError(t, removeProgramFiles(d.cwd))
	for path, content := range input {
		fullPath := filepath.Join(d.cwd, path)
		require.NoError(t, os.MkdirAll(filepath.Dir(fullPath), 0o755))
		require.NoError(t, os.WriteFile(fullPath, []byte(content), 0o600))
	}
	args := append(make([]string, 0, 3+2*len(config)), "destroy", "-auto-approve", "-refresh=false")
	for k, v := range config {
		args = append(args, "-var", k+"="+v)
	}
	_, err := d.execTf(t, args...)
	return err
}

// Plan writes input files and runs `tofu plan`. Returns the error from the plan
// command — nil means the plan succeeded (deferred checks count as success).
func (d *Driver) Plan(t *testing.T, input map[string]string, config map[string]string) error {
	t.Helper()

	require.NoError(t, removeProgramFiles(d.cwd))

	for path, content := range input {
		fullPath := filepath.Join(d.cwd, path)
		require.NoError(t, os.MkdirAll(filepath.Dir(fullPath), 0o755))
		require.NoError(t, os.WriteFile(fullPath, []byte(content), 0o600))
	}

	if _, err := d.execTf(t, "init", "-backend=false"); err != nil {
		return err
	}
	planArgs := append(make([]string, 0, 3+2*len(config)), "plan", "-refresh=false")
	for k, v := range config {
		planArgs = append(planArgs, "-var", k+"="+v)
	}
	_, err := d.execTf(t, planArgs...)
	return err
}

// readStateOutputs reads terraform.tfstate and returns its outputs.
func (d *Driver) readStateOutputs() (map[string]string, error) {
	raw, err := os.ReadFile(filepath.Join(d.cwd, "terraform.tfstate"))
	if err != nil {
		return nil, err
	}
	var state struct {
		Outputs map[string]struct {
			Value json.RawMessage `json:"value"`
		} `json:"outputs"`
	}
	if err := json.Unmarshal(raw, &state); err != nil {
		return nil, err
	}
	result := make(map[string]string, len(state.Outputs))
	for k, v := range state.Outputs {
		result[k] = normalizeStateOutput(v.Value)
	}
	return result, nil
}

// tryParseOutputs returns the state outputs, or an empty map when the state
// file is missing or unreadable, so callers can use it on failure paths
// without panicking.
func (d *Driver) tryParseOutputs() map[string]string {
	outputs, err := d.readStateOutputs()
	if err != nil {
		return map[string]string{}
	}
	return outputs
}

func (d *Driver) parseOutputs(t *testing.T) map[string]string {
	t.Helper()
	outputs, err := d.readStateOutputs()
	require.NoError(t, err)
	return outputs
}

// normalizeStateOutput converts a value from terraform.tfstate to the same
// string form pulexec produces: bare string for string values, compact JSON
// otherwise. The state file is indent-formatted, so re-marshaling drops the
// whitespace baked into RawMessage so equality checks succeed.
func normalizeStateOutput(raw json.RawMessage) string {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return string(raw)
	}
	out, err := json.Marshal(v)
	if err != nil {
		return string(raw)
	}
	return string(out)
}

func (d *Driver) formatReattachEnvVar() string {
	if len(d.reattachConfigs) == 0 {
		return ""
	}
	return "TF_REATTACH_PROVIDERS=" + FormatReattachProviders(d.reattachConfigs)
}

// FormatReattachProviders renders reattach configs as the JSON accepted by
// TF_REATTACH_PROVIDERS (and PULUMI_BRIDGE_REATTACH_PROVIDERS), keyed as
// given.
func FormatReattachProviders(configs map[string]*plugin.ReattachConfig) string {
	type reattachConfigAddr struct {
		Network string
		String  string
	}

	type reattachConfig struct {
		Protocol        string
		ProtocolVersion int
		Pid             int
		Test            bool
		Addr            reattachConfigAddr
	}

	out := make(map[string]reattachConfig, len(configs))
	for name, rc := range configs {
		out[name] = reattachConfig{
			Protocol:        string(rc.Protocol),
			ProtocolVersion: rc.ProtocolVersion,
			Pid:             rc.Pid,
			Test:            rc.Test,
			Addr: reattachConfigAddr{
				Network: rc.Addr.Network(),
				String:  rc.Addr.String(),
			},
		}
	}

	reattachBytes, err := json.Marshal(out)
	if err != nil {
		panic(fmt.Sprintf("failed to build reattach providers string: %v", err))
	}
	return string(reattachBytes)
}

func getTFCommand() string {
	if cmd := os.Getenv("TF_COMMAND_OVERRIDE"); cmd != "" {
		return cmd
	}
	return "tofu"
}

// removeProgramFiles deletes program files (everything except state and the
// .terraform plugin cache) from dir, so a subsequent Apply doesn't see leftover
// files from a previous stage.
func removeProgramFiles(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		name := e.Name()
		switch {
		case name == ".terraform", name == ".terraform.lock.hcl":
		case strings.HasPrefix(name, "terraform.tfstate"):
		default:
			if err := os.RemoveAll(filepath.Join(dir, name)); err != nil {
				return err
			}
			continue
		}
	}
	return nil
}
