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
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/googet/v2/client"
	"github.com/google/googet/v2/goolib"
	"github.com/google/googet/v2/internal/relation"
	"github.com/google/googet/v2/testutil"
)

// TestProvidesRequirementOperators checks that forward provider matching
// honors version operators through relation.ProvidedBy without emitting log
// lines on unparseable Provides versions, while preserving legacy
// client.SatisfiesProvider logging for OpMin requirements.
func TestProvidesRequirementOperators(t *testing.T) {
	for _, tc := range []struct {
		name     string
		req      relation.Requirement
		provides []string
		want     bool
		wantLog  bool
	}{
		{
			name:     "lt_unversioned_provider_matches",
			req:      relation.Requirement{Name: "foo", Op: relation.OpLT, Ver: "2.0.0"},
			provides: []string{"foo"},
			want:     true,
		},
		{
			name:     "lt_in_range_provider_matches",
			req:      relation.Requirement{Name: "foo", Op: relation.OpLT, Ver: "2.0.0"},
			provides: []string{"foo=1.0.0"},
			want:     true,
		},
		{
			name:     "lt_at_bound_provider_misses",
			req:      relation.Requirement{Name: "foo", Op: relation.OpLT, Ver: "2.0.0"},
			provides: []string{"foo=2.0.0"},
			want:     false,
		},
		{
			name:     "lt_above_bound_provider_misses",
			req:      relation.Requirement{Name: "foo", Op: relation.OpLT, Ver: "2.0.0"},
			provides: []string{"foo=3.0.0"},
			want:     false,
		},
		{
			name:     "lt_unparseable_provider_misses_silently",
			req:      relation.Requirement{Name: "foo", Op: relation.OpLT, Ver: "2.0.0"},
			provides: []string{"foo=notaver"},
			want:     false,
			wantLog:  false,
		},
		{
			name:     "lt_valid_provider_after_unparseable_matches_silently",
			req:      relation.Requirement{Name: "foo", Op: relation.OpLT, Ver: "2.0.0"},
			provides: []string{"foo=notaver", "foo=1.5.0"},
			want:     true,
			wantLog:  false,
		},
		{
			name:     "gt_at_bound_provider_misses",
			req:      relation.Requirement{Name: "foo", Op: relation.OpGT, Ver: "2.0.0"},
			provides: []string{"foo=2.0.0"},
			want:     false,
		},
		{
			name:     "gt_above_bound_provider_matches",
			req:      relation.Requirement{Name: "foo", Op: relation.OpGT, Ver: "2.0.0"},
			provides: []string{"foo=2.0.1"},
			want:     true,
		},
		{
			name:     "eq_exact_matches_and_release_suffix_misses",
			req:      relation.Requirement{Name: "foo", Op: relation.OpEQ, Ver: "2.0.0"},
			provides: []string{"foo=2.0.0@1"},
			want:     false,
		},
		{
			name:     "opmin_unparseable_provider_logs_via_client",
			req:      relation.Requirement{Name: "foo", Op: relation.OpMin, Ver: "2.0.0"},
			provides: []string{"foo=notaver"},
			want:     false,
			wantLog:  true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs := testutil.CaptureLogs(t)
			ps := &goolib.PkgSpec{Name: "provider", Arch: "noarch", Version: "1.0.0", Provides: tc.provides}
			got := providesRequirement(ps, tc.req)
			if got != tc.want {
				t.Errorf("providesRequirement(%v, %+v) = %t, want %t", tc.provides, tc.req, got, tc.want)
			}
			gotLog := logs.Len() > 0
			if gotLog != tc.wantLog {
				t.Errorf("providesRequirement(%v, %+v) logged %q (non-empty=%t), want non-empty=%t", tc.provides, tc.req, logs.String(), gotLog, tc.wantLog)
			}
		})
	}
}

