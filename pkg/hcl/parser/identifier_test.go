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

package parser

import (
	"strings"
	"testing"
	"unicode"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"
)

func TestMakeValidIdentifier(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in   string
		want string
	}{
		{"foo", "foo"},
		{"a__b", "a__b"},
		{"_foo", "_foo"},
		{"foo_", "foo_"},
		{"foo-", "foo-"},
		{"_1foo", "_1foo"},
		{"a-b", "a-b"},
		{"café", "café"},
		{"日本", "日本"},
		{"helm.sh/v3", "helm_sh_v3"},
		{"a!!b", "a_b"},
		{"a_!b", "a_b"},
		{"a!_b", "a_b"},
		{"a_!_b", "a_b"},
		{"@foo!", "foo"},
		{"_!foo", "foo"},
		{"foo!_", "foo"},
		{"12foo", "foo"},
		{"-foo", "foo"},
		{"!1foo", "foo"},
		{"1!foo", "foo"},
		{"!_1foo", "foo"},
		{"😃foo", "foo"},
		{"foo😃bar", "foo_bar"},
		{"\ufefffoo", "foo"},
		{"\ufeffῌ_", "ῌ_"},
		{"foo bar", "foo_bar"},
		{"rbac.authorization.k8s.io/v1", "rbac_authorization_k8s_io_v1"},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			t.Parallel()
			got, err := MakeValidIdentifier(tt.in)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestMakeValidIdentifierError(t *testing.T) {
	t.Parallel()

	for _, in := range []string{"", "@!", "123", "!_!", "-", "😃"} {
		t.Run(in, func(t *testing.T) {
			t.Parallel()
			_, err := MakeValidIdentifier(in)
			assert.EqualError(t, err, `cannot make "`+in+`" a valid HCL identifier: no valid characters remain`)
		})
	}
}

// TestMakeValidIdentifierProperties checks the guarantees that callers rely
// on: the result is a valid identifier, a valid identifier is unchanged, and
// the reduction to the primary character set is kept, so names that the
// schema keeps unique stay unique.
func TestMakeValidIdentifierProperties(t *testing.T) {
	t.Parallel()

	identifier := rapid.String()
	rapid.Check(t, func(t *rapid.T) {
		ident := identifier.Draw(t, "identifier")
		valid, err := MakeValidIdentifier(ident)
		if err != nil {
			t.SkipNow()
		}
		assert.True(t, ValidIdentifier(valid), "MakeValidIdentifier(%q) = %q is not valid", ident, valid)
		assert.Equal(t, reduceToASCII(ident), reduceToASCII(valid), "an ascii reduction shouldn't be changed")
		if ValidIdentifier(ident) {
			assert.Equal(t, ident, valid)
		}
	})
}

func reduceToASCII(name string) string {
	var b strings.Builder
	for _, r := range name {
		isLetter := ('a' <= r && r <= 'z') || ('A' <= r && r <= 'Z')
		isNumber := '0' <= r && r <= '9'
		if isLetter || (b.Len() > 0 && isNumber) {
			b.WriteRune(unicode.ToLower(r))
		}
	}
	return b.String()
}

func TestValidIdentifier(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in   string
		want bool
	}{
		{"foo", true},
		{"a-b", true},
		{"日本", true},
		{"1foo", false},
		{"a.b", false},
		{"\ufefffoo", false},
		{"foo\ufeff", false},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, ValidIdentifier(tt.in))
		})
	}
}
