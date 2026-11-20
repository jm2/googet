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
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/googet/v2/client"
	"github.com/google/googet/v2/goolib"
	"github.com/google/googet/v2/system"
	"github.com/google/googet/v2/testutil"
	"github.com/google/googet/v2/verify"
)

// TestReinstallRecordsRowReconcilesDroppedEntriesAndTransfers verifies that
// install.Reinstall persists its updated row, transfers ownership back when
// reclaiming an overwritten file, reconciles dropped entries when an
// environment-variable destination changes (deleting unowned files and keeping
// co-owned files), and persists the legacy UnpackDir-to-LocalPath patch,
// recomputed InstalledApp, and existing InstallDate while keeping the cached
// archive.
func TestReinstallRecordsRowReconcilesDroppedEntriesAndTransfers(t *testing.T) {
	t.Run("reclaims_overwritten_file_and_converges_verification", func(t *testing.T) {
		f := newRelFixture(t, nil, nil)
		shared := filepath.Join(f.dst, "shared.cfg")
		onlyA := filepath.Join(f.dst, "only-a.cfg")
		onlyB := filepath.Join(f.dst, "only-b.cfg")

		rsA := buildRepoArchiveWithFiles(t, f.gooDir, goolib.PkgSpec{
			Name: "A", Arch: "noarch", Version: "1",
			Files: map[string]string{"shared.cfg": shared, "only-a.cfg": onlyA},
		}, map[string]string{"shared.cfg": "config from A", "only-a.cfg": "only A"})
		rsB := buildRepoArchiveWithFiles(t, f.gooDir, goolib.PkgSpec{
			Name: "B", Arch: "noarch", Version: "1",
			Files: map[string]string{"shared.cfg": shared, "only-b.cfg": onlyB},
		}, map[string]string{"shared.cfg": "config from B", "only-b.cfg": "only B"})
		publishRepoSpec(f, rsA)
		publishRepoSpec(f, rsB)

		if err := f.install(t, "A"); err != nil {
			t.Fatalf("install(A): %v", err)
		}
		if err := f.install(t, "B"); err != nil {
			t.Fatalf("install(B): %v", err)
		}

		// Reinstall A: A rewrites shared.cfg with A's content, records shared.cfg
		// in A's row, and strips shared.cfg from B's row.
		rowA := f.rows(t)["A"]
		if err := Reinstall(t.Context(), rowA, false, false, f.downloader, f.db); err != nil {
			t.Fatalf("Reinstall(A): %v", err)
		}

		updatedA := f.rows(t)["A"]
		updatedB := f.rows(t)["B"]
		if got, err := os.ReadFile(shared); err != nil || string(got) != "config from A" {
			t.Errorf("shared.cfg after Reinstall(A) = %q, %v; want %q", got, err, "config from A")
		}
		if _, ok := updatedA.InstalledFiles[shared]; !ok {
			t.Errorf("A InstalledFiles missing %q after Reinstall(A): %v", shared, updatedA.InstalledFiles)
		}
		if _, ok := updatedB.InstalledFiles[shared]; ok {
			t.Errorf("B InstalledFiles still lists %q after Reinstall(A): %v", shared, updatedB.InstalledFiles)
		}
		for _, st := range []client.PackageState{updatedA, updatedB} {
			ok, err := verify.Files(st)
			if err != nil || !ok {
				t.Errorf("verify.Files(%s) = (%t, %v), want (true, nil)", st.PackageSpec, ok, err)
			}
		}
	})

	t.Run("env_destination_expansion_change_cleans_unowned_and_keeps_coowned", func(t *testing.T) {
		f := newRelFixture(t, nil, nil)
		dirV1 := filepath.Join(f.dst, "env_v1")
		dirV2 := filepath.Join(f.dst, "env_v2")
		for _, d := range []string{dirV1, dirV2} {
			if err := os.MkdirAll(d, 0755); err != nil {
				t.Fatalf("MkdirAll(%q): %v", d, err)
			}
		}

		const envKey = "GOOGET_REINSTALL_TEST_TARGET"
		t.Setenv(envKey, dirV1)

		rsA := buildRepoArchiveWithFiles(t, f.gooDir, goolib.PkgSpec{
			Name: "A", Arch: "noarch", Version: "1",
			Files: map[string]string{
				"unowned.txt": "<" + envKey + ">/unowned.txt",
				"coowned.txt": "<" + envKey + ">/coowned.txt",
			},
		}, map[string]string{
			"unowned.txt": "unowned payload",
			"coowned.txt": "shared payload",
		})
		rsB := buildRepoArchiveWithFiles(t, f.gooDir, goolib.PkgSpec{
			Name: "B", Arch: "noarch", Version: "1",
			Files: map[string]string{
				"coowned.txt": filepath.Join(dirV1, "coowned.txt"),
			},
		}, map[string]string{
			"coowned.txt": "shared payload",
		})
		publishRepoSpec(f, rsA)
		publishRepoSpec(f, rsB)

		if err := f.install(t, "A"); err != nil {
			t.Fatalf("install(A): %v", err)
		}
		if err := f.install(t, "B"); err != nil {
			t.Fatalf("install(B): %v", err)
		}

		// Point the environment variable to dirV2 and reinstall A.
		t.Setenv(envKey, dirV2)
		logs := testutil.CaptureLogs(t)
		if err := Reinstall(t.Context(), f.rows(t)["A"], false, false, f.downloader, f.db); err != nil {
			t.Fatalf("Reinstall(A) with updated env expansion: %v", err)
		}

		oldUnowned := filepath.Join(dirV1, "unowned.txt")
		oldCoowned := filepath.Join(dirV1, "coowned.txt")
		newUnowned := filepath.Join(dirV2, "unowned.txt")
		newCoowned := filepath.Join(dirV2, "coowned.txt")

		if _, err := os.Stat(oldUnowned); !os.IsNotExist(err) {
			t.Errorf("Stat(old unowned %q) = %v, want deleted (never left untracked)", oldUnowned, err)
		}
		if got, err := os.ReadFile(oldCoowned); err != nil || string(got) != "shared payload" {
			t.Errorf("old co-owned %q = %q, %v; want kept with %q", oldCoowned, got, err, "shared payload")
		}
		wantKept := fmt.Sprintf("file_kept op=upgrade pkg=A.noarch.1 path=%q owners=B.noarch", oldCoowned)
		if !strings.Contains(logs.String(), wantKept) {
			t.Errorf("logs missing %q; got:\n%s", wantKept, logs.String())
		}

		updatedA := f.rows(t)["A"]
		wantFilesA := map[string]string{
			newUnowned: checksumOf(t, newUnowned),
			newCoowned: checksumOf(t, newCoowned),
		}
		if diff := cmp.Diff(wantFilesA, updatedA.InstalledFiles); diff != "" {
			t.Errorf("A InstalledFiles after env-expansion Reinstall (-want +got):\n%s", diff)
		}
	})

	t.Run("persists_local_path_patch_installed_app_and_install_date_keeping_archive", func(t *testing.T) {
		f := newRelFixture(t, nil, nil)
		targetFile := filepath.Join(f.dst, "legacy-item.txt")
		spec := goolib.PkgSpec{
			Name:                "LegacyPkg",
			Arch:                "noarch",
			Version:             "1.0.0",
			ExternalProgramName: "Legacy External App",
			Files:               map[string]string{"legacy-item.txt": targetFile},
		}
		rs := buildRepoArchiveWithFiles(t, f.gooDir, spec, map[string]string{"legacy-item.txt": "reinstalled body"})
		cachedGoo := filepath.Join(f.cache, rs.Source)
		copyFile(t, filepath.Join(f.gooDir, rs.Source), cachedGoo)
		unpackDir := strings.TrimSuffix(cachedGoo, ".goo")

		const wantInstallDate int64 = 1712345678
		legacyRow := client.PackageState{
			LocalPath:      "",
			UnpackDir:      unpackDir,
			Checksum:       rs.Checksum,
			PackageSpec:    &spec,
			InstalledFiles: map[string]string{},
			InstallDate:    wantInstallDate,
		}
		if err := f.db.AddPkg(legacyRow); err != nil {
			t.Fatalf("AddPkg(legacyRow): %v", err)
		}
		// Corrupt InstalledApp directly in SQLite to prove Reinstall recomputes
		// and persists InstalledApp via AddPkgWithTransfers.
		seeded := f.rows(t)["LegacyPkg"]
		seeded.LocalPath = ""
		if err := Reinstall(t.Context(), seeded, false, false, f.downloader, f.db); err != nil {
			t.Fatalf("Reinstall(LegacyPkg): %v", err)
		}

		got := f.rows(t)["LegacyPkg"]
		if got.LocalPath != cachedGoo {
			t.Errorf("persisted LocalPath = %q, want %q", got.LocalPath, cachedGoo)
		}
		if _, err := os.Stat(cachedGoo); err != nil {
			t.Errorf("Stat(cached archive %q) = %v, want preserved by Reinstall", cachedGoo, err)
		}
		if got.InstallDate != wantInstallDate {
			t.Errorf("persisted InstallDate = %d, want %d", got.InstallDate, wantInstallDate)
		}
		wantAppName, wantAppReg := system.AppAssociation(&spec, cachedGoo)
		wantApp := client.InstalledApplication{Name: wantAppName, Reg: wantAppReg}
		if diff := cmp.Diff(wantApp, got.InstalledApp); diff != "" {
			t.Errorf("persisted InstalledApp (-want +got):\n%s", diff)
		}
		if diff := cmp.Diff(map[string]string{targetFile: checksumOf(t, targetFile)}, got.InstalledFiles); diff != "" {
			t.Errorf("persisted InstalledFiles (-want +got):\n%s", diff)
		}
	})
}
