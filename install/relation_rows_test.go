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
	"strings"
	"testing"

	"github.com/google/googet/v2/goolib"
	"github.com/google/googet/v2/internal/relation"
)

// TestCharacterizeMultiArchRowSelection pins how Conflicts, dependencies, and
// the legacy minInstalled helper behave when a package is installed for more
// than one arch. Forward Conflicts and dependency satisfaction check every
// installed row matching the relation, while the legacy minInstalled helper
// checks only the single row googetdb.FetchPkg selects.
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
		{rel: relation.ParseRelation("foo.noarch.2"), want: false, note: "Single-pick legacy helper checks the x86_64 row."},
		{rel: relation.ParseRelation("foo..2"), want: false, note: "Single-pick legacy helper checks the x86_64 row."},
		{rel: relation.ParseDependency("foo.noarch", "2"), want: false, note: "Single-pick legacy helper checks the x86_64 row."},
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

	// Installed foo.noarch 3 conflicts even though foo.x86_64 1 is also installed.
	conflicting := &goolib.PkgSpec{Name: "R", Arch: "noarch", Version: "1", Conflicts: []string{"foo.noarch.2"}}
	if _, err := resolveConflicts(conflicting, exclusions{}, f.db); err == nil || !strings.Contains(err.Error(), "foo.noarch.2") {
		t.Errorf("resolveConflicts(Conflicts: foo.noarch.2) = %v, want conflict error naming foo.noarch.2", err)
	}
	// Installed foo.noarch 3 satisfies foo.noarch >= 2 across multi-arch rows.
	state, err := f.db.FetchPkgs("")
	if err != nil {
		t.Fatalf("FetchPkgs: %v", err)
	}
	if ok, err := satisfiedIn(relation.ParseDependency("foo.noarch", "2"), state); err != nil || !ok {
		t.Errorf("satisfiedIn(foo.noarch >= 2) = %t, %v; want true, nil", ok, err)
	}
}
