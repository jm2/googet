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
	"bytes"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/google/googet/v2/client"
	"github.com/google/googet/v2/googetdb"
	"github.com/google/googet/v2/goolib"
	"github.com/google/googet/v2/internal/relation"
	"github.com/google/googet/v2/testutil"
)

// TestSelfExclusionUpgradeAndReinstall checks that an incoming package does
// not conflict with its own installed row of the same name and architecture
// when upgrading from a repository, upgrading from disk, or reinstalling the
// same version from disk.
func TestSelfExclusionUpgradeAndReinstall(t *testing.T) {
	for _, tc := range []struct {
		name     string
		spec     func(ver string) goolib.PkgSpec
		wantSkip string
	}{
		{
			name: "provides and conflicts virtual name",
			spec: func(ver string) goolib.PkgSpec {
				return goolib.PkgSpec{Name: "A", Arch: "noarch", Version: ver, Provides: []string{"X"}, Conflicts: []string{"X"}}
			},
			wantSkip: `conflict "X" with A.noarch.1 skipped: installing A.noarch.2 removes it`,
		},
		{
			name: "conflicts own package name",
			spec: func(ver string) goolib.PkgSpec {
				return goolib.PkgSpec{Name: "A", Arch: "noarch", Version: ver, Conflicts: []string{"A"}}
			},
			wantSkip: `conflict "A" with A.noarch.1 skipped: installing A.noarch.2 removes it`,
		},
	} {
		t.Run(tc.name+"/FromRepo", func(t *testing.T) {
			logs := testutil.CaptureLogs(t)
			f := newRelFixture(t, []goolib.PkgSpec{tc.spec("1")}, []goolib.PkgSpec{tc.spec("2")})
			if err := f.install(t, "A"); err != nil {
				t.Fatalf("install A: %v", err)
			}
			if diff := cmp.Diff([]string{"A.noarch.2"}, f.installed(t)); diff != "" {
				t.Errorf("installed packages (-want +got):\n%s", diff)
			}
			if got := strings.Count(logs.String(), tc.wantSkip); got != 1 {
				t.Errorf("log count for %q = %d, want 1; logs:\n%s", tc.wantSkip, got, logs.String())
			}
		})

		t.Run(tc.name+"/FromDisk_upgrade", func(t *testing.T) {
			f := newRelFixture(t, []goolib.PkgSpec{tc.spec("1")}, nil)
			dir := t.TempDir()
			rs := testutil.GenGoo(t, dir, f.dst, tc.spec("2"))
			if err := FromDisk(filepath.Join(dir, rs.Source), f.cache, false, false, false, f.db); err != nil {
				t.Fatalf("FromDisk upgrade: %v", err)
			}
			if diff := cmp.Diff([]string{"A.noarch.2"}, f.installed(t)); diff != "" {
				t.Errorf("installed packages (-want +got):\n%s", diff)
			}
		})

		t.Run(tc.name+"/FromDisk_reinstall_same_version", func(t *testing.T) {
			f := newRelFixture(t, []goolib.PkgSpec{tc.spec("1")}, nil)
			dir := t.TempDir()
			rs := testutil.GenGoo(t, dir, f.dst, tc.spec("1"))
			if err := FromDisk(filepath.Join(dir, rs.Source), f.cache, false, false, true, f.db); err != nil {
				t.Fatalf("FromDisk reinstall: %v", err)
			}
			if diff := cmp.Diff([]string{"A.noarch.1"}, f.installed(t)); diff != "" {
				t.Errorf("installed packages (-want +got):\n%s", diff)
			}
		})
	}
}

// TestSelfExclusionOtherArchProviderStillConflicts checks that self-exclusion
// keys by name.arch, so another installed architecture providing the conflicted
// name still blocks installation.
func TestSelfExclusionOtherArchProviderStillConflicts(t *testing.T) {
	for _, tc := range []struct {
		name      string
		installed []goolib.PkgSpec
		wantState []string
	}{
		{
			name: "other arch only",
			installed: []goolib.PkgSpec{
				{Name: "A", Arch: "x86_64", Version: "1", Provides: []string{"X"}},
			},
			wantState: []string{"A.x86_64.1"},
		},
		{
			name: "both same arch and other arch",
			installed: []goolib.PkgSpec{
				{Name: "A", Arch: "noarch", Version: "1", Provides: []string{"X"}},
				{Name: "A", Arch: "x86_64", Version: "1", Provides: []string{"X"}},
			},
			wantState: []string{"A.noarch.1", "A.x86_64.1"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRelFixture(t, tc.installed, []goolib.PkgSpec{
				{Name: "A", Arch: "noarch", Version: "2", Provides: []string{"X"}, Conflicts: []string{"X"}},
			})
			err := f.install(t, "A")
			wantErr := "cannot install, conflict with installed package or provider: X"
			if err == nil || !strings.Contains(err.Error(), wantErr) {
				t.Fatalf("install A = %v, want error containing %q", err, wantErr)
			}
			if diff := cmp.Diff(tc.wantState, f.installed(t)); diff != "" {
				t.Errorf("installed packages (-want +got):\n%s", diff)
			}
		})
	}
}

// TestSelfExclusionOtherArchNameFallbackStillConflicts checks that when
// FetchPkg picks the excluded same-arch row on the name path, another
// installed architecture matching the conflict still blocks the install.
func TestSelfExclusionOtherArchNameFallbackStillConflicts(t *testing.T) {
	f := newRelFixture(t,
		[]goolib.PkgSpec{
			{Name: "A", Arch: "noarch", Version: "1"},
			{Name: "A", Arch: "x86_64", Version: "1"},
		},
		[]goolib.PkgSpec{
			{Name: "A", Arch: "noarch", Version: "2", Conflicts: []string{"A"}},
		})
	err := f.install(t, "A")
	wantErr := "cannot install, conflict with installed package or provider: A"
	if err == nil || !strings.Contains(err.Error(), wantErr) {
		t.Fatalf("install A = %v, want error containing %q", err, wantErr)
	}
	if diff := cmp.Diff([]string{"A.noarch.1", "A.x86_64.1"}, f.installed(t)); diff != "" {
		t.Errorf("installed packages (-want +got):\n%s", diff)
	}
}

// TestDependenciesIgnoreSelfExclusion checks that dependency satisfaction
// continues to consider the installed version of the package being upgraded.
func TestDependenciesIgnoreSelfExclusion(t *testing.T) {
	f := newRelFixture(t,
		[]goolib.PkgSpec{
			{Name: "A", Arch: "noarch", Version: "1", Provides: []string{"X"}},
		},
		[]goolib.PkgSpec{
			{Name: "A", Arch: "noarch", Version: "2", PkgDependencies: map[string]string{"X": "0.0.0"}},
		})
	if err := f.install(t, "A"); err != nil {
		t.Fatalf("install A 2: %v", err)
	}
	if diff := cmp.Diff([]string{"A.noarch.2"}, f.installed(t)); diff != "" {
		t.Errorf("installed packages (-want +got):\n%s", diff)
	}
}

// TestCheckForwardConflictPure exercises checkForwardConflict directly on
// in-memory PkgSpec slices without a database.
func TestCheckForwardConflictPure(t *testing.T) {
	aNoarch := &goolib.PkgSpec{Name: "A", Arch: "noarch", Version: "1", Provides: []string{"virt=2.0"}}
	aX64 := &goolib.PkgSpec{Name: "A", Arch: "x86_64", Version: "1", Provides: []string{"virt=2.0"}}
	exNoarch := exclusions{self: "A.noarch"}

	for _, tc := range []struct {
		name      string
		rel       string
		installed []*goolib.PkgSpec
		ex        exclusions
		wantHit   bool
		wantEx    []string
	}{
		{
			name:      "unexcluded name match hits",
			rel:       "A",
			installed: []*goolib.PkgSpec{aNoarch},
			wantHit:   true,
		},
		{
			name:      "excluded name match with no other arch skips",
			rel:       "A",
			installed: []*goolib.PkgSpec{aNoarch},
			ex:        exNoarch,
			wantEx:    []string{"A.noarch.1"},
		},
		{
			name:      "excluded name match falls back to other arch",
			rel:       "A",
			installed: []*goolib.PkgSpec{aNoarch, aX64},
			ex:        exNoarch,
			wantHit:   true,
			wantEx:    []string{"A.noarch.1"},
		},
		{
			name:      "excluded provider with no other provider skips",
			rel:       "virt.noarch.1.0",
			installed: []*goolib.PkgSpec{aNoarch},
			ex:        exNoarch,
			wantEx:    []string{"A.noarch.1"},
		},
		{
			name:      "excluded provider still hits unexcluded provider",
			rel:       "virt.noarch.1.0",
			installed: []*goolib.PkgSpec{aNoarch, aX64},
			ex:        exNoarch,
			wantHit:   true,
			wantEx:    []string{"A.noarch.1"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hit, exempted, err := checkForwardConflict(relation.ParseRelation(tc.rel), tc.installed, tc.ex)
			if err != nil {
				t.Fatalf("checkForwardConflict(%q): %v", tc.rel, err)
			}
			if hit != tc.wantHit {
				t.Errorf("hit = %t, want %t", hit, tc.wantHit)
			}
			var gotEx []string
			for _, p := range exempted {
				gotEx = append(gotEx, p.String())
			}
			if diff := cmp.Diff(tc.wantEx, gotEx); diff != "" {
				t.Errorf("exempted (-want +got):\n%s", diff)
			}
		})
	}
}

