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

	"github.com/google/go-cmp/cmp"
	"github.com/google/googet/v2/goolib"
	"github.com/google/googet/v2/remove"
	"github.com/google/googet/v2/testutil"
)

// TestDottedDependentsAgree checks that remove.Plan, remove.EnumerateDeps,
// the replacement simulation and a real replace agree on which packages a
// replacement removes, and in which order, when several dependents have
// dotted names. Packages named after a dotted name's first segment are
// unrelated and stay.
func TestDottedDependentsAgree(t *testing.T) {
	installed := []goolib.PkgSpec{
		{Name: "P", Arch: "noarch", Version: "1"},
		{Name: "Q", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"P": "1"}},
		{Name: "x.y", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"Q": "1"}},
		{Name: "foo.bar", Arch: "x86_64", Version: "1", PkgDependencies: map[string]string{"P": "1"}},
		{Name: "foo", Arch: "noarch", Version: "1"},
		{Name: "x", Arch: "noarch", Version: "1"},
	}
	replacer := goolib.PkgSpec{Name: "R", Arch: "noarch", Version: "1", Replaces: []string{"P"}}
	f := newRelFixture(t, installed, []goolib.PkgSpec{replacer})
	state, err := f.db.FetchPkgs("")
	if err != nil {
		t.Fatal(err)
	}

	order, err := remove.Plan(goolib.PackageInfo{Name: "P"}, state)
	if err != nil {
		t.Fatalf("remove.Plan(P): %v", err)
	}
	wantOrder := []string{"foo.bar.x86_64", "x.y.noarch", "Q.noarch"}
	if diff := cmp.Diff(wantOrder, order); diff != "" {
		t.Errorf("remove.Plan(P) (-want +got):\n%s", diff)
	}

	deps, dl, err := remove.EnumerateDeps(goolib.PackageInfo{Name: "P"}, f.db)
	if err != nil {
		t.Fatalf("EnumerateDeps(P): %v", err)
	}
	slices.Sort(dl)
	if diff := cmp.Diff([]string{"P. 1", "Q.noarch 1", "foo.bar.x86_64 1", "x.y.noarch 1"}, dl); diff != "" {
		t.Errorf("EnumerateDeps(P) list (-want +got):\n%s", diff)
	}
	if _, ok := deps["x.y.noarch"]; !ok {
		t.Errorf("EnumerateDeps(P) map = %v, want a key for x.y.noarch", deps)
	}

	out, err := planReplacement(&replacer, state)
	if err != nil {
		t.Fatalf("planReplacement(R): %v", err)
	}
	var simulated []string
	for _, r := range out.removed {
		simulated = append(simulated, pkgKey(r.row.PackageSpec))
	}
	if diff := cmp.Diff(append(slices.Clone(wantOrder), "P.noarch"), simulated); diff != "" {
		t.Errorf("simulated removals (-want +got):\n%s", diff)
	}

	logs := testutil.CaptureLogs(t)
	if err := f.installWith(t, Options{OrphanedDependents: OrphanRemove}, "R"); err != nil {
		t.Fatalf("install R (OrphanRemove): %v", err)
	}
	var executed []string
	for _, l := range parseLogs(logs.String()) {
		if name, ok := strings.CutPrefix(l.text, "Executing removal of package "); ok {
			executed = append(executed, name)
		}
	}
	if diff := cmp.Diff([]string{`"foo.bar"`, `"x.y"`, `"Q"`, `"P"`}, executed); diff != "" {
		t.Errorf("removals executed (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff([]string{"R.noarch.1", "foo.noarch.1", "x.noarch.1"}, f.installed(t)); diff != "" {
		t.Errorf("installed packages (-want +got):\n%s", diff)
	}
	for _, name := range []string{"foo", "x"} {
		if !f.fileExists(name) {
			t.Errorf("%s's file was removed; nothing it depends on was replaced", name)
		}
	}
	for _, name := range []string{"P", "Q", "x.y", "foo.bar"} {
		if f.fileExists(name) {
			t.Errorf("%s's file is still installed after the replace", name)
		}
	}
}
