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
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/googet/v2/goolib"
)

// TestReplacesRemovesOnlyQualifyingArchRows checks that an arch-less versioned
// Replaces entry removes every architecture meeting the minimum version while
// keeping installed architectures below the minimum version untouched.
func TestReplacesRemovesOnlyQualifyingArchRows(t *testing.T) {
	f := newRelFixture(t,
		[]goolib.PkgSpec{
			{Name: "P", Arch: "noarch", Version: "3"},
			{Name: "P", Arch: "x86_64", Version: "1"},
		},
		[]goolib.PkgSpec{
			{Name: "R", Arch: "noarch", Version: "1", Replaces: []string{"P..2"}},
		})
	if err := f.install(t, "R"); err != nil {
		t.Fatalf("install R: %v", err)
	}
	if diff := cmp.Diff([]string{"P.x86_64.1", "R.noarch.1"}, f.installed(t)); diff != "" {
		t.Errorf("installed packages (-want +got):\n%s", diff)
	}
}

// TestReplacesDeduplicatesOverlappingEntries checks that overlapping Replaces
// entries selecting the same installed row uninstall it once without failing.
func TestReplacesDeduplicatesOverlappingEntries(t *testing.T) {
	f := newRelFixture(t,
		[]goolib.PkgSpec{
			{Name: "P", Arch: "arm64", Version: "2"},
			{Name: "P", Arch: "noarch", Version: "2"},
			{Name: "P", Arch: "x86_64", Version: "2"},
		},
		[]goolib.PkgSpec{
			{Name: "R", Arch: "noarch", Version: "1", Replaces: []string{"P", "P.noarch"}},
		})
	if err := f.install(t, "R"); err != nil {
		t.Fatalf("install R: %v", err)
	}
	if diff := cmp.Diff([]string{"R.noarch.1"}, f.installed(t)); diff != "" {
		t.Errorf("installed packages (-want +got):\n%s", diff)
	}
}

// TestReplacesOrdersInterdependentVictimsAndRecoversInReverse checks that when
// Replaces names two packages [A, B] where B depends on A, B is uninstalled
// before A, and a subsequent install failure reinstalls A before B.
func TestReplacesOrdersInterdependentVictimsAndRecoversInReverse(t *testing.T) {
	f := newRelFixture(t,
		[]goolib.PkgSpec{
			{Name: "A", Arch: "noarch", Version: "1"},
			{Name: "B", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"A": "1"}},
		},
		[]goolib.PkgSpec{
			{Name: "R", Arch: "noarch", Version: "1", Replaces: []string{"A", "B"}},
		})

	var repo string
	for r := range f.rm {
		repo = r
	}
	ins := New(f.cache, f.db, Options{})
	plan, err := ins.Plan(goolib.PackageInfo{Name: "R"}, repo, f.rm, []string{"noarch", "x86_64"})
	if err != nil {
		t.Fatalf("Plan(R): %v", err)
	}
	var gotRemovals []string
	for _, pi := range plan.Removals() {
		gotRemovals = append(gotRemovals, pi.String())
	}
	if diff := cmp.Diff([]string{"B.noarch.1", "A.noarch.1"}, gotRemovals); diff != "" {
		t.Fatalf("Plan.Removals (-want +got):\n%s", diff)
	}

	var uninstalled []string
	ri := &repoInstall{
		Installer:  ins,
		rm:         f.rm,
		archs:      []string{"noarch", "x86_64"},
		downloader: f.downloader,
		stageHook: func(stage applyStage, step string) error {
			if stage == stageAfterUninstall {
				uninstalled = append(uninstalled, step)
			}
			if stage == stageAfterReplace {
				return errors.New("boom after replace")
			}
			return nil
		},
	}
	err = ri.fromRepo(t.Context(), goolib.PackageInfo{Name: "R"}, "")
	if err == nil || !strings.Contains(err.Error(), "boom after replace") {
		t.Fatalf("fromRepo = %v, want boom after replace", err)
	}
	if diff := cmp.Diff([]string{"B.noarch.1", "A.noarch.1"}, uninstalled); diff != "" {
		t.Errorf("uninstall order (-want +got):\n%s", diff)
	}
	// Reinstalled list in error reflects exact reverse recovery order: A then B.
	wantRecovered := "reinstalled the packages that installing R.noarch.1 had removed: A.noarch.1, B.noarch.1"
	if !strings.Contains(err.Error(), wantRecovered) {
		t.Errorf("recovery error = %q, want substring %q", err.Error(), wantRecovered)
	}
	if diff := cmp.Diff([]string{"A.noarch.1", "B.noarch.1"}, f.installed(t)); diff != "" {
		t.Errorf("installed after recovery (-want +got):\n%s", diff)
	}
}