// TestReplacesExemptsVictimFromForwardConflict checks that a repository
// install exempts the replaced package from the incoming package's Conflicts,
// both by package name and via Provides, including during an in-place upgrade.
func TestReplacesExemptsVictimFromForwardConflict(t *testing.T) {
	t.Run("conflicts virtual name provided by victim", func(t *testing.T) {
		logs := testutil.CaptureLogs(t)
		f := newRelFixture(t,
			[]goolib.PkgSpec{{Name: "P", Arch: "noarch", Version: "1", Provides: []string{"virt"}}},
			[]goolib.PkgSpec{{Name: "R", Arch: "noarch", Version: "1", Conflicts: []string{"virt"}, Replaces: []string{"P"}, Provides: []string{"virt"}}})
		if err := f.install(t, "R"); err != nil {
			t.Fatalf("install R: %v", err)
		}
		if diff := cmp.Diff([]string{"R.noarch.1"}, f.installed(t)); diff != "" {
			t.Errorf("installed packages (-want +got):\n%s", diff)
		}
		if f.fileExists("P") {
			t.Errorf("P's file still exists after replacement")
		}
		wantSkip := `conflict "virt" with P.noarch.1 skipped: installing R.noarch.1 removes it`
		if got := strings.Count(logs.String(), wantSkip); got != 1 {
			t.Errorf("log count for %q = %d, want 1; logs:\n%s", wantSkip, got, logs.String())
		}
	})

	t.Run("upgrade newly declaring Conflicts and Replaces on installed X", func(t *testing.T) {
		f := newRelFixture(t,
			[]goolib.PkgSpec{
				{Name: "R", Arch: "noarch", Version: "1"},
				{Name: "X", Arch: "noarch", Version: "1"},
			},
			[]goolib.PkgSpec{
				{Name: "R", Arch: "noarch", Version: "2", Conflicts: []string{"X"}, Replaces: []string{"X"}},
			})
		if err := f.install(t, "R"); err != nil {
			t.Fatalf("upgrade R: %v", err)
		}
		if diff := cmp.Diff([]string{"R.noarch.2"}, f.installed(t)); diff != "" {
			t.Errorf("installed packages (-want +got):\n%s", diff)
		}
		if f.fileExists("X") {
			t.Errorf("X's file still exists after upgrade replacement")
		}
	})
}

// TestReplacesExemptsOnlyConcreteVictim checks that when a replaced package is
// installed for two architectures, an arch-less Replaces entry exempts and
// removes both architectures, whereas an arch-qualified Replaces entry exempts
// only its matching architecture so the remaining architecture still blocks the
// install before any removal happens.
func TestReplacesExemptsOnlyConcreteVictim(t *testing.T) {
	t.Run("arch-less Replaces exempts and removes all matching arches", func(t *testing.T) {
		f := newRelFixture(t,
			[]goolib.PkgSpec{
				{Name: "P", Arch: "noarch", Version: "1"},
				{Name: "P", Arch: "x86_64", Version: "1"},
			},
			[]goolib.PkgSpec{
				{Name: "R", Arch: "noarch", Version: "1", Conflicts: []string{"P"}, Replaces: []string{"P"}},
			})
		if err := f.install(t, "R"); err != nil {
			t.Fatalf("install R: %v", err)
		}
		if diff := cmp.Diff([]string{"R.noarch.1"}, f.installed(t)); diff != "" {
			t.Errorf("installed packages (-want +got):\n%s", diff)
		}
		if f.fileExists("P") {
			t.Errorf("P's file still exists after both P rows were replaced")
		}
	})

	t.Run("arch-qualified Replaces exempts only matching arch", func(t *testing.T) {
		f := newRelFixture(t,
			[]goolib.PkgSpec{
				{Name: "P", Arch: "noarch", Version: "1"},
				{Name: "P", Arch: "x86_64", Version: "1"},
			},
			[]goolib.PkgSpec{
				{Name: "R", Arch: "noarch", Version: "1", Conflicts: []string{"P"}, Replaces: []string{"P.noarch"}},
			})
		err := f.install(t, "R")
		wantErr := "cannot install, conflict with installed package or provider: P"
		if err == nil || !strings.Contains(err.Error(), wantErr) {
			t.Fatalf("install R = %v, want error containing %q", err, wantErr)
		}
		if diff := cmp.Diff([]string{"P.noarch.1", "P.x86_64.1"}, f.installed(t)); diff != "" {
			t.Errorf("installed packages (-want +got):\n%s", diff)
		}
		if !f.fileExists("P") {
			t.Errorf("P's file was removed before conflict check aborted")
		}

		// Also verify the post-replace defence-in-depth recheck when a deferred
		// requirement remains satisfied in the database.
		rSpec := &goolib.PkgSpec{Name: "R", Arch: "noarch", Version: "1", Conflicts: []string{"P"}, Replaces: []string{"P.noarch"}}
		def := deferredChecks{fwd: []relation.Requirement{relation.ParseRelation("P")}}
		err = def.recheck(rSpec, ConflictWarn, f.db)
		wantPostErr := "cannot install, conflict with installed package or provider: P (still installed after replacing)"
		if err == nil || err.Error() != wantPostErr {
			t.Errorf("deferred.recheck = %v, want exact error %q", err, wantPostErr)
		}
	})
}

// TestReplacesDoesNotExemptCascadedDependents checks that a dependent of a
// replaced package is not exempted from Conflicts.
func TestReplacesDoesNotExemptCascadedDependents(t *testing.T) {
	f := newRelFixture(t,
		[]goolib.PkgSpec{
			{Name: "P", Arch: "noarch", Version: "1"},
			{Name: "D", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"P": "1"}},
		},
		[]goolib.PkgSpec{
			{Name: "R", Arch: "noarch", Version: "1", Conflicts: []string{"D"}, Replaces: []string{"P"}},
		})
	err := f.install(t, "R")
	wantErr := "cannot install, conflict with installed package or provider: D"
	if err == nil || !strings.Contains(err.Error(), wantErr) {
		t.Fatalf("install R = %v, want error containing %q", err, wantErr)
	}
	if diff := cmp.Diff([]string{"D.noarch.1", "P.noarch.1"}, f.installed(t)); diff != "" {
		t.Errorf("installed packages (-want +got):\n%s", diff)
	}
}

// TestForwardConflictRecheckAfterReplacements checks that if a dependency
// installs a new conflicting row that Replaces does not remove, the install
// fails before resolveReplacements uninstalls the victim or downloads the root
// package, and that the post-replace defence-in-depth recheck still fails if a
// victim survives replacement.
func TestForwardConflictRecheckAfterReplacements(t *testing.T) {
	f := newRelFixture(t,
		[]goolib.PkgSpec{
			{Name: "P", Arch: "noarch", Version: "1"},
		},
		[]goolib.PkgSpec{
			{Name: "P", Arch: "x86_64", Version: "1"},
			{
				Name:            "R",
				Arch:            "noarch",
				Version:         "1",
				Conflicts:       []string{"P"},
				Replaces:        []string{"P.noarch"},
				PkgDependencies: map[string]string{"P.x86_64": "1"},
			},
		})
	// Corrupt R's archive so any attempt to download R would fail with a
	// checksum error instead of the conflict error.
	f.corrupt(t, "R")
	err := f.install(t, "R")
	wantErr := "cannot install, conflict with installed package or provider: P"
	if err == nil || err.Error() != wantErr {
		t.Errorf("install R = %v, want exact error %q", err, wantErr)
	}
	// The install fails while it is planned, so R's dependency P.x86_64 isn't
	// installed either.
	if diff := cmp.Diff([]string{"P.noarch.1"}, f.installed(t)); diff != "" {
		t.Errorf("installed packages (-want +got):\n%s", diff)
	}
	if !f.fileExists("P") {
		t.Errorf("P's file was removed before pre-replace conflict check aborted")
	}

	// Also verify the post-replace defence-in-depth recheck when a deferred
	// requirement remains satisfied in the database.
	if err := f.db.WriteStateToDB(client.GooGetState{{PackageSpec: &goolib.PkgSpec{Name: "P", Arch: "x86_64", Version: "1"}}}); err != nil {
		t.Fatal(err)
	}
	rSpec := &goolib.PkgSpec{Name: "R", Arch: "noarch", Version: "1", Conflicts: []string{"P"}, Replaces: []string{"P.noarch"}}
	def := deferredChecks{fwd: []relation.Requirement{relation.ParseRelation("P")}}
	err = def.recheck(rSpec, ConflictWarn, f.db)
	wantPostErr := "cannot install, conflict with installed package or provider: P (still installed after replacing)"
	if err == nil || err.Error() != wantPostErr {
		t.Errorf("deferred.recheck = %v, want exact error %q", err, wantPostErr)
	}

	// Arch-less Replaces ["P"] removes both installed P.x86_64 and newly
	// installed dependency P.noarch along with X, leaving only R.noarch.1.
	f2 := newRelFixture(t,
		[]goolib.PkgSpec{
			{Name: "P", Arch: "x86_64", Version: "1"},
			{Name: "X", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"P": "1"}},
		},
		[]goolib.PkgSpec{
			{Name: "P", Arch: "noarch", Version: "1"},
			{
				Name:            "R",
				Arch:            "noarch",
				Version:         "1",
				Conflicts:       []string{"P"},
				Replaces:        []string{"P"},
				PkgDependencies: map[string]string{"P.noarch": "1"},
			},
		})
	if err := f2.install(t, "R"); err != nil {
		t.Errorf("install R (arch-less Replaces removes both P arches): %v", err)
	}
	if diff := cmp.Diff([]string{"R.noarch.1"}, f2.installed(t)); diff != "" {
		t.Errorf("installed packages after arch-less Replaces (-want +got):\n%s", diff)
	}

	// When a pre-replacement FetchPkg pick (foo.x86_64 0.5.0) is below the
	// conflict's minimum version and is removed by B's dependent cascade while
	// B's provider exempted the entry, the post-replacement FetchPkg pick
	// shifts to surviving foo.arm64 3.0.0; the pre-check must evaluate the
	// post-replacement pick and abort before B or foo.x86_64 is removed.
	f3 := newRelFixture(t,
		[]goolib.PkgSpec{
			{Name: "B", Arch: "noarch", Version: "1", Provides: []string{"foo=2.0.0"}},
			{Name: "foo", Arch: "arm64", Version: "3.0.0"},
			{Name: "foo", Arch: "x86_64", Version: "0.5.0", PkgDependencies: map[string]string{"B": "1"}},
		},
		[]goolib.PkgSpec{
			{Name: "R", Arch: "noarch", Version: "1", Conflicts: []string{"foo..1.0.0"}, Replaces: []string{"B"}},
		})
	err = f3.install(t, "R")
	wantPickDriftErr := "cannot install, conflict with installed package or provider: foo"
	if err == nil || err.Error() != wantPickDriftErr {
		t.Errorf("install R (post-replacement FetchPkg pick shift) = %v, want exact error %q", err, wantPickDriftErr)
	}
	if diff := cmp.Diff([]string{"B.noarch.1", "foo.arm64.3.0.0", "foo.x86_64.0.5.0"}, f3.installed(t)); diff != "" {
		t.Errorf("installed packages after post-replacement pick shift (-want +got):\n%s", diff)
	}
	if !f3.fileExists("B") {
		t.Errorf("B's file was removed before pre-replace forward conflict check aborted")
	}
}

