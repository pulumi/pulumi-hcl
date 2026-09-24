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

// Adding a new language test
//
// Language tests are defined in the pulumi/pulumi repo under
// pkg/testing/pulumi-test-language/tests/. Each test has a Go file
// defining the test configuration and a testdata directory containing
// one or more .pp (PCL) source files.
//
// To enable a new language test for the HCL runtime:
//
//  1. Look up the test's .pp files in the pulumi repo to understand what
//     PCL constructs the test uses (outputs, resources, function calls, etc.).
//
//  2. Ensure the HCL codegen (pkg/codegen/generate.go) can convert the PCL
//     constructs to valid HCL. Add support for any missing expression types,
//     function calls, or resource types.
//
//  3. Ensure the HCL runtime (pkg/hcl/run/, pkg/hcl/eval/, pkg/server/)
//     can execute the generated HCL correctly. This may involve adding
//     support for new built-in resource types, functions, or path handling.
//
//  4. Remove the test name from the expectedFailures map below.
//
//  5. Run the test with PULUMI_ACCEPT=1 to generate snapshot files:
//     PULUMI_ACCEPT=1 go test ./cmd/pulumi-language-hcl/ -run 'TestLanguage/<test-name>' -count=1 -v
//     This creates/updates files under testdata/projects/<test-name>/.
//
//  6. Run the test without PULUMI_ACCEPT to verify it passes:
//     go test ./cmd/pulumi-language-hcl/ -run 'TestLanguage/<test-name>' -count=1 -v

package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"

	"github.com/pulumi/pulumi-hcl/pkg/converter"
	"github.com/pulumi/pulumi-hcl/pkg/server"

	"github.com/pulumi/pulumi/pkg/v3/resource/plugin"
	"github.com/pulumi/pulumi/sdk/v3/go/common/util/contract"
	"github.com/pulumi/pulumi/sdk/v3/go/common/util/rpcutil"
	pulumirpc "github.com/pulumi/pulumi/sdk/v3/proto/go"
	testingrpc "github.com/pulumi/pulumi/sdk/v3/proto/go/testing"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func runTestingHost(t *testing.T) (string, testingrpc.LanguageTestClient) {
	// We can't just go run the pulumi-test-language package because of
	// https://github.com/golang/go/issues/39172, so we build it to a temp file then run that.
	binary := t.TempDir() + "/pulumi-test-language"
	cmd := exec.CommandContext(t.Context(),
		"go", "build", "-o", binary,
		"github.com/pulumi/pulumi/pkg/v3/testing/pulumi-test-language")
	output, err := cmd.CombinedOutput()
	if len(output) > 0 {
		t.Logf("build output: %s", output)
	}
	require.NoError(t, err)

	cmd = exec.CommandContext(t.Context(), binary)
	stdout, err := cmd.StdoutPipe()
	require.NoError(t, err)
	stderr, err := cmd.StderrPipe()
	require.NoError(t, err)
	stderrReader := bufio.NewReader(stderr)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		for {
			text, err := stderrReader.ReadString('\n')
			if err != nil {
				wg.Done()
				return
			}
			t.Logf("engine: %s", text)
		}
	}()

	err = cmd.Start()
	require.NoError(t, err)

	stdoutBytes, err := io.ReadAll(stdout)
	require.NoError(t, err)

	address := string(stdoutBytes)

	conn, err := grpc.NewClient(
		address,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithUnaryInterceptor(rpcutil.OpenTracingClientInterceptor()),
		grpc.WithStreamInterceptor(rpcutil.OpenTracingStreamClientInterceptor()),
		rpcutil.GrpcChannelOptions(),
	)
	require.NoError(t, err)

	client := testingrpc.NewLanguageTestClient(conn)

	t.Cleanup(func() {
		assert.NoError(t, cmd.Process.Kill())
		wg.Wait()
		// We expect this to error because we just killed it.
		contract.IgnoreError(cmd.Wait())
	})

	return address, client
}

