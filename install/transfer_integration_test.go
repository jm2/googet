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
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/googet/v2/client"
	"github.com/google/googet/v2/googetdb"
	"github.com/google/googet/v2/goolib"
	"github.com/google/googet/v2/priority"
	"github.com/google/googet/v2/settings"
	"github.com/google/googet/v2/testutil"
	"github.com/google/googet/v2/verify"
)

// TestOwnershipTransferFromRepoAndFromDisk verifies file ownership transfers
// across FromRepo and FromDisk: differing content strips the path from previous
// owners (including other architectures of the same package name and multiple
// co-owners) and logs file_owner_transfer after commit while verify.Files
// passes for both packages; identical content and empty-checksum entries remain
// co-listed; and DBOnly never transfers ownership.
func TestOwnershipTransferFromRepoAndFromDisk(t *testing.T) {
	t.Run("from_repo_transfers_on_differing_content_and_verifies_both", func(t *testing.T) {
		f := newRelFixture(t, nil, nil)
		shared := filepath.Join(f.dst, "shared.dll")
		onlyA := filepath.Join(f.dst, "only-a.dll")
		onlyB := filepath.Join(f.dst, "only-b.dll")

		rsA := buildRepoArchiveWithFiles(t, f.gooDir, goolib.PkgSpec{
			Name: "A", Arch: "noarch", Version: "1",
			Files: map[string]string{"shared.dll": shared, "only-a.dll": onlyA},
		}, map[string]string{"shared.dll": "from A", "only-a.dll": "exclusive A"})
		rsB := buildRepoArchiveWithFiles(t, f.gooDir, goolib.PkgSpec{
			Name: "B", Arch: "noarch", Version: "1",
			Files: map[string]string{"shared.dll": shared, "only-b.dll": onlyB},
		}, map[string]string{"shared.dll": "from B", "only-b.dll": "exclusive B"})
		publishRepoSpec(f, rsA)
		publishRepoSpec(f, rsB)

		if err := f.install(t, "A"); err != nil {
			t.Fatalf("install(A): %v", err)
		}
		logs := testutil.CaptureLogs(t)
		if err := f.install(t, "B"); err != nil {
			t.Fatalf("install(B): %v", err)
		}

		rowA := f.rows(t)["A"]
		rowB := f.rows(t)["B"]
		if _, ok := rowA.InstalledFiles[shared]; ok {
			t.Errorf("A InstalledFiles still lists %q after B overwrote with different content: %v", shared, rowA.InstalledFiles)
		}
		if _, ok := rowA.InstalledFiles[onlyA]; !ok {
			t.Errorf("A InstalledFiles lost %q: %v", onlyA, rowA.InstalledFiles)
		}
		if gotSum, wantSum := rowB.InstalledFiles[shared], checksumOf(t, shared); gotSum != wantSum {
			t.Errorf("B InstalledFiles[%q] = %q, want %q", shared, gotSum, wantSum)
		}

		wantLog := fmt.Sprintf("file_owner_transfer from=A.noarch to=B.noarch.1 path=%q", shared)
		if !strings.Contains(logs.String(), wantLog) {
			t.Errorf("logs missing %q; got:\n%s", wantLog, logs.String())
		}

		for _, st := range []client.PackageState{rowA, rowB} {
			ok, err := verify.Files(st)
			if err != nil || !ok {
				t.Errorf("verify.Files(%s) = (%t, %v), want (true, nil)", st.PackageSpec, ok, err)
			}
		}
	})

	t.Run("from_disk_and_cross_arch_same_name_transfer", func(t *testing.T) {
		f := newRelFixture(t, nil, nil)
		shared := filepath.Join(f.dst, "cross-arch.dll")
		keepX64 := filepath.Join(f.dst, "x64-only.dll")

		rsX64 := buildRepoArchiveWithFiles(t, f.gooDir, goolib.PkgSpec{
			Name: "B", Arch: "x86_64", Version: "1",
			Files: map[string]string{"cross-arch.dll": shared, "x64-only.dll": keepX64},
		}, map[string]string{"cross-arch.dll": "from x86_64", "x64-only.dll": "keep x64"})
		rsNoarch := buildRepoArchiveWithFiles(t, f.gooDir, goolib.PkgSpec{
			Name: "B", Arch: "noarch", Version: "1",
			Files: map[string]string{"cross-arch.dll": shared},
		}, map[string]string{"cross-arch.dll": "from noarch"})

		ins := New(f.cache, f.db, Options{})
		if err := ins.FromDisk(filepath.Join(f.gooDir, rsX64.Source), false); err != nil {
			t.Fatalf("FromDisk(B.x86_64): %v", err)
		}
		logs := testutil.CaptureLogs(t)
		if err := ins.FromDisk(filepath.Join(f.gooDir, rsNoarch.Source), false); err != nil {
			t.Fatalf("FromDisk(B.noarch): %v", err)
		}

		gotX64, err := f.db.FetchPkg(goolib.PackageInfo{Name: "B", Arch: "x86_64"})
		if err != nil {
			t.Fatalf("FetchPkg(B.x86_64): %v", err)
		}
		gotNoarch, err := f.db.FetchPkg(goolib.PackageInfo{Name: "B", Arch: "noarch"})
		if err != nil {
			t.Fatalf("FetchPkg(B.noarch): %v", err)
		}
		if _, ok := gotX64.InstalledFiles[shared]; ok {
			t.Errorf("B.x86_64 InstalledFiles still lists %q: %v", shared, gotX64.InstalledFiles)
		}
		if _, ok := gotX64.InstalledFiles[keepX64]; !ok {
			t.Errorf("B.x86_64 InstalledFiles lost %q: %v", keepX64, gotX64.InstalledFiles)
		}
		if _, ok := gotNoarch.InstalledFiles[shared]; !ok {
			t.Errorf("B.noarch InstalledFiles missing %q: %v", shared, gotNoarch.InstalledFiles)
		}
		wantLog := fmt.Sprintf("file_owner_transfer from=B.x86_64 to=B.noarch.1 path=%q", shared)
		if !strings.Contains(logs.String(), wantLog) {
			t.Errorf("logs missing %q; got:\n%s", wantLog, logs.String())
		}
	})

	t.Run("identical_content_stays_colisted_and_multiple_owners_reconciled", func(t *testing.T) {
		f := newRelFixture(t, nil, nil)
		sameFile := filepath.Join(f.dst, "identical.dll")
		multiFile := filepath.Join(f.dst, "multi.dll")
		if err := os.WriteFile(sameFile, []byte("identical-bytes"), 0644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(multiFile, []byte("from B"), 0644); err != nil {
			t.Fatal(err)
		}
		sameSum := checksumOf(t, sameFile)
		newMultiSum := checksumOf(t, multiFile)

		seedStates := client.GooGetState{
			{
				PackageSpec:    &goolib.PkgSpec{Name: "O1", Arch: "noarch", Version: "1"},
				InstalledFiles: map[string]string{multiFile: "old-sum-o1", sameFile: sameSum},
			},
			{
				PackageSpec:    &goolib.PkgSpec{Name: "O2", Arch: "noarch", Version: "1"},
				InstalledFiles: map[string]string{multiFile: "old-sum-o2"},
			},
			{
				PackageSpec:    &goolib.PkgSpec{Name: "O3", Arch: "noarch", Version: "1"},
				InstalledFiles: map[string]string{multiFile: newMultiSum},
			},
			{
				PackageSpec:    &goolib.PkgSpec{Name: "O4", Arch: "noarch", Version: "1"},
				InstalledFiles: map[string]string{multiFile: ""},
			},
		}
		if err := f.db.WriteStateToDB(seedStates); err != nil {
			t.Fatalf("WriteStateToDB: %v", err)
		}

		rsB := buildRepoArchiveWithFiles(t, f.gooDir, goolib.PkgSpec{
			Name: "B", Arch: "noarch", Version: "1",
			Files: map[string]string{"identical.dll": sameFile, "multi.dll": multiFile},
		}, map[string]string{"identical.dll": "identical-bytes", "multi.dll": "from B"})
		publishRepoSpec(f, rsB)

		logs := testutil.CaptureLogs(t)
		if err := f.install(t, "B"); err != nil {
			t.Fatalf("install(B): %v", err)
		}

		rows := f.rows(t)
		if _, ok := rows["O1"].InstalledFiles[sameFile]; !ok {
			t.Errorf("O1 lost identical-checksum path %q: %v", sameFile, rows["O1"].InstalledFiles)
		}
		if _, ok := rows["B"].InstalledFiles[sameFile]; !ok {
			t.Errorf("B missing identical-checksum path %q: %v", sameFile, rows["B"].InstalledFiles)
		}
		for _, stripped := range []string{"O1", "O2"} {
			if _, ok := rows[stripped].InstalledFiles[multiFile]; ok {
				t.Errorf("%s still lists differing-checksum %q: %v", stripped, multiFile, rows[stripped].InstalledFiles)
			}
		}
		for _, kept := range []string{"O3", "O4", "B"} {
			if _, ok := rows[kept].InstalledFiles[multiFile]; !ok {
				t.Errorf("%s lost %q (want kept for equal/empty checksum and new owner): %v", kept, multiFile, rows[kept].InstalledFiles)
			}
		}
		if strings.Contains(logs.String(), fmt.Sprintf("path=%q", sameFile)) {
			t.Errorf("unexpected file_owner_transfer logged for identical-checksum %q:\n%s", sameFile, logs.String())
		}
	})

	t.Run("db_only_makes_no_transfer_in_from_repo_or_from_disk", func(t *testing.T) {
		f := newRelFixture(t, nil, nil)
		shared := filepath.Join(f.dst, "dbonly-shared.dll")

		rsA := buildRepoArchiveWithFiles(t, f.gooDir, goolib.PkgSpec{
			Name: "A", Arch: "noarch", Version: "1",
			Files: map[string]string{"dbonly-shared.dll": shared},
		}, map[string]string{"dbonly-shared.dll": "from A"})
		rsB := buildRepoArchiveWithFiles(t, f.gooDir, goolib.PkgSpec{
			Name: "B", Arch: "noarch", Version: "1",
			Files: map[string]string{"dbonly-shared.dll": shared},
		}, map[string]string{"dbonly-shared.dll": "from B"})
		rsC := buildRepoArchiveWithFiles(t, f.gooDir, goolib.PkgSpec{
			Name: "C", Arch: "noarch", Version: "1",
			Files: map[string]string{"dbonly-shared.dll": shared},
		}, map[string]string{"dbonly-shared.dll": "from C"})
		publishRepoSpec(f, rsA)
		publishRepoSpec(f, rsB)

		if err := f.install(t, "A"); err != nil {
			t.Fatalf("install(A): %v", err)
		}
		wantSumA := f.rows(t)["A"].InstalledFiles[shared]

		logs := testutil.CaptureLogs(t)
		if err := FromRepo(t.Context(), goolib.PackageInfo{Name: "B"}, "", f.cache, f.rm, []string{"noarch"}, true, false, f.downloader, f.db); err != nil {
			t.Fatalf("FromRepo(B, dbOnly=true): %v", err)
		}
		insDBOnly := New(f.cache, f.db, Options{DBOnly: true})
		if err := insDBOnly.FromDisk(filepath.Join(f.gooDir, rsC.Source), false); err != nil {
			t.Fatalf("FromDisk(C, DBOnly=true): %v", err)
		}

		if gotBytes, err := os.ReadFile(shared); err != nil || string(gotBytes) != "from A" {
			t.Errorf("disk content of %q = %q, %v; want %q", shared, gotBytes, err, "from A")
		}
		if gotSumA := f.rows(t)["A"].InstalledFiles[shared]; gotSumA != wantSumA {
			t.Errorf("A InstalledFiles[%q] after DBOnly installs = %q, want %q", shared, gotSumA, wantSumA)
		}
		if strings.Contains(logs.String(), "file_owner_transfer") {
			t.Errorf("unexpected file_owner_transfer logged during DBOnly installs:\n%s", logs.String())
		}
	})
}

// TestOwnershipTransferFailureModes verifies that failed install scripts,
// stageAddPkg failures across both replace attempts, and trigger-driven
// AddPkgWithTransfers failures on the replace path leave existing owners'
// InstalledFiles intact and never log file_owner_transfer.
func TestOwnershipTransferFailureModes(t *testing.T) {
	t.Run("install_script_failure_restores_content_and_keeps_owner", func(t *testing.T) {
		f := newRelFixture(t, nil, nil)
		shared := filepath.Join(f.dst, "script-fail.dll")

		rsA := buildRepoArchiveWithFiles(t, f.gooDir, goolib.PkgSpec{
			Name: "A", Arch: "noarch", Version: "1",
			Files: map[string]string{"script-fail.dll": shared},
		}, map[string]string{"script-fail.dll": "from A"})
		rsB := buildRepoArchiveWithFiles(t, f.gooDir, goolib.PkgSpec{
			Name: "B", Arch: "noarch", Version: "1",
			Install: failingScript,
			Files:   map[string]string{"script-fail.dll": shared},
		}, map[string]string{"script-fail.dll": "from B"})
		publishRepoSpec(f, rsA)
		publishRepoSpec(f, rsB)

		if err := f.install(t, "A"); err != nil {
			t.Fatalf("install(A): %v", err)
		}
		wantSumA := f.rows(t)["A"].InstalledFiles[shared]

		logs := testutil.CaptureLogs(t)
		if err := f.install(t, "B"); err == nil {
			t.Fatal("install(B) = nil, want failingScript error")
		}

		if got, err := os.ReadFile(shared); err != nil || string(got) != "from A" {
			t.Errorf("shared file after B rollback = %q, %v; want %q", got, err, "from A")
		}
		if gotSumA := f.rows(t)["A"].InstalledFiles[shared]; gotSumA != wantSumA {
			t.Errorf("A InstalledFiles[%q] = %q, want %q", shared, gotSumA, wantSumA)
		}
		if _, exists := f.rows(t)["B"]; exists {
			t.Error("B recorded in DB despite failing install script")
		}
		if strings.Contains(logs.String(), "file_owner_transfer") {
			t.Errorf("unexpected file_owner_transfer logged on failed install:\n%s", logs.String())
		}
	})

	t.Run("stage_add_pkg_fails_on_both_replace_attempts_rolls_back_and_recovers", func(t *testing.T) {
		f := newRelFixture(t,
			[]goolib.PkgSpec{{Name: "P", Arch: "noarch", Version: "1"}},
			nil,
		)
		shared := filepath.Join(f.dst, "replace-shared.dll")
		seedPackageWithExtraFiles(t, f, goolib.PkgSpec{Name: "A", Arch: "noarch", Version: "1"}, map[string]string{
			"replace-shared.dll": "from A",
		})
		wantSumA := f.rows(t)["A"].InstalledFiles[shared]
		pArchive := f.rows(t)["P"].LocalPath

		rsR := buildRepoArchiveWithFiles(t, f.gooDir, goolib.PkgSpec{
			Name: "R", Arch: "noarch", Version: "1",
			Replaces: []string{"P"},
			Files:    map[string]string{"replace-shared.dll": shared},
		}, map[string]string{"replace-shared.dll": "from R"})
		publishRepoSpec(f, rsR)

		logs := testutil.CaptureLogs(t)
		attempts := 0
		err := f.runWithHook(t, t.Context(), Options{}, "R", func(stage applyStage, step string) error {
			if stage == stageAddPkg && step == "R.noarch.1" {
				attempts++
				return errors.New("injected addPkg failure")
			}
			return nil
		})
		if err == nil || !strings.Contains(err.Error(), "reinstalled the packages that installing R.noarch.1 had removed: P.noarch.1") {
			t.Fatalf("runWithHook(R) = %v, want the write error and P reinstalled", err)
		}
		if _, exists := f.rows(t)["P"]; !exists {
			t.Error("P not recorded after recovery")
		}
		if got, readErr := os.ReadFile(shared); readErr != nil || string(got) != "from A" {
			t.Errorf("shared file = %q, %v; want A's content restored by the rollback", got, readErr)
		}
		if attempts != 2 {
			t.Errorf("stageAddPkg attempts = %d, want 2", attempts)
		}
		if _, statErr := os.Stat(pArchive); statErr != nil {
			t.Errorf("Stat(victim archive %q) = %v, want kept on disk", pArchive, statErr)
		}
		if gotSumA := f.rows(t)["A"].InstalledFiles[shared]; gotSumA != wantSumA {
			t.Errorf("A InstalledFiles[%q] = %q, want %q", shared, gotSumA, wantSumA)
		}
		if strings.Contains(logs.String(), "file_owner_transfer") {
			t.Errorf("unexpected file_owner_transfer logged when stageAddPkg failed:\n%s", logs.String())
		}
	})

	t.Run("sqlite_trigger_aborts_add_pkg_with_transfers_on_replace_path_rolls_back_and_recovers", func(t *testing.T) {
		f := newRelFixture(t,
			[]goolib.PkgSpec{{Name: "P", Arch: "noarch", Version: "1"}},
			nil,
		)
		shared := filepath.Join(f.dst, "trigger-shared.dll")
		seedPackageWithExtraFiles(t, f, goolib.PkgSpec{Name: "A", Arch: "noarch", Version: "1"}, map[string]string{
			"trigger-shared.dll": "from A",
		})
		wantSumA := f.rows(t)["A"].InstalledFiles[shared]
		pArchive := f.rows(t)["P"].LocalPath

		rsR := buildRepoArchiveWithFiles(t, f.gooDir, goolib.PkgSpec{
			Name: "R", Arch: "noarch", Version: "1",
			Replaces: []string{"P"},
			Files:    map[string]string{"trigger-shared.dll": shared},
		}, map[string]string{"trigger-shared.dll": "from R"})
		publishRepoSpec(f, rsR)

		rawSQL, err := sql.Open("sqlite", settings.DBFile())
		if err != nil {
			t.Fatalf("sql.Open(DBFile): %v", err)
		}
		if _, err := rawSQL.Exec(`
			CREATE TRIGGER abort_replacer_insert
			BEFORE INSERT ON InstalledPackages
			WHEN NEW.pkg_name = 'R'
			BEGIN
				SELECT RAISE(ABORT, 'simulated sqlite trigger failure');
			END;
		`); err != nil {
			_ = rawSQL.Close()
			t.Fatalf("creating trigger: %v", err)
		}
		if err := rawSQL.Close(); err != nil {
			t.Fatalf("closing rawSQL: %v", err)
		}

		logs := testutil.CaptureLogs(t)
		err = f.install(t, "R")
		if err == nil || !strings.Contains(err.Error(), "reinstalled the packages that installing R.noarch.1 had removed: P.noarch.1") {
			t.Fatalf("install(R) = %v, want the write error and P reinstalled", err)
		}
		if _, exists := f.rows(t)["P"]; !exists {
			t.Error("P not recorded after recovery")
		}
		if got, readErr := os.ReadFile(shared); readErr != nil || string(got) != "from A" {
			t.Errorf("shared file = %q, %v; want A's content restored by the rollback", got, readErr)
		}
		if _, statErr := os.Stat(pArchive); statErr != nil {
			t.Errorf("Stat(victim archive %q) = %v, want kept on disk", pArchive, statErr)
		}
		if gotSumA := f.rows(t)["A"].InstalledFiles[shared]; gotSumA != wantSumA {
			t.Errorf("A InstalledFiles[%q] = %q, want %q (SQLite rollback must preserve A's ownership)", shared, gotSumA, wantSumA)
		}
		if _, exists := f.rows(t)["R"]; exists {
			t.Error("R row exists despite BEFORE INSERT trigger")
		}
		if strings.Contains(logs.String(), "file_owner_transfer") {
			t.Errorf("unexpected file_owner_transfer logged when AddPkgWithTransfers failed:\n%s", logs.String())
		}
	})
}

// TestRecoveryReinstallDoesNotTransferOwnership verifies that when replacement
// recovery reinstalls a removed victim whose archive overwrites a file
// co-listed by a surviving package Q with different content, Q's InstalledFiles
// row remains completely unchanged and no file_owner_transfer is logged
// (pinning recovery's existing Force=true overwrite of the file on disk).
func TestRecoveryReinstallDoesNotTransferOwnership(t *testing.T) {
	f := newRelFixture(t, nil, []goolib.PkgSpec{
		{
			Name:     "R",
			Arch:     "noarch",
			Version:  "1",
			Replaces: []string{"P"},
			Install:  failingScript,
		},
	})

	seedPackageWithExtraFiles(t, f, goolib.PkgSpec{Name: "P", Arch: "noarch", Version: "1"}, map[string]string{
		"colisted.dll": "from P",
	})
	seedPackageWithExtraFiles(t, f, goolib.PkgSpec{Name: "Q", Arch: "noarch", Version: "1"}, map[string]string{
		"colisted.dll": "from Q",
	})

	colisted := filepath.Join(f.dst, "colisted.dll")
	beforeQ := f.rows(t)["Q"]

	logs := testutil.CaptureLogs(t)
	err := f.install(t, "R")
	if err == nil || !strings.Contains(err.Error(), "reinstalled the packages that installing R.noarch.1 had removed: P.noarch.1") {
		t.Fatalf("install(R) = %v, want failingScript error with P.noarch.1 recovered", err)
	}

	// As before, recovery reinstalls P with Force=true,
	// writing P's archive content onto colisted.dll.
	if gotDisk, readErr := os.ReadFile(colisted); readErr != nil || string(gotDisk) != "from P" {
		t.Errorf("colisted.dll on disk after recovery = %q, %v; want pre-existing recovery content %q", gotDisk, readErr, "from P")
	}

	// Recovery never transfers ownership away from Q.
	afterQ := f.rows(t)["Q"]
	if diff := cmp.Diff(beforeQ.InstalledFiles, afterQ.InstalledFiles); diff != "" {
		t.Errorf("Q InstalledFiles changed during recovery (-want +got):\n%s", diff)
	}
	if strings.Contains(logs.String(), "file_owner_transfer") {
		t.Errorf("unexpected file_owner_transfer logged during replacement recovery:\n%s", logs.String())
	}
}

// TestDefaultStrictConflictsFalseAndReadmeContract pins that StrictConflicts
// defaults to false under an unconfigured settings.Initialize, that overlapping
// installs proceed with a warning by default, and that README.md documents the
// file ownership contract.
func TestDefaultStrictConflictsFalseAndReadmeContract(t *testing.T) {
	settings.Initialize(t.TempDir(), false)
	if settings.StrictConflicts {
		t.Fatal("settings.StrictConflicts = true with default configuration, want false")
	}
	opts := OptionsFromSettings(false, false)
	if opts.StrictConflicts {
		t.Fatal("OptionsFromSettings(false, false).StrictConflicts = true, want false")
	}

	db, err := googetdb.NewDB(settings.DBFile())
	if err != nil {
		t.Fatalf("googetdb.NewDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	downloader, err := client.NewDownloader("")
	if err != nil {
		t.Fatalf("client.NewDownloader: %v", err)
	}

	gooDir := t.TempDir()
	cache := t.TempDir()
	dst := t.TempDir()
	shared := filepath.Join(dst, "overlap.txt")

	rsA := buildRepoArchiveWithFiles(t, gooDir, goolib.PkgSpec{
		Name: "A", Arch: "noarch", Version: "1",
		Files: map[string]string{"overlap.txt": shared},
	}, map[string]string{"overlap.txt": "alpha"})
	rsB := buildRepoArchiveWithFiles(t, gooDir, goolib.PkgSpec{
		Name: "B", Arch: "noarch", Version: "1",
		Files: map[string]string{"overlap.txt": shared},
	}, map[string]string{"overlap.txt": "beta"})

	srv := testutil.ServeGoo(t, gooDir)
	rm := client.RepoMap{
		srv.URL: client.Repo{
			Priority: priority.Default,
			Packages: []goolib.RepoSpec{rsA, rsB},
		},
	}

	if err := FromRepo(t.Context(), goolib.PackageInfo{Name: "A"}, "", cache, rm, []string{"noarch"}, false, false, downloader, db); err != nil {
		t.Fatalf("FromRepo(A): %v", err)
	}
	logs := testutil.CaptureLogs(t)
	if err := FromRepo(t.Context(), goolib.PackageInfo{Name: "B"}, "", cache, rm, []string{"noarch"}, false, false, downloader, db); err != nil {
		t.Fatalf("FromRepo(B) with default StrictConflicts=false failed: %v", err)
	}
	wantWarn := fmt.Sprintf("Warning: file conflict: %s is already owned by package A", shared)
	if !strings.Contains(logs.String(), wantWarn) {
		t.Errorf("logs missing conflict warning %q; got:\n%s", wantWarn, logs.String())
	}

	readmeBytes, err := os.ReadFile(filepath.Join("..", "README.md"))
	if err != nil {
		t.Fatalf("reading README.md: %v", err)
	}
	readme := string(readmeBytes)
	for _, required := range []string{
		"## File ownership",
		"`strictconflicts` defaults to `false`",
		"file_owner_transfer",
		"file_kept",
		"DOS 8.3 short names",
	} {
		if !strings.Contains(readme, required) {
			t.Errorf("README.md missing %q in File ownership section", required)
		}
	}
}