// TestRecheckBeforeReplacementsLogging checks that clean pre-replacement
// simulation does not emit dependency-map build logs, while an incomplete
// simulation logs the deferral reason and returns nil.
func TestRecheckBeforeReplacementsLogging(t *testing.T) {
	cleanDB := openTestDB(t)
	if err := cleanDB.WriteStateToDB(client.GooGetState{
		{PackageSpec: &goolib.PkgSpec{Name: "B", Arch: "noarch", Version: "1", Provides: []string{"foo=2.0.0"}}},
		{PackageSpec: &goolib.PkgSpec{Name: "foo", Arch: "x86_64", Version: "0.5.0", PkgDependencies: map[string]string{"B": "1"}}},
	}); err != nil {
		t.Fatal(err)
	}
	cleanLogs := testutil.CaptureLogs(t)
	defClean := deferredChecks{fwd: []relation.Requirement{relation.ParseRelation("foo..1.0.0")}}
	if err := defClean.recheckBeforeReplacements(&goolib.PkgSpec{Name: "R", Arch: "noarch", Version: "1", Replaces: []string{"B"}}, ConflictWarn, cleanDB); err != nil {
		t.Fatalf("recheckBeforeReplacements on clean simulation = %v, want nil", err)
	}
	if strings.Contains(cleanLogs.String(), "Building dependency map") {
		t.Errorf("dry-run simulation logged dependency-map build:\n%s", cleanLogs.String())
	}

	skipDB := openTestDB(t)
	if err := skipDB.WriteStateToDB(client.GooGetState{
		{PackageSpec: &goolib.PkgSpec{Name: "P", Arch: "noarch", Version: "1"}},
		{PackageSpec: &goolib.PkgSpec{Name: "A", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"P": "1", "B": "1"}}},
		{PackageSpec: &goolib.PkgSpec{Name: "B", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"A": "1"}}},
	}); err != nil {
		t.Fatal(err)
	}
	skipLogs := testutil.CaptureLogs(t)
	defSkip := deferredChecks{fwd: []relation.Requirement{relation.ParseRelation("P")}}
	if err := defSkip.recheckBeforeReplacements(&goolib.PkgSpec{Name: "R", Arch: "noarch", Version: "1", Replaces: []string{"P"}}, ConflictWarn, skipDB); err != nil {
		t.Errorf("recheckBeforeReplacements on incomplete simulation = %v, want nil deferral", err)
	}
	wantSkipMsg := "Replacement simulation for R.noarch.1 did not complete; deferring conflict checks until after replacements"
	if !strings.Contains(skipLogs.String(), wantSkipMsg) {
		t.Errorf("logs missing %q; got:\n%s", wantSkipMsg, skipLogs.String())
	}
}

// TestForwardPrecheckAllowsSequentialCascadeAndDuplicateReplaces checks that
// the pre-replacement forward conflict recheck does not falsely reject installs
// whose sequential Replaces removals (via dependent cascades or repeated
// entries) uninstall every conflicting row. Note that resolveReplacements runs
// after dependencies are installed and removes R's own freshly installed
// P.noarch dependency as pre-existing behaviour, leaving only R.noarch.1.
func TestForwardPrecheckAllowsSequentialCascadeAndDuplicateReplaces(t *testing.T) {
	t.Run("later Replaces cascade removes drifted arch", func(t *testing.T) {
		f := newRelFixture(t,
			[]goolib.PkgSpec{
				{Name: "Q", Arch: "noarch", Version: "1"},
				{Name: "P", Arch: "x86_64", Version: "1", PkgDependencies: map[string]string{"Q": "1"}},
			},
			[]goolib.PkgSpec{
				{Name: "P", Arch: "noarch", Version: "1"},
				{Name: "Q", Arch: "noarch", Version: "1"},
				{
					Name:            "R",
					Arch:            "noarch",
					Version:         "1",
					Conflicts:       []string{"P"},
					Replaces:        []string{"P", "Q"},
					PkgDependencies: map[string]string{"P.noarch": "1"},
				},
			})
		if err := f.install(t, "R"); err != nil {
			t.Fatalf("install R: %v", err)
		}
		if diff := cmp.Diff([]string{"R.noarch.1"}, f.installed(t)); diff != "" {
			t.Errorf("installed packages (-want +got):\n%s", diff)
		}
	})

	t.Run("duplicate Replaces entries remove both arches sequentially", func(t *testing.T) {
		f := newRelFixture(t,
			[]goolib.PkgSpec{{Name: "P", Arch: "x86_64", Version: "1"}},
			[]goolib.PkgSpec{
				{Name: "P", Arch: "noarch", Version: "1"},
				{
					Name:            "R",
					Arch:            "noarch",
					Version:         "1",
					Conflicts:       []string{"P"},
					Replaces:        []string{"P", "P"},
					PkgDependencies: map[string]string{"P.noarch": "1"},
				},
			})
		if err := f.install(t, "R"); err != nil {
			t.Fatalf("install R: %v", err)
		}
		if diff := cmp.Diff([]string{"R.noarch.1"}, f.installed(t)); diff != "" {
			t.Errorf("installed packages (-want +got):\n%s", diff)
		}
	})
}