// TestForwardConflictHandBuiltOperatorRequirement checks checkForwardConflict
// and recheckForwardEntries with hand-built operator requirements.
func TestForwardConflictHandBuiltOperatorRequirement(t *testing.T) {
	incoming := &goolib.PkgSpec{Name: "incoming", Arch: "noarch", Version: "1.0.0"}
	req := relation.Requirement{Name: "foo", Op: relation.OpLT, Ver: "2.0.0"}

	inRange := []*goolib.PkgSpec{{Name: "holder", Arch: "noarch", Version: "1.0.0", Provides: []string{"foo=1.0.0"}}}
	hit, _, err := checkForwardConflict(req, inRange, selfExclusion(incoming))
	if err != nil || !hit {
		t.Errorf("checkForwardConflict(foo=1.0.0) = (%t, %v), want (true, nil)", hit, err)
	}

	outOfRange := []*goolib.PkgSpec{{Name: "holder", Arch: "noarch", Version: "1.0.0", Provides: []string{"foo=3.0.0"}}}
	hit, _, err = checkForwardConflict(req, outOfRange, selfExclusion(incoming))
	if err != nil || hit {
		t.Errorf("checkForwardConflict(foo=3.0.0) = (%t, %v), want (false, nil)", hit, err)
	}

	recheckErr := recheckForwardEntries([]relation.Requirement{req}, incoming, inRange, " (still installed after replacing)")
	wantErr := "cannot install, conflict with installed package or provider: foo<2.0.0 (still installed after replacing)"
	if recheckErr == nil || recheckErr.Error() != wantErr {
		t.Errorf("recheckForwardEntries error = %v, want %q", recheckErr, wantErr)
	}
	if err := recheckForwardEntries([]relation.Requirement{req}, incoming, outOfRange, ""); err != nil {
		t.Errorf("recheckForwardEntries(outOfRange) = %v, want nil", err)
	}
	legacyA := relation.Requirement{Name: "A", Op: relation.OpMin, Ver: "3"}
	installedA := []*goolib.PkgSpec{{Name: "A", Arch: "noarch", Version: "4"}}
	legacyErr := recheckForwardEntries([]relation.Requirement{legacyA}, incoming, installedA, "")
	if legacyErr == nil || !strings.HasSuffix(legacyErr.Error(), ": A") {
		t.Errorf("recheckForwardEntries(OpMin A..3) = %v, want suffix \": A\"", legacyErr)
	}
}

