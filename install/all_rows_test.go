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
	"maps"
	"math/rand/v2"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/googet/v2/client"
	"github.com/google/googet/v2/goolib"
	"github.com/google/googet/v2/internal/relation"
	"github.com/google/googet/v2/testutil"
)

// TestAllRowConflictsAndDependenciesFromRepo checks that FromRepo evaluates
// forward Conflicts and PkgDependencies across all installed architectures of a
// package name, not just the single row FetchPkg picks.
func TestAllRowConflictsAndDependenciesFromRepo(t *testing.T) {
	t.Run("forward conflict hits non-picked arch", func(t *testing.T) {
		f := newRelFixture(t,
			[]goolib.PkgSpec{
				{Name: "foo", Arch: "noarch", Version: "3"},
				{Name: "foo", Arch: "x86_64", Version: "1"},
			},
			[]goolib.PkgSpec{
				{Name: "A", Arch: "noarch", Version: "1", Conflicts: []string{"foo.noarch.2"}},
				{Name: "B", Arch: "noarch", Version: "1", Conflicts: []string{"foo..2"}},
			})
		for _, pkg := range []string{"A", "B"} {
			err := f.install(t, pkg)
			if err == nil || !strings.Contains(err.Error(), "conflict with installed package or provider: foo") {
				t.Errorf("install(%s) = %v, want conflict error naming foo", pkg, err)
			}
		}
		if diff := cmp.Diff([]string{"foo.noarch.3", "foo.x86_64.1"}, f.installed(t)); diff != "" {
			t.Errorf("installed state changed (-want +got):\n%s", diff)
		}
	})

	t.Run("dependency satisfied by non-picked arch avoids unnecessary install", func(t *testing.T) {
		f := newRelFixture(t,
			[]goolib.PkgSpec{
				{Name: "foo", Arch: "noarch", Version: "3"},
				{Name: "foo", Arch: "x86_64", Version: "1"},
			},
			[]goolib.PkgSpec{
				{Name: "foo", Arch: "noarch", Version: "4"},
				{Name: "A", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"foo.noarch": "2"}},
				{Name: "B", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"foo": "2"}},
			})
		if err := f.install(t, "A"); err != nil {
			t.Fatalf("install(A): %v", err)
		}
		if err := f.install(t, "B"); err != nil {
			t.Fatalf("install(B): %v", err)
		}
		if diff := cmp.Diff([]string{"A.noarch.1", "B.noarch.1", "foo.noarch.3", "foo.x86_64.1"}, f.installed(t)); diff != "" {
			t.Errorf("installed state (-want +got):\n%s", diff)
		}
	})
}

// TestResolveMatchesSQLiteReference checks relation.Resolve against a direct
// SQLite query reference over multi-arch installed states.
func TestResolveMatchesSQLiteReference(t *testing.T) {
	rng := rand.New(rand.NewPCG(14, 314))
	names := []string{"foo", "bar", "a_b"}
	archs := []string{"noarch", "x86_64", "arm64"}
	vers := []string{"1", "2.0.0", "3", "notaver", ""}

	for i := range testutil.ScaledIterations(200) {
		db := openTestDB(t)
		var st client.GooGetState
		seen := map[string]bool{}
		for range rng.IntN(6) {
			n, a := names[rng.IntN(len(names))], archs[rng.IntN(len(archs))]
			if seen[n+"."+a] {
				continue
			}
			seen[n+"."+a] = true
			st = append(st, client.PackageState{
				PackageSpec: &goolib.PkgSpec{Name: n, Arch: a, Version: vers[rng.IntN(4)]},
			})
		}
		if err := db.WriteStateToDB(st); err != nil {
			t.Fatal(err)
		}
		state, err := db.FetchPkgs("")
		if err != nil {
			t.Fatal(err)
		}
		specs := specsFromState(state)
		for range 8 {
			r := relation.Requirement{
				Name: names[rng.IntN(len(names))],
				Ver:  vers[rng.IntN(len(vers))],
			}
			if rng.IntN(2) == 0 {
				r.Arch = archs[rng.IntN(len(archs))]
			}
			gotRows, gotErr := relation.Resolve(r, specs)

			allRows, err := db.FetchPkgs("")
			if err != nil {
				t.Fatal(err)
			}
			var want []string
			var wantErr error
			for _, row := range allRows {
				ps := row.PackageSpec
				if ps.Name != r.Name || (r.Arch != "" && ps.Arch != r.Arch) {
					continue
				}
				c, err := goolib.Compare(r.Ver, ps.Version)
				if err != nil {
					wantErr = err
					break
				}
				if c < 1 {
					want = append(want, ps.String())
				}
			}
			slices.Sort(want)

			if (gotErr == nil) != (wantErr == nil) {
				t.Fatalf("case %d %+v: Resolve err = %v, SQLite ref err = %v", i, r, gotErr, wantErr)
			}
			if gotErr != nil {
				continue
			}
			var got []string
			for _, ps := range gotRows {
				got = append(got, ps.String())
			}
			if diff := cmp.Diff(want, got); diff != "" {
				t.Fatalf("case %d %+v (-want +got):\n%s", i, r, diff)
			}
		}
	}
}

