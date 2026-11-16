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
// relation grammar: the version operators and the set of runes that form
// them.
//
// The package is purely lexical and imports only the standard library,
// because goolib imports it and internal/relation imports goolib. Only goolib
// and internal/relation import it; other packages use the operator names
// that internal/relation re-exports.
package relsyntax

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
