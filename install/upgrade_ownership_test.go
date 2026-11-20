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
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/googet/v2/client"
	"github.com/google/googet/v2/goolib"
	"github.com/google/googet/v2/internal/ownership"
	"github.com/google/googet/v2/testutil"
)

// TestUpgradeCleanupOwnershipAndLogging verifies that upgrading a package via
// FromRepo keeps dropped files still listed by another installed row while
// logging file_kept, and that cleanOldFiles runs unshared dropped files through
// the file pass with Info cleanup logs and Error failure logs.
func TestUpgradeCleanupOwnershipAndLogging(t *testing.T) {
	t.Run("upgrade_keeps_dropped_file_colisted_by_another_package", func(t *testing.T) {
		f := newRelFixture(t,
			[]goolib.PkgSpec{
				{Name: "A", Arch: "noarch", Version: "1"},
				{Name: "B", Arch: "noarch", Version: "1"},
			},
			[]goolib.PkgSpec{
				{Name: "A", Arch: "noarch", Version: "2"},
			},
		)

		sharedFile := filepath.Join(f.dst, "coowned-dropped.txt")
		unsharedFile := filepath.Join(f.dst, "only-a-dropped.txt")
		if err := os.WriteFile(sharedFile, []byte("from B"), 0644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(unsharedFile, []byte("from A v1"), 0644); err != nil {
			t.Fatal(err)
		}
		sumShared := checksumOf(t, sharedFile)
		sumUnshared := checksumOf(t, unsharedFile)

		rowA, err := f.db.FetchPkg(goolib.PackageInfo{Name: "A", Arch: "noarch"})
		if err != nil {
			t.Fatalf("FetchPkg(A): %v", err)
		}
		rowA.InstalledFiles[sharedFile] = sumShared
		rowA.InstalledFiles[unsharedFile] = sumUnshared
		if err := f.db.AddPkg(rowA); err != nil {
			t.Fatalf("AddPkg(A): %v", err)
		}

		rowB, err := f.db.FetchPkg(goolib.PackageInfo{Name: "B", Arch: "noarch"})
		if err != nil {
			t.Fatalf("FetchPkg(B): %v", err)
		}
		rowB.InstalledFiles[sharedFile] = sumShared
		if err := f.db.AddPkg(rowB); err != nil {
			t.Fatalf("AddPkg(B): %v", err)
		}

		logs := testutil.CaptureLogs(t)
		if err := f.install(t, "A"); err != nil {
			t.Fatalf("upgrade install(A): %v", err)
		}

		if diff := cmp.Diff([]string{"A.noarch.2", "B.noarch.1"}, f.installed(t)); diff != "" {
			t.Errorf("installed packages (-want +got):\n%s", diff)
		}
		if got, readErr := os.ReadFile(sharedFile); readErr != nil || string(got) != "from B" {
			t.Errorf("sharedFile after A upgrade = %q, %v; want kept with %q", got, readErr, "from B")
		}
		if _, statErr := os.Stat(unsharedFile); !os.IsNotExist(statErr) {
			t.Errorf("Stat(unsharedFile) after A upgrade = %v, want deleted", statErr)
		}
		wantKept := fmt.Sprintf("file_kept op=upgrade pkg=A.noarch.1 path=%q owners=B.noarch", sharedFile)
		if !strings.Contains(logs.String(), wantKept) {
			t.Errorf("logs missing %q; got:\n%s", wantKept, logs.String())
		}
		wantCleanup := fmt.Sprintf("Cleaning up old file %q", unsharedFile)
		if !strings.Contains(logs.String(), wantCleanup) {
			t.Errorf("logs missing %q; got:\n%s", wantCleanup, logs.String())
		}
	})

	t.Run("injected_remover_logs_file_cleanup_and_errors_at_error_level", func(t *testing.T) {
		t.Parallel()
		state := client.GooGetState{
			{
				PackageSpec: &goolib.PkgSpec{Name: "B", Arch: "noarch", Version: "1"},
				InstalledFiles: map[string]string{
					"/dst/shared.txt": "sum-b",
				},
			},
		}
		ix := ownership.NewIndex(state, ownership.RowID{Name: "A", Arch: "noarch"})
		oldState := client.PackageState{
			PackageSpec: &goolib.PkgSpec{Name: "A", Arch: "noarch", Version: "1"},
			InstalledFiles: map[string]string{
				"/dst/shared.txt":   "sum-a",
				"/dst/unshared.txt": "sum-unshared",
				"/dst/kept.txt":     "sum-kept",
				"/dst/olddir":       "",
			},
		}
		insFiles := map[string]string{
			"/dst/kept.txt": "sum-kept-v2",
		}

		var removedOrder []string
		var infoLines, errLines []string
		rm := func(p string) (string, error) {
			removedOrder = append(removedOrder, p)
			if p == "/dst/unshared.txt" {
				return "", errors.New("sharing violation")
			}
			return "", errors.New("directory not empty")
		}
		lg := ownership.Logger{
			Infof: func(format string, args ...any) {
				infoLines = append(infoLines, fmt.Sprintf(format, args...))
			},
			Errorf: func(format string, args ...any) {
				errLines = append(errLines, fmt.Sprintf(format, args...))
			},
		}

		cleanOldFiles(oldState, insFiles, ix, rm, lg)

		if diff := cmp.Diff([]string{"/dst/unshared.txt", "/dst/olddir"}, removedOrder); diff != "" {
			t.Errorf("remover call order (-want +got):\n%s", diff)
		}
		wantInfo := []string{
			`file_kept op=upgrade pkg=A.noarch.1 path="/dst/shared.txt" owners=B.noarch`,
			`Cleaning up old file "/dst/unshared.txt"`,
			"directory not empty",
		}
		if diff := cmp.Diff(wantInfo, infoLines); diff != "" {
			t.Errorf("cleanOldFiles info logs (-want +got):\n%s", diff)
		}
		if diff := cmp.Diff([]string{"sharing violation"}, errLines); diff != "" {
			t.Errorf("cleanOldFiles error logs (-want +got):\n%s", diff)
		}
	})
}
