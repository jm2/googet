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
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/googet/v2/googetdb"
	"github.com/google/googet/v2/goolib"
	"github.com/google/googet/v2/settings"
)

// TestInstallUnderNonDirectoryFails checks that an install that needs a
// directory where a regular file is, either as the parent of one of its
// files or as a directory it ships, fails with an error that names the path,
// leaves the file alone and records nothing.
func TestInstallUnderNonDirectoryFails(t *testing.T) {
	for _, tc := range []struct {
		desc     string
		contents map[string]string
		files    func(dst string) map[string]string
	}{
		{
			desc:     "parent of a file",
			contents: map[string]string{"f": "payload"},
			files:    func(dst string) map[string]string { return map[string]string{"f": filepath.Join(dst, "blocker", "f")} },
		},
		{
			desc:     "directory the package ships",
			contents: map[string]string{"d/f": "payload"},
			files:    func(dst string) map[string]string { return map[string]string{"d": filepath.Join(dst, "blocker")} },
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			settings.Initialize(t.TempDir(), false)
			db, err := googetdb.NewDB(settings.DBFile())
			if err != nil {
				t.Fatalf("googetdb.NewDB: %v", err)
			}
			defer db.Close()
			dst := t.TempDir()
			blocker := filepath.Join(dst, "blocker")
			if err := os.WriteFile(blocker, []byte("blocker"), 0644); err != nil {
				t.Fatal(err)
			}
			spec := &goolib.PkgSpec{Name: "pkg", Arch: "noarch", Version: "1", Files: tc.files(dst)}
			b, err := json.Marshal(spec)
			if err != nil {
				t.Fatal(err)
			}
			contents := map[string]string{"pkg.pkgspec": string(b)}
			for k, v := range tc.contents {
				contents[k] = v
			}
			pkg := filepath.Join(t.TempDir(), "pkg.goo")
			writeGoo(t, pkg, contents)

			err = FromDisk(pkg, t.TempDir(), false, false, false, db)
			if want := blocker + " exists and is not a directory"; err == nil || !strings.Contains(err.Error(), want) {
				t.Errorf("FromDisk() = %v, want an error containing %q", err, want)
			}
			if diff := cmp.Diff(map[string]string{"blocker": "blocker"}, tree(t, dst)); diff != "" {
				t.Errorf("tree after FromDisk() (-want +got):\n%s", diff)
			}
			state, err := db.FetchPkgs("")
			if err != nil {
				t.Fatalf("db.FetchPkgs: %v", err)
			}
			if len(state) != 0 {
				t.Errorf("FromDisk() recorded %v, want nothing", state)
			}
		})
	}
}

// TestDirectoryHelpersRejectNonDirectory checks the error the directory
// helpers return for a path that exists as a regular file.
func TestDirectoryHelpersRejectNonDirectory(t *testing.T) {
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0644); err != nil {
		t.Fatal(err)
	}
	want := file + " exists and is not a directory"
	var u undo
	if err := u.ensureParents(file); err == nil || err.Error() != want {
		t.Errorf("ensureParents(%q) = %v, want %q", file, err, want)
	}
	if err := u.ensurePackageDir(file, 0755); err == nil || err.Error() != want {
		t.Errorf("ensurePackageDir(%q) = %v, want %q", file, err, want)
	}
	if len(u.changes) != 0 {
		t.Errorf("recorded %v, want no changes", u.changes)
	}
}
