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
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/googet/v2/client"
	"github.com/google/googet/v2/goolib"
	"github.com/google/googet/v2/settings"
	"github.com/google/googet/v2/testutil"
)

// TestParseOrphanModeAndString checks strict parsing, case/whitespace
// normalization, and string formatting for OrphanMode.
func TestParseOrphanModeAndString(t *testing.T) {
	for _, tc := range []struct {
		in      string
		want    OrphanMode
		wantErr bool
	}{
		{in: "fail", want: OrphanFail},
		{in: "  FAIL\t", want: OrphanFail},
		{in: "remove", want: OrphanRemove},
		{in: " ReMoVe ", want: OrphanRemove},
		{in: "", wantErr: true},
		{in: "   ", wantErr: true},
		{in: "warn", wantErr: true},
		{in: "error", wantErr: true},
		{in: "ignore", wantErr: true},
	} {
		got, err := ParseOrphanMode(tc.in)
		if (err != nil) != tc.wantErr {
			t.Errorf("ParseOrphanMode(%q) error = %v, want error: %t", tc.in, err, tc.wantErr)
			continue
		}
		if !tc.wantErr && got != tc.want {
			t.Errorf("ParseOrphanMode(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
	if got := OrphanFail.String(); got != "fail" {
		t.Errorf("OrphanFail.String() = %q, want fail", got)
	}
	if got := OrphanRemove.String(); got != "remove" {
		t.Errorf("OrphanRemove.String() = %q, want remove", got)
	}
	if got := OrphanMode(99).String(); got != "OrphanMode(99)" {
		t.Errorf("OrphanMode(99).String() = %q, want OrphanMode(99)", got)
	}
	var nilErr *OrphanedDependentsError
	if got := nilErr.Error(); got != ErrOrphanedDependents.Error() {
		t.Errorf("(*OrphanedDependentsError)(nil).Error() = %q, want %q", got, ErrOrphanedDependents.Error())
	}
}

// TestSharedDependentAcrossMultipleVictimsListedOnce checks that an installed
// dependent broken by removing multiple replaced packages reports each broken
// requirement accurately without mixing unrelated Removed attributions, while
// OrphanRemove still uninstalls the dependent once.
func TestSharedDependentAcrossMultipleVictimsListedOnce(t *testing.T) {
	t.Run("dependent requires two distinct replaced packages", func(t *testing.T) {
		f := newRelFixture(t,
			[]goolib.PkgSpec{
				{Name: "A", Arch: "noarch", Version: "1"},
				{Name: "B", Arch: "noarch", Version: "1"},
				{Name: "D", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"A": "1", "B": "1"}},
			},
			[]goolib.PkgSpec{
				{Name: "R", Arch: "noarch", Version: "1", Replaces: []string{"A", "B"}},
			})
		err := f.install(t, "R")
		var oe *OrphanedDependentsError
		if !errors.As(err, &oe) {
			t.Fatalf("install R = %v, want *OrphanedDependentsError", err)
		}
		want := []OrphanedDependent{
			{
				Package:     goolib.PackageInfo{Name: "D", Arch: "noarch", Ver: "1"},
				Requirement: goolib.PackageInfo{Name: "A", Ver: "1"},
				Removed:     []goolib.PackageInfo{{Name: "A", Arch: "noarch", Ver: "1"}},
				ReplacedBy:  goolib.PackageInfo{Name: "R", Arch: "noarch", Ver: "1"},
			},
			{
				Package:     goolib.PackageInfo{Name: "D", Arch: "noarch", Ver: "1"},
				Requirement: goolib.PackageInfo{Name: "B", Ver: "1"},
				Removed:     []goolib.PackageInfo{{Name: "B", Arch: "noarch", Ver: "1"}},
				ReplacedBy:  goolib.PackageInfo{Name: "R", Arch: "noarch", Ver: "1"},
			},
		}
		if diff := cmp.Diff(want, oe.Dependents); diff != "" {
			t.Errorf("Dependents (-want +got):\n%s", diff)
		}
		if !strings.Contains(err.Error(), "D.noarch.1 (requires A >= 1, broken by removing A.noarch.1 for R.noarch.1); D.noarch.1 (requires B >= 1, broken by removing B.noarch.1 for R.noarch.1)") {
			t.Errorf("Error() = %q, want separate per-requirement clauses", err.Error())
		}
		if err := f.installWith(t, Options{OrphanedDependents: OrphanRemove}, "R"); err != nil {
			t.Fatalf("install R (OrphanRemove): %v", err)
		}
		if diff := cmp.Diff([]string{"R.noarch.1"}, f.installed(t)); diff != "" {
			t.Errorf("installed (-want +got):\n%s", diff)
		}
	})

	t.Run("dependent requires arch-less package satisfied by two replaced architectures", func(t *testing.T) {
		f := newRelFixture(t,
			[]goolib.PkgSpec{
				{Name: "P", Arch: "noarch", Version: "1"},
				{Name: "P", Arch: "x86_64", Version: "1"},
				{Name: "D", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"P": "1"}},
			},
			[]goolib.PkgSpec{
				{Name: "R", Arch: "noarch", Version: "1", Replaces: []string{"P"}},
			})
		err := f.install(t, "R")
		var oe *OrphanedDependentsError
		if !errors.As(err, &oe) {
			t.Fatalf("install R = %v, want *OrphanedDependentsError", err)
		}
		want := []OrphanedDependent{{
			Package:     goolib.PackageInfo{Name: "D", Arch: "noarch", Ver: "1"},
			Requirement: goolib.PackageInfo{Name: "P", Ver: "1"},
			Removed: []goolib.PackageInfo{
				{Name: "P", Arch: "noarch", Ver: "1"},
				{Name: "P", Arch: "x86_64", Ver: "1"},
			},
			ReplacedBy: goolib.PackageInfo{Name: "R", Arch: "noarch", Ver: "1"},
		}}
		if diff := cmp.Diff(want, oe.Dependents); diff != "" {
			t.Errorf("Dependents (-want +got):\n%s", diff)
		}
	})
}

// TestOrphanFailListsTransitiveClosureAndChangesNothing checks that default
// OrphanFail reports the entire transitive name-based dependent closure
// without downloading archives or altering files, cache, or database state,
// while OrphanRemove removes the exact closure in reverse dependency order.
func TestOrphanFailListsTransitiveClosureAndChangesNothing(t *testing.T) {
	f := newRelFixture(t,
		[]goolib.PkgSpec{
			{Name: "P", Arch: "noarch", Version: "1"},
			{Name: "D1", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"P": "1"}},
			{Name: "D2", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"D1": "1"}},
			{Name: "Keep", Arch: "noarch", Version: "1"},
		},
		[]goolib.PkgSpec{
			{Name: "R", Arch: "noarch", Version: "1", Replaces: []string{"P"}},
		})
	before := f.hostState(t)
	err := f.install(t, "R")
	if !errors.Is(err, ErrOrphanedDependents) {
		t.Fatalf("install R = %v, want ErrOrphanedDependents", err)
	}
	var oe *OrphanedDependentsError
	if !errors.As(err, &oe) {
		t.Fatalf("errors.As(*OrphanedDependentsError) = false for %v", err)
	}
	var gotPkgs []string
	for _, d := range oe.Dependents {
		gotPkgs = append(gotPkgs, d.Package.String())
	}
	if diff := cmp.Diff([]string{"D1.noarch.1", "D2.noarch.1"}, gotPkgs); diff != "" {
		t.Errorf("reported orphans (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff(before, f.hostState(t), cmp.AllowUnexported(hostState{})); diff != "" {
		t.Errorf("host state changed on OrphanFail (-before +after):\n%s", diff)
	}
	if f.totalGets() != 0 {
		t.Errorf("totalGets = %d, want 0", f.totalGets())
	}

	var uninstalled []string
	err = f.runWithHook(t, t.Context(), Options{OrphanedDependents: OrphanRemove}, "R", func(stage applyStage, step string) error {
		if stage == stageAfterUninstall {
			uninstalled = append(uninstalled, step)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("install R (OrphanRemove): %v", err)
	}
	if diff := cmp.Diff([]string{"D2.noarch.1", "D1.noarch.1", "P.noarch.1"}, uninstalled); diff != "" {
		t.Errorf("OrphanRemove uninstall order (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff([]string{"Keep.noarch.1", "R.noarch.1"}, f.installed(t)); diff != "" {
		t.Errorf("installed after OrphanRemove (-want +got):\n%s", diff)
	}
}

// TestDependentSatisfactionByReplacerProviderOrSiblingArch checks when
// dependents remain satisfied across replacer Provides entries, other
// installed providers, and remaining sibling architecture rows.
func TestDependentSatisfactionByReplacerProviderOrSiblingArch(t *testing.T) {
	for _, tc := range []struct {
		name      string
		installed []goolib.PkgSpec
		replacer  goolib.PkgSpec
		wantOrph  bool
		wantFinal []string
	}{
		{
			name: "replacer versioned Provides satisfies dependent",
			installed: []goolib.PkgSpec{
				{Name: "P", Arch: "noarch", Version: "2"},
				{Name: "D", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"P": "2"}},
			},
			replacer:  goolib.PkgSpec{Name: "R", Arch: "noarch", Version: "1", Replaces: []string{"P"}, Provides: []string{"P=2"}},
			wantFinal: []string{"D.noarch.1", "R.noarch.1"},
		},
		{
			name: "replacer Provides version below minimum orphans dependent",
			installed: []goolib.PkgSpec{
				{Name: "P", Arch: "noarch", Version: "2"},
				{Name: "D", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"P": "2"}},
			},
			replacer: goolib.PkgSpec{Name: "R", Arch: "noarch", Version: "1", Replaces: []string{"P"}, Provides: []string{"P=1"}},
			wantOrph: true,
		},
		{
			name: "replacer unversioned Provides satisfies versioned requirement",
			installed: []goolib.PkgSpec{
				{Name: "P", Arch: "noarch", Version: "2"},
				{Name: "D", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"P": "1"}},
			},
			replacer:  goolib.PkgSpec{Name: "R", Arch: "noarch", Version: "1", Replaces: []string{"P"}, Provides: []string{"P"}},
			wantFinal: []string{"D.noarch.1", "R.noarch.1"},
		},
		{
			name: "replacer Provides satisfies arch-qualified requirement",
			installed: []goolib.PkgSpec{
				{Name: "P", Arch: "noarch", Version: "2"},
				{Name: "D", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"P.noarch": "1"}},
			},
			replacer:  goolib.PkgSpec{Name: "R", Arch: "noarch", Version: "1", Replaces: []string{"P"}, Provides: []string{"P=2"}},
			wantFinal: []string{"D.noarch.1", "R.noarch.1"},
		},
		{
			name: "replacer malformed Provides version does not satisfy versioned requirement",
			installed: []goolib.PkgSpec{
				{Name: "P", Arch: "noarch", Version: "2"},
				{Name: "D", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"P": "1"}},
			},
			replacer: goolib.PkgSpec{Name: "R", Arch: "noarch", Version: "1", Replaces: []string{"P"}, Provides: []string{"P=notaver"}},
			wantOrph: true,
		},
		{
			name: "another remaining installed provider keeps dependent satisfied",
			installed: []goolib.PkgSpec{
				{Name: "P", Arch: "noarch", Version: "1"},
				{Name: "Alt", Arch: "noarch", Version: "1", Provides: []string{"P=1"}},
				{Name: "D", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"P": "1"}},
			},
			replacer:  goolib.PkgSpec{Name: "R", Arch: "noarch", Version: "1", Replaces: []string{"P"}},
			wantFinal: []string{"Alt.noarch.1", "D.noarch.1", "R.noarch.1"},
		},
		{
			name: "remaining sibling architecture row keeps arch-less dependent satisfied",
			installed: []goolib.PkgSpec{
				{Name: "P", Arch: "noarch", Version: "1"},
				{Name: "P", Arch: "x86_64", Version: "1"},
				{Name: "D", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"P": "1"}},
			},
			replacer:  goolib.PkgSpec{Name: "R", Arch: "noarch", Version: "1", Replaces: []string{"P.noarch"}},
			wantFinal: []string{"D.noarch.1", "P.x86_64.1", "R.noarch.1"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRelFixture(t, tc.installed, []goolib.PkgSpec{tc.replacer})
			err := f.install(t, "R")
			if tc.wantOrph {
				if !errors.Is(err, ErrOrphanedDependents) {
					t.Fatalf("install R = %v, want ErrOrphanedDependents", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("install R: %v", err)
			}
			if diff := cmp.Diff(tc.wantFinal, f.installed(t)); diff != "" {
				t.Errorf("installed (-want +got):\n%s", diff)
			}
		})
	}
}