// openTestDB opens a fresh GooDB in a temporary directory and registers
// t.Cleanup to close it before the temporary directory is removed.
func openTestDB(t *testing.T) *googetdb.GooDB {
	t.Helper()
	dir := t.TempDir()
	db, err := googetdb.NewDB(filepath.Join(dir, "googet.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// TestSimulateReplacementsMatchesResolveReplacements differentially verifies
// simulateReplacements against resolveReplacements (with DBOnly) across
// randomized installed states and Replaces lists, covering multi-arch rows,
// dotted package names, versioned entries, arch-less and arch-qualified
// dependents (including self-referential and replaced-name dependencies),
// Conflicts and Provides fields, duplicate entries, dependency cycles, and
// unparseable versions.
func TestSimulateReplacementsMatchesResolveReplacements(t *testing.T) {
	rng := rand.New(rand.NewPCG(3, 4))
	names := []string{"P", "Q", "D", "dotted.pkg"}
	archs := []string{"noarch", "x86_64", "arm64"}
	validVers := []string{"1", "2", "1.0.0", "2.1.0@1", "3.0.0"}
	provPool := []string{"virt", "virt=2.0.0", "P=2.0.0"}
	confPool := []string{"R", "P", "virt..1.0.0"}
	repCandidates := []string{
		"P", "Q", "D", "dotted.pkg", "Missing",
		"P.noarch", "P.x86_64", "Q.arm64", "dotted.pkg.noarch",
		"P.noarch.2", "Q.x86_64.2.0.0",
	}

	const fullScenarios = 300
	numScenarios := testutil.ScaledIterations(fullScenarios)
	var okRemoved, multiRemoved, failedCases, selfRefCases, dottedCases, archlessReplacedDepCases int

	for i := range numScenarios {
		db := openTestDB(t)
		numInstalled := 1 + rng.IntN(6)
		seen := make(map[string]bool)
		var state client.GooGetState
		var hasSelfRef, hasDotted bool
		for range numInstalled {
			name := names[rng.IntN(len(names))]
			arch := archs[rng.IntN(len(archs))]
			key := name + "." + arch
			if seen[key] {
				continue
			}
			seen[key] = true
			if strings.Contains(name, ".") {
				hasDotted = true
			}
			ver := validVers[rng.IntN(len(validVers))]
			if rng.IntN(16) == 0 {
				ver = "notaver"
			}
			var deps map[string]string
			numDeps := rng.IntN(3)
			for range numDeps {
				depName := names[rng.IntN(len(names))]
				depKey := depName
				if rng.IntN(2) == 0 {
					depKey = depName + "." + archs[rng.IntN(len(archs))]
				}
				if depName == name {
					hasSelfRef = true
				}
				if deps == nil {
					deps = make(map[string]string)
				}
				deps[depKey] = "1"
			}
			var provs, confs []string
			if rng.IntN(3) == 0 {
				provs = []string{provPool[rng.IntN(len(provPool))]}
			}
			if rng.IntN(3) == 0 {
				confs = []string{confPool[rng.IntN(len(confPool))]}
			}
			state = append(state, client.PackageState{
				PackageSpec: &goolib.PkgSpec{
					Name:            name,
					Arch:            arch,
					Version:         ver,
					Provides:        provs,
					Conflicts:       confs,
					PkgDependencies: deps,
				},
			})
		}
		if err := db.WriteStateToDB(state); err != nil {
			t.Fatal(err)
		}
		snapshot, err := db.FetchPkgs("")
		if err != nil {
			t.Fatal(err)
		}

		numReplaces := 1 + rng.IntN(4)
		replaces := make([]string, 0, numReplaces)
		replacedNames := make(map[string]bool)
		for range numReplaces {
			if len(replaces) > 0 && rng.IntN(4) == 0 {
				// Duplicate an earlier Replaces entry to exercise sequential re-picking.
				dup := replaces[rng.IntN(len(replaces))]
				replaces = append(replaces, dup)
				replacedNames[relation.ParseRelation(dup).Name] = true
				continue
			}
			cand := repCandidates[rng.IntN(len(repCandidates))]
			replaces = append(replaces, cand)
			replacedNames[relation.ParseRelation(cand).Name] = true
		}
		var hasArchlessReplacedDep bool
		for _, st := range snapshot {
			for depKey := range st.PackageSpec.PkgDependencies {
				if !strings.Contains(depKey, ".") && replacedNames[depKey] {
					hasArchlessReplacedDep = true
				}
			}
		}
		if hasArchlessReplacedDep {
			archlessReplacedDepCases++
		}
		if hasSelfRef {
			selfRefCases++
		}
		if hasDotted {
			dottedCases++
		}

		rSpec := &goolib.PkgSpec{
			Name:      "R",
			Arch:      "noarch",
			Version:   "1",
			Provides:  []string{"virt=2.0.0"},
			Conflicts: []string{"P..2.0.0"},
			Replaces:  replaces,
		}

		simPost, simOK := simulateReplacements(rSpec, snapshot)
		realErr := (&repoInstall{Installer: New("", db, Options{DBOnly: true})}).resolveReplacements(t.Context(), rSpec)

		if simOK != (realErr == nil) {
			t.Fatalf("scenario %d (replaces=%v): simulateReplacements ok=%t, resolveReplacements err=%v", i, replaces, simOK, realErr)
		}
		if realErr != nil {
			failedCases++
			continue
		}

		after, err := db.FetchPkgs("")
		if err != nil {
			t.Fatal(err)
		}
		if diff := cmp.Diff(after, simPost, cmpopts.EquateEmpty()); diff != "" {
			t.Fatalf("scenario %d (replaces=%v) post-replacement state mismatch (-want +got):\n%s", i, replaces, diff)
		}
		removedCount := len(snapshot) - len(simPost)
		if removedCount > 0 {
			okRemoved++
		}
		if removedCount >= 2 {
			multiRemoved++
		}
	}
	// A scaled-down corpus, as on Windows, can miss a case class; the full
	// corpus must reach every one.
	if numScenarios == fullScenarios && (okRemoved == 0 || multiRemoved == 0 || failedCases == 0 || selfRefCases == 0 || dottedCases == 0 || archlessReplacedDepCases == 0) {
		t.Errorf("degenerate differential corpus: okRemoved=%d, multiRemoved=%d, failedCases=%d, selfRef=%d, dotted=%d, archlessReplacedDep=%d; want all > 0",
			okRemoved, multiRemoved, failedCases, selfRefCases, dottedCases, archlessReplacedDepCases)
	}
}

// TestReplacesVersionedVictimExemption checks that a versioned Replaces entry
// only exempts an installed package that meets its minimum version.
func TestReplacesVersionedVictimExemption(t *testing.T) {
	t.Run("below minimum version is not exempt", func(t *testing.T) {
		f := newRelFixture(t,
			[]goolib.PkgSpec{{Name: "P", Arch: "noarch", Version: "1"}},
			[]goolib.PkgSpec{{Name: "R", Arch: "noarch", Version: "1", Conflicts: []string{"P"}, Replaces: []string{"P.noarch.2"}}})
		err := f.install(t, "R")
		wantErr := "cannot install, conflict with installed package or provider: P"
		if err == nil || !strings.Contains(err.Error(), wantErr) {
			t.Fatalf("install R = %v, want error containing %q", err, wantErr)
		}
		if diff := cmp.Diff([]string{"P.noarch.1"}, f.installed(t)); diff != "" {
			t.Errorf("installed packages (-want +got):\n%s", diff)
		}
	})

	t.Run("at or above minimum version is exempt and replaced", func(t *testing.T) {
		f := newRelFixture(t,
			[]goolib.PkgSpec{{Name: "P", Arch: "noarch", Version: "3"}},
			[]goolib.PkgSpec{{Name: "R", Arch: "noarch", Version: "1", Conflicts: []string{"P"}, Replaces: []string{"P.noarch.2"}}})
		if err := f.install(t, "R"); err != nil {
			t.Fatalf("install R: %v", err)
		}
		if diff := cmp.Diff([]string{"R.noarch.1"}, f.installed(t)); diff != "" {
			t.Errorf("installed packages (-want +got):\n%s", diff)
		}
	})
}

// TestFromDiskDoesNotExemptReplacesFromConflicts checks that local .goo
// installs never exempt Replaces targets from Conflicts, because FromDisk does
// not remove replaced packages.
func TestFromDiskDoesNotExemptReplacesFromConflicts(t *testing.T) {
	f := newRelFixture(t, []goolib.PkgSpec{{Name: "P", Arch: "noarch", Version: "1"}}, nil)
	dir := t.TempDir()
	rs := testutil.GenGoo(t, dir, f.dst, goolib.PkgSpec{
		Name:      "R",
		Arch:      "noarch",
		Version:   "1",
		Conflicts: []string{"P"},
		Replaces:  []string{"P"},
	})
	err := FromDisk(filepath.Join(dir, rs.Source), f.cache, false, false, false, f.db)
	wantErr := "cannot install, conflict with installed package or provider: P"
	if err == nil || err.Error() != wantErr {
		t.Fatalf("FromDisk = %v, want exact error %q", err, wantErr)
	}
	if diff := cmp.Diff([]string{"P.noarch.1"}, f.installed(t)); diff != "" {
		t.Errorf("installed packages (-want +got):\n%s", diff)
	}
}

// captureConsole captures os.Stdout output written during fn.
func captureConsole(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	fn()
	w.Close()
	os.Stdout = old
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, r); err != nil {
		t.Fatal(err)
	}
	r.Close()
	return buf.String()
}

// TestReverseConflictModes verifies ConflictWarn, ConflictError, and
// ConflictIgnore across Installer.FromRepo and Installer.FromDisk.
func TestReverseConflictModes(t *testing.T) {
	installedB := goolib.PkgSpec{Name: "B", Arch: "noarch", Version: "1", Conflicts: []string{"A"}}
	incomingA := goolib.PkgSpec{Name: "A", Arch: "noarch", Version: "2"}

	for _, tc := range []struct {
		mode       ConflictMode
		wantResult string
		wantTag    string
		wantWarn   bool
		wantErr    bool
		wantState  []string
	}{
		{
			mode:       ConflictWarn,
			wantResult: "warned",
			wantTag:    "WARN : ",
			wantWarn:   true,
			wantState:  []string{"A.noarch.2", "B.noarch.1"},
		},
		{
			mode:       ConflictError,
			wantResult: "failed",
			wantTag:    "ERROR: ",
			wantErr:    true,
			wantState:  []string{"B.noarch.1"},
		},
		{
			mode:       ConflictIgnore,
			wantResult: "ignored",
			wantTag:    "INFO : ",
			wantState:  []string{"A.noarch.2", "B.noarch.1"},
		},
	} {
		for _, via := range []string{"FromRepo", "FromDisk"} {
			t.Run(tc.mode.String()+"/"+via, func(t *testing.T) {
				logs := testutil.CaptureLogs(t)
				f := newRelFixture(t, []goolib.PkgSpec{installedB}, []goolib.PkgSpec{incomingA})
				ins := New(f.cache, f.db, Options{PackageConflicts: tc.mode})

				var err error
				out := captureConsole(t, func() {
					if via == "FromRepo" {
						err = ins.FromRepo(t.Context(), goolib.PackageInfo{Name: "A"}, "", f.rm, []string{"noarch"}, f.downloader)
					} else {
						dir := t.TempDir()
						rs := testutil.GenGoo(t, dir, f.dst, incomingA)
						err = ins.FromDisk(filepath.Join(dir, rs.Source), false)
					}
				})

				if (err != nil) != tc.wantErr {
					t.Fatalf("%s error = %v, want error: %t", via, err, tc.wantErr)
				}
				if tc.wantErr {
					if !errors.Is(err, ErrPackageConflict) {
						t.Errorf("errors.Is(%v, ErrPackageConflict) = false, want true", err)
					}
					if !strings.Contains(err.Error(), "package conflict mode is error") {
						t.Errorf("error = %q, want 'package conflict mode is error'", err.Error())
					}
					if strings.Contains(err.Error(), "-package_conflicts") {
						t.Errorf("library error = %q, must not hard-code CLI flag name", err.Error())
					}
				}
				if diff := cmp.Diff(tc.wantState, f.installed(t)); diff != "" {
					t.Errorf("installed packages (-want +got):\n%s", diff)
				}

				wantLine := conflictLine(tc.mode, tc.wantResult, &installedB, "A", &incomingA)
				var matchedTag bool
				for _, line := range strings.Split(logs.String(), "\n") {
					if strings.Contains(line, wantLine) && strings.HasPrefix(line, tc.wantTag) {
						matchedTag = true
					}
				}
				if !matchedTag {
					t.Errorf("logs missing %q with prefix %q; got:\n%s", wantLine, tc.wantTag, logs.String())
				}

				wantStdout := `Warning: installed package B.noarch.1 conflicts with A.noarch.2 (entry "A"); installing anyway (package_conflicts=warn)`
				if gotWarn := strings.Contains(out, wantStdout); gotWarn != tc.wantWarn {
					t.Errorf("stdout contains warning = %t, want %t; stdout:\n%s", gotWarn, tc.wantWarn, out)
				}
			})
		}
	}
}

// TestReverseConflictsPure exercises reverseConflicts directly on in-memory
// PkgSpec slices without a database.
func TestReverseConflictsPure(t *testing.T) {
	installed := []*goolib.PkgSpec{
		{Name: "B", Arch: "noarch", Version: "1", Conflicts: []string{"virt.noarch.2.0", "A.x86_64", "C.noarch.2"}},
	}
	for _, tc := range []struct {
		name     string
		incoming *goolib.PkgSpec
		ex       exclusions
		want     []string
	}{
		{
			name:     "provides meets requirement",
			incoming: &goolib.PkgSpec{Name: "P", Arch: "noarch", Version: "1", Provides: []string{"virt=2.5"}},
			want:     []string{"virt.noarch.2.0"},
		},
		{
			name:     "provides below requirement does not hit",
			incoming: &goolib.PkgSpec{Name: "P", Arch: "noarch", Version: "1", Provides: []string{"virt=1.5"}},
		},
		{
			name:     "non-matching arch does not hit",
			incoming: &goolib.PkgSpec{Name: "A", Arch: "noarch", Version: "1"},
		},
		{
			name:     "matching arch hits",
			incoming: &goolib.PkgSpec{Name: "A", Arch: "x86_64", Version: "1"},
			want:     []string{"A.x86_64"},
		},
		{
			name:     "below minimum version does not hit",
			incoming: &goolib.PkgSpec{Name: "C", Arch: "noarch", Version: "1"},
		},
		{
			name:     "at minimum version hits",
			incoming: &goolib.PkgSpec{Name: "C", Arch: "noarch", Version: "2"},
			want:     []string{"C.noarch.2"},
		},
		{
			name:     "excluded installed row produces zero hits",
			incoming: &goolib.PkgSpec{Name: "C", Arch: "noarch", Version: "2"},
			ex:       exclusions{victims: map[string]bool{"B.noarch": true}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hits := reverseConflicts(tc.incoming, installed, tc.ex)
			var got []string
			for _, h := range hits {
				got = append(got, h.entry)
			}
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("reverseConflicts entries (-want +got):\n%s", diff)
			}
		})
	}
}

// TestReverseConflictMatching verifies end-to-end reverse conflict matching by
// Provides, architecture, and minimum version through Installer.FromRepo.
func TestReverseConflictMatching(t *testing.T) {
	for _, tc := range []struct {
		name      string
		installed goolib.PkgSpec
		incoming  goolib.PkgSpec
		wantHit   bool
	}{
		{
			name:      "provides hit",
			installed: goolib.PkgSpec{Name: "B", Arch: "noarch", Version: "1", Conflicts: []string{"X"}},
			incoming:  goolib.PkgSpec{Name: "A", Arch: "noarch", Version: "1", Provides: []string{"X"}},
			wantHit:   true,
		},
		{
			name:      "arch mismatch no hit",
			installed: goolib.PkgSpec{Name: "B", Arch: "noarch", Version: "1", Conflicts: []string{"A.x86_64"}},
			incoming:  goolib.PkgSpec{Name: "A", Arch: "noarch", Version: "1"},
		},
		{
			name:      "version below minimum no hit",
			installed: goolib.PkgSpec{Name: "B", Arch: "noarch", Version: "1", Conflicts: []string{"A.noarch.2"}},
			incoming:  goolib.PkgSpec{Name: "A", Arch: "noarch", Version: "1"},
		},
		{
			name:      "version at minimum hits",
			installed: goolib.PkgSpec{Name: "B", Arch: "noarch", Version: "1", Conflicts: []string{"A.noarch.2"}},
			incoming:  goolib.PkgSpec{Name: "A", Arch: "noarch", Version: "2"},
			wantHit:   true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRelFixture(t, []goolib.PkgSpec{tc.installed}, []goolib.PkgSpec{tc.incoming})
			ins := New(f.cache, f.db, Options{PackageConflicts: ConflictError})
			err := ins.FromRepo(t.Context(), goolib.PackageInfo{Name: tc.incoming.Name}, "", f.rm, []string{"noarch", "x86_64"}, f.downloader)
			if (err != nil) != tc.wantHit {
				t.Errorf("FromRepo error = %v, want hit: %t", err, tc.wantHit)
			}
		})
	}
}

