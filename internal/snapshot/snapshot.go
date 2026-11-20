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

// Package snapshot answers the googet database's package lookups over an
// in-memory copy of the installed package state, so that an install can be
// planned without reading or changing the database.
//
// Each function mirrors one googetdb query exactly, including its quirks.
// googetdb's tests compare each one with the real query; keep the two in sync.
package snapshot

import (
	"cmp"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/google/googet/v2/client"
	"github.com/google/googet/v2/goolib"
)

// Pick mirrors googetdb.GooDB.FetchPkg over st. Among entries whose
// PackageSpec.Name equals pi.Name, ordered bytewise by (Arch, Version) as
// SQLite's default BINARY collation orders them, it returns the first entry
// matching pi, or else the last entry with that name. It returns false if no
// entry has that name. Entries with a nil PackageSpec are skipped.
func Pick(pi goolib.PackageInfo, st client.GooGetState) (client.PackageState, bool) {
	var sameName []client.PackageState
	for _, ps := range st {
		if ps.PackageSpec != nil && ps.PackageSpec.Name == pi.Name {
			sameName = append(sameName, ps)
		}
	}
	if len(sameName) == 0 {
		return client.PackageState{}, false
	}
	slices.SortFunc(sameName, func(a, b client.PackageState) int {
		return cmp.Or(
			strings.Compare(a.PackageSpec.Arch, b.PackageSpec.Arch),
			strings.Compare(a.PackageSpec.Version, b.PackageSpec.Version),
		)
	})
	var last client.PackageState
	for _, ps := range sameName {
		if ps.Match(pi) {
			return ps, true
		}
		last = ps
	}
	return last, true
}

// LockState mirrors the installed arch and lock flag that
// googetdb.GooDB.InstalledLockState returns for name over st. Like the
// database, it selects entries whose name matches name as a SQLite LIKE
// pattern, or every entry when name is empty, and reports the first one in
// bytewise (Name, Arch) order. It returns "" and false if nothing matches.
// Entries with a nil PackageSpec are skipped; the database can't store them.
func LockState(name string, st client.GooGetState) (arch string, locked bool) {
	var first *goolib.PkgSpec
	for _, ps := range st {
		spec := ps.PackageSpec
		if spec == nil || (name != "" && !like(name, spec.Name)) {
			continue
		}
		if first == nil || cmp.Or(strings.Compare(spec.Name, first.Name), strings.Compare(spec.Arch, first.Arch)) < 0 {
			first = spec
		}
	}
	if first == nil {
		return "", false
	}
	return first.Arch, first.LockArch
}

// like reports whether s matches pattern under SQLite's default LIKE rules:
// "%" matches any sequence of characters, "_" matches exactly one character,
// ASCII letters match case-insensitively, any other character matches only
// itself, and there is no escape character. Both strings are read as UTF-8.
func like(pattern, s string) bool {
	// px and sx are the positions to resume from when the most recent "%" has
	// to absorb one more character of s.
	p, i := 0, 0
	px, sx := -1, -1
	for i < len(s) || p < len(pattern) {
		if p < len(pattern) {
			pc, pw := utf8.DecodeRuneInString(pattern[p:])
			switch {
			case pc == '%':
				px, sx = p, i
				p += pw
				continue
			case i < len(s):
				sc, sw := utf8.DecodeRuneInString(s[i:])
				if pc == '_' || foldASCII(pc) == foldASCII(sc) {
					p += pw
					i += sw
					continue
				}
			}
		}
		if px < 0 || sx >= len(s) {
			return false
		}
		_, sw := utf8.DecodeRuneInString(s[sx:])
		sx += sw
		p, i = px, sx
	}
	return true
}

// foldASCII lowercases ASCII letters and returns every other rune unchanged,
// as SQLite's LIKE does without the ICU extension.
func foldASCII(r rune) rune {
	if 'A' <= r && r <= 'Z' {
		return r + 'a' - 'A'
	}
	return r
}
