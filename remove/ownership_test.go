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
	"archive/tar"
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/googet/v2/client"
	"github.com/google/googet/v2/googetdb"
	"github.com/google/googet/v2/goolib"
	"github.com/google/googet/v2/settings"
	"github.com/google/googet/v2/testutil"
)

type archiveEntry struct {
	name string
	body string
	mode int64
}

func writeOwnershipTestGoo(t *testing.T, path string, spec goolib.PkgSpec, entries ...archiveEntry) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	gw := gzip.NewWriter(f)
	tw := tar.NewWriter(gw)

	specBytes, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	allEntries := append([]archiveEntry{{
		name: spec.Name + ".pkgspec",
		body: string(specBytes),
		mode: 0644,
	}}, entries...)

	for _, e := range allEntries {
		mode := e.mode
		if mode == 0 {
			mode = 0644
		}
		if err := tw.WriteHeader(&tar.Header{
			Name: e.name,
			Mode: mode,
			Size: int64(len(e.body)),
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(e.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func newRemoveTestDB(t *testing.T) (*googetdb.GooDB, *client.Downloader, string) {
	t.Helper()
	root := t.TempDir()
	settings.Initialize(root, false)
	dbFile := settings.DBFile()
	db, err := googetdb.NewDB(dbFile)
	if err != nil {
		t.Fatalf("googetdb.NewDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	downloader, err := client.NewDownloader("")
	if err != nil {
		t.Fatalf("client.NewDownloader: %v", err)
	}
	return db, downloader, dbFile
}

// TestRemoveCoListedFileKeptUntilLastOwnerRemoved verifies that removing one
// co-listing package preserves the shared file on disk and logs file_kept, and
// removing the final owner deletes the file.
func TestRemoveCoListedFileKeptUntilLastOwnerRemoved(t *testing.T) {
	db, downloader, _ := newRemoveTestDB(t)
	cacheDir := t.TempDir()
	dstDir := t.TempDir()

	sharedFile := filepath.Join(dstDir, "shared.txt")
	onlyAFile := filepath.Join(dstDir, "only-a.txt")
	if err := os.WriteFile(sharedFile, []byte("from B"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(onlyAFile, []byte("from A"), 0644); err != nil {
		t.Fatal(err)
	}

	specA := goolib.PkgSpec{Name: "pkgA", Arch: "noarch", Version: "1"}
	specB := goolib.PkgSpec{Name: "pkgB", Arch: "noarch", Version: "1"}
	gooA := filepath.Join(cacheDir, "pkgA.noarch.1.goo")
	gooB := filepath.Join(cacheDir, "pkgB.noarch.1.goo")
	writeOwnershipTestGoo(t, gooA, specA)
	writeOwnershipTestGoo(t, gooB, specB)

	if err := db.WriteStateToDB(client.GooGetState{
		{
			PackageSpec: &specA,
			LocalPath:   gooA,
			InstalledFiles: map[string]string{
				sharedFile: "sum-a",
				onlyAFile:  "sum-only-a",
			},
		},
		{
			PackageSpec: &specB,
			LocalPath:   gooB,
			InstalledFiles: map[string]string{
				sharedFile: "sum-b",
			},
		},
	}); err != nil {
		t.Fatalf("WriteStateToDB: %v", err)
	}

	logs := testutil.CaptureLogs(t)
	if err := All(t.Context(), goolib.PackageInfo{Name: "pkgA", Arch: "noarch"}, DepMap{}, false, downloader, db); err != nil {
		t.Fatalf("All(pkgA): %v", err)
	}
	if _, err := os.Stat(onlyAFile); !os.IsNotExist(err) {
		t.Errorf("Stat(onlyAFile) = %v, want removed with pkgA", err)
	}
	if got, err := os.ReadFile(sharedFile); err != nil || string(got) != "from B" {
		t.Errorf("sharedFile after removing pkgA = %q, %v; want kept with %q", got, err, "from B")
	}
	wantKept := fmt.Sprintf("file_kept op=remove pkg=pkgA.noarch.1 path=%q owners=pkgB.noarch", sharedFile)
	if !strings.Contains(logs.String(), wantKept) {
		t.Errorf("logs missing %q; got:\n%s", wantKept, logs.String())
	}

	if err := All(t.Context(), goolib.PackageInfo{Name: "pkgB", Arch: "noarch"}, DepMap{}, false, downloader, db); err != nil {
		t.Fatalf("All(pkgB): %v", err)
	}
	if _, err := os.Stat(sharedFile); !os.IsNotExist(err) {
		t.Errorf("Stat(sharedFile) after removing pkgB = %v, want removed", err)
	}
	state, err := db.FetchPkgs("")
	if err != nil {
		t.Fatalf("FetchPkgs: %v", err)
	}
	if len(state) != 0 {
		t.Errorf("remaining state = %v, want empty", state)
	}
}

// TestRemoveCascadeAndSharedEmptyDirectory verifies live-snapshot ownership
// checks across dependency cascades and empty directories co-listed by another
// package row.
func TestRemoveCascadeAndSharedEmptyDirectory(t *testing.T) {
	t.Run("cascade_dependent_and_dependency_colist_file", func(t *testing.T) {
		db, downloader, _ := newRemoveTestDB(t)
		cacheDir := t.TempDir()
		dstDir := t.TempDir()

		sharedFile := filepath.Join(dstDir, "cascade-shared.txt")
		onlyDFile := filepath.Join(dstDir, "only-d.txt")
		onlyAFile := filepath.Join(dstDir, "only-a.txt")
		for p, body := range map[string]string{
			sharedFile: "shared",
			onlyDFile:  "only D",
			onlyAFile:  "only A",
		} {
			if err := os.WriteFile(p, []byte(body), 0644); err != nil {
				t.Fatal(err)
			}
		}

		specA := goolib.PkgSpec{Name: "A", Arch: "noarch", Version: "1"}
		specD := goolib.PkgSpec{
			Name:            "D",
			Arch:            "noarch",
			Version:         "1",
			PkgDependencies: map[string]string{"A": "1"},
		}
		gooA := filepath.Join(cacheDir, "A.noarch.1.goo")
		gooD := filepath.Join(cacheDir, "D.noarch.1.goo")
		writeOwnershipTestGoo(t, gooA, specA)
		writeOwnershipTestGoo(t, gooD, specD)

		if err := db.WriteStateToDB(client.GooGetState{
			{
				PackageSpec:    &specA,
				LocalPath:      gooA,
				InstalledFiles: map[string]string{sharedFile: "sum-shared", onlyAFile: "sum-a"},
			},
			{
				PackageSpec:    &specD,
				LocalPath:      gooD,
				InstalledFiles: map[string]string{sharedFile: "sum-shared", onlyDFile: "sum-d"},
			},
		}); err != nil {
			t.Fatalf("WriteStateToDB: %v", err)
		}

		logs := testutil.CaptureLogs(t)
		piA := goolib.PackageInfo{Name: "A", Arch: "noarch"}
		deps, _, err := EnumerateDeps(piA, db)
		if err != nil {
			t.Fatalf("EnumerateDeps(A): %v", err)
		}
		if err := All(t.Context(), piA, deps, false, downloader, db); err != nil {
			t.Fatalf("All(A): %v", err)
		}

		// When D was removed first, A still owned sharedFile, logging file_kept;
		// when A was removed second, sharedFile became unowned and was deleted.
		wantKeptAtD := fmt.Sprintf("file_kept op=remove pkg=D.noarch.1 path=%q owners=A.noarch", sharedFile)
		if !strings.Contains(logs.String(), wantKeptAtD) {
			t.Errorf("logs missing %q; got:\n%s", wantKeptAtD, logs.String())
		}
		for _, p := range []string{sharedFile, onlyDFile, onlyAFile} {
			if _, statErr := os.Stat(p); !os.IsNotExist(statErr) {
				t.Errorf("Stat(%s) after cascade = %v, want removed", p, statErr)
			}
		}
	})

	t.Run("cascade_aborted_at_root_keeps_shared_file_on_disk", func(t *testing.T) {
		db, downloader, _ := newRemoveTestDB(t)
		cacheDir := t.TempDir()
		dstDir := t.TempDir()

		sharedFile := filepath.Join(dstDir, "cascade-shared.txt")
		onlyDFile := filepath.Join(dstDir, "only-d.txt")
		if err := os.WriteFile(sharedFile, []byte("shared"), 0644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(onlyDFile, []byte("only D"), 0644); err != nil {
			t.Fatal(err)
		}

		specA := goolib.PkgSpec{
			Name:      "A",
			Arch:      "noarch",
			Version:   "1",
			Uninstall: goolib.ExecFile{Path: "missing-uninstall.exe"},
		}
		specD := goolib.PkgSpec{
			Name:            "D",
			Arch:            "noarch",
			Version:         "1",
			PkgDependencies: map[string]string{"A": "1"},
		}
		gooA := filepath.Join(cacheDir, "A.noarch.1.goo")
		gooD := filepath.Join(cacheDir, "D.noarch.1.goo")
		writeOwnershipTestGoo(t, gooA, specA)
		writeOwnershipTestGoo(t, gooD, specD)

		if err := db.WriteStateToDB(client.GooGetState{
			{
				PackageSpec:    &specA,
				LocalPath:      gooA,
				InstalledFiles: map[string]string{sharedFile: "sum-shared"},
			},
			{
				PackageSpec:    &specD,
				LocalPath:      gooD,
				InstalledFiles: map[string]string{sharedFile: "sum-shared", onlyDFile: "sum-d"},
			},
		}); err != nil {
			t.Fatalf("WriteStateToDB: %v", err)
		}

		piA := goolib.PackageInfo{Name: "A", Arch: "noarch"}
		deps, _, err := EnumerateDeps(piA, db)
		if err != nil {
			t.Fatalf("EnumerateDeps(A): %v", err)
		}
		if err := All(t.Context(), piA, deps, false, downloader, db); err == nil {
			t.Fatal("All(A) succeeded, want error from A's missing uninstall script")
		}
		if _, statErr := os.Stat(onlyDFile); !os.IsNotExist(statErr) {
			t.Errorf("Stat(onlyDFile) = %v, want removed with D", statErr)
		}
		if got, readErr := os.ReadFile(sharedFile); readErr != nil || string(got) != "shared" {
			t.Errorf("sharedFile after aborted cascade = %q, %v; want kept because A remains installed", got, readErr)
		}
	})

	t.Run("empty_directory_listed_by_another_row_is_kept", func(t *testing.T) {
		db, downloader, _ := newRemoveTestDB(t)
		cacheDir := t.TempDir()
		dstDir := t.TempDir()

		sharedDir := filepath.Join(dstDir, "shared-empty-dir")
		onlyADir := filepath.Join(dstDir, "only-a-empty-dir")
		if err := os.MkdirAll(sharedDir, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(onlyADir, 0755); err != nil {
			t.Fatal(err)
		}

		specA := goolib.PkgSpec{Name: "pkgA", Arch: "noarch", Version: "1"}
		specB := goolib.PkgSpec{Name: "pkgB", Arch: "noarch", Version: "1"}
		gooA := filepath.Join(cacheDir, "pkgA.noarch.1.goo")
		gooB := filepath.Join(cacheDir, "pkgB.noarch.1.goo")
		writeOwnershipTestGoo(t, gooA, specA)
		writeOwnershipTestGoo(t, gooB, specB)

		rowA := client.PackageState{
			PackageSpec: &specA,
			LocalPath:   gooA,
			InstalledFiles: map[string]string{
				sharedDir: "",
				onlyADir:  "",
			},
		}
		rowB := client.PackageState{
			PackageSpec: &specB,
			LocalPath:   gooB,
			InstalledFiles: map[string]string{
				sharedDir: "",
			},
		}
		if err := db.WriteStateToDB(client.GooGetState{rowA, rowB}); err != nil {
			t.Fatalf("WriteStateToDB: %v", err)
		}

		logs := testutil.CaptureLogs(t)
		if err := UninstallRow(t.Context(), rowA, Options{Downloader: downloader}, db); err != nil {
			t.Fatalf("UninstallRow(pkgA): %v", err)
		}
		if _, err := os.Stat(onlyADir); !os.IsNotExist(err) {
			t.Errorf("Stat(onlyADir) = %v, want removed with pkgA", err)
		}
		if fi, err := os.Stat(sharedDir); err != nil || !fi.IsDir() {
			t.Errorf("Stat(sharedDir) after removing pkgA = %v, %v; want empty directory kept", fi, err)
		}
		wantKept := fmt.Sprintf("file_kept op=remove pkg=pkgA.noarch.1 path=%q owners=pkgB.noarch", sharedDir)
		if !strings.Contains(logs.String(), wantKept) {
			t.Errorf("logs missing %q; got:\n%s", wantKept, logs.String())
		}

		if err := UninstallRow(t.Context(), rowB, Options{Downloader: downloader}, db); err != nil {
			t.Fatalf("UninstallRow(pkgB): %v", err)
		}
		if _, err := os.Stat(sharedDir); !os.IsNotExist(err) {
			t.Errorf("Stat(sharedDir) after removing pkgB = %v, want removed", err)
		}
	})
}

// TestRemoveDBOnlySkipsCorruptUnrelatedRow verifies that db_only removal
// succeeds without decoding unrelated database rows or deleting files, while a
// non-db_only removal in the same database fails before running the uninstall
// script and deletes nothing.
func TestRemoveDBOnlySkipsCorruptUnrelatedRow(t *testing.T) {
	db, downloader, dbFile := newRemoveTestDB(t)
	cacheDir := t.TempDir()
	dstDir := t.TempDir()

	fileA := filepath.Join(dstDir, "a.txt")
	if err := os.WriteFile(fileA, []byte("payload A"), 0644); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(dstDir, "uninstall-ran.txt")

	uninstallExec := goolib.ExecFile{Path: "missing-uninstall.exe"}
	var extraEntries []archiveEntry
	if runtime.GOOS != "windows" {
		uninstallExec = goolib.ExecFile{Path: "uninstall.sh", Args: []string{sentinel}}
		extraEntries = append(extraEntries, archiveEntry{
			name: "uninstall.sh",
			body: "#!/bin/sh\necho ran > \"$1\"\n",
			mode: 0755,
		})
	}

	specA := goolib.PkgSpec{
		Name:      "A",
		Arch:      "noarch",
		Version:   "1",
		Uninstall: uninstallExec,
	}
	gooA := filepath.Join(cacheDir, "A.noarch.1.goo")
	writeOwnershipTestGoo(t, gooA, specA, extraEntries...)

	rowA := client.PackageState{
		PackageSpec:    &specA,
		LocalPath:      gooA,
		InstalledFiles: map[string]string{fileA: "sum-a"},
	}
	if err := db.WriteStateToDB(client.GooGetState{rowA}); err != nil {
		t.Fatalf("WriteStateToDB: %v", err)
	}

	// Insert an unrelated row with undecodable pkg_json directly into SQLite.
	rawDB, err := sql.Open("sqlite", dbFile)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { rawDB.Close() })
	if _, err := rawDB.ExecContext(context.Background(),
		`INSERT INTO InstalledPackages (pkg_name, pkg_arch, pkg_ver, pkg_json) VALUES (?, ?, ?, ?)`,
		"corrupt", "noarch", "1", []byte("{not valid json"),
	); err != nil {
		t.Fatalf("inserting corrupt row: %v", err)
	}

	// Non-db_only removal fails on FetchPkgs("") before extracting or running A's
	// uninstall script, leaving A's file, archive, and DB row untouched.
	logs := testutil.CaptureLogs(t)
	err = UninstallRow(t.Context(), rowA, Options{DBOnly: false, Downloader: downloader}, db)
	var syntaxErr *json.SyntaxError
	if !errors.As(err, &syntaxErr) {
		t.Fatalf("UninstallRow(DBOnly=false) error = %v, want *json.SyntaxError", err)
	}
	if _, statErr := os.Stat(sentinel); !os.IsNotExist(statErr) {
		t.Errorf("Stat(sentinel) = %v, want uninstall script not executed", statErr)
	}
	if strings.Contains(logs.String(), "Running uninstall command") {
		t.Errorf("logs show uninstall script started despite corrupt row:\n%s", logs.String())
	}
	if got, readErr := os.ReadFile(fileA); readErr != nil || string(got) != "payload A" {
		t.Errorf("fileA after failed non-db_only removal = %q, %v; want preserved", got, readErr)
	}
	if _, statErr := os.Stat(gooA); statErr != nil {
		t.Errorf("Stat(gooA) after failed non-db_only removal = %v, want preserved", statErr)
	}
	stillThere, err := db.FetchPkg(goolib.PackageInfo{Name: "A", Arch: "noarch"})
	if err != nil || stillThere.PackageSpec == nil {
		t.Fatalf("FetchPkg(A) after failed non-db_only removal = %+v, %v; want A still installed", stillThere, err)
	}

	// All(dbOnly=false) likewise fails before running the uninstall script.
	err = All(t.Context(), goolib.PackageInfo{Name: "A", Arch: "noarch"}, DepMap{}, false, downloader, db)
	if !errors.As(err, &syntaxErr) {
		t.Fatalf("All(dbOnly=false) error = %v, want *json.SyntaxError", err)
	}
	if _, statErr := os.Stat(sentinel); !os.IsNotExist(statErr) {
		t.Errorf("Stat(sentinel) after All(dbOnly=false) = %v, want uninstall script not executed", statErr)
	}

	// DBOnly removal skips FetchPkgs(""), deletes A's row, and leaves files intact.
	if err := UninstallRow(t.Context(), rowA, Options{DBOnly: true, Downloader: downloader}, db); err != nil {
		t.Fatalf("UninstallRow(DBOnly=true) unexpected error: %v", err)
	}
	afterRemove, err := db.FetchPkg(goolib.PackageInfo{Name: "A", Arch: "noarch"})
	if err != nil {
		t.Fatalf("FetchPkg(A) after DBOnly removal: %v", err)
	}
	if diff := cmp.Diff(client.PackageState{}, afterRemove); diff != "" {
		t.Errorf("FetchPkg(A) after DBOnly removal (-want +got):\n%s", diff)
	}
	if got, readErr := os.ReadFile(fileA); readErr != nil || string(got) != "payload A" {
		t.Errorf("fileA after DBOnly removal = %q, %v; want untouched", got, readErr)
	}
	if _, statErr := os.Stat(sentinel); !os.IsNotExist(statErr) {
		t.Errorf("Stat(sentinel) after DBOnly removal = %v, want uninstall script not executed", statErr)
	}
}
