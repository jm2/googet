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
	"github.com/google/googet/v2/testutil"
)

// TestFromDiskRefusesReplacedPackage checks that installing a local package
// that replaces an installed package fails, asking for the installed package
// to be removed first, whichever installed arch qualifies.
func TestFromDiskRefusesReplacedPackage(t *testing.T) {
	installed := []goolib.PkgSpec{
		{Name: "foo", Arch: "noarch", Version: "3"},
		{Name: "foo", Arch: "x86_64", Version: "1"},
	}
	for _, tc := range []struct {
		replaces string
		wantErr  bool
	}{
		{replaces: "foo", wantErr: true},
		{replaces: "foo.x86_64", wantErr: true},
		// googetdb.FetchPkg picks the x86_64 row for these, which doesn't
		// qualify, so they were allowed before every arch was checked.
		{replaces: "foo.noarch.2", wantErr: true},
		{replaces: "foo..2", wantErr: true},
		{replaces: "foo.noarch.4"},
		{replaces: "foo.x86_64.2"},
		{replaces: "bar"},
	} {
		t.Run(tc.replaces, func(t *testing.T) {
			f := newRelFixture(t, installed, nil)
			dir := t.TempDir()
			rs := testutil.GenGoo(t, dir, f.dst, goolib.PkgSpec{Name: "R", Arch: "noarch", Version: "1", Replaces: []string{tc.replaces}})
			err := FromDisk(filepath.Join(dir, rs.Source), f.cache, true, false, false, f.db)
			if !tc.wantErr {
				if err != nil {
					t.Fatalf("FromDisk(R replaces %s): %v", tc.replaces, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), "remove first") {
				t.Fatalf("FromDisk(R replaces %s) = %v, want a remove-first error", tc.replaces, err)
			}
			if diff := cmp.Diff([]string{"foo.noarch.3", "foo.x86_64.1"}, f.installed(t)); diff != "" {
				t.Errorf("installed packages (-want +got):\n%s", diff)
			}
		})
	}
}
