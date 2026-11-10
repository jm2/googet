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
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/google/googet/v2/goolib"
)

// TestFromRepoDependencyCycle checks that installing a package whose
// dependencies lead back to it fails with an error naming the cycle, before
// any package on the cycle is downloaded or installed.
func TestFromRepoDependencyCycle(t *testing.T) {
	f := newRelFixture(t, nil, []goolib.PkgSpec{
		{Name: "A", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"B": "1"}},
		{Name: "B", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"A": "1"}},
	})
	err := f.install(t, "A")
	if want := "dependency cycle A.noarch -> B.noarch -> A.noarch"; err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("install A = %v, want an error containing %q", err, want)
	}
	if got := f.installed(t); len(got) != 0 {
		t.Errorf("installed packages = %v, want none", got)
	}
	goos, err := filepath.Glob(filepath.Join(f.cache, "*.goo"))
	if err != nil {
		t.Fatal(err)
	}
	if len(goos) != 0 {
		t.Errorf("downloaded archives = %v, want none", goos)
	}
	for _, name := range []string{"A", "B"} {
		if _, err := os.Stat(filepath.Join(f.dst, name)); !os.IsNotExist(err) {
			t.Errorf("%s's file: Stat error = %v, want it not installed", name, err)
		}
	}
}

// TestListDepsListsEachPackageOnce checks that the dependency listing shown
// before an install names each package once, for a shared dependency and for
// a dependency cycle.
func TestListDepsListsEachPackageOnce(t *testing.T) {
	for _, tc := range []struct {
		name     string
		packages []goolib.PkgSpec
		want     []string
	}{
		{
			name: "shared dependency",
			packages: []goolib.PkgSpec{
				{Name: "A", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"B": "1", "C": "1"}},
				{Name: "B", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"D": "1"}},
				{Name: "C", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"D": "1"}},
				{Name: "D", Arch: "noarch", Version: "1"},
			},
			want: []string{"A.noarch.1", "B.noarch.1", "C.noarch.1", "D.noarch.1"},
		},
		{
			name: "cycle",
			packages: []goolib.PkgSpec{
				{Name: "A", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"B": "1"}},
				{Name: "B", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"A": "1"}},
			},
			want: []string{"A.noarch.1", "B.noarch.1"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRelFixture(t, nil, tc.packages)
			var repo string
			for r := range f.rm {
				repo = r
			}
			pi := goolib.PackageInfo{Name: "A", Arch: "noarch", Ver: "1"}
			dl, err := ListDeps(pi, f.rm, repo, []string{"noarch"}, f.db)
			if err != nil {
				t.Fatalf("ListDeps(A): %v", err)
			}
			var got []string
			for _, d := range dl {
				got = append(got, d.String())
			}
			if diff := cmp.Diff(tc.want, got, cmpopts.SortSlices(func(a, b string) bool { return a < b })); diff != "" {
				t.Errorf("ListDeps(A) (-want +got):\n%s", diff)
			}
		})
	}
}

// TestCharacterizeReplaceCycleWithoutArch pins that replacing a package that
// is in a dependency cycle fails, removing nothing, when the Replaces entry
// doesn't name an arch. The replace path enumerates dependents under the
// arch-less key "P.", while the cycle comes back to P under "P.noarch", so
// the cycle isn't recognised as passing through the package being replaced.
//
// Known wrong: a fix should resolve P to its installed arch first, so that D
// is removed and then P, as googet remove P does.
//
// TODO: flip this when the replace path resolves victims to installed rows
// (planned with the dependent-aware replace).
func TestCharacterizeReplaceCycleWithoutArch(t *testing.T) {
	f := newRelFixture(t,
		[]goolib.PkgSpec{
			{Name: "P", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"D": "1"}},
			{Name: "D", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"P": "1"}},
		},
		[]goolib.PkgSpec{{Name: "R", Arch: "noarch", Version: "1", Replaces: []string{"P"}}})
	err := f.install(t, "R")
	if want := "D.noarch, P.noarch"; err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("install R = %v, want a cycle error naming %q", err, want)
	}
	if diff := cmp.Diff([]string{"D.noarch.1", "P.noarch.1"}, f.installed(t)); diff != "" {
		t.Errorf("installed packages (-want +got):\n%s", diff)
	}
}