// TestProvidesOnlyDependentListedAndNeverRemoved checks that dependents whose
// broken requirement was satisfied only via Provides are listed in Plan.String,
// printed on stdout, and logged at Warning without failing or removing the
// dependent under either OrphanFail or OrphanRemove.
func TestProvidesOnlyDependentListedAndNeverRemoved(t *testing.T) {
	for _, mode := range []OrphanMode{OrphanFail, OrphanRemove} {
		t.Run(mode.String(), func(t *testing.T) {
			f := newRelFixture(t,
				[]goolib.PkgSpec{
					{Name: "P1", Arch: "noarch", Version: "1", Provides: []string{"virt=2"}},
					{Name: "P2", Arch: "noarch", Version: "1", Provides: []string{"virt=2"}},
					{Name: "Q", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"virt": "1"}},
				},
				[]goolib.PkgSpec{
					{Name: "R", Arch: "noarch", Version: "1", Replaces: []string{"P1", "P2"}},
				})
			ins := New(f.cache, f.db, Options{OrphanedDependents: mode})
			p, err := ins.Plan(goolib.PackageInfo{Name: "R"}, "", f.rm, corpusArchs)
			if err != nil {
				t.Fatalf("Plan: %v", err)
			}
			wantSection := "The following packages will have an unsatisfied dependency (not removed):\n  Q.noarch.1 (requires virt >= 1, provided by P1.noarch.1, P2.noarch.1; replaced by R.noarch.1)\n"
			if !strings.Contains(p.String(), wantSection) {
				t.Errorf("Plan.String() = %q, want substring %q", p.String(), wantSection)
			}
			var rem []string
			for _, r := range p.Removals() {
				rem = append(rem, r.String())
			}
			if diff := cmp.Diff([]string{"P1.noarch.1", "P2.noarch.1"}, rem); diff != "" {
				t.Errorf("Plan.Removals() (-want +got):\n%s", diff)
			}

			logs := testutil.CaptureLogs(t)
			stdout := captureStdout(t, func() {
				err = ins.FromRepo(t.Context(), goolib.PackageInfo{Name: "R"}, "", f.rm, corpusArchs, f.downloader)
			})
			if err != nil {
				t.Fatalf("FromRepo: %v", err)
			}
			if !strings.Contains(stdout, wantSection) {
				t.Errorf("stdout = %q, want substring %q", stdout, wantSection)
			}
			wantLog := `dependency_unsatisfied op=replace result=listed dependent=Q.noarch.1 requirement="virt >= 1" removed=P1.noarch.1,P2.noarch.1 replacer=R.noarch.1`
			if !strings.Contains(logs.String(), "WARN :") || !strings.Contains(logs.String(), wantLog) {
				t.Errorf("logs missing warning %q:\n%s", wantLog, logs.String())
			}
			if diff := cmp.Diff([]string{"Q.noarch.1", "R.noarch.1"}, f.installed(t)); diff != "" {
				t.Errorf("installed (-want +got):\n%s", diff)
			}
			if !f.fileExists("Q") {
				t.Errorf("Q's file was removed")
			}
		})
	}
}