// TestUnsatisfiedAgreesWithSatisfiedIn checks over random multi-arch states and
// removal subsets that relation.Unsatisfied reports a remaining package's
// requirement as orphaned if and only if satisfiedIn held before removal and no
// longer holds after removal.
func TestUnsatisfiedAgreesWithSatisfiedIn(t *testing.T) {
	rng := rand.New(rand.NewPCG(314, 159))
	names := []string{"P", "Q", "R", "virt"}
	archs := []string{"noarch", "x86_64"}
	vers := []string{"1", "2", "3", ""}
	provs := []string{"virt", "virt=1", "virt=2", "virt=notaver", "P=2", "Q=1"}

	for seed := range 400 {
		seen := map[string]bool{}
		var removing, remaining []*goolib.PkgSpec
		var beforeState, afterState client.GooGetState

		nPkgs := 1 + rng.IntN(6)
		for range nPkgs {
			n := names[rng.IntN(3)]
			a := archs[rng.IntN(len(archs))]
			if seen[n+"."+a] {
				continue
			}
			seen[n+"."+a] = true
			deps := map[string]string{}
			for range rng.IntN(3) {
				dn := names[rng.IntN(len(names))]
				if rng.IntN(3) == 0 {
					dn += "." + archs[rng.IntN(len(archs))]
				}
				deps[dn] = vers[rng.IntN(len(vers))]
			}
			var pv []string
			for range rng.IntN(3) {
				pv = append(pv, provs[rng.IntN(len(provs))])
			}
			ps := &goolib.PkgSpec{
				Name:            n,
				Arch:            a,
				Version:         vers[rng.IntN(3)],
				PkgDependencies: deps,
				Provides:        pv,
			}
			beforeState = append(beforeState, client.PackageState{PackageSpec: ps})
			if rng.IntN(2) == 0 {
				removing = append(removing, ps)
			} else {
				remaining = append(remaining, ps)
				afterState = append(afterState, client.PackageState{PackageSpec: ps})
			}
		}

		gotOrphans, err := relation.Unsatisfied(removing, remaining)
		if err != nil {
			t.Fatalf("seed %d: Unsatisfied error: %v", seed, err)
		}
		var got []string
		for _, o := range gotOrphans {
			got = append(got, fmt.Sprintf("%s|%s|%s|%s", o.Dependent, o.Requirement.Name, o.Requirement.Arch, o.Requirement.Ver))
		}
		slices.Sort(got)

		var want []string
		for _, dep := range remaining {
			for _, key := range slices.Sorted(maps.Keys(dep.PkgDependencies)) {
				req := relation.ParseDependency(key, dep.PkgDependencies[key])
				wasOK, err := satisfiedIn(req, beforeState)
				if err != nil {
					t.Fatalf("seed %d: satisfiedIn(before): %v", seed, err)
				}
				nowOK, err := satisfiedIn(req, afterState)
				if err != nil {
					t.Fatalf("seed %d: satisfiedIn(after): %v", seed, err)
				}
				if wasOK && !nowOK {
					entry := fmt.Sprintf("%s|%s|%s|%s", dep, req.Name, req.Arch, req.Ver)
					if !slices.Contains(want, entry) {
						want = append(want, entry)
					}
				}
			}
		}
		slices.Sort(want)

		if diff := cmp.Diff(want, got); diff != "" {
			t.Fatalf("seed %d: Unsatisfied disagrees with satisfiedIn (-satisfiedIn +Unsatisfied):\n%s", seed, diff)
		}
	}
}

