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
	"testing"

	"github.com/google/googet/v2/goolib"
	"github.com/google/googet/v2/internal/relation"
)

// TestCharacterizeMultiArchRowSelection pins how Conflicts, Replaces and
// dependencies are checked when a package is installed for more than one
// arch. minInstalled checks only the row that googetdb.FetchPkg picks: the
// row matching the exact version asked for, or else the last row by
// (arch, version). Because the version asked for is a minimum, that is
// usually the last arch, whatever arch the relation names.
//
// Known wrong: a fix should check every installed row the relation names, so
// that the rows marked wrong below become true.
//
// TODO: flip the known-wrong rows when the replace path is reworked to check
// every installed row (planned with the dependent-aware replace).
func TestCharacterizeMultiArchRowSelection(t *testing.T) {
	f := newRelFixture(t, []goolib.PkgSpec{
		{Name: "foo", Arch: "noarch", Version: "3"},
		{Name: "foo", Arch: "x86_64", Version: "1"},
	}, nil)
	for _, tc := range []struct {
		rel  relation.Requirement
		want bool
		note string
	}{
		{rel: relation.ParseRelation("foo"), want: true, note: "Intended: no version matches the first row."},
		{rel: relation.ParseRelation("foo.x86_64.1"), want: true, note: "Intended: exact version picks that row."},
		{rel: relation.ParseRelation("foo.noarch.2"), want: false, note: "Known wrong: foo.noarch 3 qualifies, but the x86_64 row is checked."},
		{rel: relation.ParseRelation("foo..2"), want: false, note: "Known wrong: foo.noarch 3 qualifies, but the x86_64 row is checked."},
		{rel: relation.ParseDependency("foo.noarch", "2"), want: false, note: "Known wrong: foo.noarch 3 qualifies, but the x86_64 row is checked."},
	} {
		got, err := minInstalled(tc.rel, f.db)
		if err != nil {
			t.Errorf("minInstalled(%+v): %v", tc.rel, err)
			continue
		}
		if got != tc.want {
			t.Errorf("minInstalled(%+v) = %t, want %t. %s", tc.rel, got, tc.want, tc.note)
		}
	}

	// Known wrong: installed foo.noarch 3 should conflict.
	conflicting := &goolib.PkgSpec{Name: "R", Arch: "noarch", Version: "1", Conflicts: []string{"foo.noarch.2"}}
	if _, err := resolveConflicts(conflicting, exclusions{}, f.db); err != nil {
		t.Errorf("resolveConflicts(Conflicts: foo.noarch.2) = %v, want nil (today the x86_64 row is checked)", err)
	}
	// Known wrong: installed foo.noarch 3 should satisfy the dependency.
	if ok, err := isSatisfied(relation.ParseDependency("foo.noarch", "2"), f.db); err != nil || ok {
		t.Errorf("isSatisfied(foo.noarch >= 2) = %t, %v; want false, nil (today the x86_64 row is checked)", ok, err)
	}
}