// TestHardErrorPrecedenceOverCollectedOrphans checks that when an earlier step
// collects an orphan under OrphanFail and a later step hits a hard error,
// buildPlan and FromRepo return only the hard error without matching
// ErrOrphanedDependents.
func TestHardErrorPrecedenceOverCollectedOrphans(t *testing.T) {
	for _, tc := range []struct {
		name      string
		installed []goolib.PkgSpec
		available []goolib.PkgSpec
		mode      ConflictMode
		wantSub   string
	}{
		{
			name: "later forward conflict wins over earlier orphan",
			installed: []goolib.PkgSpec{
				{Name: "P", Arch: "noarch", Version: "1"},
				{Name: "D", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"P": "1"}},
				{Name: "Blocker", Arch: "noarch", Version: "1"},
			},
			available: []goolib.PkgSpec{
				{Name: "T", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"B": "1", "C": "1"}},
				{Name: "B", Arch: "noarch", Version: "1", Replaces: []string{"P"}},
				{Name: "C", Arch: "noarch", Version: "1", Conflicts: []string{"Blocker"}},
			},
			wantSub: "conflict with installed package or provider: Blocker",
		},
		{
			name: "later reverse conflict error wins over earlier orphan",
			installed: []goolib.PkgSpec{
				{Name: "P", Arch: "noarch", Version: "1"},
				{Name: "D", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"P": "1"}},
				{Name: "Blocker", Arch: "noarch", Version: "1", Conflicts: []string{"C"}},
			},
			available: []goolib.PkgSpec{
				{Name: "T", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"B": "1", "C": "1"}},
				{Name: "B", Arch: "noarch", Version: "1", Replaces: []string{"P"}},
				{Name: "C", Arch: "noarch", Version: "1"},
			},
			mode:    ConflictError,
			wantSub: "conflict with installed package Blocker.noarch.1",
		},
		{
			name: "later missing dependency wins over earlier orphan",
			installed: []goolib.PkgSpec{
				{Name: "P", Arch: "noarch", Version: "1"},
				{Name: "D", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"P": "1"}},
			},
			available: []goolib.PkgSpec{
				{Name: "T", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"B": "1", "Missing": "1"}},
				{Name: "B", Arch: "noarch", Version: "1", Replaces: []string{"P"}},
			},
			wantSub: "no package found satisfying Missing",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRelFixture(t, tc.installed, tc.available)
			err := f.installWith(t, Options{PackageConflicts: tc.mode, OrphanedDependents: OrphanFail}, "T")
			if err == nil || !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("install T = %v, want error containing %q", err, tc.wantSub)
			}
			if errors.Is(err, ErrOrphanedDependents) {
				t.Errorf("errors.Is(%v, ErrOrphanedDependents) = true, want false when a hard error occurred", err)
			}
		})
	}
}