// TestAllRowConflictsAndDependenciesFromDiskAndPlan checks that FromDisk and
// Installer.Plan also evaluate forward Conflicts and PkgDependencies across all
// installed architectures rather than only the single row FetchPkg picks.
func TestAllRowConflictsAndDependenciesFromDiskAndPlan(t *testing.T) {
	t.Run("FromDisk_conflicts_with_non_picked_arch", func(t *testing.T) {
		f := newRelFixture(t, []goolib.PkgSpec{
			{Name: "foo", Arch: "noarch", Version: "3"},
			{Name: "foo", Arch: "x86_64", Version: "1"},
		}, nil)
		dir := t.TempDir()
		for _, spec := range []goolib.PkgSpec{
			{Name: "A", Arch: "noarch", Version: "1", Conflicts: []string{"foo.noarch.2"}},
			{Name: "B", Arch: "noarch", Version: "1", Conflicts: []string{"foo..2"}},
		} {
			rs := testutil.GenGoo(t, dir, f.dst, spec)
			err := FromDisk(filepath.Join(dir, rs.Source), f.cache, false, false, false, f.db)
			if err == nil || !strings.Contains(err.Error(), "conflict with installed package or provider: foo") {
				t.Errorf("FromDisk(%s) = %v, want conflict error naming foo", spec.Name, err)
			}
		}
		if diff := cmp.Diff([]string{"foo.noarch.3", "foo.x86_64.1"}, f.installed(t)); diff != "" {
			t.Errorf("installed state changed (-want +got):\n%s", diff)
		}
	})

	t.Run("FromDisk_dependency_satisfied_by_non_picked_arch", func(t *testing.T) {
		f := newRelFixture(t, []goolib.PkgSpec{
			{Name: "foo", Arch: "noarch", Version: "3"},
			{Name: "foo", Arch: "x86_64", Version: "1"},
		}, nil)
		dir := t.TempDir()
		for _, spec := range []goolib.PkgSpec{
			{Name: "A", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"foo.noarch": "2"}},
			{Name: "B", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"foo": "2"}},
		} {
			rs := testutil.GenGoo(t, dir, f.dst, spec)
			if err := FromDisk(filepath.Join(dir, rs.Source), f.cache, false, false, false, f.db); err != nil {
				t.Fatalf("FromDisk(%s): %v", spec.Name, err)
			}
		}
		if diff := cmp.Diff([]string{"A.noarch.1", "B.noarch.1", "foo.noarch.3", "foo.x86_64.1"}, f.installed(t)); diff != "" {
			t.Errorf("installed state (-want +got):\n%s", diff)
		}
	})

	t.Run("Installer_Plan_multi_arch_conflicts_and_dependencies", func(t *testing.T) {
		logs := testutil.CaptureLogs(t)
		f := newRelFixture(t,
			[]goolib.PkgSpec{
				{Name: "foo", Arch: "noarch", Version: "3"},
				{Name: "foo", Arch: "x86_64", Version: "1"},
			},
			[]goolib.PkgSpec{
				{Name: "foo", Arch: "noarch", Version: "4"},
				{Name: "C", Arch: "noarch", Version: "1", Conflicts: []string{"foo..2"}},
				{Name: "D", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"foo": "2", "foo.noarch": "2"}},
			})
		ins := New(f.cache, f.db, Options{DBOnly: true})
		archs := []string{"noarch", "x86_64"}

		if _, err := ins.Plan(goolib.PackageInfo{Name: "C"}, "", f.rm, archs); err == nil || !strings.Contains(err.Error(), "conflict with installed package or provider: foo") {
			t.Errorf("Plan(C) = %v, want conflict error naming foo", err)
		}

		p, err := ins.Plan(goolib.PackageInfo{Name: "D"}, "", f.rm, archs)
		if err != nil {
			t.Fatalf("Plan(D): %v", err)
		}
		wantInstalls := []goolib.PackageInfo{{Name: "D", Arch: "noarch", Ver: "1"}}
		if diff := cmp.Diff(wantInstalls, p.Installs()); diff != "" {
			t.Errorf("Plan(D).Installs() (-want +got):\n%s", diff)
		}
		if logs.Len() != 0 {
			t.Errorf("Installer.Plan emitted unexpected logs:\n%s", logs.String())
		}
	})
}

