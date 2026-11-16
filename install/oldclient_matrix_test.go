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

package install

import (
	"slices"
	"strings"
	"testing"

	"github.com/google/googet/v2/client"
	"github.com/google/googet/v2/goolib"
	"github.com/google/googet/v2/internal/relation"
)

// oldClientMatrixCell describes one operator relation string in the 3.4.x and
// v3.0.0 backward-compatibility matrix.
type oldClientMatrixCell struct {
	lhs   string
	op    string
	wsL   string
	wsR   string
	ver   string
	entry string
}

// oldClientMatrixCells returns the 320 operator entries across left-hand side,
// operator, whitespace variant, and version form.
func oldClientMatrixCells() []oldClientMatrixCell {
	lhsForms := []string{"foo", "foo.noarch", "foo.x86_64", "dotted.pkg"}
	ops := []string{"<", "<=", "=", ">=", ">"}
	wsForms := [][2]string{{"", ""}, {" ", " "}, {" ", ""}, {"", " "}}
	versions := []string{"2.0.0", "2", "2.5.1@3", "1.5.0-rc1"}

	cells := make([]oldClientMatrixCell, 0, len(lhsForms)*len(ops)*len(wsForms)*len(versions))
	for _, lhs := range lhsForms {
		for _, op := range ops {
			for _, ws := range wsForms {
				for _, ver := range versions {
					cells = append(cells, oldClientMatrixCell{
						lhs:   lhs,
						op:    op,
						wsL:   ws[0],
						wsR:   ws[1],
						ver:   ver,
						entry: lhs + ws[0] + op + ws[1] + ver,
					})
				}
			}
		}
	}
	return cells
}

// oldClientNamedState pairs a descriptive name with an installed package state.
type oldClientNamedState struct {
	name  string
	state client.GooGetState
}

func stateRow(name, arch, ver string, provs ...string) client.PackageState {
	return client.PackageState{
		PackageSpec: &goolib.PkgSpec{
			Name:     name,
			Arch:     arch,
			Version:  ver,
			Provides: provs,
		},
	}
}

// oldClientMatrixStates builds installed package states seeded from both the
// legacy PkgNameSplit identity and the operator-parsed identity for cell.
func oldClientMatrixStates(cell oldClientMatrixCell) []oldClientNamedState {
	out := []oldClientNamedState{
		{name: "empty", state: nil},
		{name: "target_noarch_001", state: client.GooGetState{stateRow("foo", "noarch", "0.0.1")}},
		{name: "target_noarch_1", state: client.GooGetState{stateRow("foo", "noarch", "1.0.0")}},
		{name: "target_noarch_exact", state: client.GooGetState{stateRow("foo", "noarch", cell.ver)}},
		{name: "target_x64_exact", state: client.GooGetState{stateRow("foo", "x86_64", cell.ver)}},
		{name: "target_x64_3", state: client.GooGetState{stateRow("foo", "x86_64", "3.0.0")}},
		{name: "multiarch_foo", state: client.GooGetState{
			stateRow("foo", "noarch", "0.0.1"),
			stateRow("foo", "x86_64", "3.0.0"),
		}},
		{name: "unver_provider_foo", state: client.GooGetState{stateRow("bar", "noarch", "1.0.0", "foo")}},
		{name: "ver_provider_foo_001", state: client.GooGetState{stateRow("bar", "noarch", "1.0.0", "foo=0.0.1")}},
		{name: "ver_provider_foo_1", state: client.GooGetState{stateRow("bar", "noarch", "1.0.0", "foo=1.0.0")}},
		{name: "ver_provider_foo_3", state: client.GooGetState{stateRow("bar", "noarch", "1.0.0", "foo=3.0.0")}},
		{name: "unrelated", state: client.GooGetState{stateRow("baz", "noarch", "1.0.0")}},
		{name: "dotted_whole_target", state: client.GooGetState{stateRow("dotted.pkg", "noarch", "1.0.0")}},
		{name: "dotted_first_seg_unver_prov", state: client.GooGetState{stateRow("bar", "noarch", "1.0.0", "dotted")}},
		{name: "dotted_first_seg_ver_prov_1", state: client.GooGetState{stateRow("bar", "noarch", "1.0.0", "dotted=1.0.0")}},
		{name: "dotted_split_arch_low", state: client.GooGetState{stateRow("dotted", "pkg", "0.0.1")}},
		{name: "dotted_split_arch_exact", state: client.GooGetState{stateRow("dotted", "pkg", cell.ver)}},
		{name: "dotted_split_arch_high", state: client.GooGetState{stateRow("dotted", "pkg", "3.0.0")}},
	}

	// Also seed explicit rows from both PkgNameSplit and ParseRelation when
	// their names contain no forbidden characters, ensuring neither identity
	// can go untested.
	pi := goolib.PkgNameSplit(cell.entry)
	if pi.Name != "" && !strings.ContainsAny(pi.Name, "<>= \t\r\n") {
		var rows client.GooGetState
		for _, arch := range goolib.ValidArchs() {
			rows = append(rows, stateRow(pi.Name, arch, "1.0.0"))
		}
		rows = append(rows, stateRow("prov-split", "noarch", "1.0.0", pi.Name, pi.Name+"=1.0.0", pi.Name+"=3.0.0"))
		out = append(out, oldClientNamedState{name: "seeded_split_identity", state: rows})
	}
	req := relation.ParseRelation(cell.entry)
	if req.Name != "" && !strings.ContainsAny(req.Name, "<>= \t\r\n") {
		var rows client.GooGetState
		for _, arch := range goolib.ValidArchs() {
			rows = append(rows, stateRow(req.Name, arch, "1.0.0"))
		}
		if req.Arch != "" && !slices.Contains(goolib.ValidArchs(), req.Arch) {
			rows = append(rows, stateRow(req.Name, req.Arch, "1.0.0"))
		}
		rows = append(rows, stateRow("prov-rel", "noarch", "1.0.0", req.Name, req.Name+"=1.0.0", req.Name+"=3.0.0"))
		out = append(out, oldClientNamedState{name: "seeded_relation_identity", state: rows})
	}
	return out
}