// TestReplacerSelfOrphanIsHardErrorInBothModes checks that a replacer
// depending directly or transitively on its own replaced victim without
// satisfying it fails with a non-orphan hard error in both OrphanFail and
// OrphanRemove modes.
func TestReplacerSelfOrphanIsHardErrorInBothModes(t *testing.T) {
	for _, mode := range []OrphanMode{OrphanFail, OrphanRemove} {
		t.Run("direct/"+mode.String(), func(t *testing.T) {
			f := newRelFixture(t,
				[]goolib.PkgSpec{{Name: "P", Arch: "noarch", Version: "1"}},
				[]goolib.PkgSpec{{
					Name:            "R",
					Arch:            "noarch",
					Version:         "1",
					Replaces:        []string{"P"},
					PkgDependencies: map[string]string{"P": "1"},
				}})
			err := f.installWith(t, Options{OrphanedDependents: mode}, "R")
			want := "cannot install R.noarch.1: it depends on P.noarch.1, which it replaces and doesn't provide; nothing was changed"
			if err == nil || err.Error() != want {
				t.Fatalf("install R = %v, want exact error %q", err, want)
			}
			if errors.Is(err, ErrOrphanedDependents) {
				t.Errorf("self-orphan matched ErrOrphanedDependents")
			}
			if diff := cmp.Diff([]string{"P.noarch.1"}, f.installed(t)); diff != "" {
				t.Errorf("installed (-want +got):\n%s", diff)
			}
		})

		t.Run("transitive/"+mode.String(), func(t *testing.T) {
			f := newRelFixture(t,
				[]goolib.PkgSpec{
					{Name: "P", Arch: "noarch", Version: "1"},
					{Name: "D", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"P": "1"}},
				},
				[]goolib.PkgSpec{{
					Name:            "R",
					Arch:            "noarch",
					Version:         "1",
					Replaces:        []string{"P"},
					PkgDependencies: map[string]string{"D": "1"},
				}})
			err := f.installWith(t, Options{OrphanedDependents: mode}, "R")
			want := "cannot install R.noarch.1: it depends on D.noarch.1, which depends on a package R.noarch.1 replaces and isn't satisfied afterwards; nothing was changed"
			if err == nil || err.Error() != want {
				t.Fatalf("install R = %v, want exact error %q", err, want)
			}
			if errors.Is(err, ErrOrphanedDependents) {
				t.Errorf("transitive self-orphan matched ErrOrphanedDependents")
			}
			if diff := cmp.Diff([]string{"D.noarch.1", "P.noarch.1"}, f.installed(t)); diff != "" {
				t.Errorf("installed (-want +got):\n%s", diff)
			}
		})
	}
}

// TestDependentSatisfiedOnlyByLaterStepStillFailsPerStep checks that when an
// earlier step replaces P (breaking D) and a later step in the same run would
// install a provider of P, OrphanFail still treats D as orphaned at the
// replacing step and explains per-step evaluation in the error text.
func TestDependentSatisfiedOnlyByLaterStepStillFailsPerStep(t *testing.T) {
	f := newRelFixture(t,
		[]goolib.PkgSpec{
			{Name: "P", Arch: "noarch", Version: "1"},
			{Name: "D", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"P": "1"}},
		},
		[]goolib.PkgSpec{
			{Name: "T", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"B": "1", "Z": "1"}},
			{Name: "B", Arch: "noarch", Version: "1", Replaces: []string{"P"}},
			{Name: "Z", Arch: "noarch", Version: "1", Provides: []string{"P=1"}},
		})
	err := f.install(t, "T")
	if !errors.Is(err, ErrOrphanedDependents) {
		t.Fatalf("install T = %v, want ErrOrphanedDependents", err)
	}
	wantNote := "(evaluated at each replacement step before later packages in the same run are installed)"
	if !strings.Contains(err.Error(), "D.noarch.1") || !strings.Contains(err.Error(), wantNote) {
		t.Errorf("error = %q, want D.noarch.1 and %q", err.Error(), wantNote)
	}
	if diff := cmp.Diff([]string{"D.noarch.1", "P.noarch.1"}, f.installed(t)); diff != "" {
		t.Errorf("installed (-want +got):\n%s", diff)
	}
}