// TestUnversionedProvidesWithOperatorConflicts checks how unversioned Provides
// entries interact with bounded operator Conflicts in forward, reverse and
// cross-arch self-install cases, and verifies that versioned Provides entries
// avoid those blocks.
func TestUnversionedProvidesWithOperatorConflicts(t *testing.T) {
	archs := []string{"noarch", "x86_64"}

	t.Run("forward_unversioned_vs_versioned_provider", func(t *testing.T) {
		for _, conflictEntry := range []string{"P<2.0.0", "P>3.0.0"} {
			q := &goolib.PkgSpec{Name: "Q", Arch: "noarch", Version: "1.0.0", Conflicts: []string{conflictEntry}}
			rm := client.RepoMap{"repo": {Packages: []goolib.RepoSpec{{PackageSpec: q}}}}

			unversionedState := client.GooGetState{
				{PackageSpec: &goolib.PkgSpec{Name: "R", Arch: "noarch", Version: "2.0.0", Provides: []string{"P"}}},
			}
			_, err := PlanState(goolib.PackageInfo{Name: "Q"}, "repo", rm, unversionedState, archs, Options{})
			wantErr := "cannot install, conflict with installed package or provider: " + conflictEntry
			if err == nil || err.Error() != wantErr {
				t.Errorf("PlanState(%s vs unversioned Provides P) = %v, want %q", conflictEntry, err, wantErr)
			}

			versionedState := client.GooGetState{
				{PackageSpec: &goolib.PkgSpec{Name: "R", Arch: "noarch", Version: "2.0.0", Provides: []string{"P=2.0.0"}}},
			}
			if _, err := PlanState(goolib.PackageInfo{Name: "Q"}, "repo", rm, versionedState, archs, Options{}); err != nil {
				t.Errorf("PlanState(%s vs Provides P=2.0.0) = %v, want nil", conflictEntry, err)
			}
		}
	})

	t.Run("reverse_modes_unversioned_vs_versioned_provider", func(t *testing.T) {
		installedX := &goolib.PkgSpec{Name: "X", Arch: "noarch", Version: "1.0.0", Conflicts: []string{"P<2.0.0"}}
		state := client.GooGetState{{PackageSpec: installedX}}

		for _, incoming := range []*goolib.PkgSpec{
			{Name: "R", Arch: "noarch", Version: "2.0.0", Provides: []string{"P"}},
			{Name: "P", Arch: "noarch", Version: "3.0.0", Provides: []string{"P"}},
		} {
			for _, tc := range []struct {
				mode       ConflictMode
				wantErr    bool
				wantLevel  eventLevel
				wantResult string
				wantWarn   bool
			}{
				{mode: ConflictWarn, wantLevel: levelWarning, wantResult: "warned", wantWarn: true},
				{mode: ConflictError, wantErr: true, wantLevel: levelError, wantResult: "failed"},
				{mode: ConflictIgnore, wantLevel: levelInfo, wantResult: "ignored"},
			} {
				t.Run(incoming.Name+"/"+tc.mode.String(), func(t *testing.T) {
					sink := &recordingSink{}
					_, err := checkConflictsIn(incoming, selfExclusion(incoming), tc.mode, state, sink)
					if tc.wantErr {
						if !errors.Is(err, ErrPackageConflict) {
							t.Fatalf("checkConflictsIn(%s, %s) = %v, want ErrPackageConflict", incoming, tc.mode, err)
						}
					} else if err != nil {
						t.Fatalf("checkConflictsIn(%s, %s) = %v, want nil", incoming, tc.mode, err)
					}
					wantLine := conflictLine(revConflict, tc.mode, tc.wantResult, installedX, "P<2.0.0", incoming)
					foundLine := false
					foundWarn := false
					for _, ev := range sink.events {
						if ev.level == tc.wantLevel && ev.text == wantLine {
							foundLine = true
						}
						if ev.level == levelStdout && strings.Contains(ev.text, "Warning: installed package X.noarch.1.0.0 conflicts with "+incoming.String()) {
							foundWarn = true
						}
					}
					if !foundLine {
						t.Errorf("missing expected telemetry event (%s, %q) in %v", tc.wantLevel, wantLine, sink.events)
					}
					if foundWarn != tc.wantWarn {
						t.Errorf("console warning present = %t, want %t (events: %v)", foundWarn, tc.wantWarn, sink.events)
					}
				})
			}
		}

		// With versioned Provides outside the <2.0.0 range, no reverse hit occurs in any mode.
		for _, incoming := range []*goolib.PkgSpec{
			{Name: "R", Arch: "noarch", Version: "2.0.0", Provides: []string{"P=2.0.0"}},
			{Name: "P", Arch: "noarch", Version: "3.0.0", Provides: []string{"P=3.0.0"}},
		} {
			for _, mode := range []ConflictMode{ConflictWarn, ConflictError, ConflictIgnore} {
				sink := &recordingSink{}
				if _, err := checkConflictsIn(incoming, selfExclusion(incoming), mode, state, sink); err != nil {
					t.Errorf("versioned Provides checkConflictsIn(%s, %s) = %v, want nil", incoming, mode, err)
				}
				if len(sink.events) != 0 {
					t.Errorf("versioned Provides checkConflictsIn(%s, %s) emitted %v, want none", incoming, mode, sink.events)
				}
			}
		}
	})

	t.Run("cross_arch_self_unversioned_vs_versioned", func(t *testing.T) {
		installedX64 := &goolib.PkgSpec{
			Name:      "R",
			Arch:      "x86_64",
			Version:   "2.0.0",
			Conflicts: []string{"P<2.0.0"},
			Provides:  []string{"P"},
		}
		incomingNoarch := &goolib.PkgSpec{
			Name:      "R",
			Arch:      "noarch",
			Version:   "2.0.0",
			Conflicts: []string{"P<2.0.0"},
			Provides:  []string{"P"},
		}
		rm := client.RepoMap{"repo": {Packages: []goolib.RepoSpec{{PackageSpec: incomingNoarch}}}}
		state := client.GooGetState{{PackageSpec: installedX64}}
		_, err := PlanState(goolib.PackageInfo{Name: "R", Arch: "noarch"}, "repo", rm, state, archs, Options{PackageConflicts: ConflictError})
		wantErr := "cannot install, conflict with installed package or provider: P<2.0.0"
		if err == nil || err.Error() != wantErr {
			t.Errorf("cross-arch unversioned PlanState = %v, want %q", err, wantErr)
		}

		versionedX64 := &goolib.PkgSpec{
			Name:      "R",
			Arch:      "x86_64",
			Version:   "2.0.0",
			Conflicts: []string{"P<2.0.0"},
			Provides:  []string{"P=2.0.0"},
		}
		versionedNoarch := &goolib.PkgSpec{
			Name:      "R",
			Arch:      "noarch",
			Version:   "2.0.0",
			Conflicts: []string{"P<2.0.0"},
			Provides:  []string{"P=2.0.0"},
		}
		rmVer := client.RepoMap{"repo": {Packages: []goolib.RepoSpec{{PackageSpec: versionedNoarch}}}}
		stateVer := client.GooGetState{{PackageSpec: versionedX64}}
		if _, err := PlanState(goolib.PackageInfo{Name: "R", Arch: "noarch"}, "repo", rmVer, stateVer, archs, Options{PackageConflicts: ConflictError}); err != nil {
			t.Errorf("cross-arch versioned PlanState = %v, want nil", err)
		}
	})
}