// TestOldClientBackwardCompatibilityMatrix verifies 3.4.x (oracle34), v3.0.0
// (oracle30), and current-client evaluation across every combination of field,
// left-hand side, operator, whitespace, version form, and installed state.
func TestOldClientBackwardCompatibilityMatrix(t *testing.T) {
	var o34 oracle34
	var o30 oracle30
	incoming := &goolib.PkgSpec{Name: "incoming", Arch: "noarch", Version: "1.0.0"}

	var sawHazardBlock bool
	for _, cell := range oldClientMatrixCells() {
		hasDotInLHS := strings.Contains(cell.lhs, ".")
		pi := goolib.PkgNameSplit(cell.entry)
		req := relation.ParseRelation(cell.entry)
		if req.Op == relation.OpMin {
			t.Fatalf("ParseRelation(%q) returned OpMin, want operator", cell.entry)
		}

		for _, ns := range oldClientMatrixStates(cell) {
			fetcher := snapshotFetcher{state: ns.state}

			// Replaces is a no-op on both 3.4.x and v3.0.0 for every operator cell.
			if got := o34.checkReplacesEntry(cell.entry, fetcher); got != oldOutcomeNoop {
				t.Fatalf("oracle34 Replaces(%q) on %s = %s, want noop", cell.entry, ns.name, got)
			}
			if got := o30.checkReplacesEntry(cell.entry, ns.state); got != oldOutcomeNoop {
				t.Fatalf("oracle30 Replaces(%q) on %s = %s, want noop", cell.entry, ns.name, got)
			}

			// v3.0.0 has no Provides loop, so every operator Conflicts cell passes.
			if got := o30.checkConflictsEntry(cell.entry, ns.state); got != oldOutcomePass {
				t.Fatalf("oracle30 Conflicts(%q) on %s = %s, want pass", cell.entry, ns.name, got)
			}

			// 3.4.x Conflicts fails open on every arch-less, non-dotted cell and
			// blocks on dotted/arch-qualified cells exactly when an installed
			// package provides the segment before the first dot at >= pi.Ver.
			got34 := o34.checkConflictsEntry(cell.entry, fetcher)
			if got34 == oldOutcomeError {
				t.Fatalf("oracle34 Conflicts(%q) on %s returned unexpected error", cell.entry, ns.name)
			}
			wantBlock34 := false
			if hasDotInLHS {
				for _, p := range ns.state {
					for _, prov := range p.PackageSpec.Provides {
						if o34.satisfiesProvider(prov, pi.Name, pi.Ver) {
							wantBlock34 = true
						}
					}
				}
			}
			want34 := oldOutcomePass
			if wantBlock34 {
				want34 = oldOutcomeBlock
				sawHazardBlock = true
			}
			if got34 != want34 {
				t.Fatalf("oracle34 Conflicts(%q) on %s = %s, want %s (split=%+v)", cell.entry, ns.name, got34, want34, pi)
			}
			holderConflicts := &goolib.PkgSpec{Name: "incoming", Arch: "noarch", Version: "1.0.0", Conflicts: []string{cell.entry}}
			if err := o34.resolveConflicts(holderConflicts, fetcher); (err != nil) != wantBlock34 {
				t.Fatalf("oracle34.resolveConflicts(%q) on %s error = %v, wantBlock=%t", cell.entry, ns.name, err, wantBlock34)
			}
			if err := o30.resolveConflicts(holderConflicts, ns.state); err != nil {
				t.Fatalf("oracle30.resolveConflicts(%q) on %s = %v, want nil", cell.entry, ns.name, err)
			}

			// Verify specific known hazard cells explicitly.
			if hasDotInLHS && strings.HasPrefix(cell.lhs, "foo.") &&
				(ns.name == "unver_provider_foo" || ns.name == "ver_provider_foo_1" || ns.name == "ver_provider_foo_3") &&
				got34 != oldOutcomeBlock {
				t.Fatalf("expected 3.4.x hazard block for %q on %s, got %s", cell.entry, ns.name, got34)
			}
			if cell.lhs == "dotted.pkg" &&
				(ns.name == "dotted_first_seg_unver_prov" || ns.name == "dotted_first_seg_ver_prov_1") &&
				got34 != oldOutcomeBlock {
				t.Fatalf("expected 3.4.x dotted hazard block for %q on %s, got %s", cell.entry, ns.name, got34)
			}

			// Current client evaluation agrees with relation.Requirement on every cell.
			specs := specsFromState(ns.state)
			gotFwd, _, err := checkForwardConflict(req, specs, selfExclusion(incoming))
			if err != nil {
				t.Fatalf("checkForwardConflict(%q) on %s unexpected error: %v", cell.entry, ns.name, err)
			}
			wantFwd := false
			for _, ps := range specs {
				okID, err := req.MinInstalled(ps.Name, ps.Arch, ps.Version)
				if err != nil {
					t.Fatalf("MinInstalled(%q, %s) unexpected error: %v", cell.entry, ps, err)
				}
				okProv, _ := relation.ProvidedBy(ps, req)
				if okID || okProv {
					wantFwd = true
				}
			}
			if gotFwd != wantFwd {
				t.Fatalf("checkForwardConflict(%q) on %s = %t, want %t", cell.entry, ns.name, gotFwd, wantFwd)
			}

			holderReplaces := &goolib.PkgSpec{Name: "incoming", Arch: "noarch", Version: "1.0.0", Replaces: []string{cell.entry}}
			gotVictims, err := victimsIn(holderReplaces, ns.state)
			if err != nil {
				t.Fatalf("victimsIn(%q) on %s unexpected error: %v", cell.entry, ns.name, err)
			}
			wantVictims, err := relation.Resolve(req, specs)
			if err != nil {
				t.Fatalf("relation.Resolve(%q) on %s unexpected error: %v", cell.entry, ns.name, err)
			}
			if len(gotVictims) != len(wantVictims) {
				t.Fatalf("victimsIn(%q) on %s returned %d rows, want %d", cell.entry, ns.name, len(gotVictims), len(wantVictims))
			}
			for i := range gotVictims {
				if gotVictims[i].PackageSpec.String() != wantVictims[i].String() {
					t.Fatalf("victimsIn(%q)[%d] = %s, want %s", cell.entry, i, gotVictims[i].PackageSpec, wantVictims[i])
				}
			}
		}
	}
	if !sawHazardBlock {
		t.Fatal("matrix failed to exercise any 3.4.x fail-closed hazard cell")
	}
}