// TestOrphanFailMatchesOrphanRemoveProperty checks across randomized package
// graphs that every OrphanFail plan failing with OrphanedDependentsError
// succeeds under OrphanRemove, that non-orphan hard errors match identically
// between modes, and that the non-victim dependents removed by OrphanRemove
// equal the exact Dependents reported by OrphanFail.
func TestOrphanFailMatchesOrphanRemoveProperty(t *testing.T) {
	rng := rand.New(rand.NewPCG(316, 2026))
	var orphanHits, hardErrs, cleanSuccess int
	for iter := range 300 {
		seed := rng.Uint64()
		c := genCorpusCase(seed)
		f := newRelFixture(t, c.Installed, c.Available)
		state, err := f.db.FetchPkgs("")
		if err != nil {
			t.Fatal(err)
		}

		failPlan, failErr := buildPlan(planTarget{pi: goolib.PackageInfo{Name: c.Target}}, f.rm, state, corpusArchs, planPolicy{conflicts: c.Mode, orphans: OrphanFail}, &recordingSink{})
		remPlan, remErr := buildPlan(planTarget{pi: goolib.PackageInfo{Name: c.Target}}, f.rm, state, corpusArchs, planPolicy{conflicts: c.Mode, orphans: OrphanRemove}, &recordingSink{})

		if failErr == nil {
			cleanSuccess++
			if remErr != nil {
				t.Fatalf("iter %d seed %d: OrphanFail succeeded but OrphanRemove failed: %v", iter, seed, remErr)
			}
			if len(failPlan.steps) != len(remPlan.steps) {
				t.Fatalf("iter %d seed %d: step count mismatch (%d vs %d)", iter, seed, len(failPlan.steps), len(remPlan.steps))
			}
			continue
		}

		var oe *OrphanedDependentsError
		if !errors.As(failErr, &oe) {
			hardErrs++
			if remErr == nil || remErr.Error() != failErr.Error() {
				t.Fatalf("iter %d seed %d: non-orphan hard error mismatch: OrphanFail=%v vs OrphanRemove=%v", iter, seed, failErr, remErr)
			}
			continue
		}

		orphanHits++
		if remErr != nil {
			t.Fatalf("iter %d seed %d: OrphanFail returned OrphanedDependentsError (%v), but OrphanRemove failed: %v", iter, seed, failErr, remErr)
		}

		// Collect non-victim cascaded removals across all steps of remPlan.
		var listedSet []string
		for _, d := range oe.Dependents {
			listedSet = append(listedSet, d.Package.String())
		}
		slices.Sort(listedSet)
		listedSet = slices.Compact(listedSet)

		var cascadedSet []string
		work := slices.Clone(state)
		for _, step := range remPlan.steps {
			victims, vErr := victimsIn(step.spec.PackageSpec, work)
			if vErr != nil {
				t.Fatalf("iter %d seed %d: victimsIn: %v", iter, seed, vErr)
			}
			victimKeys := make(map[string]bool, len(victims))
			for _, v := range victims {
				victimKeys[pkgKey(v.PackageSpec)] = true
			}
			removedKeys := make(map[string]bool, len(step.removed))
			for _, r := range step.removed {
				rk := pkgKey(r.row.PackageSpec)
				removedKeys[rk] = true
				if !victimKeys[rk] {
					cascadedSet = append(cascadedSet, r.row.PackageSpec.String())
				}
			}
			selfKey := pkgKey(step.spec.PackageSpec)
			work = slices.DeleteFunc(work, func(st client.PackageState) bool {
				k := pkgKey(st.PackageSpec)
				return removedKeys[k] || k == selfKey
			})
			work = append(work, client.PackageState{PackageSpec: step.spec.PackageSpec})
		}
		slices.Sort(cascadedSet)
		cascadedSet = slices.Compact(cascadedSet)

		if diff := cmp.Diff(listedSet, cascadedSet); diff != "" {
			t.Fatalf("iter %d seed %d: OrphanFail listed dependents vs OrphanRemove cascaded removals (-listed +removed):\n%s", iter, seed, diff)
		}
	}
	if orphanHits == 0 || hardErrs == 0 || cleanSuccess == 0 {
		t.Fatalf("degenerate property distribution: orphanHits=%d hardErrs=%d cleanSuccess=%d", orphanHits, hardErrs, cleanSuccess)
	}
}

// TestOptionsFromSettingsAndPackageLevelFromRepoOrphanPolicy checks
// OptionsFromSettings fallback logging on invalid direct settings mutations and
// verifies that package-level FromRepo honours settings.OrphanedDependents.
func TestOptionsFromSettingsAndPackageLevelFromRepoOrphanPolicy(t *testing.T) {
	t.Cleanup(settings.Snapshot())

	settings.OrphanedDependents = "remove"
	if got := OptionsFromSettings(false, false).OrphanedDependents; got != OrphanRemove {
		t.Errorf("OptionsFromSettings(remove) = %v, want OrphanRemove", got)
	}

	logs := testutil.CaptureLogs(t)
	settings.OrphanedDependents = "bogus"
	if got := OptionsFromSettings(false, true).OrphanedDependents; got != OrphanFail {
		t.Errorf("OptionsFromSettings(bogus, force=true) = %v, want OrphanFail", got)
	}
	if !strings.Contains(logs.String(), "ERROR:") || !strings.Contains(logs.String(), `Invalid settings.OrphanedDependents: invalid orphaned dependents mode "bogus"`) {
		t.Errorf("logs missing invalid OptionsFromSettings error:\n%s", logs.String())
	}

	f := newRelFixture(t,
		[]goolib.PkgSpec{
			{Name: "P", Arch: "noarch", Version: "1"},
			{Name: "D", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"P": "1"}},
		},
		[]goolib.PkgSpec{
			{Name: "R", Arch: "noarch", Version: "1", Replaces: []string{"P"}},
		})
	settings.OrphanedDependents = "fail"
	err := FromRepo(t.Context(), goolib.PackageInfo{Name: "R"}, "", f.cache, f.rm, corpusArchs, false, true, f.downloader, f.db)
	if !errors.Is(err, ErrOrphanedDependents) {
		t.Fatalf("FromRepo(fail, force=true) = %v, want ErrOrphanedDependents", err)
	}

	settings.OrphanedDependents = "remove"
	if err := FromRepo(t.Context(), goolib.PackageInfo{Name: "R"}, "", f.cache, f.rm, corpusArchs, false, false, f.downloader, f.db); err != nil {
		t.Fatalf("FromRepo(remove): %v", err)
	}
	if diff := cmp.Diff([]string{"R.noarch.1"}, f.installed(t)); diff != "" {
		t.Errorf("installed (-want +got):\n%s", diff)
	}
}