// TestReverseConflictExclusionsAndDrift checks that self-upgrades and replaced
// victims produce zero reverse hits when removed, while a victim that survives
// replacement due to dependency-induced FetchPkg drift is caught before
// downloading the incoming package.
func TestReverseConflictExclusionsAndDrift(t *testing.T) {
	t.Run("self upgrade produces no reverse hit", func(t *testing.T) {
		logs := testutil.CaptureLogs(t)
		spec := func(v string) goolib.PkgSpec {
			return goolib.PkgSpec{Name: "A", Arch: "noarch", Version: v, Provides: []string{"X"}, Conflicts: []string{"X"}}
		}
		f := newRelFixture(t, []goolib.PkgSpec{spec("1")}, []goolib.PkgSpec{spec("2")})
		ins := New(f.cache, f.db, Options{PackageConflicts: ConflictError})
		if err := ins.FromRepo(t.Context(), goolib.PackageInfo{Name: "A"}, "", f.rm, []string{"noarch"}, f.downloader); err != nil {
			t.Fatalf("FromRepo upgrade: %v", err)
		}
		if strings.Contains(logs.String(), "package_conflict") {
			t.Errorf("unexpected package_conflict log on self-upgrade:\n%s", logs.String())
		}
	})

	t.Run("replaced victim produces no reverse hit", func(t *testing.T) {
		logs := testutil.CaptureLogs(t)
		f := newRelFixture(t,
			[]goolib.PkgSpec{{Name: "P", Arch: "noarch", Version: "1", Conflicts: []string{"R"}}},
			[]goolib.PkgSpec{{Name: "R", Arch: "noarch", Version: "1", Replaces: []string{"P"}}})
		ins := New(f.cache, f.db, Options{PackageConflicts: ConflictError})
		if err := ins.FromRepo(t.Context(), goolib.PackageInfo{Name: "R"}, "", f.rm, []string{"noarch"}, f.downloader); err != nil {
			t.Fatalf("FromRepo replace: %v", err)
		}
		if diff := cmp.Diff([]string{"R.noarch.1"}, f.installed(t)); diff != "" {
			t.Errorf("installed packages (-want +got):\n%s", diff)
		}
		if strings.Contains(logs.String(), "package_conflict") {
			t.Errorf("unexpected package_conflict log on replaced victim:\n%s", logs.String())
		}
	})

	t.Run("arch-less Replaces removes all matching arches despite dependency drift", func(t *testing.T) {
		logs := testutil.CaptureLogs(t)
		f := newRelFixture(t,
			[]goolib.PkgSpec{
				{Name: "P", Arch: "x86_64", Version: "1", Conflicts: []string{"R"}},
				{Name: "X", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"P": "1"}},
			},
			[]goolib.PkgSpec{
				{Name: "P", Arch: "noarch", Version: "1"},
				{
					Name:            "R",
					Arch:            "noarch",
					Version:         "1",
					Replaces:        []string{"P"},
					PkgDependencies: map[string]string{"P.noarch": "1"},
				},
			})
		ins := New(f.cache, f.db, Options{PackageConflicts: ConflictError})
		if err := ins.FromRepo(t.Context(), goolib.PackageInfo{Name: "R"}, "", f.rm, []string{"noarch", "x86_64"}, f.downloader); err != nil {
			t.Fatalf("FromRepo = %v, want nil because arch-less Replaces removes both P arches", err)
		}
		if diff := cmp.Diff([]string{"R.noarch.1"}, f.installed(t)); diff != "" {
			t.Errorf("installed packages (-want +got):\n%s", diff)
		}
		if strings.Contains(logs.String(), "package_conflict") {
			t.Errorf("unexpected package_conflict log when all P arches were replaced:\n%s", logs.String())
		}
	})

	t.Run("surviving victim after dependency version downgrade fails before replacement or download", func(t *testing.T) {
		logs := testutil.CaptureLogs(t)
		f := newRelFixture(t,
			[]goolib.PkgSpec{
				{Name: "P", Arch: "x86_64", Version: "2", Conflicts: []string{"R"}},
			},
			[]goolib.PkgSpec{
				{Name: "D", Arch: "noarch", Version: "1", Replaces: []string{"P.x86_64"}},
				{Name: "P", Arch: "x86_64", Version: "1", Conflicts: []string{"R"}},
				{Name: "E", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"P.x86_64": "1"}},
				{
					Name:            "R",
					Arch:            "noarch",
					Version:         "1",
					Replaces:        []string{"P.x86_64.2"},
					PkgDependencies: map[string]string{"D": "1", "E": "1"},
				},
			})
		f.corrupt(t, "R")
		ins := New(f.cache, f.db, Options{PackageConflicts: ConflictError})
		err := ins.FromRepo(t.Context(), goolib.PackageInfo{Name: "R"}, "", f.rm, []string{"noarch", "x86_64"}, f.downloader)
		if err == nil || !strings.Contains(err.Error(), "conflict with installed package P.x86_64.1") {
			t.Fatalf("FromRepo = %v, want reverse conflict with surviving P.x86_64.1", err)
		}
		if diff := cmp.Diff([]string{"P.x86_64.2"}, f.installed(t)); diff != "" {
			t.Errorf("installed packages (-want +got):\n%s", diff)
		}
		wantLine := `package_conflict direction=reverse mode=error result=failed installed=P.x86_64.1 entry="R" incoming=R.noarch.1`
		if got := strings.Count(logs.String(), wantLine); got != 1 {
			t.Errorf("count of %q = %d, want 1; logs:\n%s", wantLine, got, logs.String())
		}
	})

	t.Run("unremoved reverse victim fails pre-check without unparseable warning", func(t *testing.T) {
		logs := testutil.CaptureLogs(t)
		f := newRelFixture(t,
			[]goolib.PkgSpec{
				{Name: "P", Arch: "noarch", Version: "1", Conflicts: []string{"Bad.noarch.notaver", "R"}},
				{Name: "Other", Arch: "noarch", Version: "1"},
			},
			nil)
		rSpec := &goolib.PkgSpec{
			Name:     "R",
			Arch:     "noarch",
			Version:  "1",
			Replaces: []string{"P.noarch.2", "Other"},
		}
		def := deferredChecks{rev: map[string]bool{"P.noarch": true}}
		err := def.recheckBeforeReplacements(rSpec, ConflictError, f.db)
		if !errors.Is(err, ErrPackageConflict) || !strings.Contains(err.Error(), "conflict with installed package P.noarch.1") {
			t.Fatalf("recheckBeforeReplacements = %v, want ErrPackageConflict naming P.noarch.1", err)
		}
		if strings.Contains(logs.String(), "result=unparseable") {
			t.Errorf("unexpected unparseable warning during pre-replace ConflictError failure:\n%s", logs.String())
		}
	})

	t.Run("surviving victim after dependency version downgrade warns once post-replace under warn mode", func(t *testing.T) {
		logs := testutil.CaptureLogs(t)
		f := newRelFixture(t,
			[]goolib.PkgSpec{{Name: "P", Arch: "x86_64", Version: "2", Conflicts: []string{"R"}}},
			[]goolib.PkgSpec{
				{Name: "D", Arch: "noarch", Version: "1", Replaces: []string{"P.x86_64"}},
				{Name: "P", Arch: "x86_64", Version: "1", Conflicts: []string{"R"}},
				{Name: "E", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"P.x86_64": "1"}},
				{
					Name:            "R",
					Arch:            "noarch",
					Version:         "1",
					Replaces:        []string{"P.x86_64.2"},
					PkgDependencies: map[string]string{"D": "1", "E": "1"},
				},
			})
		ins := New(f.cache, f.db, Options{PackageConflicts: ConflictWarn})
		if err := ins.FromRepo(t.Context(), goolib.PackageInfo{Name: "R"}, "", f.rm, []string{"noarch", "x86_64"}, f.downloader); err != nil {
			t.Fatalf("FromRepo under warn mode: %v", err)
		}
		if diff := cmp.Diff([]string{"D.noarch.1", "E.noarch.1", "P.x86_64.1", "R.noarch.1"}, f.installed(t)); diff != "" {
			t.Errorf("installed packages (-want +got):\n%s", diff)
		}
		wantLine := `package_conflict direction=reverse mode=warn result=warned installed=P.x86_64.1 entry="R" incoming=R.noarch.1`
		if got := strings.Count(logs.String(), wantLine); got != 1 {
			t.Errorf("count of %q = %d, want 1; logs:\n%s", wantLine, got, logs.String())
		}
	})

	t.Run("later Replaces cascade removes drifted reverse-conflicting arch", func(t *testing.T) {
		f := newRelFixture(t,
			[]goolib.PkgSpec{
				{Name: "Q", Arch: "noarch", Version: "1"},
				{Name: "P", Arch: "x86_64", Version: "1", Conflicts: []string{"R"}, PkgDependencies: map[string]string{"Q": "1"}},
			},
			[]goolib.PkgSpec{
				{Name: "P", Arch: "noarch", Version: "1"},
				{Name: "Q", Arch: "noarch", Version: "1"},
				{
					Name:            "R",
					Arch:            "noarch",
					Version:         "1",
					Replaces:        []string{"P", "Q"},
					PkgDependencies: map[string]string{"P.noarch": "1"},
				},
			})
		ins := New(f.cache, f.db, Options{PackageConflicts: ConflictError})
		if err := ins.FromRepo(t.Context(), goolib.PackageInfo{Name: "R"}, "", f.rm, []string{"noarch", "x86_64"}, f.downloader); err != nil {
			t.Fatalf("FromRepo(R) = %v, want nil", err)
		}
		if diff := cmp.Diff([]string{"R.noarch.1"}, f.installed(t)); diff != "" {
			t.Errorf("installed packages (-want +got):\n%s", diff)
		}
	})

	t.Run("duplicate Replaces entries remove drifted reverse-conflicting arch", func(t *testing.T) {
		f := newRelFixture(t,
			[]goolib.PkgSpec{{Name: "P", Arch: "x86_64", Version: "1", Conflicts: []string{"R"}}},
			[]goolib.PkgSpec{
				{Name: "P", Arch: "noarch", Version: "1"},
				{
					Name:            "R",
					Arch:            "noarch",
					Version:         "1",
					Replaces:        []string{"P", "P"},
					PkgDependencies: map[string]string{"P.noarch": "1"},
				},
			})
		ins := New(f.cache, f.db, Options{PackageConflicts: ConflictError})
		if err := ins.FromRepo(t.Context(), goolib.PackageInfo{Name: "R"}, "", f.rm, []string{"noarch", "x86_64"}, f.downloader); err != nil {
			t.Fatalf("FromRepo(R) = %v, want nil", err)
		}
		if diff := cmp.Diff([]string{"R.noarch.1"}, f.installed(t)); diff != "" {
			t.Errorf("installed packages (-want +got):\n%s", diff)
		}
	})
}