// TestOldClientMatrixAgainstSQLite runs oracle34 directly against real
// googetdb.GooDB FetchPkg and FetchPkgs queries via newRelFixture and verifies
// agreement with snapshotFetcher across all 320 operator matrix entries.
func TestOldClientMatrixAgainstSQLite(t *testing.T) {
	installedWithoutProviders := []goolib.PkgSpec{
		{Name: "foo", Arch: "noarch", Version: "1.0.0"},
		{Name: "foo", Arch: "x86_64", Version: "3.0.0"},
		{Name: "dotted.pkg", Arch: "noarch", Version: "1.0.0"},
		{Name: "baz", Arch: "noarch", Version: "1.0.0"},
	}
	installedWithProviders := append(slices.Clone(installedWithoutProviders),
		goolib.PkgSpec{Name: "prov-foo", Arch: "noarch", Version: "1.0.0", Provides: []string{"foo=1.0.0"}},
		goolib.PkgSpec{Name: "prov-dotted", Arch: "noarch", Version: "1.0.0", Provides: []string{"dotted"}},
	)

	var o34 oracle34
	for _, tc := range []struct {
		name      string
		installed []goolib.PkgSpec
		wantDotC  oldClientOutcome
	}{
		{name: "without_providers", installed: installedWithoutProviders, wantDotC: oldOutcomePass},
		{name: "with_providers", installed: installedWithProviders, wantDotC: oldOutcomeBlock},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRelFixture(t, tc.installed, nil)
			dbState, err := f.db.FetchPkgs("")
			if err != nil {
				t.Fatalf("FetchPkgs failed: %v", err)
			}
			snap := snapshotFetcher{state: dbState}

			for _, cell := range oldClientMatrixCells() {
				gotDBC := o34.checkConflictsEntry(cell.entry, f.db)
				gotSnapC := o34.checkConflictsEntry(cell.entry, snap)
				if gotDBC != gotSnapC {
					t.Fatalf("Conflicts(%q): GooDB=%s != snapshot=%s", cell.entry, gotDBC, gotSnapC)
				}
				wantC := oldOutcomePass
				if strings.Contains(cell.lhs, ".") {
					wantC = tc.wantDotC
				}
				if gotDBC != wantC {
					t.Fatalf("Conflicts(%q) against GooDB = %s, want %s", cell.entry, gotDBC, wantC)
				}

				gotDBR := o34.checkReplacesEntry(cell.entry, f.db)
				gotSnapR := o34.checkReplacesEntry(cell.entry, snap)
				if gotDBR != gotSnapR || gotDBR != oldOutcomeNoop {
					t.Fatalf("Replaces(%q): GooDB=%s snapshot=%s, want both noop", cell.entry, gotDBR, gotSnapR)
				}
			}
		})
	}
}