// TestSiblingArchBelowMinRemovalReasonAndTransitiveProvidesOrphans covers
// version-gated sibling architectures, Plan.String removal reasons on
// version-gated replacements, and Provides-only dependents exposed transitively
// by cascading a name-based orphan.
func TestSiblingArchBelowMinRemovalReasonAndTransitiveProvidesOrphans(t *testing.T) {
	t.Run("remaining_sibling_arch_below_minimum_version_orphans_dependent", func(t *testing.T) {
		f := newRelFixture(t,
			[]goolib.PkgSpec{
				{Name: "P", Arch: "noarch", Version: "1"},
				{Name: "P", Arch: "x86_64", Version: "2"},
				{Name: "D", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"P": "2"}},
			},
			[]goolib.PkgSpec{
				{Name: "R", Arch: "noarch", Version: "1", Replaces: []string{"P.x86_64"}},
			})
		err := f.install(t, "R")
		if !errors.Is(err, ErrOrphanedDependents) || !strings.Contains(err.Error(), "D.noarch.1 (requires P >= 2, broken by removing P.x86_64.2 for R.noarch.1)") {
			t.Fatalf("install R (OrphanFail) = %v, want ErrOrphanedDependents for D.noarch.1", err)
		}
		if err := f.installWith(t, Options{OrphanedDependents: OrphanRemove}, "R"); err != nil {
			t.Fatalf("install R (OrphanRemove): %v", err)
		}
		if diff := cmp.Diff([]string{"P.noarch.1", "R.noarch.1"}, f.installed(t)); diff != "" {
			t.Errorf("installed (-want +got):\n%s", diff)
		}
	})

	t.Run("plan_string_distinguishes_version_gated_victim_from_cascaded_sibling_arch", func(t *testing.T) {
		f := newRelFixture(t,
			[]goolib.PkgSpec{
				{Name: "P", Arch: "noarch", Version: "3"},
				{Name: "P", Arch: "x86_64", Version: "1", PkgDependencies: map[string]string{"P.noarch": "1"}},
			},
			[]goolib.PkgSpec{
				{Name: "R", Arch: "noarch", Version: "1", Replaces: []string{"P..2"}},
			})
		p, err := New(f.cache, f.db, Options{OrphanedDependents: OrphanRemove}).Plan(goolib.PackageInfo{Name: "R"}, "", f.rm, corpusArchs)
		if err != nil {
			t.Fatalf("Plan: %v", err)
		}
		wantRemovals := "The following packages will be removed:\n" +
			"  P.x86_64.1 (depends on a package R.noarch.1 replaces)\n" +
			"  P.noarch.3 (replaced by R.noarch.1)\n"
		if !strings.Contains(p.String(), wantRemovals) {
			t.Errorf("Plan.String() = %q, want substring %q", p.String(), wantRemovals)
		}
	})

	t.Run("cascaded_byname_orphan_exposing_provides_dependent_lists_and_keeps_it", func(t *testing.T) {
		f := newRelFixture(t,
			[]goolib.PkgSpec{
				{Name: "P", Arch: "noarch", Version: "1"},
				{Name: "D", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"P": "1"}, Provides: []string{"virt=2"}},
				{Name: "Q", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"virt": "1"}},
			},
			[]goolib.PkgSpec{
				{Name: "R", Arch: "noarch", Version: "1", Replaces: []string{"P"}},
			})
		ins := New(f.cache, f.db, Options{OrphanedDependents: OrphanRemove})
		p, err := ins.Plan(goolib.PackageInfo{Name: "R"}, "", f.rm, corpusArchs)
		if err != nil {
			t.Fatalf("Plan: %v", err)
		}
		wantListed := "The following packages will have an unsatisfied dependency (not removed):\n  Q.noarch.1 (requires virt >= 1, provided by D.noarch.1; replaced by R.noarch.1)\n"
		if !strings.Contains(p.String(), wantListed) {
			t.Errorf("Plan.String() = %q, want substring %q", p.String(), wantListed)
		}
		logs := testutil.CaptureLogs(t)
		stdout := captureStdout(t, func() {
			err = ins.FromRepo(t.Context(), goolib.PackageInfo{Name: "R"}, "", f.rm, corpusArchs, f.downloader)
		})
		if err != nil {
			t.Fatalf("FromRepo: %v", err)
		}
		if !strings.Contains(stdout, wantListed) {
			t.Errorf("stdout = %q, want substring %q", stdout, wantListed)
		}
		if !strings.Contains(logs.String(), `dependency_unsatisfied op=replace result=listed dependent=Q.noarch.1 requirement="virt >= 1" removed=D.noarch.1 replacer=R.noarch.1`) {
			t.Errorf("logs missing listed warning:\n%s", logs.String())
		}
		if diff := cmp.Diff([]string{"Q.noarch.1", "R.noarch.1"}, f.installed(t)); diff != "" {
			t.Errorf("installed (-want +got):\n%s", diff)
		}
	})

	t.Run("dependent_with_both_byname_and_provides_dependencies_is_not_listed_as_kept", func(t *testing.T) {
		f := newRelFixture(t,
			[]goolib.PkgSpec{
				{Name: "P", Arch: "noarch", Version: "1", Provides: []string{"virt=2"}},
				{Name: "D", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"P": "1", "virt": "1"}},
			},
			[]goolib.PkgSpec{
				{Name: "R", Arch: "noarch", Version: "1", Replaces: []string{"P"}},
			})
		ins := New(f.cache, f.db, Options{OrphanedDependents: OrphanRemove})
		p, err := ins.Plan(goolib.PackageInfo{Name: "R"}, "", f.rm, corpusArchs)
		if err != nil {
			t.Fatalf("Plan: %v", err)
		}
		if strings.Contains(p.String(), "(not removed)") {
			t.Errorf("Plan.String() = %q, want removed D omitted from (not removed) section", p.String())
		}
	})
}

