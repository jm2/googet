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
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/googet/v2/client"
	"github.com/google/googet/v2/googetdb"
	"github.com/google/googet/v2/goolib"
)

// writeUninstallScriptGoo writes a .goo archive containing an uninstall script
// with the given contents and file mode.
func writeUninstallScriptGoo(t *testing.T, archivePath, scriptName, scriptBody string) {
	t.Helper()
	f, err := os.Create(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	gw := gzip.NewWriter(f)
	tw := tar.NewWriter(gw)
	if err := tw.WriteHeader(&tar.Header{
		Name: scriptName,
		Mode: 0755,
		Size: int64(len(scriptBody)),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte(scriptBody)); err != nil {
		t.Fatal(err)
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

// TestUninstallRowMultiArchAndOptions checks that UninstallRow acts only on
// the exact (name, arch, version) row, refuses missing or version-mismatched
// sibling architectures without falling back, honors KeepCache and DBOnly,
// resolves old-client UnpackDir archives, and keeps the database row when the
// uninstall script fails.
func TestUninstallRowMultiArchAndOptions(t *testing.T) {
	t.Run("exact_arch_and_version_selection", func(t *testing.T) {
		dir := t.TempDir()
		noarchArchive := filepath.Join(dir, "foo.noarch.1.goo")
		x64Archive := filepath.Join(dir, "foo.x86_64.2.goo")
		writeTestGoo(t, noarchArchive)
		writeTestGoo(t, x64Archive)
		noarchFile := filepath.Join(dir, "foo-noarch")
		x64File := filepath.Join(dir, "foo-x64")
		if err := os.WriteFile(noarchFile, []byte("noarch"), 0644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(x64File, []byte("x64"), 0644); err != nil {
			t.Fatal(err)
		}

		db, err := googetdb.NewDB(filepath.Join(dir, "googet.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { db.Close() })

		noarchRow := client.PackageState{
			PackageSpec:    &goolib.PkgSpec{Name: "foo", Arch: "noarch", Version: "1"},
			LocalPath:      noarchArchive,
			InstalledFiles: map[string]string{noarchFile: "hash1"},
		}
		x64Row := client.PackageState{
			PackageSpec:    &goolib.PkgSpec{Name: "foo", Arch: "x86_64", Version: "2"},
			LocalPath:      x64Archive,
			InstalledFiles: map[string]string{x64File: "hash2"},
		}
		if err := db.WriteStateToDB(client.GooGetState{noarchRow, x64Row}); err != nil {
			t.Fatal(err)
		}

		// Refuses a zero PackageState without panicking.
		if err := UninstallRow(t.Context(), client.PackageState{}, Options{}, db); err == nil || !strings.Contains(err.Error(), "missing package spec") {
			t.Fatalf("UninstallRow(empty) = %v, want missing package spec error", err)
		}

		// Refuses an uninstalled architecture even though FetchPkg falls back
		// to the last installed row of the same name.
		armRow := client.PackageState{PackageSpec: &goolib.PkgSpec{Name: "foo", Arch: "arm64", Version: "2"}}
		if err := UninstallRow(t.Context(), armRow, Options{}, db); err == nil || !strings.Contains(err.Error(), "foo.arm64.2 is no longer installed") {
			t.Fatalf("UninstallRow(arm64) = %v, want no-longer-installed error", err)
		}

		// Removes only foo.noarch.1 and deletes its archive when KeepCache is false.
		if err := UninstallRow(t.Context(), noarchRow, Options{KeepCache: false}, db); err != nil {
			t.Fatalf("UninstallRow(noarch): %v", err)
		}
		if _, err := os.Stat(noarchArchive); !os.IsNotExist(err) {
			t.Errorf("noarch archive Stat = %v, want NotExist", err)
		}
		if _, err := os.Stat(noarchFile); !os.IsNotExist(err) {
			t.Errorf("noarch installed file Stat = %v, want NotExist", err)
		}
		if _, err := os.Stat(x64Archive); err != nil {
			t.Errorf("x86_64 archive Stat = %v, want preserved", err)
		}
		if _, err := os.Stat(x64File); err != nil {
			t.Errorf("x86_64 installed file Stat = %v, want preserved", err)
		}
		state, err := db.FetchPkgs("")
		if err != nil {
			t.Fatal(err)
		}
		var remaining []string
		for _, ps := range state {
			remaining = append(remaining, ps.PackageSpec.String())
		}
		if diff := cmp.Diff([]string{"foo.x86_64.2"}, remaining); diff != "" {
			t.Errorf("remaining rows (-want +got):\n%s", diff)
		}
	})

	t.Run("db_only_skips_missing_archive_and_keeps_files", func(t *testing.T) {
		dir := t.TempDir()
		installedFile := filepath.Join(dir, "bar-file")
		if err := os.WriteFile(installedFile, []byte("keep"), 0644); err != nil {
			t.Fatal(err)
		}
		db, err := googetdb.NewDB(filepath.Join(dir, "googet.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { db.Close() })
		row := client.PackageState{
			PackageSpec:    &goolib.PkgSpec{Name: "bar", Arch: "noarch", Version: "1", Uninstall: goolib.ExecFile{Path: "missing-uninstall.sh"}},
			LocalPath:      filepath.Join(dir, "nonexistent.goo"),
			InstalledFiles: map[string]string{installedFile: "hash"},
		}
		if err := db.WriteStateToDB(client.GooGetState{row}); err != nil {
			t.Fatal(err)
		}
		if err := UninstallRow(t.Context(), row, Options{DBOnly: true}, db); err != nil {
			t.Fatalf("UninstallRow(DBOnly): %v", err)
		}
		if _, err := os.Stat(installedFile); err != nil {
			t.Errorf("installed file Stat = %v, want kept under DBOnly", err)
		}
		if state, _ := db.FetchPkgs(""); len(state) != 0 {
			t.Errorf("remaining rows = %v, want empty", state)
		}
	})

	t.Run("old_client_unpack_dir_and_failing_uninstall_script", func(t *testing.T) {
		dir := t.TempDir()
		unpackDir := filepath.Join(dir, "legacy")
		archive := unpackDir + ".goo"
		writeUninstallScriptGoo(t, archive, "uninstall.sh", "#!/bin/sh\nexit 1\n")

		db, err := googetdb.NewDB(filepath.Join(dir, "googet.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { db.Close() })

		row := client.PackageState{
			PackageSpec: &goolib.PkgSpec{Name: "legacy", Arch: "noarch", Version: "1", Uninstall: goolib.ExecFile{Path: "uninstall.sh"}},
			UnpackDir:   unpackDir,
		}
		if err := db.WriteStateToDB(client.GooGetState{row}); err != nil {
			t.Fatal(err)
		}

		if err := UninstallRow(t.Context(), row, Options{KeepCache: true}, db); err == nil {
			t.Fatal("UninstallRow with failing uninstall script succeeded, want error")
		}
		state, err := db.FetchPkgs("")
		if err != nil {
			t.Fatal(err)
		}
		if !slices.ContainsFunc(state, func(ps client.PackageState) bool { return ps.PackageSpec.String() == "legacy.noarch.1" }) {
			t.Errorf("state after failed uninstall = %v, want legacy.noarch.1 still recorded", state)
		}
		if _, err := os.Stat(archive); err != nil {
			t.Errorf("archive %s Stat = %v, want kept after failed uninstall", archive, err)
		}

		// Replace archive with a succeeding script and verify KeepCache: false removes UnpackDir+".goo".
		writeUninstallScriptGoo(t, archive, "uninstall.sh", "#!/bin/sh\nexit 0\n")
		if err := UninstallRow(t.Context(), row, Options{KeepCache: false}, db); err != nil {
			t.Fatalf("UninstallRow(KeepCache: false): %v", err)
		}
		if _, err := os.Stat(archive); !os.IsNotExist(err) {
			t.Errorf("archive %s Stat = %v, want deleted when KeepCache is false", archive, err)
		}
	})
}