// TestAllRowDeferredConflictRecheckAfterDependencyDrift checks that when an
// initial forward conflict passes via a single-arch Replaces exemption and a
// dependency subsequently installs two architectures of the conflicted package
// where FetchPkg selects the lower-version non-conflicting architecture, both
// recheckBeforeReplacementsIn and recheck reject the surviving higher-version
// architecture before any change is made.
func TestAllRowDeferredConflictRecheckAfterDependencyDrift(t *testing.T) {
	f := newRelFixture(t,
		[]goolib.PkgSpec{
			{Name: "P", Arch: "x86_64", Version: "3"},
		},
		[]goolib.PkgSpec{
			{Name: "P", Arch: "noarch", Version: "3"},
			{Name: "P", Arch: "x86_32", Version: "1"},
			{Name: "D", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{
				"P.noarch": "3",
				"P.x86_32": "1",
			}},
			{
				Name:            "R",
				Arch:            "noarch",
				Version:         "1",
				Conflicts:       []string{"P..2"},
				Replaces:        []string{"P.x86_64"},
				PkgDependencies: map[string]string{"D": "1"},
			},
		})
	ins := New(f.cache, f.db, Options{DBOnly: true})
	err := ins.FromRepo(t.Context(), goolib.PackageInfo{Name: "R"}, "", f.rm, []string{"noarch", "x86_32", "x86_64"}, f.downloader)
	if err == nil || !strings.Contains(err.Error(), "conflict with installed package or provider: P") {
		t.Fatalf("FromRepo(R) = %v, want conflict error naming P", err)
	}
	if diff := cmp.Diff([]string{"P.x86_64.3"}, f.installed(t)); diff != "" {
		t.Errorf("installed state changed after failed recheck (-want +got):\n%s", diff)
	}

	// Also verify direct post-replacement recheck over multi-arch surviving rows
	// where FetchPkg selects P.x86_32.1 (< 2) while P.noarch.3 (>= 2) survives.
	dbPost := openTestDB(t)
	if err := dbPost.WriteStateToDB(client.GooGetState{
		{PackageSpec: &goolib.PkgSpec{Name: "P", Arch: "noarch", Version: "3"}},
		{PackageSpec: &goolib.PkgSpec{Name: "P", Arch: "x86_32", Version: "1"}},
	}); err != nil {
		t.Fatal(err)
	}
	def := deferredChecks{fwd: []relation.Requirement{relation.ParseRelation("P..2")}}
	rSpec := &goolib.PkgSpec{Name: "R", Arch: "noarch", Version: "1", Conflicts: []string{"P..2"}, Replaces: []string{"P.x86_64"}}
	postErr := def.recheck(rSpec, ConflictWarn, dbPost)
	wantPostErr := "cannot install, conflict with installed package or provider: P (still installed after replacing)"
	if postErr == nil || postErr.Error() != wantPostErr {
		t.Errorf("deferred.recheck = %v, want %q", postErr, wantPostErr)
	}
}

// TestAllRowSelfExclusionWithMultiArchVersionedConflicts checks how
// self-exclusion interacts with versioned forward Conflicts when two
// architectures of the same package are installed.
func TestAllRowSelfExclusionWithMultiArchVersionedConflicts(t *testing.T) {
	t.Run("other_arch_below_minimum_allows_self_excluded_upgrade", func(t *testing.T) {
		f := newRelFixture(t,
			[]goolib.PkgSpec{
				{Name: "A", Arch: "noarch", Version: "2"},
				{Name: "A", Arch: "x86_64", Version: "1"},
			},
			[]goolib.PkgSpec{
				{Name: "A", Arch: "noarch", Version: "3", Conflicts: []string{"A..2"}},
			})
		if err := f.install(t, "A"); err != nil {
			t.Fatalf("install(A): %v", err)
		}
		if diff := cmp.Diff([]string{"A.noarch.3", "A.x86_64.1"}, f.installed(t)); diff != "" {
			t.Errorf("installed state (-want +got):\n%s", diff)
		}
	})

	t.Run("non_picked_other_arch_meeting_minimum_blocks_upgrade", func(t *testing.T) {
		f := newRelFixture(t,
			[]goolib.PkgSpec{
				{Name: "A", Arch: "noarch", Version: "3"},
				{Name: "A", Arch: "x86_64", Version: "1"},
			},
			[]goolib.PkgSpec{
				{Name: "A", Arch: "x86_64", Version: "2", Conflicts: []string{"A..2"}},
			})
		ins := New(f.cache, f.db, Options{DBOnly: true})
		err := ins.FromRepo(t.Context(), goolib.PackageInfo{Name: "A", Arch: "x86_64"}, "", f.rm, []string{"x86_64", "noarch"}, f.downloader)
		if err == nil || !strings.Contains(err.Error(), "conflict with installed package or provider: A") {
			t.Fatalf("FromRepo(A.x86_64) = %v, want conflict error naming A", err)
		}
		if diff := cmp.Diff([]string{"A.noarch.3", "A.x86_64.1"}, f.installed(t)); diff != "" {
			t.Errorf("installed state (-want +got):\n%s", diff)
		}
	})
}