// TestApplyTimeRemovalSetDriftOnNewlyAppearingOrphan checks that if a new
// dependent is written to the database between planning and applying a
// replacement step, checkRemovals aborts with errPlanDivergence before any
// package is uninstalled.
func TestApplyTimeRemovalSetDriftOnNewlyAppearingOrphan(t *testing.T) {
	f := newRelFixture(t,
		[]goolib.PkgSpec{{Name: "P", Arch: "noarch", Version: "1"}},
		[]goolib.PkgSpec{{Name: "R", Arch: "noarch", Version: "1", Replaces: []string{"P"}}},
	)
	logs := testutil.CaptureLogs(t)
	var uninstalled []string
	err := f.runWithHook(t, t.Context(), Options{OrphanedDependents: OrphanFail}, "R", func(stage applyStage, step string) error {
		if stage == stageFetched {
			return f.db.AddPkg(client.PackageState{
				PackageSpec: &goolib.PkgSpec{
					Name:            "LateDep",
					Arch:            "noarch",
					Version:         "1",
					PkgDependencies: map[string]string{"P": "1"},
				},
			})
		}
		if stage == stageBeforeUninstall {
			uninstalled = append(uninstalled, step)
		}
		return nil
	})
	if !errors.Is(err, errPlanDivergence) {
		t.Fatalf("runWithHook = %v, want errPlanDivergence", err)
	}
	if len(uninstalled) != 0 {
		t.Errorf("uninstalled = %v, want empty before divergence abort", uninstalled)
	}
	if !strings.Contains(logs.String(), "plan_divergence step=R.noarch.1 check=removal-set") {
		t.Errorf("logs missing removal-set plan_divergence:\n%s", logs.String())
	}
	if diff := cmp.Diff([]string{"LateDep.noarch.1", "P.noarch.1"}, f.installed(t)); diff != "" {
		t.Errorf("installed (-want +got):\n%s", diff)
	}
}

