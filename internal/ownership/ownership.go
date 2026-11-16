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

// Package ownership indexes installed file and directory paths across GooGet
// state rows under platform path-equivalence rules.
//
// Still-owned rule: a path is still owned whenever any installed row other
// than the row being removed or replaced lists a path with the same ownership
// key, regardless of checksum (including empty-checksum directory entries and
// legacy db_only entries). Still-owned paths are never deleted by uninstall or
// upgrade cleanup.
//
// On Windows, NTFS compares file names case-insensitively through its volume
// $UpCase table. Key cleans paths and FoldWindows normalizes forward slashes
// to backslashes and upper-cases each rune with unicode.ToUpper, which is the
// closest standard-library match to NTFS upcasing without expanding runes such
// as 'ß' or the Kelvin sign ('K').
//
// Scope limits: Key normalizes lexical path cleaning, separator direction on
// Windows, and per-rune upper-casing on Windows only. It does not resolve DOS
// 8.3 short names, NTFS junctions or symbolic links, drive-relative paths
// (such as "\foo" or "C:foo" versus "C:\foo"), trailing dots or spaces
// stripped by Win32 path parsing, or case-insensitive APFS volumes on macOS.
package ownership

import (
	"cmp"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"unicode"

	"github.com/google/googet/v2/client"
)

// FoldWindows applies Windows' separator and case rule to p: '/' becomes '\'
// and each rune is upper-cased with unicode.ToUpper. It performs no path
// cleaning.
func FoldWindows(p string) string {
	return strings.Map(func(r rune) rune {
		if r == '/' {
			return '\\'
		}
		return unicode.ToUpper(r)
	}, p)
}

// Key returns the canonical ownership key for p on the current operating
// system.
func Key(p string) string {
	cleaned := filepath.Clean(p)
	if runtime.GOOS == "windows" {
		return FoldWindows(cleaned)
	}
	return cleaned
}

// RowID identifies one installed package row by name and architecture; the
// zero value identifies no row.
type RowID struct {
	Name string
	Arch string
}

// String formats r as "Name.Arch".
func (r RowID) String() string {
	return r.Name + "." + r.Arch
}

// Owner records one installed row's entry for a path.
type Owner struct {
	Row      RowID
	Path     string
	Checksum string
}

// KeyRule defines the path-folding function and path separator used by an
// Index.
type KeyRule struct {
	Fold func(string) string
	Sep  byte
}

// WindowsRule is the Windows case-folding and backslash-separator rule,
// exposed so cross-platform unit tests can exercise Windows ownership
// semantics on any host OS. Unlike Key on Windows, WindowsRule does not run
// filepath.Clean, so cross-platform test inputs should omit uncleaned segments
// or trailing separators.
var WindowsRule = KeyRule{Fold: FoldWindows, Sep: '\\'}

func defaultRule() KeyRule {
	return KeyRule{Fold: Key, Sep: os.PathSeparator}
}

func normalizeRule(rule KeyRule) KeyRule {
	if rule.Fold == nil {
		rule.Fold = Key
	}
	if rule.Sep == 0 {
		rule.Sep = os.PathSeparator
	}
	return rule
}

// Index is an opaque ownership lookup table over a GooGetState snapshot.
// Callers never index raw maps directly; every lookup applies the index's
// KeyRule.
type Index struct {
	rule KeyRule
	m    map[string][]Owner
}

// NewIndex indexes every InstalledFiles entry of every row in st except skip,
// using Key. Rows with a nil PackageSpec are ignored, and owners for each path
// are stored in ascending (Name, Arch) order.
func NewIndex(st client.GooGetState, skip RowID) *Index {
	return NewIndexWithRule(st, skip, defaultRule())
}

// NewIndexWithRule builds an Index like NewIndex using rule instead of the
// host OS default. It is intended for cross-platform tests.
func NewIndexWithRule(st client.GooGetState, skip RowID, rule KeyRule) *Index {
	rule = normalizeRule(rule)
	var rows []client.PackageState
	for _, ps := range st {
		if ps.PackageSpec == nil {
			continue
		}
		if skip != (RowID{}) && ps.PackageSpec.Name == skip.Name && ps.PackageSpec.Arch == skip.Arch {
			continue
		}
		rows = append(rows, ps)
	}
	slices.SortFunc(rows, func(a, b client.PackageState) int {
		return cmp.Or(
			strings.Compare(a.PackageSpec.Name, b.PackageSpec.Name),
			strings.Compare(a.PackageSpec.Arch, b.PackageSpec.Arch),
		)
	})

	m := make(map[string][]Owner)
	for _, ps := range rows {
		id := RowID{Name: ps.PackageSpec.Name, Arch: ps.PackageSpec.Arch}
		paths := make([]string, 0, len(ps.InstalledFiles))
		for p := range ps.InstalledFiles {
			paths = append(paths, p)
		}
		slices.Sort(paths)
		for _, p := range paths {
			k := rule.Fold(p)
			m[k] = append(m[k], Owner{
				Row:      id,
				Path:     p,
				Checksum: ps.InstalledFiles[p],
			})
		}
	}
	return &Index{rule: rule, m: m}
}

func (ix *Index) key(path string) string {
	if ix == nil {
		return Key(path)
	}
	return ix.rule.Fold(path)
}

// Owners returns every indexed Owner whose entry matches path under ix's rule,
// in ascending (Name, Arch) order.
func (ix *Index) Owners(path string) []Owner {
	if ix == nil {
		return nil
	}
	return slices.Clone(ix.m[ix.rule.Fold(path)])
}

