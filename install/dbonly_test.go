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
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/googet/v2/client"
	"github.com/google/googet/v2/goolib"
	"github.com/google/googet/v2/verify"
)

func sha256Hex(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// installDBOnly records version 1 of package name as installed with
// -db_only, from an archive holding contents and a spec mapping archive
// paths to destinations as files does. It returns the installed files.
func installDBOnly(t *testing.T, f *relFixture, name string, contents, files map[string]string) map[string]string {
	t.Helper()
	goo := filepath.Join(f.cache, name+".noarch.1.goo")
	writeGoo(t, goo, contents)
	ps := &goolib.PkgSpec{Name: name, Arch: "noarch", Version: "1", Files: files}
	installed, err := installPkg(goo, ps, Options{DBOnly: true}, f.db, func(insFiles map[string]string) error {
		return f.db.AddPkg(client.PackageState{LocalPath: goo, PackageSpec: ps, InstalledFiles: insFiles})
	})
	if err != nil {
		t.Fatalf("installPkg(%s, dbOnly): %v", name, err)
	}
	return installed
}

// TestDBOnlyRecordsChecksums checks that a -db_only install records the
// payload's checksum for each file and an empty hash for each directory,
// without writing to the destination.
func TestDBOnlyRecordsChecksums(t *testing.T) {
	f := newRelFixture(t, nil, nil)
	bin := filepath.Join(f.dst, "bin")
	got := installDBOnly(t, f, "A", map[string]string{"bin/tool": "from A"}, map[string]string{"bin": bin})

	want := map[string]string{
		bin:                        "",
		filepath.Join(bin, "tool"): sha256Hex("from A"),
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("InstalledFiles (-want +got):\n%s", diff)
	}
	if _, err := os.Stat(bin); !os.IsNotExist(err) {
		t.Errorf("%s: Stat error = %v, want a -db_only install not to create it", bin, err)
	}
}

// TestDBOnlyFilesSeenByConflictCheck checks that the files of a package
// installed with -db_only take part in file-conflict checks, as the files of
// any other installed package do.
func TestDBOnlyFilesSeenByConflictCheck(t *testing.T) {
	for _, tc := range []struct {
		name    string
		strict  bool
		force   bool
		wantErr bool
	}{
		{name: "warn by default"},
		{name: "error when strict", strict: true, wantErr: true},
		{name: "force overrides strict", strict: true, force: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRelFixture(t, nil, nil)
			shared := filepath.Join(f.dst, "shared")
			installDBOnly(t, f, "A", map[string]string{"shared": "from A"}, map[string]string{"shared": shared})

			goo := filepath.Join(f.cache, "B.noarch.1.goo")
			writeGoo(t, goo, map[string]string{"shared": "from B"})
			ps := &goolib.PkgSpec{Name: "B", Arch: "noarch", Version: "1", Files: map[string]string{"shared": shared}}
			_, err := installPkg(goo, ps, Options{StrictConflicts: tc.strict, Force: tc.force}, f.db, nil)
			if gotErr := err != nil; gotErr != tc.wantErr {
				t.Fatalf("installPkg(B) = %v, want error: %t", err, tc.wantErr)
			}
			if tc.wantErr && !strings.Contains(err.Error(), "already owned by package A") {
				t.Errorf("installPkg(B) = %v, want a file conflict with A", err)
			}
		})
	}
}

// TestDBOnlyRowPassesVerify checks that verify accepts a package installed
// with -db_only once its files on disk match the payload.
func TestDBOnlyRowPassesVerify(t *testing.T) {
	f := newRelFixture(t, nil, nil)
	file := filepath.Join(f.dst, "file")
	installDBOnly(t, f, "A", map[string]string{"file": "from A"}, map[string]string{"file": file})
	if err := os.WriteFile(file, []byte("from A"), 0644); err != nil {
		t.Fatal(err)
	}
	ps, err := f.db.FetchPkg(goolib.PackageInfo{Name: "A", Arch: "noarch"})
	if err != nil {
		t.Fatalf("FetchPkg(A): %v", err)
	}
	ok, err := verify.Files(ps)
	if err != nil || !ok {
		t.Errorf("verify.Files(A) = %t, %v; want true, nil", ok, err)
	}
}

// TestDBOnlyRowRemoval checks that removing a package installed with
// -db_only removes its files and directories from disk.
func TestDBOnlyRowRemoval(t *testing.T) {
	f := newRelFixture(t, nil, nil)
	bin := filepath.Join(f.dst, "bin")
	tool := filepath.Join(bin, "tool")
	installDBOnly(t, f, "A", map[string]string{"bin/tool": "from A"}, map[string]string{"bin": bin})
	if err := os.MkdirAll(bin, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tool, []byte("from A"), 0644); err != nil {
		t.Fatal(err)
	}

	f.remove(t, "A")
	for _, p := range []string{tool, bin} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s: Stat error = %v, want it removed with A", p, err)
		}
	}
}