var expectedFailures = map[string]string{
	"l2-plain": "unsupported in HCL:" +
		" requires that HCL can distinguish between an empty and null List<Object>" +
		" - not compatible with block syntax",
	"l2-resource-option-hooks":       "PCL pcl.Hook nodes not yet supported by HCL codegen",
	"l2-resource-hook-after-failure": "PCL pcl.Hook nodes not yet supported by HCL codegen",
	"l2-resource-hook-on-error":      "PCL pcl.Hook nodes not yet supported by HCL codegen",
	"l2-resource-hook-ignore-errors": "PCL pcl.Hook nodes not yet supported by HCL codegen",
	"l2-resource-hook-panic":         "PCL pcl.Hook nodes not yet supported by HCL codegen (added in v3.259.0)",
	"l1-component-sourceless": "sourceless PCL components (token-only, no source directory) are not supported" +
		" by HCL codegen: the emitted module source \"./.\" includes the root module in itself and hangs" +
		" (added in v3.259.0)",
	"l2-resource-read":            "PCL pcl.ReadResource nodes not yet supported by HCL codegen",
	"l2-resource-read-unknown":    "PCL pcl.ReadResource nodes not yet supported by HCL codegen (added in v3.260.0)",
	"l2-resource-read-depends-on": "PCL pcl.ReadResource nodes not yet supported by HCL codegen (added in v3.264.0)",
	"l2-map-keys-invoke-call": "HCL codegen renames a call() result property to its TF name before" +
		" traversing the schema type: unknown property 'string_map' among [stringMap] (added in v3.260.0)",
	"l2-discriminated-union-marked-key": "whole objects in stack outputs must keep their wire-format" +
		" (camelCase) keys, but the runtime emits the TF-renamed snake_case keys:" +
		" expected 'discriminantKind', got 'discriminant_kind'" +
		" (added in v3.260.0, https://github.com/pulumi/pulumi-hcl/issues/589)",
	"l2-reserved-names": "whole objects in stack outputs must keep their wire-format (camelCase) keys," +
		" but the runtime emits the TF-renamed snake_case keys: expected 'elementType', got 'element_type'" +
		" (asserted since v3.260.0, https://github.com/pulumi/pulumi-hcl/issues/589)",
	"l2-provider-call-explicit": "upstream fixture declares `provider \"call\"`, which" +
		" is reserved as the namespace for resource method calls in HCL",
	"l2-config-default-from-invoke": "HCL codegen does not support a config variable whose" +
		" default is sourced from an invoke result",
}

// expectedEjectFailures lists tests whose eject (HCL→PCL conversion) step is
// expected to fail because the converter does not yet support resources, data
// sources, or other constructs used by those tests.
var expectedEjectFailures = map[string]string{
	"l1-config-types-optional": "HCL drops PCL optional() in config types so round-tripping a null-bearing" +
		" default through HCL loses the nullability the original PCL declared",
}

func has[K comparable, V any, M ~map[K]V](m M, k K) bool {
	_, ok := m[k]
	return ok
}

func log(t *testing.T, name, message string) {
	if os.Getenv("PULUMI_LANGUAGE_TEST_SHOW_FULL_OUTPUT") != "true" {
		if len(message) > 1024 {
			message = message[:1024] + "... (truncated, run with PULUMI_LANGUAGE_TEST_SHOW_FULL_OUTPUT=true to see full logs))"
		}
	}
	t.Logf("%s: %s", name, message)
}

func TestLanguage(t *testing.T) {
	t.Parallel()

	engineAddress, engine := runTestingHost(t)

	tests, err := engine.GetLanguageTests(t.Context(), &testingrpc.GetLanguageTestsRequest{})
	require.NoError(t, err)

	cancel := make(chan bool)

	handle, err := rpcutil.ServeWithOptions(rpcutil.ServeOptions{
		Init: func(srv *grpc.Server) error {
			// Conformance fixtures use Pulumi semantics where explicitly
			// declared providers are resources that must appear in the
			// snapshot, so opt out of Terraform's lazy provider registration.
			host, err := server.NewLanguageHost(engineAddress, server.WithAlwaysRegisterProviders())
			if err != nil {
				return err
			}
			t.Cleanup(func() { contract.IgnoreClose(host) })
			pulumirpc.RegisterLanguageRuntimeServer(srv, host)
			pulumirpc.RegisterConverterServer(srv, plugin.NewConverterServer(converter.New()))
			return nil
		},
		Cancel: cancel,
	})
	require.NoError(t, err)

	rootDir := t.TempDir()

	snapshotDir := "./testdata/"

	prepare, err := engine.PrepareLanguageTests(t.Context(), &testingrpc.PrepareLanguageTestsRequest{
		LanguagePluginName:    "hcl",
		LanguagePluginTarget:  fmt.Sprintf("127.0.0.1:%d", handle.Port),
		ConverterPluginTarget: fmt.Sprintf("127.0.0.1:%d", handle.Port),
		TemporaryDirectory:    rootDir,
		SnapshotDirectory:     snapshotDir,
		ProvidersDirectory:    "testdata/providers",
	})
	require.NoError(t, err)

	for _, tt := range tests.Tests {
		t.Run(tt, func(t *testing.T) {
			t.Parallel()
			if strings.HasPrefix(tt, "policy-") {
				t.Skip("HCL does not support policy tests")
			}
			if expected, ok := expectedFailures[tt]; ok {
				t.Skipf("Skipping known failure: %s", expected)
			}

			result, err := engine.RunLanguageTest(t.Context(), &testingrpc.RunLanguageTestRequest{
				Token:            prepare.Token,
				Test:             tt,
				SkipConvertTests: has(expectedEjectFailures, tt),
			}, grpc.MaxCallRecvMsgSize(1024*1024*1024))

			require.NoError(t, err)
			for _, msg := range result.Messages {
				t.Log(msg)
			}
			log(t, "stdout", result.Stdout)
			log(t, "stderr", result.Stderr)
			assert.True(t, result.Success)
		})
	}

	t.Cleanup(func() {
		close(cancel)
		assert.NoError(t, <-handle.Done)
	})
}
