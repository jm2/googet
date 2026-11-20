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
	"fmt"
	"slices"
	"strings"
	"testing"
	"unicode"

	"github.com/google/googet/v2/client"
	"github.com/google/googet/v2/goolib"
	"github.com/google/googet/v2/internal/relation"
)

// isValidInstalledToken reports whether s can appear as an installed package
// name, provider name, or architecture segment without violating the package
// name and provider syntax invariant (non-empty, with no '<', '>', '=' or
// Unicode whitespace).
func isValidInstalledToken(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r == '<' || r == '>' || r == '=' || unicode.IsSpace(r) {
			return false
		}
	}
	return true
}

// hasOpRune reports whether s contains '<', '>', or '='.
func hasOpRune(s string) bool {
	return strings.ContainsAny(s, "<>=")
}

// appendUnique appends s to list if it is not already present.
func appendUnique(list []string, s string) []string {
	if !slices.Contains(list, s) {
		return append(list, s)
	}
	return list
}

// adversarialState constructs a worst-case installed package set for entry
// seeded from both the legacy PkgNameSplit parse and the operator parse exposed
// by relation.ParseRelation (which delegates operator splitting to relsyntax),
// plus the segment before the first dot. Package and provider names containing
// operator runes or whitespace are excluded because goopack and RelationProblems
// reject them as fatal syntax in Name and Provides.
func adversarialState(entry string) []goolib.PkgSpec {
	pi := goolib.PkgNameSplit(entry)
	req := relation.ParseRelation(entry)
	firstSeg, _, _ := strings.Cut(strings.TrimSpace(entry), ".")

	var names []string
	for _, cand := range []string{pi.Name, req.Name, firstSeg} {
		if isValidInstalledToken(cand) {
			names = appendUnique(names, cand)
		}
	}
	if len(names) == 0 {
		return nil
	}

	archs := slices.Clone(goolib.ValidArchs())
	for _, candArch := range []string{pi.Arch, req.Arch} {
		if isValidInstalledToken(candArch) {
			archs = appendUnique(archs, candArch)
		}
	}

	versions := []string{"0.0.0"}
	if req.Ver != "" && isValidInstalledToken(req.Ver) {
		if _, err := parseVersion34(req.Ver); err == nil {
			versions = appendUnique(versions, req.Ver)
		}
	}
	versions = appendUnique(versions, "99.0.0")
	if pi.Ver != "" && isValidInstalledToken(pi.Ver) {
		if _, err := parseVersion34(pi.Ver); err == nil {
			versions = appendUnique(versions, pi.Ver)
		}
	}
	versions = appendUnique(versions, "")

	var specs []goolib.PkgSpec
	for _, name := range names {
		for _, arch := range archs {
			for _, ver := range versions {
				specs = append(specs, goolib.PkgSpec{
					Name:    name,
					Arch:    arch,
					Version: ver,
				})
			}
		}
	}

	// Add separate provider packages offering unversioned and versioned
	// Provides entries (including the 3.4.x garbage version segment when
	// present) so each variant can match independently.
	provIdx := 0
	addProvider := func(prov string) {
		specs = append(specs, goolib.PkgSpec{
			Name:     fmt.Sprintf("adv-provider-%d", provIdx),
			Arch:     "noarch",
			Version:  "1.0.0",
			Provides: []string{prov},
		})
		provIdx++
	}
	for _, name := range names {
		addProvider(name)
		addProvider(name + "=0.0.0")
		if pi.Ver != "" && isValidInstalledToken(pi.Ver) {
			addProvider(name + "=" + pi.Ver)
		}
		if req.Ver != "" && isValidInstalledToken(req.Ver) {
			addProvider(name + "=" + req.Ver)
		}
		addProvider(name + "=99.0.0")
	}
	return specs
}

func stateFromSpecs(specs []goolib.PkgSpec) client.GooGetState {
	out := make(client.GooGetState, len(specs))
	for i := range specs {
		ps := specs[i]
		out[i] = client.PackageState{PackageSpec: &ps}
	}
	return out
}