// PathSet holds a set of paths normalized under an Index's KeyRule.
type PathSet struct {
	fold func(string) string
	set  map[string]struct{}
}

// KeySet builds a PathSet containing the keys of m normalized under ix's rule.
func (ix *Index) KeySet(m map[string]string) PathSet {
	fold := Key
	if ix != nil {
		fold = ix.rule.Fold
	}
	if len(m) == 0 {
		return PathSet{fold: fold}
	}
	set := make(map[string]struct{}, len(m))
	for p := range m {
		set[fold(p)] = struct{}{}
	}
	return PathSet{fold: fold, set: set}
}

// Add inserts path into s under s's KeyRule and reports whether path's key was
// newly added.
func (s *PathSet) Add(path string) bool {
	fold := s.fold
	if fold == nil {
		fold = Key
	}
	if s.set == nil {
		s.set = make(map[string]struct{})
	}
	k := fold(path)
	if _, exists := s.set[k]; exists {
		return false
	}
	s.set[k] = struct{}{}
	return true
}

// Has reports whether path belongs to s under s's KeyRule.
func (s PathSet) Has(path string) bool {
	if len(s.set) == 0 {
		return false
	}
	fold := s.fold
	if fold == nil {
		fold = Key
	}
	_, ok := s.set[fold(path)]
	return ok
}

func (ix *Index) sep() byte {
	if ix == nil || ix.rule.Sep == 0 {
		return os.PathSeparator
	}
	return ix.rule.Sep
}

// Kept describes one path skipped during deletion because one or more other
// installed rows still list it.
type Kept struct {
	Path   string
	Owners []RowID
}

// DeletePlan partitions candidate deletion entries into unowned files, unowned
// directories ordered deepest-first, and entries kept because another row owns
// them.
type DeletePlan struct {
	Files []string
	Dirs  []string
	Kept  []Kept
}

// PlanDelete classifies and orders entries (path -> checksum) against ix.
// Entries still owned by ix go to Kept; unowned entries with a non-empty
// checksum go to Files sorted by key; unowned entries with an empty checksum
// go to Dirs sorted by key separator depth descending and then key descending
// so every descendant precedes its ancestor regardless of byte case.
func PlanDelete(entries map[string]string, ix *Index) DeletePlan {
	if len(entries) == 0 {
		return DeletePlan{}
	}
	rawPaths := make([]string, 0, len(entries))
	for p := range entries {
		rawPaths = append(rawPaths, p)
	}
	slices.Sort(rawPaths)

	var plan DeletePlan
	for _, p := range rawPaths {
		if owners := ix.Owners(p); len(owners) > 0 {
			var ids []RowID
			for _, o := range owners {
				if len(ids) == 0 || ids[len(ids)-1] != o.Row {
					ids = append(ids, o.Row)
				}
			}
			plan.Kept = append(plan.Kept, Kept{Path: p, Owners: ids})
			continue
		}
		if entries[p] != "" {
			plan.Files = append(plan.Files, p)
		} else {
			plan.Dirs = append(plan.Dirs, p)
		}
	}

	slices.SortFunc(plan.Files, func(a, b string) int {
		return cmp.Or(strings.Compare(ix.key(a), ix.key(b)), strings.Compare(a, b))
	})
	sepStr := string(ix.sep())
	slices.SortFunc(plan.Dirs, func(a, b string) int {
		ka, kb := ix.key(a), ix.key(b)
		da, db := strings.Count(ka, sepStr), strings.Count(kb, sepStr)
		return cmp.Or(cmp.Compare(db, da), strings.Compare(kb, ka), strings.Compare(b, a))
	})
	slices.SortFunc(plan.Kept, func(a, b Kept) int {
		return cmp.Or(strings.Compare(ix.key(a.Path), ix.key(b.Path)), strings.Compare(a.Path, b.Path))
	})
	return plan
}

// keptLine formats a structured informational log line for a skipped deletion
// entry.
func keptLine(op, pkg string, k Kept) string {
	parts := make([]string, len(k.Owners))
	for i, o := range k.Owners {
		parts[i] = o.String()
	}
	return fmt.Sprintf("file_kept op=%s pkg=%s path=%q owners=%s", op, pkg, k.Path, strings.Join(parts, ","))
}

// Logger receives informational and error log lines during Execute.
type Logger struct {
	Infof  func(format string, args ...any)
	Errorf func(format string, args ...any)
}

// Execute runs p using rm (client.RemoveOrRename in production) and logs via
// lg for operation op ("remove" or "upgrade") on package pkg.
func Execute(p DeletePlan, op, pkg string, rm func(string) (string, error), lg Logger) {
	infof := lg.Infof
	if infof == nil {
		infof = func(string, ...any) {}
	}
	errorf := lg.Errorf
	if errorf == nil {
		errorf = func(string, ...any) {}
	}

	for _, k := range p.Kept {
		infof("%s", keptLine(op, pkg, k))
	}
	for _, file := range p.Files {
		if op == "upgrade" {
			infof("Cleaning up old file %q", file)
		} else {
			infof("Removing %q", file)
		}
		if _, err := rm(file); err != nil {
			errorf("%v", err)
		}
	}
	for _, dir := range p.Dirs {
		if op == "remove" {
			infof("Removing %q", dir)
		}
		if _, err := rm(dir); err != nil {
			infof("%v", err)
		}
	}
}