// TestMultiArchVictimsReverseConflictExemptionAndCorruptVersion checks reverse
// conflict exemptions across multi-architecture Replaces entries and clean
// planning failure when a candidate row has an unparseable version.
func TestMultiArchVictimsReverseConflictExemptionAndCorruptVersion(t *testing.T) {
	t.Run("arch_less_replaces_exempts_all_matching_arches_from_reverse_conflict", func(t *testing.T) {
		f := newRelFixture(t,
			[]goolib.PkgSpec{
				{Name: "P", Arch: "noarch", Version: "2", Conflicts: []string{"R"}},
				{Name: "P", Arch: "x86_64", Version: "2", Conflicts: []string{"R"}},
			},
			[]goolib.PkgSpec{
				{Name: "R", Arch: "noarch", Version: "1", Replaces: []string{"P"}},
			})
		if err := f.installWith(t, Options{PackageConflicts: ConflictError}, "R"); err != nil {
			t.Fatalf("install R: %v", err)
		}
		if diff := cmp.Diff([]string{"R.noarch.1"}, f.installed(t)); diff != "" {
			t.Errorf("installed packages (-want +got):\n%s", diff)
		}
	})

	t.Run("versioned_replaces_keeps_reverse_conflict_on_below_minimum_arch", func(t *testing.T) {
		f := newRelFixture(t,
			[]goolib.PkgSpec{
				{Name: "P", Arch: "noarch", Version: "3", Conflicts: []string{"R"}},
				{Name: "P", Arch: "x86_64", Version: "1", Conflicts: []string{"R"}},
			},
			[]goolib.PkgSpec{
				{Name: "R", Arch: "noarch", Version: "1", Replaces: []string{"P..2"}},
			})
		err := f.installWith(t, Options{PackageConflicts: ConflictError}, "R")
		if !errors.Is(err, ErrPackageConflict) || !strings.Contains(err.Error(), "P.x86_64.1") {
			t.Fatalf("install R = %v, want ErrPackageConflict naming P.x86_64.1", err)
		}
		if diff := cmp.Diff([]string{"P.noarch.3", "P.x86_64.1"}, f.installed(t)); diff != "" {
			t.Errorf("installed packages (-want +got):\n%s", diff)
		}
		if f.totalGets() != 0 {
			t.Errorf("totalGets = %d, want 0", f.totalGets())
		}
	})

	t.Run("unparseable_installed_candidate_version_fails_before_download", func(t *testing.T) {
		f := newRelFixture(t,
			[]goolib.PkgSpec{
				{Name: "P", Arch: "noarch", Version: "3"},
				{Name: "P", Arch: "x86_64", Version: "not-a-semver"},
			},
			[]goolib.PkgSpec{
				{Name: "R", Arch: "noarch", Version: "1", Replaces: []string{"P..2"}},
			})
		err := f.install(t, "R")
		if err == nil || !strings.Contains(err.Error(), "P.x86_64.not-a-semver") {
			t.Fatalf("install R = %v, want version parse error naming P.x86_64.not-a-semver", err)
		}
		if diff := cmp.Diff([]string{"P.noarch.3", "P.x86_64.not-a-semver"}, f.installed(t)); diff != "" {
			t.Errorf("installed packages (-want +got):\n%s", diff)
		}
		if f.totalGets() != 0 {
			t.Errorf("totalGets = %d, want 0", f.totalGets())
		}
	})
}

// TestMultiArchVictimsMutualCycleAndPartialUninstallRecovery checks that two
// architecture rows of the same package in a mutual dependency cycle are both
// removed when matched by Replaces, and that a failure while uninstalling the
// second architecture reinstalls the first architecture cleanly.
func TestMultiArchVictimsMutualCycleAndPartialUninstallRecovery(t *testing.T) {
	t.Run("mutual_cycle_between_multiarch_victims_succeeds", func(t *testing.T) {
		f := newRelFixture(t,
			[]goolib.PkgSpec{
				{Name: "P", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"P.x86_64": "1"}},
				{Name: "P", Arch: "x86_64", Version: "1", PkgDependencies: map[string]string{"P.noarch": "1"}},
			},
			[]goolib.PkgSpec{
				{Name: "R", Arch: "noarch", Version: "1", Replaces: []string{"P"}},
			})
		if err := f.install(t, "R"); err != nil {
			t.Fatalf("install R: %v", err)
		}
		if diff := cmp.Diff([]string{"R.noarch.1"}, f.installed(t)); diff != "" {
			t.Errorf("installed packages (-want +got):\n%s", diff)
		}
	})

	t.Run("failure_uninstalling_second_arch_reinstalls_first_arch", func(t *testing.T) {
		f := replaceFixture(t,
			goolib.PkgSpec{Name: "R", Arch: "noarch", Version: "1", Replaces: []string{"P"}},
			goolib.PkgSpec{Name: "P", Arch: "noarch", Version: "1"},
			goolib.PkgSpec{Name: "P", Arch: "x86_64", Version: "1", Uninstall: failingScript},
		)
		err := f.install(t, "R")
		if err == nil || !strings.Contains(err.Error(), "missing-script.exe") || !strings.Contains(err.Error(), "reinstalled the packages that installing R.noarch.1 had removed: P.noarch.1") {
			t.Fatalf("install R = %v, want P.x86_64 uninstall error and P.noarch.1 reinstalled", err)
		}
		if diff := cmp.Diff([]string{"P.noarch.1", "P.x86_64.1"}, f.installed(t)); diff != "" {
			t.Errorf("installed packages after partial-uninstall recovery (-want +got):\n%s", diff)
		}
	})
}