// TestMultiStepOrphanCollectionAndCycleHardErrorPrecedence checks that
// OrphanFail aggregates orphans across multiple replacement steps with each
// step's own ReplacedBy attribution, while later cycle or self-orphan hard
// errors still take precedence and discard earlier orphans.
func TestMultiStepOrphanCollectionAndCycleHardErrorPrecedence(t *testing.T) {
	t.Run("aggregates_orphans_across_multiple_replacement_steps", func(t *testing.T) {
		f := newRelFixture(t,
			[]goolib.PkgSpec{
				{Name: "P1", Arch: "noarch", Version: "1"},
				{Name: "D1", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"P1": ""}},
				{Name: "P2", Arch: "noarch", Version: "1"},
				{Name: "D2", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"P2.noarch": ""}},
			},
			[]goolib.PkgSpec{
				{Name: "T", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"B": "1", "C": "1"}},
				{Name: "B", Arch: "noarch", Version: "1", Replaces: []string{"P1"}},
				{Name: "C", Arch: "noarch", Version: "1", Replaces: []string{"P2"}},
			})
		err := f.install(t, "T")
		var oe *OrphanedDependentsError
		if !errors.As(err, &oe) {
			t.Fatalf("install T = %v, want *OrphanedDependentsError", err)
		}
		if len(oe.Dependents) != 2 {
			t.Fatalf("Dependents = %+v, want 2 entries", oe.Dependents)
		}
		if oe.Dependents[0].Package.String() != "D1.noarch.1" || oe.Dependents[0].ReplacedBy.String() != "B.noarch.1" {
			t.Errorf("Dependents[0] = %+v, want D1.noarch.1 replaced by B.noarch.1", oe.Dependents[0])
		}
		if oe.Dependents[1].Package.String() != "D2.noarch.1" || oe.Dependents[1].ReplacedBy.String() != "C.noarch.1" {
			t.Errorf("Dependents[1] = %+v, want D2.noarch.1 replaced by C.noarch.1", oe.Dependents[1])
		}
		if !strings.Contains(err.Error(), "D1.noarch.1 (requires P1,") || !strings.Contains(err.Error(), "D2.noarch.1 (requires P2.noarch,") {
			t.Errorf("error = %q, want unversioned requirement formatting for P1 and P2.noarch", err.Error())
		}
	})

	t.Run("later_dependency_cycle_wins_over_earlier_orphan", func(t *testing.T) {
		f := newRelFixture(t,
			[]goolib.PkgSpec{
				{Name: "P", Arch: "noarch", Version: "1"},
				{Name: "D", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"P": "1"}},
			},
			[]goolib.PkgSpec{
				{Name: "T", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"B": "1", "X": "1"}},
				{Name: "B", Arch: "noarch", Version: "1", Replaces: []string{"P"}},
				{Name: "X", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"Y": "1"}},
				{Name: "Y", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"X": "1"}},
			})
		err := f.install(t, "T")
		if err == nil || !strings.Contains(err.Error(), "dependency cycle") {
			t.Fatalf("install T = %v, want dependency cycle error", err)
		}
		if errors.Is(err, ErrOrphanedDependents) {
			t.Errorf("errors.Is(%v, ErrOrphanedDependents) = true, want false", err)
		}
	})

	t.Run("later_non_victim_dependent_cycle_wins_over_earlier_orphan", func(t *testing.T) {
		f := newRelFixture(t,
			[]goolib.PkgSpec{
				{Name: "P1", Arch: "noarch", Version: "1"},
				{Name: "D1", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"P1": "1"}},
				{Name: "P2", Arch: "noarch", Version: "1"},
				{Name: "C1", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"P2": "1", "C2": "1"}},
				{Name: "C2", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"C1": "1"}},
			},
			[]goolib.PkgSpec{
				{Name: "T", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"B": "1", "C": "1"}},
				{Name: "B", Arch: "noarch", Version: "1", Replaces: []string{"P1"}},
				{Name: "C", Arch: "noarch", Version: "1", Replaces: []string{"P2"}},
			})
		err := f.install(t, "T")
		if err == nil || !strings.Contains(err.Error(), "C1.noarch, C2.noarch") {
			t.Fatalf("install T = %v, want cycle error naming C1.noarch, C2.noarch", err)
		}
		if errors.Is(err, ErrOrphanedDependents) {
			t.Errorf("errors.Is(%v, ErrOrphanedDependents) = true, want false", err)
		}
	})

	t.Run("later_step_projects_remove_state_so_cascaded_provider_exposes_downstream_orphan", func(t *testing.T) {
		// Step 1 (B replacing P1) cascades D1, which also provides P2=1. When
		// Step 2 (C replacing P2) runs against the projected remove-mode state,
		// D1 is already gone, so E (requiring P2 >= 1) is also reported in
		// OrphanFail, matching the exact set OrphanRemove uninstalls.
		f := newRelFixture(t,
			[]goolib.PkgSpec{
				{Name: "P1", Arch: "noarch", Version: "1"},
				{Name: "D1", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"P1": "1"}, Provides: []string{"P2=1"}},
				{Name: "P2", Arch: "noarch", Version: "1"},
				{Name: "E", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"P2": "1"}},
			},
			[]goolib.PkgSpec{
				{Name: "T", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"B": "1", "C": "1"}},
				{Name: "B", Arch: "noarch", Version: "1", Replaces: []string{"P1"}},
				{Name: "C", Arch: "noarch", Version: "1", Replaces: []string{"P2"}},
			})
		err := f.install(t, "T")
		var oe *OrphanedDependentsError
		if !errors.As(err, &oe) {
			t.Fatalf("install T (OrphanFail) = %v, want *OrphanedDependentsError", err)
		}
		var gotOrphans []string
		for _, d := range oe.Dependents {
			gotOrphans = append(gotOrphans, d.Package.String())
		}
		if diff := cmp.Diff([]string{"D1.noarch.1", "E.noarch.1"}, gotOrphans); diff != "" {
			t.Errorf("OrphanFail reported dependents (-want +got):\n%s", diff)
		}
		if err := f.installWith(t, Options{OrphanedDependents: OrphanRemove}, "T"); err != nil {
			t.Fatalf("install T (OrphanRemove): %v", err)
		}
		if diff := cmp.Diff([]string{"B.noarch.1", "C.noarch.1", "T.noarch.1"}, f.installed(t)); diff != "" {
			t.Errorf("installed after OrphanRemove (-want +got):\n%s", diff)
		}
	})

	t.Run("later_step_projects_remove_state_and_surfaces_unresolvable_dependency_hard_error", func(t *testing.T) {
		f := newRelFixture(t,
			[]goolib.PkgSpec{
				{Name: "P", Arch: "noarch", Version: "1"},
				{Name: "LocalOnly", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"P": "1"}},
			},
			[]goolib.PkgSpec{
				{Name: "T", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"B": "1", "LocalOnly": "1"}},
				{Name: "B", Arch: "noarch", Version: "1", Replaces: []string{"P"}},
			})
		failErr := f.install(t, "T")
		if failErr == nil || !strings.Contains(failErr.Error(), "no package found satisfying LocalOnly") {
			t.Fatalf("install T (OrphanFail) = %v, want missing LocalOnly hard error", failErr)
		}
		if errors.Is(failErr, ErrOrphanedDependents) {
			t.Errorf("errors.Is(%v, ErrOrphanedDependents) = true, want false", failErr)
		}
		remErr := f.installWith(t, Options{OrphanedDependents: OrphanRemove}, "T")
		if remErr == nil || remErr.Error() != failErr.Error() {
			t.Errorf("OrphanRemove error = %v, want exact match with OrphanFail %v", remErr, failErr)
		}
	})

	t.Run("overlapping_replaces_entries_name_deduplicated_victim_once_on_dependent_cycle", func(t *testing.T) {
		f := newRelFixture(t,
			[]goolib.PkgSpec{
				{Name: "P", Arch: "noarch", Version: "2"},
				{Name: "C", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"P": "1", "D": "1"}},
				{Name: "D", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"C": "1"}},
			},
			[]goolib.PkgSpec{
				{Name: "R", Arch: "noarch", Version: "1", Replaces: []string{"P", "P.noarch"}},
			})
		err := f.install(t, "R")
		if err == nil || !strings.Contains(err.Error(), "cannot remove P.noarch: these packages depend on each other in a cycle: C.noarch, D.noarch") {
			t.Fatalf("install R = %v, want deduplicated 'cannot remove P.noarch:' cycle error", err)
		}
	})

	t.Run("dotted_removed_package_missing_archive_hint_includes_arch", func(t *testing.T) {
		f := newRelFixture(t,
			[]goolib.PkgSpec{
				{Name: "V", Arch: "noarch", Version: "1"},
				{Name: "foo.bar", Arch: "x86_64", Version: "1", PkgDependencies: map[string]string{"V": "1"}},
			},
			[]goolib.PkgSpec{
				{Name: "R", Arch: "noarch", Version: "1", Replaces: []string{"V"}},
			})
		fbx, err := f.db.FetchPkg(goolib.PackageInfo{Name: "foo.bar", Arch: "x86_64"})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(fbx.LocalPath); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(filepath.Join(f.gooDir, filepath.Base(fbx.LocalPath))); err != nil {
			t.Fatal(err)
		}
		err = f.installWith(t, Options{OrphanedDependents: OrphanRemove}, "R")
		want := `remove foo.bar.x86_64 first with "googet remove -db_only foo.bar.x86_64", which doesn't run its uninstall script`
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("install R (dotted cascaded orphan missing archive) = %v, want substring %q", err, want)
		}
	})
}