// oldClientFuzzSeeds returns deterministic seeds covering Legacy, unspaced
// Operator, spaced Operator, and Malformed forms.
func oldClientFuzzSeeds() []string {
	return []string{
		// Legacy entries.
		"foo",
		"foo.noarch",
		"foo.x86_64.1.0.0",
		"dotted.pkg",
		"foo.noarch.notaver",
		"",
		"   ",
		// Unspaced Operator entries (allowed and rejected shapes).
		"foo<2.0.0",
		"foo<=2.0.0",
		"foo=2.0.0",
		"foo>=2.0.0",
		"foo>2.0.0",
		"foo<2",
		"foo<2.5.1@3",
		"foo<1.5.0-rc1",
		"foo=1.0.0+build5",
		" foo<2.0.0 ",
		"foo.noarch<2.0.0",
		"foo.x86_64<=2",
		"foo.noarch=2.5.1@3",
		"foo.noarch>=1.5.0-rc1",
		"foo.x86_64>2.0.0",
		"dotted.pkg<2.0.0",
		"dotted.pkg<=2",
		"dotted.pkg=2.5.1@3",
		"foo<notaver",
		"foo.noarch<notaver",
		// Spaced Operator entries.
		"foo < 2.0.0",
		"foo <= 2.0.0",
		"foo = 2.0.0",
		"foo >= 2.0.0",
		"foo > 2.0.0",
		"foo.noarch < 2.0.0",
		"dotted.pkg < 2.0.0",
		"foo\u00a0<\u00a02.0.0",
		"foo\u0085<2.0.0",
		// Malformed entries containing operator runes.
		"foo<",
		"<2",
		"=",
		"<",
		"foo==2",
		"foo=<2",
		"foo=>2",
		"foo<>2",
		"foo<2<3",
		"a.b.c<2",
		"foo bar<2",
		"foo.<2",
		".noarch<2",
		"foo .noarch<2",
		"foo.noarch=1=2",
	}
}

// checkOldClientFailOpen asserts the old-client fail-open invariant for a
// single relation entry string.
func checkOldClientFailOpen(t *testing.T, entry string) {
	t.Helper()
	var o34 oracle34
	var o30 oracle30

	specs := adversarialState(entry)
	req := relation.ParseRelation(entry)
	if req.Op != relation.OpMin && isValidInstalledToken(req.Name) {
		hasTargetRow := slices.ContainsFunc(specs, func(ps goolib.PkgSpec) bool {
			return ps.Name == req.Name
		})
		if !hasTargetRow {
			t.Fatalf("adversarialState(%q) omitted operator target %q", entry, req.Name)
		}
	}

	st := stateFromSpecs(specs)
	fetcher := snapshotFetcher{state: st}

	psC := &goolib.PkgSpec{Name: "holder", Arch: "noarch", Version: "1.0.0", Conflicts: []string{entry}}
	psR := &goolib.PkgSpec{Name: "holder", Arch: "noarch", Version: "1.0.0", Replaces: []string{entry}}
	cProbs := psC.RelationProblems()
	rProbs := psR.RelationProblems()

	if !hasOpRune(entry) {
		if req.Op != relation.OpMin || req.PackageInfo() != goolib.PkgNameSplit(entry) {
			t.Fatalf("legacy ParseRelation(%q) = %+v, want OpMin %+v", entry, req, goolib.PkgNameSplit(entry))
		}
		// Clean legacy entries must never cause a version comparison error on
		// valid installed package versions, though empty-version defensive rows
		// still parse as 0.0.0 in compare34.
		if len(cProbs) == 0 {
			if got := o34.checkConflictsEntry(entry, fetcher); got == oldOutcomeError {
				t.Fatalf("clean legacy Conflicts(%q) caused oracle34 error", entry)
			}
			if got := o30.checkConflictsEntry(entry, st); got == oldOutcomeError {
				t.Fatalf("clean legacy Conflicts(%q) caused oracle30 error", entry)
			}
		}
		if len(rProbs) == 0 {
			if got := o34.checkReplacesEntry(entry, fetcher); got == oldOutcomeError {
				t.Fatalf("clean legacy Replaces(%q) caused oracle34 error", entry)
			}
			if got := o30.checkReplacesEntry(entry, st); got == oldOutcomeError {
				t.Fatalf("clean legacy Replaces(%q) caused oracle30 error", entry)
			}
		}
		return
	}

	if !hasFatalRelationSyntax(cProbs) {
		if got := o34.checkConflictsEntry(entry, fetcher); !got.isFailOpen() {
			t.Fatalf("non-fatal operator Conflicts(%q) (probs=%v): oracle34=%s, want fail-open", entry, cProbs, got)
		}
		if got := o30.checkConflictsEntry(entry, st); !got.isFailOpen() {
			t.Fatalf("non-fatal operator Conflicts(%q) (probs=%v): oracle30=%s, want fail-open", entry, cProbs, got)
		}
	}
	if !hasFatalRelationSyntax(rProbs) {
		if got := o34.checkReplacesEntry(entry, fetcher); !got.isFailOpen() {
			t.Fatalf("non-fatal operator Replaces(%q) (probs=%v): oracle34=%s, want fail-open", entry, rProbs, got)
		}
		if got := o30.checkReplacesEntry(entry, st); !got.isFailOpen() {
			t.Fatalf("non-fatal operator Replaces(%q) (probs=%v): oracle30=%s, want fail-open", entry, rProbs, got)
		}
	}
}

