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

// Package pulexec runs a Pulumi program through the real Pulumi engine and
// `pulumi-language-hcl` runtime, attaching bridged TF providers in-process.
package pulexec

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"testing"

	pfprovider "github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	"github.com/pulumi/pulumi-hcl/tests/testutil/tfexec"
	"github.com/pulumi/pulumi-terraform-bridge/v3/pkg/pf"
	pftfbridge "github.com/pulumi/pulumi-terraform-bridge/v3/pkg/pf/tfbridge"
	pftfgen "github.com/pulumi/pulumi-terraform-bridge/v3/pkg/pf/tfgen"
	"github.com/pulumi/pulumi-terraform-bridge/v3/pkg/tfbridge"
	"github.com/pulumi/pulumi-terraform-bridge/v3/pkg/tfbridge/tokens"
	"github.com/pulumi/pulumi-terraform-bridge/v3/pkg/tfgen"
	shimv2 "github.com/pulumi/pulumi-terraform-bridge/v3/pkg/tfshim/sdk-v2"
	pulumidiag "github.com/pulumi/pulumi/sdk/v3/go/common/diag"
	"github.com/pulumi/pulumi/sdk/v3/go/common/diag/colors"
	pulumirpc "github.com/pulumi/pulumi/sdk/v3/proto/go"
	"github.com/stretchr/testify/require"
)

// SDKv2ProviderDynamic serves an SDKv2 (helper/schema) provider in-process
// over go-plugin; the engine reaches it through the real terraform-provider
// plugin via PULUMI_BRIDGE_REATTACH_PROVIDERS, running the full
// parameterization flow. Pass an already-wrapped factory (tfexec.Wrap) to
// record at the CRUD boundary.
func SDKv2ProviderDynamic(
	t *testing.T, providerName string, factory func() *schema.Provider,
) Provider {
	t.Helper()
	return Provider{
		Name:     providerName,
		Reattach: tfexec.ServeProvider(t, tfexec.SDKv2Provider(t, providerName, factory)),
	}
}

// PFProviderDynamic is SDKv2ProviderDynamic for terraform-plugin-framework
// providers, recording provider operations to rec at the tfprotov6 boundary —
// the same boundary tfexec.PFProvider records at on the OpenTofu path.
func PFProviderDynamic(
	t *testing.T, providerName string, factory func() pfprovider.Provider, rec *tfexec.Recorder,
) Provider {
	t.Helper()
	requireValidPFSchema(t, factory())
	return Provider{
		Name:     providerName,
		Reattach: tfexec.ServeProvider(t, tfexec.PFProvider(providerName, factory, rec)),
	}
}

// SDKv2Provider builds a Provider from an SDKv2 (helper/schema) provider
// factory via the classic bridge. Start builds a fresh provider per call so
// each engine-side provider instance configures its own copy (see
// connRoutedProvider).
func SDKv2Provider(
	t *testing.T, providerName string, factory func() *schema.Provider,
	customize func(*testing.T, *tfbridge.ProviderInfo),
) Provider {
	t.Helper()
	// Surface validation problems at construction time.
	bridgedProvider(t, providerName, factory(), customize)
	return Provider{Name: providerName, Start: func(ctx context.Context) (pulumirpc.ResourceProviderServer, error) {
		return providerServerFromInfo(ctx, bridgedProvider(t, providerName, factory(), customize))
	}}
}

