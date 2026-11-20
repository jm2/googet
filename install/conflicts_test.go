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
	"math/rand/v2"
	"path/filepath"
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
		pick      *goolib.PkgSpec
		installed []*goolib.PkgSpec
		ex        exclusions
		wantHit   bool
		wantEx    []string
	}{
		{
			name:      "unexcluded name pick hits",
			rel:       "A",
			pick:      aNoarch,
			installed: []*goolib.PkgSpec{aNoarch},
			wantHit:   true,
		},
		{
			name:      "excluded name pick with no other arch skips",
			rel:       "A",
			pick:      aNoarch,
			installed: []*goolib.PkgSpec{aNoarch},
			ex:        exNoarch,
			wantEx:    []string{"A.noarch.1"},
		},
		{
			name:      "excluded name pick falls back to other arch",
			rel:       "A",
			pick:      aNoarch,
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
			hit, exempted, err := checkForwardConflict(relation.ParseRelation(tc.rel), tc.pick, tc.installed, tc.ex)
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
// installed for two architectures, only the single row that Replaces removes
// is exempted, so the remaining architecture still blocks the install before
// any removal happens.
func TestReplacesExemptsOnlyConcreteVictim(t *testing.T) {
	f := newRelFixture(t,
		[]goolib.PkgSpec{
			{Name: "P", Arch: "noarch", Version: "1"},
			{Name: "P", Arch: "x86_64", Version: "1"},
		},
		[]goolib.PkgSpec{
			{Name: "R", Arch: "noarch", Version: "1", Conflicts: []string{"P"}, Replaces: []string{"P"}},
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
		t.Errorf("P's file was removed before pre-replace conflict check aborted")
	}

	// Also verify the post-replace defence-in-depth recheck when a deferred
	// requirement remains satisfied in the database.
	rSpec := &goolib.PkgSpec{Name: "R", Arch: "noarch", Version: "1", Conflicts: []string{"P"}, Replaces: []string{"P.noarch"}}
	def := deferredChecks{fwd: []relation.Requirement{relation.ParseRelation("P")}}
	err = def.recheck(rSpec, f.db)
	wantPostErr := "cannot install, conflict with installed package or provider: P (still installed after replacing)"
	if err == nil || err.Error() != wantPostErr {
		t.Errorf("deferred.recheck = %v, want exact error %q", err, wantPostErr)
	}
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
	if diff := cmp.Diff([]string{"P.noarch.1", "P.x86_64.1"}, f.installed(t)); diff != "" {
		t.Errorf("installed packages (-want +got):\n%s", diff)
	}
	if !f.fileExists("P") {
		t.Errorf("P's file was removed before pre-replace conflict check aborted")
	}

	// Also verify the post-replace defence-in-depth recheck when a deferred
	// requirement remains satisfied in the database.
	rSpec := &goolib.PkgSpec{Name: "R", Arch: "noarch", Version: "1", Conflicts: []string{"P"}, Replaces: []string{"P.noarch"}}
	def := deferredChecks{fwd: []relation.Requirement{relation.ParseRelation("P")}}
	err = def.recheck(rSpec, f.db)
	wantPostErr := "cannot install, conflict with installed package or provider: P (still installed after replacing)"
	if err == nil || err.Error() != wantPostErr {
		t.Errorf("deferred.recheck = %v, want exact error %q", err, wantPostErr)
	}

	// Arch-less Replaces ["P"] with a dependency on P.noarch drifts FetchPkg
	// from P.x86_64 to P.noarch; when X depends on P, failing before
	// resolveReplacements must leave X.noarch.1 and P.noarch.1 intact.
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
	err = f2.install(t, "R")
	if err == nil || err.Error() != wantErr {
		t.Errorf("install R (arch-less Replaces with dependent X) = %v, want exact error %q", err, wantErr)
	}
	if diff := cmp.Diff([]string{"P.noarch.1", "P.x86_64.1", "X.noarch.1"}, f2.installed(t)); diff != "" {
		t.Errorf("installed packages after arch-less Replaces conflict (-want +got):\n%s", diff)
	}
	if !f2.fileExists("X") {
		t.Errorf("X's file was removed before pre-replace forward conflict check aborted")
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
	if err := defClean.recheckBeforeReplacements(&goolib.PkgSpec{Name: "R", Arch: "noarch", Version: "1", Replaces: []string{"B"}}, cleanDB); err != nil {
		t.Fatalf("recheckBeforeReplacements on clean simulation = %v, want nil", err)
	}
	if strings.Contains(cleanLogs.String(), "Building dependency map") {
		t.Errorf("dry-run simulation logged dependency-map build:\n%s", cleanLogs.String())
	}

	skipDB := openTestDB(t)
	if err := skipDB.WriteStateToDB(client.GooGetState{
		{PackageSpec: &goolib.PkgSpec{Name: "P", Arch: "noarch", Version: "1"}},
		{PackageSpec: &goolib.PkgSpec{Name: "dotted.pkg", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"P": "1"}}},
	}); err != nil {
		t.Fatal(err)
	}
	skipLogs := testutil.CaptureLogs(t)
	defSkip := deferredChecks{fwd: []relation.Requirement{relation.ParseRelation("P")}}
	if err := defSkip.recheckBeforeReplacements(&goolib.PkgSpec{Name: "R", Arch: "noarch", Version: "1", Replaces: []string{"P"}}, skipDB); err != nil {
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

// TestFetchPickMatchesFetchPkg differentially verifies fetchPick against
// googetdb.GooDB.FetchPkg across randomized multi-arch, multi-version database
// snapshots.
func TestFetchPickMatchesFetchPkg(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	names := []string{"P", "Q", "R", "dotted.pkg"}
	archs := []string{"noarch", "x86_64", "x86_32", "arm64", "arm"}
	vers := []string{"1", "1.0.0", "2.0.0", "1.2.3@4", "10.0.0", "0.0.1-rc1", "notaver"}
	qNames := []string{"P", "Q", "R", "dotted.pkg", "nonexistent", ""}
	qArchs := []string{"", "noarch", "x86_64", "x86_32", "arm64", "arm", "unknown"}
	qVers := []string{"", "1", "1.0.0", "2.0.0", "1.2.3@4", "99.9.9"}

	const (
		numStates       = 200
		queriesPerState = 25
	)
	for i := range numStates {
		db := openTestDB(t)
		count := rng.IntN(9)
		var state client.GooGetState
		for range count {
			ps := &goolib.PkgSpec{
				Name:    names[rng.IntN(len(names))],
				Arch:    archs[rng.IntN(len(archs))],
				Version: vers[rng.IntN(len(vers))],
			}
			state = append(state, client.PackageState{PackageSpec: ps})
		}
		if err := db.WriteStateToDB(state); err != nil {
			t.Fatal(err)
		}
		snapshot, err := db.FetchPkgs("")
		if err != nil {
			t.Fatal(err)
		}
		for q := range queriesPerState {
			pi := goolib.PackageInfo{
				Name: qNames[rng.IntN(len(qNames))],
				Arch: qArchs[rng.IntN(len(qArchs))],
				Ver:  qVers[rng.IntN(len(qVers))],
			}
			want, err := db.FetchPkg(pi)
			if err != nil {
				t.Fatalf("state %d query %d FetchPkg(%+v): %v", i, q, pi, err)
			}
			got, gotOK := fetchPick(pi, snapshot)
			wantOK := want.PackageSpec != nil
			if gotOK != wantOK {
				t.Errorf("state %d query %d (%+v): fetchPick ok=%t, want %t", i, q, pi, gotOK, wantOK)
			}
			if diff := cmp.Diff(want, got); diff != "" {
				t.Errorf("state %d query %d (%+v) mismatch (-want +got):\n%s", i, q, pi, diff)
			}
		}
	}
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
		realErr := (&installer{db: db, dbOnly: true}).resolveReplacements(t.Context(), rSpec)

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
