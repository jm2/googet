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
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
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