// TestReverseConflictForceAndReinstall checks that Options.Force never bypasses
// ConflictError, while Reinstall continues to skip all relation checks.
func TestReverseConflictForceAndReinstall(t *testing.T) {
	f := newRelFixture(t,
		[]goolib.PkgSpec{
			{Name: "B", Arch: "noarch", Version: "1", Conflicts: []string{"A"}},
			{Name: "C", Arch: "noarch", Version: "1", Conflicts: []string{"B"}},
		},
		[]goolib.PkgSpec{{Name: "A", Arch: "noarch", Version: "1"}})

	ins := New(f.cache, f.db, Options{Force: true, PackageConflicts: ConflictError})
	err := ins.FromRepo(t.Context(), goolib.PackageInfo{Name: "A"}, "", f.rm, []string{"noarch"}, f.downloader)
	if err == nil || !errors.Is(err, ErrPackageConflict) {
		t.Fatalf("FromRepo with Force=true and ConflictError = %v, want ErrPackageConflict", err)
	}

	dir := t.TempDir()
	rs := testutil.GenGoo(t, dir, f.dst, goolib.PkgSpec{Name: "A", Arch: "noarch", Version: "1"})
	err = ins.FromDisk(filepath.Join(dir, rs.Source), false)
	if err == nil || !errors.Is(err, ErrPackageConflict) {
		t.Fatalf("FromDisk with Force=true and ConflictError = %v, want ErrPackageConflict", err)
	}

	// Reinstall bypasses relation checks even when settings.PackageConflicts is error.
	setPackageConflicts(t, "error")
	psB, err := f.db.FetchPkg(goolib.PackageInfo{Name: "B", Arch: "noarch"})
	if err != nil {
		t.Fatal(err)
	}
	if err := Reinstall(t.Context(), psB, false, false, f.downloader, f.db); err != nil {
		t.Fatalf("Reinstall(B) under packageconflicts=error: %v", err)
	}
}

// TestReverseConflictOnDependencyFailsBeforeSubdependencies checks that a
// reverse conflict on a dependency under ConflictError aborts before that
// dependency's own dependencies are downloaded or installed.
func TestReverseConflictOnDependencyFailsBeforeSubdependencies(t *testing.T) {
	f := newRelFixture(t,
		[]goolib.PkgSpec{{Name: "B", Arch: "noarch", Version: "1", Conflicts: []string{"D"}}},
		[]goolib.PkgSpec{
			{Name: "R", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"D": "1"}},
			{Name: "D", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"Sub": "1"}},
			{Name: "Sub", Arch: "noarch", Version: "1"},
		})
	ins := New(f.cache, f.db, Options{PackageConflicts: ConflictError})
	err := ins.FromRepo(t.Context(), goolib.PackageInfo{Name: "R"}, "", f.rm, []string{"noarch"}, f.downloader)
	if err == nil || !strings.Contains(err.Error(), "D.noarch.1") {
		t.Fatalf("FromRepo(R) = %v, want conflict error naming D.noarch.1", err)
	}
	if diff := cmp.Diff([]string{"B.noarch.1"}, f.installed(t)); diff != "" {
		t.Errorf("installed packages (-want +got):\n%s", diff)
	}
	if f.fileExists("Sub") {
		t.Errorf("Sub's file was installed before D's reverse conflict aborted")
	}
}

// TestReverseConflictUnparseableAndAtomicProvides checks malformed stored
// versions and atomic (installed package, entry) evaluation across name and
// multiple Provides entries.
func TestReverseConflictUnparseableAndAtomicProvides(t *testing.T) {
	for _, mode := range []ConflictMode{ConflictWarn, ConflictError, ConflictIgnore} {
		t.Run("unparseable/"+mode.String(), func(t *testing.T) {
			logs := testutil.CaptureLogs(t)
			f := newRelFixture(t,
				[]goolib.PkgSpec{{Name: "Q", Arch: "noarch", Version: "1", Conflicts: []string{"A.noarch.notaver"}}},
				[]goolib.PkgSpec{{Name: "A", Arch: "noarch", Version: "1"}})
			ins := New(f.cache, f.db, Options{PackageConflicts: mode})
			if err := ins.FromRepo(t.Context(), goolib.PackageInfo{Name: "A"}, "", f.rm, []string{"noarch"}, f.downloader); err != nil {
				t.Fatalf("FromRepo under %s failed on unparseable stored conflict: %v", mode, err)
			}
			wantLine := fmt.Sprintf(`package_conflict direction=reverse mode=%s result=unparseable installed=Q.noarch.1 entry="A.noarch.notaver" incoming=A.noarch.1`, mode)
			if got := strings.Count(logs.String(), wantLine); got != 1 {
				t.Errorf("count of %q = %d, want 1; logs:\n%s", wantLine, got, logs.String())
			}
			if strings.Contains(logs.String(), "ERROR:") {
				t.Errorf("unparseable conflict logged at ERROR in %s mode:\n%s", mode, logs.String())
			}
		})
	}

	for _, tc := range []struct {
		name     string
		incoming goolib.PkgSpec
	}{
		{
			name:     "valid versioned sibling provide wins over unparseable provide",
			incoming: goolib.PkgSpec{Name: "P", Arch: "noarch", Version: "1", Provides: []string{"virt=notaver", "virt=2.0"}},
		},
		{
			name:     "unversioned sibling provide wins over unparseable provide",
			incoming: goolib.PkgSpec{Name: "P", Arch: "noarch", Version: "1", Provides: []string{"virt=notaver", "virt"}},
		},
		{
			name:     "matching both by name and provides emits one line",
			incoming: goolib.PkgSpec{Name: "virt", Arch: "noarch", Version: "2.0", Provides: []string{"virt=2.0"}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs := testutil.CaptureLogs(t)
			f := newRelFixture(t,
				[]goolib.PkgSpec{{Name: "Q", Arch: "noarch", Version: "1", Conflicts: []string{"virt.noarch.1.0"}}},
				[]goolib.PkgSpec{tc.incoming})
			ins := New(f.cache, f.db, Options{PackageConflicts: ConflictError})
			err := ins.FromRepo(t.Context(), goolib.PackageInfo{Name: tc.incoming.Name}, "", f.rm, []string{"noarch"}, f.downloader)
			if err == nil {
				t.Fatalf("FromRepo succeeded, want ConflictError")
			}
			if got := strings.Count(logs.String(), "package_conflict "); got != 1 {
				t.Errorf("package_conflict count = %d, want 1; logs:\n%s", got, logs.String())
			}
		})
	}
}