// TestInstallConsumersOperatorMatrix checks every install-side Conflicts and
// Replaces consumer across all five version operators, multi-arch installed
// rows, prerelease/build/release versions, and provider vs identity rules.
func TestInstallConsumersOperatorMatrix(t *testing.T) {
	ops := []struct {
		sym   string
		match func(ver string) bool
	}{
		{sym: "<", match: func(v string) bool { c, _ := goolib.Compare(v, "2.0.0"); return c < 0 }},
		{sym: "<=", match: func(v string) bool { c, _ := goolib.Compare(v, "2.0.0"); return c <= 0 }},
		{sym: "=", match: func(v string) bool { c, _ := goolib.Compare(v, "2.0.0"); return c == 0 }},
		{sym: ">=", match: func(v string) bool { c, _ := goolib.Compare(v, "2.0.0"); return c >= 0 }},
		{sym: ">", match: func(v string) bool { c, _ := goolib.Compare(v, "2.0.0"); return c > 0 }},
	}
	versions := []string{"1.0.0", "2.0.0-rc1", "2.0.0", "2.0.0+build", "2.0.0@1", "3.0.0"}
	archs := []string{"noarch", "x86_64"}

	for _, op := range ops {
		for _, ver := range versions {
			wantMatch := op.match(ver)
			compactEntry := "P" + op.sym + "2.0.0"
			spacedEntry := "P " + op.sym + " 2.0.0"
			archEntry := "P.x86_64" + op.sym + "2.0.0"

			t.Run(op.sym+"/"+ver, func(t *testing.T) {
				// Forward Conflicts by identity and by versioned Provides, including compact error text from spaced input.
				incomingConf := &goolib.PkgSpec{Name: "Q", Arch: "noarch", Version: "1.0.0", Conflicts: []string{spacedEntry}}
				identState := client.GooGetState{{PackageSpec: &goolib.PkgSpec{Name: "P", Arch: "noarch", Version: ver}}}
				_, err := resolveConflictsIn(incomingConf, selfExclusion(incomingConf), identState, &recordingSink{})
				if wantMatch {
					wantErr := "cannot install, conflict with installed package or provider: " + compactEntry
					if err == nil || err.Error() != wantErr {
						t.Errorf("resolveConflictsIn(identity %s) = %v, want %q", ver, err, wantErr)
					}
				} else if err != nil {
					t.Errorf("resolveConflictsIn(identity %s) = %v, want nil", ver, err)
				}

				provState := client.GooGetState{{PackageSpec: &goolib.PkgSpec{Name: "H", Arch: "noarch", Version: "1.0.0", Provides: []string{"P=" + ver}}}}
				_, err = resolveConflictsIn(incomingConf, selfExclusion(incomingConf), provState, &recordingSink{})
				if wantMatch {
					wantErr := "cannot install, conflict with installed package or provider: " + compactEntry
					if err == nil || err.Error() != wantErr {
						t.Errorf("resolveConflictsIn(provider P=%s) = %v, want %q", ver, err, wantErr)
					}
				} else if err != nil {
					t.Errorf("resolveConflictsIn(provider P=%s) = %v, want nil", ver, err)
				}

				// Reverse Conflicts by incoming identity and by incoming versioned Provides.
				gotRevIdent, unpIdent := matchReverseEntry(compactEntry, &goolib.PkgSpec{Name: "P", Arch: "x86_64", Version: ver})
				if unpIdent || gotRevIdent != wantMatch {
					t.Errorf("matchReverseEntry(%q, P.x86_64.%s) = (%t, %t), want (%t, false)", compactEntry, ver, gotRevIdent, unpIdent, wantMatch)
				}
				gotRevProv, unpProv := matchReverseEntry(compactEntry, &goolib.PkgSpec{Name: "H", Arch: "noarch", Version: "1.0.0", Provides: []string{"P=" + ver}})
				if unpProv || gotRevProv != wantMatch {
					t.Errorf("matchReverseEntry(%q, Provides P=%s) = (%t, %t), want (%t, false)", compactEntry, ver, gotRevProv, unpProv, wantMatch)
				}

				// Reverse Replaces matches identity only and never incoming Provides.
				gotRevRepl, unpRepl := matchReverseReplaces(compactEntry, &goolib.PkgSpec{Name: "P", Arch: "x86_64", Version: ver})
				if unpRepl || gotRevRepl != wantMatch {
					t.Errorf("matchReverseReplaces(%q, P.x86_64.%s) = (%t, %t), want (%t, false)", compactEntry, ver, gotRevRepl, unpRepl, wantMatch)
				}
				gotRevReplNoarch, _ := matchReverseReplaces(archEntry, &goolib.PkgSpec{Name: "P", Arch: "noarch", Version: ver})
				if gotRevReplNoarch {
					t.Errorf("matchReverseReplaces(%q, P.noarch.%s) = true, want false", archEntry, ver)
				}
				gotRevReplProv, _ := matchReverseReplaces(compactEntry, &goolib.PkgSpec{Name: "H", Arch: "noarch", Version: "1.0.0", Provides: []string{"P", "P=" + ver}})
				if gotRevReplProv {
					t.Errorf("matchReverseReplaces(%q, provider) = true, want false", compactEntry)
				}

				// Multi-arch victimsIn, repoExclusionsIn, diskNode, PlanState, Replaced and removalReason.
				// Pair the tested version on x86_64 with an opposite-range version on noarch so arch scoping is exercised.
				otherVer := "3.0.0"
				if op.match(otherVer) {
					otherVer = "1.0.0"
				}
				otherMatch := op.match(otherVer)

				multiState := client.GooGetState{
					{PackageSpec: &goolib.PkgSpec{Name: "P", Arch: "noarch", Version: otherVer}},
					{PackageSpec: &goolib.PkgSpec{Name: "P", Arch: "x86_64", Version: ver}},
					{PackageSpec: &goolib.PkgSpec{Name: "D", Arch: "noarch", Version: "1.0.0", PkgDependencies: map[string]string{"P.x86_64": "0.0.1"}}},
				}
				stateWithProvider := append(slices.Clone(multiState), client.PackageState{
					PackageSpec: &goolib.PkgSpec{Name: "H", Arch: "noarch", Version: "1.0.0", Provides: []string{"P", "P=" + ver}},
				})

				for _, entryCase := range []struct {
					label      string
					entry      string
					wantNoarch bool
					wantX64    bool
				}{
					{label: "archless", entry: compactEntry, wantNoarch: otherMatch, wantX64: wantMatch},
					{label: "x86_64_only", entry: archEntry, wantNoarch: false, wantX64: wantMatch},
				} {
					replacer := &goolib.PkgSpec{Name: "R", Arch: "noarch", Version: "1.0.0", Replaces: []string{entryCase.entry}}
					var wantVictims []string
					if entryCase.wantNoarch {
						wantVictims = append(wantVictims, "P.noarch."+otherVer)
					}
					if entryCase.wantX64 {
						wantVictims = append(wantVictims, "P.x86_64."+ver)
					}

					victims, err := victimsIn(replacer, stateWithProvider)
					if err != nil {
						t.Fatalf("victimsIn(%s) failed: %v", entryCase.entry, err)
					}
					var gotVictims []string
					for _, v := range victims {
						gotVictims = append(gotVictims, v.PackageSpec.String())
					}
					if diff := cmp.Diff(wantVictims, gotVictims); diff != "" {
						t.Errorf("%s victimsIn(%s) mismatch (-want +got):\n%s", entryCase.label, entryCase.entry, diff)
					}

					ex, err := repoExclusionsIn(replacer, stateWithProvider)
					if err != nil {
						t.Fatalf("repoExclusionsIn(%s) failed: %v", entryCase.entry, err)
					}
					if ex.has("P.noarch") != entryCase.wantNoarch || ex.has("P.x86_64") != entryCase.wantX64 || ex.has("H.noarch") {
						t.Errorf("%s repoExclusionsIn(%s) = %+v, want noarch=%t x86_64=%t H=false", entryCase.label, entryCase.entry, ex.victims, entryCase.wantNoarch, entryCase.wantX64)
					}

					// diskNode refuses if and only if at least one installed row matches Replaces, formatting the requirement with Entry().
					dp := &planner{state: cloneState(multiState), sink: &recordingSink{}}
					diskErr := dp.diskNode(replacer)
					if len(wantVictims) > 0 {
						wantDiskPrefix := "cannot install, replaces installed package, remove first then try installation again: " + entryCase.entry + " (installed: " + wantVictims[0] + ")"
						if diskErr == nil || diskErr.Error() != wantDiskPrefix {
							t.Errorf("%s diskNode(%s) = %v, want %q", entryCase.label, entryCase.entry, diskErr, wantDiskPrefix)
						}
					} else if diskErr != nil {
						t.Errorf("%s diskNode(%s) = %v, want nil", entryCase.label, entryCase.entry, diskErr)
					}

					// PlanState, Plan.Replaced and removalReason agree with victimsIn, while cascaded dependent D gets the dependent reason.
					rm := client.RepoMap{"repo": {Packages: []goolib.RepoSpec{{PackageSpec: replacer}}}}
					plan, err := PlanState(goolib.PackageInfo{Name: "R"}, "repo", rm, multiState, archs, Options{OrphanedDependents: OrphanRemove})
					if err != nil {
						t.Fatalf("%s PlanState(%s) failed: %v", entryCase.label, entryCase.entry, err)
					}
					if diff := cmp.Diff(wantVictims, infoStrings(plan.Replaced())); diff != "" {
						t.Errorf("%s Plan.Replaced(%s) mismatch (-want +got):\n%s", entryCase.label, entryCase.entry, diff)
					}
					rendered := plan.String()
					for _, row := range []struct {
						specStr  string
						isVictim bool
						removed  bool
					}{
						{specStr: "P.noarch." + otherVer, isVictim: entryCase.wantNoarch, removed: entryCase.wantNoarch},
						{specStr: "P.x86_64." + ver, isVictim: entryCase.wantX64, removed: entryCase.wantX64},
						{specStr: "D.noarch.1.0.0", isVictim: false, removed: entryCase.wantX64},
					} {
						replacedLine := "  " + row.specStr + " (replaced by R.noarch.1.0.0)"
						depLine := "  " + row.specStr + " (depends on a package R.noarch.1.0.0 replaces)"
						if !row.removed {
							if strings.Contains(rendered, row.specStr) {
								t.Errorf("%s Plan.String(%s) unexpectedly mentions unremoved %s:\n%s", entryCase.label, entryCase.entry, row.specStr, rendered)
							}
							continue
						}
						if row.isVictim {
							if !strings.Contains(rendered, replacedLine) {
								t.Errorf("%s Plan.String(%s) missing %q in:\n%s", entryCase.label, entryCase.entry, replacedLine, rendered)
							}
						} else if !strings.Contains(rendered, depLine) {
							t.Errorf("%s Plan.String(%s) missing %q in:\n%s", entryCase.label, entryCase.entry, depLine, rendered)
						}
					}
				}
			})
		}
	}
}

