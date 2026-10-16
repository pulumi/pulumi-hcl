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

package packages

import (
	"strings"
	"unicode"

	"github.com/pulumi/pulumi-hcl/pkg/hcl/parser"
	"github.com/pulumi/pulumi/pkg/v3/codegen/schema"
)

// suggestEditDistanceThreshold caps how far a candidate can be from the
// user's input before we stay silent rather than emit a misleading hint.
const suggestEditDistanceThreshold = 3

func nearestHCLToken(pkg schema.PackageReference, hclToken string, isFunction bool) string {
	bestDist := suggestEditDistanceThreshold + 1
	var best string
	// Candidates are valid identifiers, so compare a label such as
	// "kubernetes_helm.sh_v3_release" by its valid spelling; otherwise every
	// "." counts as an edit.
	if id, err := parser.MakeValidIdentifier(hclToken); err == nil {
		hclToken = id
	}
	visit := func(tok string) {
		candidate := pulumiTokenToHCLForm(pkg, tok, isFunction)
		if candidate == "" {
			return
		}
		d := levenshtein(hclToken, candidate)
		if d < bestDist {
			bestDist = d
			best = candidate
		}
	}
	if isFunction {
		for iter := pkg.Functions().Range(); iter.Next(); {
			visit(iter.Token())
		}
	} else {
		for iter := pkg.Resources().Range(); iter.Next(); {
			visit(iter.Token())
		}
	}
	return best
}

// pulumiTokenToHCLForm converts a Pulumi token (e.g. "aws:ec2/vpc:Vpc") to
// the HCL form the resolver accepts (e.g. "aws_ec2_vpc"). TokenToModule
// applies the schema's ModuleFormat regex, so bridged-provider tokens
// normalize correctly.
func pulumiTokenToHCLForm(pkg schema.PackageReference, token string, isFunction bool) string {
	parts := strings.SplitN(token, ":", 3)
	if len(parts) < 3 {
		return ""
	}
	pkgName := parts[0]
	name := parts[2]
	mod := pkg.TokenToModule(token)

	if isFunction && strings.HasPrefix(name, "get") && len(name) > 3 {
		r := rune(name[3])
		if r >= 'A' && r <= 'Z' {
			name = name[3:]
		}
	}

	var b strings.Builder
	b.WriteString(pkgName)
	if mod != "" && mod != "index" {
		b.WriteRune('_')
		b.WriteString(strings.ToLower(mod))
	}
	b.WriteRune('_')
	b.WriteString(camelToSnake(name))
	id, err := parser.MakeValidIdentifier(b.String())
	if err != nil {
		return ""
	}
	return id
}

// camelToSnake converts a camelCase token segment to snake_case. It keeps
// runes that an HCL identifier cannot contain; parser.MakeValidIdentifier
// replaces them and absorbs the "_" added next to them.
func camelToSnake(s string) string {
	var b strings.Builder
	for i, r := range s {
		if i > 0 && unicode.IsUpper(r) && s[i-1] != '_' {
			b.WriteRune('_')
		}
		b.WriteRune(unicode.ToLower(r))
	}
	return b.String()
}

func levenshtein(a, b string) int {
	if a == b {
		return 0
	}
	if len(a) == 0 {
		return len(b)
	}
	if len(b) == 0 {
		return len(a)
	}
	prev := make([]int, len(b)+1)
	curr := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		curr[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			curr[j] = min(curr[j-1]+1, prev[j]+1, prev[j-1]+cost)
		}
		prev, curr = curr, prev
	}
	return prev[len(b)]
}