// TestUnsatisfiedAgreesWithSatisfiedInAcrossReplacementsAndErrors extends the
// Unsatisfied vs satisfiedIn property check to cover newly added replacer rows,
// in-place same-arch upgrades/downgrades, and invalid-version error agreement.
func TestUnsatisfiedAgreesWithSatisfiedInAcrossReplacementsAndErrors(t *testing.T) {
	rng := rand.New(rand.NewPCG(2718, 2818))
	names := []string{"P", "Q", "R", "virt"}
	archs := []string{"noarch", "x86_64"}
	vers := []string{"1", "2", "3", ""}
	provs := []string{"virt", "virt=1", "virt=2", "virt=notaver", "P=2", "Q=1"}

	genSpec := func(n, a string, allowInvalidVer bool) *goolib.PkgSpec {
		deps := map[string]string{}
		for range rng.IntN(3) {
			dn := names[rng.IntN(len(names))]
			if rng.IntN(3) == 0 {
				dn += "." + archs[rng.IntN(len(archs))]
			}
			deps[dn] = vers[rng.IntN(len(vers))]
		}
		var pv []string
		for range rng.IntN(3) {
			pv = append(pv, provs[rng.IntN(len(provs))])
		}
		v := vers[rng.IntN(3)]
		if allowInvalidVer && rng.IntN(10) == 0 {
			v = "notaver"
		}
		return &goolib.PkgSpec{
			Name:            n,
			Arch:            a,
			Version:         v,
			PkgDependencies: deps,
			Provides:        pv,
		}
	}

	for seed := range 300 {
		var removing, remaining []*goolib.PkgSpec
		var remState, afterState client.GooGetState

		for _, n := range names[:3] {
			for _, a := range archs {
				switch rng.IntN(5) {
				case 0:
					// Removed without replacement.
					ps := genSpec(n, a, true)
					removing = append(removing, ps)
					remState = append(remState, client.PackageState{PackageSpec: ps})
				case 1:
					// Stayed unchanged.
					ps := genSpec(n, a, true)
					remaining = append(remaining, ps)
					afterState = append(afterState, client.PackageState{PackageSpec: ps})
				case 2:
					// Upgraded or replaced in place at the same (name, arch).
					oldPS := genSpec(n, a, false)
					newPS := genSpec(n, a, false)
					removing = append(removing, oldPS)
					remState = append(remState, client.PackageState{PackageSpec: oldPS})
					remaining = append(remaining, newPS)
					afterState = append(afterState, client.PackageState{PackageSpec: newPS})
				case 3:
					// Newly added replacer not present before the step.
					newPS := genSpec(n, a, false)
					remaining = append(remaining, newPS)
					afterState = append(afterState, client.PackageState{PackageSpec: newPS})
				}
			}
		}

		gotOrphans, gotErr := relation.Unsatisfied(removing, remaining)
		if len(removing) == 0 || len(remaining) == 0 {
			if gotErr != nil || len(gotOrphans) != 0 {
				t.Fatalf("seed %d: empty input returned %v, %v", seed, gotOrphans, gotErr)
			}
			continue
		}

		var want []string
		var wantErr error
		for _, dep := range remaining {
			for _, key := range slices.Sorted(maps.Keys(dep.PkgDependencies)) {
				req := relation.ParseDependency(key, dep.PkgDependencies[key])
				remOK, err := satisfiedIn(req, remState)
				if err != nil {
					wantErr = err
					break
				}
				if !remOK {
					continue
				}
				nowOK, err := satisfiedIn(req, afterState)
				if err != nil {
					wantErr = err
					break
				}
				if !nowOK {
					entry := fmt.Sprintf("%s|%s|%s|%s", dep, req.Name, req.Arch, req.Ver)
					if !slices.Contains(want, entry) {
						want = append(want, entry)
					}
				}
			}
			if wantErr != nil {
				break
			}
		}

		if (gotErr == nil) != (wantErr == nil) {
			t.Fatalf("seed %d: Unsatisfied err = %v, satisfiedIn err = %v", seed, gotErr, wantErr)
		}
		if gotErr != nil {
			continue
		}
		var got []string
		for _, o := range gotOrphans {
			got = append(got, fmt.Sprintf("%s|%s|%s|%s", o.Dependent, o.Requirement.Name, o.Requirement.Arch, o.Requirement.Ver))
		}
		slices.Sort(got)
		slices.Sort(want)
		if diff := cmp.Diff(want, got); diff != "" {
			t.Fatalf("seed %d: Unsatisfied disagrees with satisfiedIn (-satisfiedIn +Unsatisfied):\n%s", seed, diff)
		}
	}
}