// TestLegacyWrappersHonourPackageConflicts checks that the package-level
// FromRepo and FromDisk wrappers (including dbOnly repo installs used by
// googet check) honour settings.PackageConflicts.
func TestLegacyWrappersHonourPackageConflicts(t *testing.T) {
	for _, tc := range []struct {
		conf    string
		wantErr bool
	}{
		{conf: "warn", wantErr: false},
		{conf: "error", wantErr: true},
		{conf: "ignore", wantErr: false},
	} {
		t.Run("FromRepo_dbOnly_"+tc.conf, func(t *testing.T) {
			f := newRelFixture(t,
				[]goolib.PkgSpec{{Name: "A", Arch: "noarch", Version: "1", Conflicts: []string{"B"}}},
				[]goolib.PkgSpec{{Name: "B", Arch: "noarch", Version: "1"}})
			setPackageConflicts(t, tc.conf)

			var repoURL string
			for u := range f.rm {
				repoURL = u
			}
			err := FromRepo(t.Context(), goolib.PackageInfo{Name: "B", Arch: "noarch", Ver: "1"}, repoURL, f.cache, f.rm, []string{"noarch"}, true, false, f.downloader, f.db)
			if (err != nil) != tc.wantErr {
				t.Errorf("FromRepo(dbOnly=true) with conf %q error = %v, want error: %t", tc.conf, err, tc.wantErr)
			}
		})

		t.Run("FromDisk_"+tc.conf, func(t *testing.T) {
			f := newRelFixture(t, []goolib.PkgSpec{{Name: "A", Arch: "noarch", Version: "1", Conflicts: []string{"B"}}}, nil)
			setPackageConflicts(t, tc.conf)

			dir := t.TempDir()
			rs := testutil.GenGoo(t, dir, f.dst, goolib.PkgSpec{Name: "B", Arch: "noarch", Version: "1"})
			err := FromDisk(filepath.Join(dir, rs.Source), f.cache, false, false, false, f.db)
			if (err != nil) != tc.wantErr {
				t.Errorf("FromDisk with conf %q error = %v, want error: %t", tc.conf, err, tc.wantErr)
			}
		})
	}
}

// precheckScenario holds a single generated pre-check vs post-recheck
// differential test case and its structural feature flags.
type precheckScenario struct {
	initState              client.GooGetState
	driftState             client.GooGetState
	rSpec                  *goolib.PkgSpec
	hasSelfRef             bool
	hasDotted              bool
	hasArchlessReplacedDep bool
}

// precheckGen generates randomized package topologies for
// TestPrecheckVerdictMatchesPostRecheck from a deterministic RNG.
type precheckGen struct {
	rng          *rand.Rand
	names        []string
	archs        []string
	validVers    []string
	instProvPool []string
	instConfPool []string
	rProvPool    []string
	rConfPool    []string
	repPool      []string
}

func newPrecheckGen(rng *rand.Rand) *precheckGen {
	return &precheckGen{
		rng:          rng,
		names:        []string{"P", "Q", "foo", "dotted.pkg"},
		archs:        []string{"noarch", "x86_64", "arm64"},
		validVers:    []string{"0.5.0", "1", "1.0.0", "2.0.0", "3.0.0"},
		instProvPool: []string{"foo", "foo=0.5.0", "foo=2.0.0", "virt", "virt=2.0.0", "P=2.0.0"},
		instConfPool: []string{"R", "R.noarch", "R.noarch.1.0.0", "R.x86_64", "R..2.0.0", "virt", "virt..1.5.0", "R.noarch.notaver"},
		rProvPool:    []string{"virt", "virt=1.0.0", "virt=2.0.0", "virt=notaver"},
		rConfPool:    []string{"P", "Q", "foo", "P.noarch", "P.x86_64", "P..1.0.0", "foo..1.0.0", "virt", "virt..1.5.0", "dotted.pkg"},
		repPool:      []string{"P", "Q", "foo", "P.noarch", "P.x86_64", "Q.noarch", "P.noarch.2.0.0", "foo..1.0.0", "dotted.pkg", "Missing"},
	}
}

func (g *precheckGen) genSpec(name, arch string) *goolib.PkgSpec {
	ver := g.validVers[g.rng.IntN(len(g.validVers))]
	if g.rng.IntN(25) == 0 {
		ver = "notaver"
	}
	var deps map[string]string
	for range g.rng.IntN(3) {
		depName := g.names[g.rng.IntN(len(g.names))]
		depKey := depName
		if g.rng.IntN(2) == 0 {
			depKey = depName + "." + g.archs[g.rng.IntN(len(g.archs))]
		}
		if deps == nil {
			deps = make(map[string]string)
		}
		deps[depKey] = "0.5.0"
	}
	var provs []string
	if g.rng.IntN(3) == 0 {
		provs = append(provs, g.instProvPool[g.rng.IntN(len(g.instProvPool))])
	}
	var confs []string
	if g.rng.IntN(3) == 0 {
		confs = append(confs, g.instConfPool[g.rng.IntN(len(g.instConfPool))])
	}
	return &goolib.PkgSpec{
		Name:            name,
		Arch:            arch,
		Version:         ver,
		Provides:        provs,
		Conflicts:       confs,
		PkgDependencies: deps,
	}
}

// buildPickShift populates initState with a multi-arch provider and dependent
// topology so post-replacement FetchPkg pick shifts are exercised without
// dependency drift.
func (g *precheckGen) buildPickShift(addTo func(*client.GooGetState, *goolib.PkgSpec), initState *client.GooGetState) (replaces, rConfs []string) {
	pair := [][2]string{{"P", "foo"}, {"Q", "foo"}, {"Q", "P"}}[g.rng.IntN(3)]
	provPkg, targetPkg := pair[0], pair[1]
	highArch := []string{"arm64", "noarch"}[g.rng.IntN(2)]

	pSpec := g.genSpec(provPkg, "noarch")
	pSpec.Version = "1.0.0"
	pSpec.Conflicts = nil
	pSpec.PkgDependencies = nil
	pSpec.Provides = append(pSpec.Provides, targetPkg+"=2.0.0")
	addTo(initState, pSpec)

	highSpec := g.genSpec(targetPkg, highArch)
	highSpec.Version = "3.0.0"
	highSpec.Conflicts = nil
	highSpec.PkgDependencies = nil
	addTo(initState, highSpec)

	lowSpec := g.genSpec(targetPkg, "x86_64")
	lowSpec.Version = "0.5.0"
	lowSpec.Conflicts = nil
	lowSpec.PkgDependencies = map[string]string{provPkg: "0.5.0"}
	addTo(initState, lowSpec)

	return []string{provPkg}, []string{targetPkg + "..1.0.0"}
}

// buildDriftVictim populates initState and driftState with an arch-qualified
// or version-gated Replaces topology where post-checkConflicts dependency
// writes either add a non-replaced architecture (forward conflict) or
// downgrade a version-gated victim below the Replaces minimum (reverse
// conflict).
func (g *precheckGen) buildDriftVictim(addTo func(*client.GooGetState, *goolib.PkgSpec), initState, driftState *client.GooGetState) (replaces, rConfs []string) {
	vName := []string{"P", "Q", "foo"}[g.rng.IntN(3)]
	vSpec := g.genSpec(vName, "x86_64")
	vSpec.Version = "2.0.0"
	vSpec.PkgDependencies = nil
	revCase := g.rng.IntN(2) == 0
	if revCase {
		vSpec.Conflicts = []string{g.instConfPool[g.rng.IntN(3)]}
	} else {
		vSpec.Conflicts = nil
		rConfs = append(rConfs, vName)
	}
	addTo(initState, vSpec)

	if g.rng.IntN(2) == 0 {
		depSpec := g.genSpec([]string{"P", "Q", "foo"}[g.rng.IntN(3)], "noarch")
		depSpec.Version = "1.0.0"
		depSpec.Conflicts = nil
		depSpec.PkgDependencies = map[string]string{vName: "0.5.0"}
		addTo(initState, depSpec)
	}

	if revCase {
		lowVictim := *vSpec
		lowVictim.Version = "1.0.0"
		*driftState = append(*driftState, client.PackageState{PackageSpec: &lowVictim})
	} else {
		driftArch := []string{"arm64", "noarch"}[g.rng.IntN(2)]
		dSpec := g.genSpec(vName, driftArch)
		dSpec.Version = "1.0.0"
		dSpec.Conflicts = nil
		dSpec.PkgDependencies = nil
		addTo(driftState, dSpec)
	}

	replaces = []string{vName + ".x86_64.2.0.0"}
	if g.rng.IntN(4) == 0 {
		replaces = append(replaces, vName+".x86_64.2.0.0")
	}
	return replaces, rConfs
}