// TestAdversarialStateMinimumRowsAndVacuityGuard checks that adversarialState
// populates rows and providers from both parses (so arch-less operator inputs
// are non-empty) and that every arch-qualified or dotted operator seed reaches
// oldOutcomeBlock on oracle34 when evaluated directly.
func TestAdversarialStateMinimumRowsAndVacuityGuard(t *testing.T) {
	hasSpec := func(specs []goolib.PkgSpec, name, arch, ver string) bool {
		return slices.ContainsFunc(specs, func(ps goolib.PkgSpec) bool {
			return ps.Name == name && ps.Arch == arch && ps.Version == ver
		})
	}
	hasProvide := func(specs []goolib.PkgSpec, prov string) bool {
		return slices.ContainsFunc(specs, func(ps goolib.PkgSpec) bool {
			return slices.Contains(ps.Provides, prov)
		})
	}

	for _, tc := range []struct {
		name          string
		entry         string
		wantNames     []string
		wantArchs     []string
		wantVers      []string
		wantProvides  []string
		want34Outcome oldClientOutcome
		want30Outcome oldClientOutcome
	}{
		{
			name:          "archless_operator_seeds_from_relation_parse",
			entry:         "foo<2.0.0",
			wantNames:     []string{"foo"},
			wantArchs:     []string{"noarch", "x86_64"},
			wantVers:      []string{"0.0.0", "2.0.0", "99.0.0", "0", ""},
			wantProvides:  []string{"foo", "foo=0.0.0", "foo=0", "foo=2.0.0", "foo=99.0.0"},
			want34Outcome: oldOutcomePass,
			want30Outcome: oldOutcomePass,
		},
		{
			name:          "arch_qualified_operator_reaches_block",
			entry:         "foo.noarch<2.5.1@3",
			wantNames:     []string{"foo"},
			wantArchs:     []string{"noarch", "x86_64"},
			wantVers:      []string{"0.0.0", "2.5.1@3", "99.0.0", "5.1@3", ""},
			wantProvides:  []string{"foo", "foo=0.0.0", "foo=5.1@3", "foo=2.5.1@3", "foo=99.0.0"},
			want34Outcome: oldOutcomeBlock,
			want30Outcome: oldOutcomePass,
		},
		{
			name:          "dotted_operator_seeds_custom_arch_and_reaches_block",
			entry:         "dotted.pkg<2.0.0",
			wantNames:     []string{"dotted"},
			wantArchs:     []string{"noarch", "x86_64", "pkg"},
			wantVers:      []string{"0.0.0", "2.0.0", "99.0.0", "0.0", ""},
			wantProvides:  []string{"dotted", "dotted=0.0.0", "dotted=0.0", "dotted=2.0.0", "dotted=99.0.0"},
			want34Outcome: oldOutcomeBlock,
			want30Outcome: oldOutcomePass,
		},
		{
			name:          "two_dot_malformed_blocks_on_34_and_errors_on_30",
			entry:         "a.b.c<2.0.0",
			wantNames:     []string{"a"},
			wantArchs:     []string{"noarch", "b"},
			wantVers:      []string{"0.0.0", "99.0.0", ""},
			wantProvides:  []string{"a", "a=0.0.0", "a=99.0.0"},
			want34Outcome: oldOutcomeBlock,
			want30Outcome: oldOutcomeError,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			specs := adversarialState(tc.entry)
			if len(specs) == 0 {
				t.Fatalf("adversarialState(%q) returned empty slice", tc.entry)
			}
			for _, n := range tc.wantNames {
				for _, a := range tc.wantArchs {
					for _, v := range tc.wantVers {
						if !hasSpec(specs, n, a, v) {
							t.Errorf("adversarialState(%q) missing spec (%q, %q, %q)", tc.entry, n, a, v)
						}
					}
				}
			}
			for _, prov := range tc.wantProvides {
				if !hasProvide(specs, prov) {
					t.Errorf("adversarialState(%q) missing provider %q", tc.entry, prov)
				}
			}
			st := stateFromSpecs(specs)
			var o34 oracle34
			if got := o34.checkConflictsEntry(tc.entry, snapshotFetcher{state: st}); got != tc.want34Outcome {
				t.Errorf("oracle34.checkConflictsEntry(%q) = %s, want %s", tc.entry, got, tc.want34Outcome)
			}
			var o30 oracle30
			if got := o30.checkConflictsEntry(tc.entry, st); got != tc.want30Outcome {
				t.Errorf("oracle30.checkConflictsEntry(%q) = %s, want %s", tc.entry, got, tc.want30Outcome)
			}
		})
	}

	// Vacuity guard across every seed: every operator seed yields a non-empty
	// state, and every arch-qualified or dotted operator seed reaches block on
	// oracle34 so mutating away the fatal publishing check cannot pass silently.
	var o34 oracle34
	var checkedHazards int
	for _, seed := range oldClientFuzzSeeds() {
		req := relation.ParseRelation(seed)
		if req.Op == relation.OpMin {
			continue
		}
		specs := adversarialState(seed)
		if len(specs) == 0 {
			t.Fatalf("vacuity guard: operator seed %q produced empty adversarialState", seed)
		}
		if req.Arch != "" {
			checkedHazards++
			got := o34.checkConflictsEntry(seed, snapshotFetcher{state: stateFromSpecs(specs)})
			if got != oldOutcomeBlock {
				t.Fatalf("vacuity guard: arch/dotted operator seed %q yielded %s on oracle34, want block", seed, got)
			}
		}
	}
	if checkedHazards == 0 {
		t.Fatal("vacuity guard checked zero arch/dotted operator seeds")
	}
}

