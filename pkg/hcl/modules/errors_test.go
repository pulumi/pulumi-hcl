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

package modules

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	regaddr "github.com/opentofu/registry-address/v2"
	"github.com/opentofu/svchost/disco"
	"github.com/stretchr/testify/require"
)

// TestRegistryHTTPErrorClassification verifies that a failed modules.v1
// request wraps the classification sentinel implied by its HTTP status and by
// whether the request carried credentials.
func TestRegistryHTTPErrorClassification(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name         string
		status       int
		credentialed bool
		want         error
	}{
		{"not found", http.StatusNotFound, true, ErrNotFound},
		{"unauthorized", http.StatusUnauthorized, true, ErrUnauthenticated},
		{"forbidden", http.StatusForbidden, true, ErrPermissionDenied},
		{"unauthorized without credentials", http.StatusUnauthorized, false, ErrNoCredentials},
		{"forbidden without credentials", http.StatusForbidden, false, ErrNoCredentials},
		{"not found without credentials", http.StatusNotFound, false, ErrNotFound},
		{"rate limited", http.StatusTooManyRequests, true, ErrTransient},
		{"server error", http.StatusInternalServerError, true, ErrTransient},
		{"bad gateway", http.StatusBadGateway, true, ErrTransient},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, "nope", tc.status)
			}))
			t.Cleanup(srv.Close)
			n := newTestNetworkResolver(t, srv.URL)
			if tc.credentialed {
				n.disco.SetCredentialsSource(credentialsForRegistry(
					&cloudRegistry{host: regaddr.DefaultModuleRegistryHost, token: "secret-token"}))
			}

			_, _, err := n.resolveRegistrySource("acme/thing/aws", "")
			require.ErrorIs(t, err, tc.want)
		})
	}
}

func TestRegistryNetworkFailureIsTransient(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.NotFoundHandler())
	srv.Close() // connection refused from here on
	n := newTestNetworkResolver(t, srv.URL)

	_, _, err := n.resolveRegistrySource("acme/thing/aws", "")
	require.ErrorIs(t, err, ErrTransient)
}

func TestRegistryVersionErrorsAreNotFound(t *testing.T) {
	t.Parallel()
	srv, _ := newFakeRegistry(t, []string{"1.0.0", "2.0.0"}, map[string]string{})
	n := newTestNetworkResolver(t, srv.URL)

	t.Run("constraint unsatisfied", func(t *testing.T) {
		t.Parallel()
		_, _, err := n.resolveRegistrySource("acme/thing/aws", "~> 99.0")
		require.ErrorIs(t, err, ErrNotFound)
	})
	t.Run("invalid constraint", func(t *testing.T) {
		t.Parallel()
		_, _, err := n.resolveRegistrySource("acme/thing/aws", "not-a-constraint")
		require.ErrorIs(t, err, ErrInvalid)
	})
	t.Run("no versions published", func(t *testing.T) {
		t.Parallel()
		empty, _ := newFakeRegistry(t, nil, map[string]string{})
		_, _, err := newTestNetworkResolver(t, empty.URL).resolveRegistrySource("acme/thing/aws", "")
		require.ErrorIs(t, err, ErrNotFound)
	})
}

func TestHostWithoutModuleRegistryIsNotFound(t *testing.T) {
	t.Parallel()
	d := disco.New()
	// The host answers discovery but provides no modules.v1 service.
	d.ForceHostServices(regaddr.DefaultModuleRegistryHost, map[string]any{})
	n := &networkResolver{cacheDir: t.TempDir(), disco: d}

	_, _, err := n.resolveRegistrySource("acme/thing/aws", "")
	require.ErrorIs(t, err, ErrNotFound)
}

func TestLocalModuleErrorsAreNotFound(t *testing.T) {
	t.Parallel()
	t.Run("missing directory", func(t *testing.T) {
		t.Parallel()
		_, err := statDir(filepath.Join(t.TempDir(), "no-such-dir"))
		require.ErrorIs(t, err, ErrNotFound)
	})
	t.Run("missing subdir", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		l := NewLoader(func(string, string, string) (string, string, error) { return dir, "", nil })
		_, _, err := l.resolveSource("acme/thing/aws//missing", "", ".")
		require.ErrorIs(t, err, ErrNotFound)
	})
}