// buildRandom populates initState and driftState with an unconstrained random
// installed state, optional post-checkConflicts drift rows, and random
// Replaces and Conflicts.
func (g *precheckGen) buildRandom(addTo func(*client.GooGetState, *goolib.PkgSpec), initState, driftState *client.GooGetState) (replaces, rConfs []string) {
	var installedNames []string
	for range 1 + g.rng.IntN(5) {
		name := g.names[g.rng.IntN(len(g.names))]
		arch := g.archs[g.rng.IntN(len(g.archs))]
		ps := g.genSpec(name, arch)
		before := len(*initState)
		addTo(initState, ps)
		if len(*initState) > before {
			installedNames = append(installedNames, name)
		}
	}
	for range 1 + g.rng.IntN(3) {
		var cand string
		switch {
		case len(replaces) > 0 && g.rng.IntN(5) == 0:
			cand = replaces[g.rng.IntN(len(replaces))]
		case len(installedNames) > 0 && g.rng.IntN(2) == 0:
			cand = installedNames[g.rng.IntN(len(installedNames))]
			if g.rng.IntN(3) == 0 {
				cand += "." + g.archs[g.rng.IntN(len(g.archs))]
			}
		default:
			cand = g.repPool[g.rng.IntN(len(g.repPool))]
		}
		replaces = append(replaces, cand)
	}
	if g.rng.IntN(2) == 0 {
		for range 1 + g.rng.IntN(2) {
			name := g.names[g.rng.IntN(len(g.names))]
			arch := g.archs[g.rng.IntN(len(g.archs))]
			addTo(driftState, g.genSpec(name, arch))
		}
	}
	for range g.rng.IntN(3) {
		rConfs = append(rConfs, g.rConfPool[g.rng.IntN(len(g.rConfPool))])
	}
	return replaces, rConfs
}

func (g *precheckGen) nextScenario() precheckScenario {
	seen := make(map[string]bool)
	var sc precheckScenario
	addTo := func(dst *client.GooGetState, ps *goolib.PkgSpec) {
		key := ps.Name + "." + ps.Arch
		if seen[key] {
			return
		}
		seen[key] = true
		*dst = append(*dst, client.PackageState{PackageSpec: ps})
	}

	var rProvs []string
	for range g.rng.IntN(3) {
		rProvs = append(rProvs, g.rProvPool[g.rng.IntN(len(g.rProvPool))])
	}

	var replaces, rConfs []string
	switch g.rng.IntN(6) {
	case 0:
		replaces, rConfs = g.buildPickShift(addTo, &sc.initState)
	case 1:
		replaces, rConfs = g.buildDriftVictim(addTo, &sc.initState, &sc.driftState)
	default:
		replaces, rConfs = g.buildRandom(addTo, &sc.initState, &sc.driftState)
	}

	rVer := "1.0.0"
	if g.rng.IntN(2) == 0 {
		rVer = "2.0.0"
	}
	sc.rSpec = &goolib.PkgSpec{
		Name:      "R",
		Arch:      "noarch",
		Version:   rVer,
		Provides:  rProvs,
		Conflicts: rConfs,
		Replaces:  replaces,
	}

	replacedNames := make(map[string]bool, len(replaces))
	for _, cand := range replaces {
		replacedNames[relation.ParseRelation(cand).Name] = true
	}
	for _, st := range slices.Concat(sc.initState, sc.driftState) {
		if strings.Contains(st.PackageSpec.Name, ".") {
			sc.hasDotted = true
		}
		for depKey := range st.PackageSpec.PkgDependencies {
			if relation.ParseDependency(depKey, "").Name == st.PackageSpec.Name {
				sc.hasSelfRef = true
			}
			if !strings.Contains(depKey, ".") && replacedNames[depKey] {
				sc.hasArchlessReplacedDep = true
			}
		}
	}
	return sc
}

// TestPrecheckVerdictMatchesPostRecheck differentially verifies that
// recheckBeforeReplacements returns the same fail/no-fail verdict under both
// ConflictError and ConflictWarn as running resolveReplacements (with DBOnly)
// followed by recheck on a database copy, across a randomized corpus with
// Conflicts and Provides on installed rows and on the incoming package, dotted
// package names, self-referential dependencies, arch-less dependencies on
// replaced names, and post-checkConflicts dependency state drift.
func TestPrecheckVerdictMatchesPostRecheck(t *testing.T) {
	gen := newPrecheckGen(rand.New(rand.NewPCG(5, 6)))

	const fullScenarios = 250
	numScenarios := testutil.ScaledIterations(fullScenarios)
	var (
		subtestsRun              int
		evaluated                int
		fwdFails                 int
		noDriftFwdFails          int
		revErrorFails            int
		warnSurvivingRevPasses   int
		cleanPasses              int
		repErrCases              int
		selfRefCases             int
		dottedCases              int
		archlessReplacedDepCases int
	)

	for i := range numScenarios {
		sc := gen.nextScenario()
		for _, mode := range []ConflictMode{ConflictError, ConflictWarn} {
			t.Run(fmt.Sprintf("scenario_%03d/%s", i, mode), func(t *testing.T) {
				subtestsRun++
				db := openTestDB(t)
				if err := db.WriteStateToDB(sc.initState); err != nil {
					t.Fatal(err)
				}

				ex, err := repoExclusions(sc.rSpec, db)
				if err != nil {
					return
				}
				var def deferredChecks
				captureConsole(t, func() {
					def, err = checkConflicts(sc.rSpec, ex, mode, db)
				})
				if err != nil {
					return
				}
				// Mirror any database writes performed during dependency installation,
				// then clone the pre-replacement state into dbCopy only for scenarios
				// that passed initial checkConflicts.
				if len(sc.driftState) > 0 {
					if err := db.WriteStateToDB(sc.driftState); err != nil {
						t.Fatal(err)
					}
				}

				beforePre, err := db.FetchPkgs("")
				if err != nil {
					t.Fatal(err)
				}
				dbCopy := openTestDB(t)
				if err := dbCopy.WriteStateToDB(beforePre); err != nil {
					t.Fatal(err)
				}

				var preErr error
				captureConsole(t, func() {
					preErr = def.recheckBeforeReplacements(sc.rSpec, mode, db)
				})
				afterPre, err := db.FetchPkgs("")
				if err != nil {
					t.Fatal(err)
				}
				if diff := cmp.Diff(beforePre, afterPre, cmpopts.EquateEmpty()); diff != "" {
					t.Fatalf("recheckBeforeReplacements mutated db (-before +after):\n%s", diff)
				}

				_, simOK := simulateReplacements(sc.rSpec, beforePre)
				repErr := (&repoInstall{Installer: New("", dbCopy, Options{DBOnly: true, PackageConflicts: mode})}).resolveReplacements(t.Context(), sc.rSpec)
				if simOK != (repErr == nil) {
					t.Fatalf("simulateReplacements ok=%t, resolveReplacements err=%v", simOK, repErr)
				}
				if repErr != nil {
					if preErr != nil {
						t.Fatalf("pre-check failed (%v) when simulation did not complete (repErr=%v)", preErr, repErr)
					}
					repErrCases++
					return
				}

				var postErr error
				captureConsole(t, func() {
					postErr = def.recheck(sc.rSpec, mode, dbCopy)
				})
				evaluated++
				if sc.hasSelfRef {
					selfRefCases++
				}
				if sc.hasDotted {
					dottedCases++
				}
				if sc.hasArchlessReplacedDep {
					archlessReplacedDepCases++
				}

				switch {
				case preErr == nil && postErr != nil:
					t.Fatalf("pre-check passed on surviving conflict (replaces=%v, conflicts=%v): postErr=%v",
						sc.rSpec.Replaces, sc.rSpec.Conflicts, postErr)
				case preErr != nil && postErr == nil:
					t.Fatalf("pre-check falsely rejected clean replacement (replaces=%v, conflicts=%v): preErr=%v",
						sc.rSpec.Replaces, sc.rSpec.Conflicts, preErr)
				}

				if preErr != nil {
					if errors.Is(preErr, ErrPackageConflict) {
						revErrorFails++
					} else {
						fwdFails++
						if len(sc.driftState) == 0 {
							noDriftFwdFails++
						}
					}
					return
				}
				cleanPasses++
				if mode == ConflictWarn && len(def.rev) > 0 {
					afterCopy, err := dbCopy.FetchPkgs("")
					if err != nil {
						t.Fatal(err)
					}
					for _, h := range reverseConflicts(sc.rSpec, def.revSpecs(specsFromState(afterCopy)), selfExclusion(sc.rSpec)) {
						if !h.unparseable {
							warnSurvivingRevPasses++
							break
						}
					}
				}
			})
		}
	}

	if subtestsRun != numScenarios*2 {
		return
	}
	t.Logf("pre-check differential corpus: scenarios=%d evaluated=%d fwdFails=%d (noDrift=%d) revErrorFails=%d warnSurvivingRevPasses=%d cleanPasses=%d repErrCases=%d selfRef=%d dotted=%d archlessReplacedDep=%d",
		numScenarios, evaluated, fwdFails, noDriftFwdFails, revErrorFails, warnSurvivingRevPasses, cleanPasses, repErrCases, selfRefCases, dottedCases, archlessReplacedDepCases)
	if noDriftFwdFails != 0 {
		t.Errorf("checkConflicts missed %d zero-drift multi-arch forward conflicts before recheckBeforeReplacements", noDriftFwdFails)
	}
	// A scaled-down corpus, as on Windows, can miss a case class; the full
	// corpus must reach every one.
	if numScenarios == fullScenarios && (evaluated == 0 || fwdFails == 0 || revErrorFails == 0 || warnSurvivingRevPasses == 0 || cleanPasses == 0 || repErrCases == 0 || selfRefCases == 0 || dottedCases == 0 || archlessReplacedDepCases == 0) {
		t.Errorf("degenerate pre-check differential corpus: evaluated=%d fwdFails=%d revErrorFails=%d warnSurvivingRevPasses=%d cleanPasses=%d repErrCases=%d selfRef=%d dotted=%d archlessReplacedDep=%d; want all > 0",
			evaluated, fwdFails, revErrorFails, warnSurvivingRevPasses, cleanPasses, repErrCases, selfRefCases, dottedCases, archlessReplacedDepCases)
	}
}