// TestDeferredRecheckPreservesOperator checks that a forward Conflicts
// requirement deferred via a Replaces victim exemption retains its operator
// during both pre-replacement simulation and post-replacement recheck.
func TestDeferredRecheckPreservesOperator(t *testing.T) {
	archs := []string{"noarch", "x86_64"}

	t.Run("surviving_out_of_range_arch_passes_plan_state", func(t *testing.T) {
		replacer := &goolib.PkgSpec{
			Name:      "R",
			Arch:      "noarch",
			Version:   "1.0.0",
			Conflicts: []string{"P<2.0.0"},
			Replaces:  []string{"P.noarch"},
		}
		state := client.GooGetState{
			{PackageSpec: &goolib.PkgSpec{Name: "P", Arch: "noarch", Version: "1.0.0"}},
			{PackageSpec: &goolib.PkgSpec{Name: "P", Arch: "x86_64", Version: "3.0.0"}},
		}
		rm := client.RepoMap{"repo": {Packages: []goolib.RepoSpec{{PackageSpec: replacer}}}}
		plan, err := PlanState(goolib.PackageInfo{Name: "R"}, "repo", rm, state, archs, Options{})
		if err != nil {
			t.Fatalf("PlanState with out-of-range P.x86_64.3.0.0 failed: %v", err)
		}
		if diff := cmp.Diff([]string{"P.noarch.1.0.0"}, infoStrings(plan.Removals())); diff != "" {
			t.Errorf("Removals mismatch (-want +got):\n%s", diff)
		}
		if diff := cmp.Diff([]string{"P.x86_64.3.0.0", "R.noarch.1.0.0"}, stateStrings(plan.State())); diff != "" {
			t.Errorf("final state mismatch (-want +got):\n%s", diff)
		}
	})

	t.Run("dependency_introducing_in_range_arch_blocks_on_deferred_recheck", func(t *testing.T) {
		replacer := &goolib.PkgSpec{
			Name:            "R",
			Arch:            "noarch",
			Version:         "1.0.0",
			Conflicts:       []string{"P < 2.0.0"},
			Replaces:        []string{"P.noarch"},
			PkgDependencies: map[string]string{"P.x86_64": "1.0.0"},
		}
		rmInRange := client.RepoMap{"repo": {Packages: []goolib.RepoSpec{
			{PackageSpec: replacer},
			{PackageSpec: &goolib.PkgSpec{Name: "P", Arch: "x86_64", Version: "1.5.0"}},
		}}}
		initial := client.GooGetState{
			{PackageSpec: &goolib.PkgSpec{Name: "P", Arch: "noarch", Version: "1.0.0"}},
		}
		_, err := PlanState(goolib.PackageInfo{Name: "R"}, "repo", rmInRange, initial, archs, Options{})
		wantErr := "cannot install, conflict with installed package or provider: P<2.0.0"
		if err == nil || err.Error() != wantErr {
			t.Errorf("PlanState with dependency installing P.x86_64.1.5.0 = %v, want %q", err, wantErr)
		}

		rmOutOfRange := client.RepoMap{"repo": {Packages: []goolib.RepoSpec{
			{PackageSpec: replacer},
			{PackageSpec: &goolib.PkgSpec{Name: "P", Arch: "x86_64", Version: "3.0.0"}},
		}}}
		if _, err := PlanState(goolib.PackageInfo{Name: "R"}, "repo", rmOutOfRange, initial, archs, Options{}); err != nil {
			t.Errorf("PlanState with dependency installing P.x86_64.3.0.0 = %v, want nil", err)
		}
	})

	t.Run("direct_deferred_checks_pre_and_post_replacement", func(t *testing.T) {
		replacer := &goolib.PkgSpec{
			Name:      "R",
			Arch:      "noarch",
			Version:   "1.0.0",
			Conflicts: []string{"P < 2.0.0"},
			Replaces:  []string{"P.noarch"},
		}
		initial := client.GooGetState{
			{PackageSpec: &goolib.PkgSpec{Name: "P", Arch: "noarch", Version: "1.0.0"}},
		}
		ex, err := repoExclusionsIn(replacer, initial)
		if err != nil {
			t.Fatalf("repoExclusionsIn failed: %v", err)
		}
		deferred, err := checkConflictsIn(replacer, ex, ConflictWarn, initial, &recordingSink{})
		if err != nil {
			t.Fatalf("checkConflictsIn failed: %v", err)
		}
		if len(deferred.fwd) != 1 || deferred.fwd[0].Op != relation.OpLT {
			t.Fatalf("deferred.fwd = %+v, want single OpLT requirement", deferred.fwd)
		}

		withInRange := client.GooGetState{
			{PackageSpec: &goolib.PkgSpec{Name: "P", Arch: "noarch", Version: "1.0.0"}},
			{PackageSpec: &goolib.PkgSpec{Name: "P", Arch: "x86_64", Version: "1.5.0"}},
		}
		preErr := deferred.recheckBeforeReplacementsIn(replacer, ConflictWarn, withInRange, &recordingSink{})
		wantPre := "cannot install, conflict with installed package or provider: P<2.0.0"
		if preErr == nil || preErr.Error() != wantPre {
			t.Errorf("recheckBeforeReplacementsIn(1.5.0) = %v, want %q", preErr, wantPre)
		}

		postInRange := client.GooGetState{
			{PackageSpec: &goolib.PkgSpec{Name: "P", Arch: "x86_64", Version: "1.5.0"}},
		}
		postErr := deferred.recheckIn(replacer, ConflictWarn, postInRange, &recordingSink{})
		wantPost := "cannot install, conflict with installed package or provider: P<2.0.0 (still installed after replacing)"
		if postErr == nil || postErr.Error() != wantPost {
			t.Errorf("recheckIn(1.5.0) = %v, want %q", postErr, wantPost)
		}

		postOutOfRange := client.GooGetState{
			{PackageSpec: &goolib.PkgSpec{Name: "P", Arch: "x86_64", Version: "3.0.0"}},
		}
		if err := deferred.recheckIn(replacer, ConflictWarn, postOutOfRange, &recordingSink{}); err != nil {
			t.Errorf("recheckIn(3.0.0) = %v, want nil", err)
		}
	})
}

