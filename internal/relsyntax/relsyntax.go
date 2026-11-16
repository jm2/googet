/*
Copyright 2026 Google Inc. All Rights Reserved.
Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at
    http://www.apache.org/licenses/LICENSE-2.0
Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package relsyntax holds the lexical layer of the Conflicts and Replaces
// relation grammar: the version operators, the set of runes that form them,
// and Split, which classifies an entry as Legacy, Operator or Malformed.
// Callers parse Legacy and Malformed entries as before, with
// goolib.PkgNameSplit; Split only splits Operator entries.
//
// The package is purely lexical and imports only the standard library,
// because goolib imports it and internal/relation imports goolib. Only goolib
// and internal/relation import it; other packages use the operator names
// that internal/relation re-exports.
package relsyntax

import (
	"strings"
	"unicode"
)

// Op is a version operator in a Conflicts or Replaces entry.
type Op uint8

// The version operators. OpNone is the zero Op: an entry with no operator.
// internal/relation names it OpMin, the legacy minimum-version meaning.
const (
	OpNone Op = iota
	OpLT
	OpLE
	OpEQ
	OpGE
	OpGT
)

// String returns the operator as it is written in an entry, or an empty
// string for OpNone and for values that aren't operators.
func (o Op) String() string {
	switch o {
	case OpLT:
		return "<"
	case OpLE:
		return "<="
	case OpEQ:
		return "="
	case OpGE:
		return ">="
	case OpGT:
		return ">"
	default:
		return ""
	}
}

// IsOpRune reports whether r is one of the runes that form a version
// operator: '<', '>' or '='.
func IsOpRune(r rune) bool {
	return r == '<' || r == '>' || r == '='
}

// Form classifies a Conflicts or Replaces entry.
type Form uint8

// The entry forms.
const (
	// Legacy is an entry with no operator rune: name[.arch[.version]].
	Legacy Form = iota
	// Operator is a well-formed entry of the form name[.arch]OPversion.
	Operator
	// Malformed is an entry that has an operator rune but isn't a
	// well-formed Operator entry.
	Malformed
)

// String returns the name of the form.
func (f Form) String() string {
	switch f {
	case Legacy:
		return "Legacy"
	case Operator:
		return "Operator"
	case Malformed:
		return "Malformed"
	default:
		return "Form(invalid)"
	}
}

// Parts is the result of Split. Fields other than Form are set only when
// Form is Operator.
type Parts struct {
	Form Form
	// Name and Arch are the left side of the operator, split at its only
	// dot. Arch is empty when the left side has no dot.
	Name, Arch string
	Op         Op
	Ver        string
	// Spaced reports whether whitespace separates the operator from the
	// left side or from the version. Whitespace around the whole entry
	// doesn't count.
	Spaced bool
}

// Split classifies the Conflicts or Replaces entry s and, for an Operator
// entry, splits it into its parts. The grammar is:
//
//	entry   = legacy | opform
//	legacy  = any string with no operator rune
//	opform  = ws* lhs ws* op ws* version ws*
//	lhs     = name [ "." arch ]
//	op      = "<=" | ">=" | "<" | ">" | "="
//	name, arch, version = 1*( rune - operator rune - whitespace )
//
// The operator is located at the first operator rune, before the left side
// is split at its dot, and it is the longest operator that starts there.
// Whitespace is any rune for which unicode.IsSpace reports true. An entry
// that has an operator rune but doesn't match opform is Malformed: for
// example a second operator rune, an empty name, arch or version, more than
// one dot on the left side, or whitespace inside the left side or the
// version.
func Split(s string) Parts {
	i := strings.IndexFunc(s, IsOpRune)
	if i < 0 {
		return Parts{Form: Legacy}
	}
	op, n := opAt(s[i:])
	left, right := s[:i], s[i+n:]
	if strings.IndexFunc(right, IsOpRune) >= 0 {
		return Parts{Form: Malformed}
	}
	lhs := strings.TrimRightFunc(left, unicode.IsSpace)
	ver := strings.TrimLeftFunc(right, unicode.IsSpace)
	spaced := len(lhs) != len(left) || len(ver) != len(right)
	lhs = strings.TrimLeftFunc(lhs, unicode.IsSpace)
	ver = strings.TrimRightFunc(ver, unicode.IsSpace)
	if !isToken(ver) {
		return Parts{Form: Malformed}
	}
	name, arch, hasDot := strings.Cut(lhs, ".")
	if !isToken(name) || (hasDot && !isToken(arch)) || strings.Contains(arch, ".") {
		return Parts{Form: Malformed}
	}
	return Parts{Form: Operator, Name: name, Arch: arch, Op: op, Ver: ver, Spaced: spaced}
}

// opAt returns the longest operator at the start of s, which must begin with
// an operator rune, and its length in bytes.
func opAt(s string) (Op, int) {
	two := len(s) >= 2 && s[1] == '='
	switch s[0] {
	case '<':
		if two {
			return OpLE, 2
		}
		return OpLT, 1
	case '>':
		if two {
			return OpGE, 2
		}
		return OpGT, 1
	default:
		return OpEQ, 1
	}
}

// isToken reports whether s is a non-empty run of runes that are neither
// operator runes nor whitespace.
func isToken(s string) bool {
	return s != "" && strings.IndexFunc(s, func(r rune) bool { return IsOpRune(r) || unicode.IsSpace(r) }) < 0
}