// PFProvider builds a Provider from a terraform-plugin-framework provider
// factory via the plugin-framework bridge, recording provider operations to
// rec at the tfprotov6 boundary — the same boundary tfexec.PFProvider records
// at on the OpenTofu path. customize plays the same role as in
// BridgedProvider. Start builds a fresh provider per call so each engine-side
// provider instance configures its own copy (see connRoutedProvider).
func PFProvider(
	t *testing.T, providerName string, factory func() pfprovider.Provider, rec *tfexec.Recorder,
	customize func(*testing.T, *tfbridge.ProviderInfo),
) Provider {
	t.Helper()

	requireValidPFSchema(t, factory())

	buildInfo := func(p pfprovider.Provider) (tfbridge.ProviderInfo, error) {
		shimProvider, ok := pftfbridge.ShimProvider(p).(pf.ShimProvider)
		if !ok {
			return tfbridge.ProviderInfo{}, errors.New("pftfbridge.ShimProvider did not return a pf.ShimProvider")
		}
		info := tfbridge.ProviderInfo{
			P:            recordingShim{ShimProvider: shimProvider, rec: rec},
			Name:         providerName,
			Version:      "0.0.1",
			MetadataInfo: tfbridge.NewProviderMetadata(nil),
		}
		info.MustComputeTokens(tokens.SingleModule(providerName, "index", tokens.MakeStandard(providerName)))
		if customize != nil {
			customize(t, &info)
		}
		return info, nil
	}
	_, err := buildInfo(factory())
	require.NoError(t, err)

	return Provider{Name: providerName, Start: func(ctx context.Context) (pulumirpc.ResourceProviderServer, error) {
		info, err := buildInfo(factory())
		if err != nil {
			return nil, err
		}
		res, err := pftfgen.GenerateSchema(ctx, pftfgen.GenerateSchemaOptions{
			ProviderInfo:    info,
			DiagnosticsSink: discardSink(),
		})
		if err != nil {
			return nil, fmt.Errorf("pf tfgen.GenerateSchema failed: %w", err)
		}
		return pftfbridge.NewProviderServer(ctx, nil, info, res.ProviderMetadata)
	}}
}

// requireValidPFSchema is the plugin-framework analog of SDKv2's
// InternalValidate: the framework validates every schema implementation while
// answering GetProviderSchema, so a clean response means the provider's
// schemas are well-formed.
func requireValidPFSchema(t *testing.T, p pfprovider.Provider) {
	t.Helper()
	srv := providerserver.NewProtocol6(p)()
	resp, err := srv.GetProviderSchema(t.Context(), &tfprotov6.GetProviderSchemaRequest{})
	require.NoError(t, err)
	for _, d := range resp.Diagnostics {
		require.NotEqual(t, tfprotov6.DiagnosticSeverityError, d.Severity,
			"provider schema diagnostic: %s: %s", d.Summary, d.Detail)
	}
}

// recordingShim wraps the pf shim so the tfprotov6 server it hands the bridge
// records operations, mirroring tfexec.WrapServer on the OpenTofu path.
type recordingShim struct {
	pf.ShimProvider
	rec *tfexec.Recorder
}

func (s recordingShim) Server(ctx context.Context) (tfprotov6.ProviderServer, error) {
	srv, err := s.ShimProvider.Server(ctx)
	if err != nil {
		return nil, err
	}
	return tfexec.WrapServer(srv, s.rec), nil
}

// bridgedProvider wraps a *schema.Provider into a tfbridge.ProviderInfo with
// auto-generated tokens and no-op CRUD methods where missing. customize, if
// non-nil, runs after token computation so tests can apply non-default
// Pulumi-side renames (or any other ProviderInfo tweak) before the provider
// is served.
func bridgedProvider(
	t *testing.T, providerName string, tfp *schema.Provider,
	customize func(*testing.T, *tfbridge.ProviderInfo),
) tfbridge.ProviderInfo {
	t.Helper()

	require.NoError(t, tfp.InternalValidate())

	shimProvider := shimv2.NewProvider(tfp)

	provider := tfbridge.ProviderInfo{
		P:                              shimProvider,
		Name:                           providerName,
		Version:                        "0.0.1",
		MetadataInfo:                   &tfbridge.MetadataInfo{},
		EnableZeroDefaultSchemaVersion: true,
	}
	provider.MustComputeTokens(tokens.SingleModule(providerName, "index", tokens.MakeStandard(providerName)))

	if customize != nil {
		customize(t, &provider)
	}

	return provider
}

func discardSink() pulumidiag.Sink {
	return pulumidiag.DefaultSink(io.Discard, io.Discard, pulumidiag.FormatOptions{
		Color: colors.Never,
	})
}

func providerServerFromInfo(
	ctx context.Context, providerInfo tfbridge.ProviderInfo,
) (pulumirpc.ResourceProviderServer, error) {
	sink := discardSink()

	pkgSchema, err := tfgen.GenerateSchema(providerInfo, sink)
	if err != nil {
		return nil, fmt.Errorf("tfgen.GenerateSchema failed: %w", err)
	}

	schemaBytes, err := json.MarshalIndent(pkgSchema, "", " ")
	if err != nil {
		return nil, fmt.Errorf("json.MarshalIndent(schema, ..) failed: %w", err)
	}

	return tfbridge.NewProvider(
		ctx, nil, providerInfo.Name, providerInfo.Version, providerInfo.P, providerInfo, schemaBytes), nil
}
