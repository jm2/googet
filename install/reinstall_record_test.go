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
	"github.com/google/googet/v2/client"
	"github.com/google/googet/v2/googetdb"
	"github.com/google/googet/v2/goolib"
	"github.com/google/googet/v2/settings"
	"github.com/google/googet/v2/testutil"
)

// TestReinstallRecordFailureKeepsPreviousRow checks that when recording a
// reinstall over a previously recorded row fails, the files it overwrote are
// put back, the previous row is left as it was and the file it no longer
// ships isn't cleaned up; and that when recording succeeds, the row lists
// the reinstalled files and the dropped file is cleaned up.
func TestReinstallRecordFailureKeepsPreviousRow(t *testing.T) {
	for _, recordFails := range []bool{false, true} {
		t.Run(map[bool]string{false: "recorded", true: "record fails"}[recordFails], func(t *testing.T) {
			settings.Initialize(t.TempDir(), false)
			db, err := googetdb.NewDB(settings.DBFile())
			if err != nil {
				t.Fatalf("googetdb.NewDB: %v", err)
			}
			defer db.Close()
			dst := t.TempDir()
			before := map[string]string{"keep": "old", "oldonly": "old"}
			for name, body := range before {
				if err := os.WriteFile(filepath.Join(dst, name), []byte(body), 0644); err != nil {
					t.Fatal(err)
				}
			}
			pkg := filepath.Join(t.TempDir(), "pkg.goo")
			writeGoo(t, pkg, map[string]string{"keep": "new", "added": "added"})
			prev := client.PackageState{
				LocalPath: pkg,
				PackageSpec: &goolib.PkgSpec{
					Name:    "pkg",
					Arch:    "noarch",
					Version: "1.0.0@1",
					Files: map[string]string{
						"keep":  filepath.Join(dst, "keep"),
						"added": filepath.Join(dst, "added"),
					},
				},
				InstalledFiles: map[string]string{
					filepath.Join(dst, "keep"):    goolib.Checksum(strings.NewReader("old")),
					filepath.Join(dst, "oldonly"): goolib.Checksum(strings.NewReader("old")),
				},
			}
			if err := db.AddPkg(prev); err != nil {
				t.Fatalf("db.AddPkg: %v", err)
			}
			if recordFails {
				if err := failDatabaseInserts(t); err != nil {
					t.Fatal(err)
				}
			}

			logs := testutil.CaptureLogs(t)
			err = Reinstall(t.Context(), prev, false, false, nil, db)

			wantTree := map[string]string{"keep": "new", "added": "added"}
			wantFiles := map[string]string{
				filepath.Join(dst, "keep"):  goolib.Checksum(strings.NewReader("new")),
				filepath.Join(dst, "added"): goolib.Checksum(strings.NewReader("added")),
			}
			if recordFails {
				if err == nil || !strings.Contains(err.Error(), "simulated database failure") {
					t.Errorf("Reinstall() = %v, want the database error", err)
				}
				if strings.Contains(logs.String(), "Cleaning up old file") {
					t.Errorf("Reinstall() cleaned up dropped files although recording failed; logs:\n%s", logs)
				}
				wantTree, wantFiles = before, prev.InstalledFiles
			} else if err != nil {
				t.Errorf("Reinstall() = %v, want success", err)
			}
			if diff := cmp.Diff(wantTree, tree(t, dst)); diff != "" {
				t.Errorf("tree after Reinstall() (-want +got):\n%s", diff)
			}
			got, err := db.FetchPkg(goolib.PackageInfo{Name: "pkg", Arch: "noarch"})
			if err != nil {
				t.Fatalf("db.FetchPkg: %v", err)
			}
			if diff := cmp.Diff(wantFiles, got.InstalledFiles); diff != "" {
				t.Errorf("recorded installed files (-want +got):\n%s", diff)
			}
		})
	}
}
