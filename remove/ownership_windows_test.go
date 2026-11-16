//go:build windows

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

package remove

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/googet/v2/client"
	"github.com/google/googet/v2/goolib"
	"github.com/google/googet/v2/testutil"
)

// TestRemoveKeepsCaseVariantCoOwnedFile verifies that on Windows, removing a
// package keeps a file that another installed package lists under different
// path casing, and deleting the second package removes it.
func TestRemoveKeepsCaseVariantCoOwnedFile(t *testing.T) {
	db, downloader, _ := newRemoveTestDB(t)
	cacheDir := t.TempDir()
	dstDir := t.TempDir()

	subDir := filepath.Join(dstDir, "SubDir")
	if err := os.MkdirAll(subDir, 0755); err != nil {
		t.Fatal(err)
	}
	pathA := filepath.Join(dstDir, "SubDir", "Shared.txt")
	pathB := filepath.Join(dstDir, "subdir", "shared.txt")
	if err := os.WriteFile(pathB, []byte("from B"), 0644); err != nil {
		t.Fatal(err)
	}

	specA := goolib.PkgSpec{Name: "A", Arch: "noarch", Version: "1"}
	specB := goolib.PkgSpec{Name: "B", Arch: "noarch", Version: "1"}
	gooA := filepath.Join(cacheDir, "A.noarch.1.goo")
	gooB := filepath.Join(cacheDir, "B.noarch.1.goo")
	writeOwnershipTestGoo(t, gooA, specA)
	writeOwnershipTestGoo(t, gooB, specB)

	if err := db.WriteStateToDB(client.GooGetState{
		{
			PackageSpec:    &specA,
			LocalPath:      gooA,
			InstalledFiles: map[string]string{pathA: "sum-a"},
		},
		{
			PackageSpec:    &specB,
			LocalPath:      gooB,
			InstalledFiles: map[string]string{pathB: "sum-b"},
		},
	}); err != nil {
		t.Fatalf("WriteStateToDB: %v", err)
	}

	logs := testutil.CaptureLogs(t)
	if err := All(t.Context(), goolib.PackageInfo{Name: "A", Arch: "noarch"}, DepMap{}, false, downloader, db); err != nil {
		t.Fatalf("All(A): %v", err)
	}
	if got, err := os.ReadFile(pathB); err != nil || string(got) != "from B" {
		t.Errorf("shared file after removing A = %q, %v; want kept with %q", got, err, "from B")
	}
	if !strings.Contains(logs.String(), "file_kept op=remove pkg=A.noarch.1") {
		t.Errorf("logs missing file_kept line; got:\n%s", logs.String())
	}

	if err := All(t.Context(), goolib.PackageInfo{Name: "B", Arch: "noarch"}, DepMap{}, false, downloader, db); err != nil {
		t.Fatalf("All(B): %v", err)
	}
	if _, err := os.Stat(pathA); !os.IsNotExist(err) {
		t.Errorf("Stat(pathA) after removing B = %v, want removed", err)
	}
}