// TestVictimExemptionWithBoundedReplaces checks that an unbounded Conflicts
// entry combined with a bounded operator Replaces entry blocks out-of-range
// installed versions while exempting and replacing in-range victims with a
// single exemption log line.
func TestVictimExemptionWithBoundedReplaces(t *testing.T) {
	archs := []string{"noarch", "x86_64"}
	replacer := &goolib.PkgSpec{
		Name:      "R",
		Arch:      "noarch",
		Version:   "1.0.0",
		Conflicts: []string{"P"},
		Replaces:  []string{"P<2.0.0"},
	}
	rm := client.RepoMap{"repo": {Packages: []goolib.RepoSpec{{PackageSpec: replacer}}}}

	t.Run("out_of_range_installed_blocks", func(t *testing.T) {
		for _, ver := range []string{"2.0.0", "3.0.0"} {
			state := client.GooGetState{{PackageSpec: &goolib.PkgSpec{Name: "P", Arch: "noarch", Version: ver}}}
			_, err := PlanState(goolib.PackageInfo{Name: "R"}, "repo", rm, state, archs, Options{})
			wantErr := "cannot install, conflict with installed package or provider: P"
			if err == nil || err.Error() != wantErr {
				t.Errorf("PlanState(P %s) = %v, want %q", ver, err, wantErr)
			}
		}
	})

	t.Run("in_range_installed_is_exempted_once_and_replaced", func(t *testing.T) {
		victim := &goolib.PkgSpec{Name: "P", Arch: "noarch", Version: "1.0.0", Provides: []string{"P=1.0.0"}}
		state := client.GooGetState{{PackageSpec: victim}}
		ex, err := repoExclusionsIn(replacer, state)
		if err != nil {
			t.Fatalf("repoExclusionsIn failed: %v", err)
		}
		sink := &recordingSink{}
		if _, err := resolveConflictsIn(replacer, ex, state, sink); err != nil {
			t.Fatalf("resolveConflictsIn(P 1.0.0) = %v, want nil", err)
		}
		wantSkip := `conflict "P" with P.noarch.1.0.0 skipped: installing R.noarch.1.0.0 removes it`
		var skipLines []string
		for _, ev := range sink.events {
			if ev.level == levelInfo && ev.text == wantSkip {
				skipLines = append(skipLines, ev.text)
			}
		}
		if !slices.Equal(skipLines, []string{wantSkip}) {
			t.Errorf("skip log lines = %v, want single %q (all events: %v)", skipLines, wantSkip, sink.events)
		}

		plan, err := PlanState(goolib.PackageInfo{Name: "R"}, "repo", rm, state, archs, Options{})
		if err != nil {
			t.Fatalf("PlanState(P 1.0.0) failed: %v", err)
		}
		if diff := cmp.Diff([]string{"P.noarch.1.0.0"}, infoStrings(plan.Removals())); diff != "" {
			t.Errorf("Removals mismatch (-want +got):\n%s", diff)
		}
		if diff := cmp.Diff([]string{"R.noarch.1.0.0"}, stateStrings(plan.State())); diff != "" {
			t.Errorf("final state mismatch (-want +got):\n%s", diff)
		}
	})
}