// TestFuzzOldClientFailOpenSeedCoverage verifies that oldClientFuzzSeeds hits
// Legacy, unspaced Operator, spaced Operator, and Malformed branches and passes
// checkOldClientFailOpen on every seed.
func TestFuzzOldClientFailOpenSeedCoverage(t *testing.T) {
	var sawLegacy, sawUnspacedOp, sawSpacedOp, sawMalformed bool
	for _, seed := range oldClientFuzzSeeds() {
		req := relation.ParseRelation(seed)
		switch {
		case !hasOpRune(seed):
			sawLegacy = true
		case req.Op == relation.OpMin:
			sawMalformed = true
		case req.Entry() == strings.TrimSpace(seed):
			sawUnspacedOp = true
		default:
			sawSpacedOp = true
		}
		checkOldClientFailOpen(t, seed)
	}
	if !sawLegacy || !sawUnspacedOp || !sawSpacedOp || !sawMalformed {
		t.Fatalf("seed coverage incomplete: legacy=%t unspaced=%t spaced=%t malformed=%t",
			sawLegacy, sawUnspacedOp, sawSpacedOp, sawMalformed)
	}
}

// FuzzOldClientFailOpen fuzzes arbitrary relation strings to verify that any
// operator-bearing entry accepted without a fatal ErrRelationSyntax error fails
// open on both GooGet 3.4.x and v3.0.0 against adversarialState.
func FuzzOldClientFailOpen(f *testing.F) {
	for _, seed := range oldClientFuzzSeeds() {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, entry string) {
		checkOldClientFailOpen(t, entry)
	})
}
