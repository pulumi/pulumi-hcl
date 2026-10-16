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
	"fmt"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/hashicorp/hcl/v2/hclsyntax"
)

// MakeValidIdentifier turns s into a valid HCL identifier.
//
// Each run of runes that an identifier cannot contain becomes one "_", and
// that "_" absorbs the "_" runes next to it, so "a_!b" becomes "a_b". A "_"
// made this way is dropped at either end, as are leading runes that cannot
// start an identifier. A valid identifier is returned unchanged.
//
// Every ASCII letter and digit is kept, except digits before the first ASCII
// letter, so the reduction of a schema name to its primary character set is
// the same before and after.
//
// MakeValidIdentifier returns an error if nothing remains.
func MakeValidIdentifier(s string) (string, error) {
	var b strings.Builder
	// underscores counts the source "_" runes not yet written to b, and
	// invalid records an invalid rune since the last rune written to b.
	underscores, invalid := 0, false
	flush := func() {
		switch {
		case invalid && b.Len() > 0:
			b.WriteByte('_')
		case !invalid:
			b.WriteString(strings.Repeat("_", underscores))
		}
		underscores, invalid = 0, false
	}
	for _, r := range s {
		switch {
		case r == '_':
			underscores++
		case !IsIdentifierRune(r):
			invalid = true
		default:
			flush()
			if b.Len() == 0 && !canStartIdentifier(r) {
				continue
			}
			b.WriteRune(r)
		}
	}
	// A trailing replaced run is dropped; trailing source "_" runes stay.
	if !invalid {
		flush()
	}
	out := b.String()
	if out == "" {
		return "", fmt.Errorf("cannot make %q a valid HCL identifier: no valid characters remain", s)
	}
	return out, nil
}

// IsIdentifierRune reports whether r can appear in an HCL identifier after
// its first rune.
func IsIdentifierRune(r rune) bool {
	return classify(r).canContinue
}

func canStartIdentifier(r rune) bool {
	return classify(r).canStart
}

type runeClass struct {
	canStart, canContinue bool
}

// classify asks the HCL scanner how it treats r, so the result matches the
// Unicode tables that HCL uses and not the tables of the Go standard library.
func classify(r rune) runeClass {
	if r < utf8.RuneSelf {
		return asciiClasses[r]
	}
	if c, ok := runeClasses.Load(r); ok {
		return c.(runeClass)
	}
	c := scanRuneClass(r)
	runeClasses.Store(r, c)
	return c
}

func scanRuneClass(r rune) runeClass {
	return runeClass{
		canStart:    hclsyntax.ValidIdentifier(string(r)),
		canContinue: hclsyntax.ValidIdentifier("a" + string(r)),
	}
}

var (
	asciiClasses = func() (classes [utf8.RuneSelf]runeClass) {
		for r := range rune(utf8.RuneSelf) {
			classes[r] = scanRuneClass(r)
		}
		return classes
	}()
	runeClasses sync.Map
)
