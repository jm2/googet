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
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/googet/v2/client"
	"github.com/google/googet/v2/googetdb"
	"github.com/google/googet/v2/goolib"
	"github.com/google/googet/v2/remove"
	"github.com/google/googet/v2/settings"
	"github.com/google/googet/v2/testutil"
	"github.com/google/googet/v2/verify"
)

// frozen07fd257UninstallFilesAndDirs is a standalone copy of commit 07fd257
// (GooGet 3.4.2)'s file and directory deletion loop from remove/remove.go,
// kept independent of the ownership deletion planner to verify backward
// compatibility when an older client uninstalls a row containing recorded
// parent directories.
func frozen07fd257UninstallFilesAndDirs(installedFiles map[string]string) {
	if len(installedFiles) > 0 {
		var dirs []string
		for file, chksum := range installedFiles {
			if chksum == "" {
				dirs = append(dirs, file)
				continue
			}
			_, _ = client.RemoveOrRename(file)
		}
		sort.Sort(sort.Reverse(sort.StringSlice(dirs)))
		for _, dir := range dirs {
			_, _ = client.RemoveOrRename(dir)
		}
	}
}

// TestCreatedParentsLifecycleAndCarryForward verifies created-parent recording,
// removal, ownership protection, rollback, DBOnly exclusion, pre-existing
// parent vs pre-existing payload root semantics, shared-parent gap behaviour,
// and carry-forward across FromRepo upgrades (including non-empty dropped
// payload directories), FromDisk installs, replacement recovery, and Reinstall.
func TestCreatedParentsLifecycleAndCarryForward(t *testing.T) {
	t.Run("shared_parent_created_by_first_package_stays_after_both_removed", func(t *testing.T) {
		f := newRelFixture(t, nil, nil)
		sharedParent := filepath.Join(f.dst, "shared_vendor")
		dirA := filepath.Join(sharedParent, "app_a")
		dirB := filepath.Join(sharedParent, "app_b")
		fileA := filepath.Join(dirA, "a.exe")
		fileB := filepath.Join(dirB, "b.exe")

		rsA := buildRepoArchiveWithFiles(t, f.gooDir, goolib.PkgSpec{
			Name: "A", Arch: "noarch", Version: "1",
			Files: map[string]string{"a.exe": fileA},
		}, map[string]string{"a.exe": "from A"})
		rsB := buildRepoArchiveWithFiles(t, f.gooDir, goolib.PkgSpec{
			Name: "B", Arch: "noarch", Version: "1",
			Files: map[string]string{"b.exe": fileB},
		}, map[string]string{"b.exe": "from B"})
		publishRepoSpec(f, rsA)
		publishRepoSpec(f, rsB)

		if err := f.install(t, "A"); err != nil {
			t.Fatalf("install(A): %v", err)
		}
		if err := f.install(t, "B"); err != nil {
			t.Fatalf("install(B): %v", err)
		}

		// Only A created sharedParent; B saw sharedParent already existing.
		if _, ok := f.rows(t)["A"].InstalledFiles[sharedParent]; !ok {
			t.Errorf("A InstalledFiles missing created parent %q", sharedParent)
		}
		if _, ok := f.rows(t)["B"].InstalledFiles[sharedParent]; ok {
			t.Errorf("B InstalledFiles unexpectedly recorded pre-existing parent %q", sharedParent)
		}

		// Removing A deletes dirA and keeps non-empty sharedParent for B;
		// removing B afterwards deletes dirB and leaves unrecorded sharedParent
		// on disk as documented.
		f.remove(t, "A")
		if _, err := os.Stat(dirA); !os.IsNotExist(err) {
			t.Errorf("Stat(%q) after removing A = %v, want removed", dirA, err)
		}
		if _, err := os.Stat(fileB); err != nil {
			t.Errorf("Stat(%q) after removing A = %v, want intact", fileB, err)
		}
		f.remove(t, "B")
		if _, err := os.Stat(dirB); !os.IsNotExist(err) {
			t.Errorf("Stat(%q) after removing B = %v, want removed", dirB, err)
		}
		if _, err := os.Stat(sharedParent); err != nil {
			t.Errorf("Stat(shared parent %q) after removing both = %v, want left on disk", sharedParent, err)
		}
	})

	t.Run("parent_holding_user_file_and_parent_listed_by_another_row", func(t *testing.T) {
		f := newRelFixture(t, nil, nil)

		// User file inside created parent prevents deleting that parent while
		// still removing empty deeper created directories.
		userParent := filepath.Join(f.dst, "with_user_file")
		deepDir := filepath.Join(userParent, "sub")
		pkgFile := filepath.Join(deepDir, "tool.exe")
		rsUser := buildRepoArchiveWithFiles(t, f.gooDir, goolib.PkgSpec{
			Name: "U", Arch: "noarch", Version: "1",
			Files: map[string]string{"tool.exe": pkgFile},
		}, map[string]string{"tool.exe": "tool"})
		publishRepoSpec(f, rsUser)
		if err := f.install(t, "U"); err != nil {
			t.Fatalf("install(U): %v", err)
		}
		userNote := filepath.Join(userParent, "custom.ini")
		if err := os.WriteFile(userNote, []byte("user data"), 0644); err != nil {
			t.Fatal(err)
		}
		f.remove(t, "U")
		if _, err := os.Stat(deepDir); !os.IsNotExist(err) {
			t.Errorf("Stat(%q) = %v, want empty child dir removed", deepDir, err)
		}
		if got, err := os.ReadFile(userNote); err != nil || string(got) != "user data" {
			t.Errorf("user file %q = %q, %v; want preserved", userNote, got, err)
		}

		// Empty created parent still listed by another installed row is kept on
		// A's removal and removed once the last owner B is removed.
		coParent := filepath.Join(f.dst, "co_owned_parent")
		childA := filepath.Join(coParent, "only_a")
		rsA := buildRepoArchiveWithFiles(t, f.gooDir, goolib.PkgSpec{
			Name: "A", Arch: "noarch", Version: "1",
			Files: map[string]string{"a.txt": filepath.Join(childA, "a.txt")},
		}, map[string]string{"a.txt": "a"})
		rsB := buildRepoArchiveWithFiles(t, f.gooDir, goolib.PkgSpec{
			Name: "B", Arch: "noarch", Version: "1",
			Files: map[string]string{"bdir": coParent},
		}, map[string]string{"bdir/b.txt": "b"})
		publishRepoSpec(f, rsA)
		publishRepoSpec(f, rsB)

		if err := f.install(t, "A"); err != nil {
			t.Fatalf("install(A): %v", err)
		}
		if err := f.install(t, "B"); err != nil {
			t.Fatalf("install(B): %v", err)
		}
		// Remove B's file manually so coParent is empty if childA is gone,
		// proving ownership (not non-emptiness) keeps coParent when A is removed.
		if err := os.Remove(filepath.Join(coParent, "b.txt")); err != nil {
			t.Fatal(err)
		}

		logs := testutil.CaptureLogs(t)
		f.remove(t, "A")
		if _, err := os.Stat(childA); !os.IsNotExist(err) {
			t.Errorf("Stat(%q) = %v, want removed with A", childA, err)
		}
		if _, err := os.Stat(coParent); err != nil {
			t.Errorf("Stat(%q) after removing A = %v, want kept while B lists it", coParent, err)
		}
		wantKept := fmt.Sprintf("file_kept op=remove pkg=A.noarch.1 path=%q owners=B.noarch", coParent)
		if !strings.Contains(logs.String(), wantKept) {
			t.Errorf("logs missing %q; got:\n%s", wantKept, logs.String())
		}

		f.remove(t, "B")
		if _, err := os.Stat(coParent); !os.IsNotExist(err) {
			t.Errorf("Stat(%q) after removing last owner B = %v, want removed", coParent, err)
		}
	})

	t.Run("failed_install_and_db_only_record_no_parents", func(t *testing.T) {
		f := newRelFixture(t, nil, nil)
		failParent := filepath.Join(f.dst, "fail_outer")
		failTarget := filepath.Join(failParent, "fail_inner", "item.txt")
		rsFail := buildRepoArchiveWithFiles(t, f.gooDir, goolib.PkgSpec{
			Name: "FailPkg", Arch: "noarch", Version: "1",
			Install: failingScript,
			Files:   map[string]string{"item.txt": failTarget},
		}, map[string]string{"item.txt": "boom"})
		publishRepoSpec(f, rsFail)

		if err := f.install(t, "FailPkg"); err == nil {
			t.Fatal("install(FailPkg) = nil, want error")
		}
		if _, err := os.Stat(failParent); !os.IsNotExist(err) {
			t.Errorf("Stat(%q) after failed install = %v, want rolled back", failParent, err)
		}
		if _, exists := f.rows(t)["FailPkg"]; exists {
			t.Error("FailPkg recorded in DB despite install failure")
		}

		// DBOnly neither creates missing parents on disk nor records them in
		// InstalledFiles.
		dbOnlyParent := filepath.Join(f.dst, "dbonly_outer")
		dbOnlyLeafDir := filepath.Join(dbOnlyParent, "dbonly_inner", "pkgdir")
		dbOnlyLone := filepath.Join(dbOnlyParent, "lone_parent", "lone.txt")
		rsDBOnly := buildRepoArchiveWithFiles(t, f.gooDir, goolib.PkgSpec{
			Name: "DBOnlyPkg", Arch: "noarch", Version: "1",
			Files: map[string]string{
				"pkgdir":   dbOnlyLeafDir,
				"lone.txt": dbOnlyLone,
			},
		}, map[string]string{
			"pkgdir/sub.txt": "sub",
			"lone.txt":       "lone",
		})
		publishRepoSpec(f, rsDBOnly)

		if err := FromRepo(t.Context(), goolib.PackageInfo{Name: "DBOnlyPkg"}, "", f.cache, f.rm, []string{"noarch"}, true, false, f.downloader, f.db); err != nil {
			t.Fatalf("FromRepo(DBOnlyPkg, dbOnly=true): %v", err)
		}
		if _, err := os.Stat(dbOnlyParent); !os.IsNotExist(err) {
			t.Errorf("Stat(%q) after DBOnly install = %v, want not created", dbOnlyParent, err)
		}
		gotDBOnly := f.rows(t)["DBOnlyPkg"].InstalledFiles
		for _, unrecordedParent := range []string{
			dbOnlyParent,
			filepath.Join(dbOnlyParent, "dbonly_inner"),
			filepath.Join(dbOnlyParent, "lone_parent"),
		} {
			if _, ok := gotDBOnly[unrecordedParent]; ok {
				t.Errorf("DBOnly InstalledFiles unexpectedly recorded missing parent %q: %v", unrecordedParent, gotDBOnly)
			}
		}
	})

	t.Run("preexisting_parents_not_recorded_while_preexisting_payload_root_stays_recorded_and_overlapping_payload_dir_deduped", func(t *testing.T) {
		f := newRelFixture(t, nil, nil)
		preOuter := filepath.Join(f.dst, "pre1")
		preInner := filepath.Join(preOuter, "pre2")
		prePayloadRoot := filepath.Join(f.dst, "existing_payload_root")
		for _, d := range []string{preInner, prePayloadRoot} {
			if err := os.MkdirAll(d, 0755); err != nil {
				t.Fatal(err)
			}
		}

		createdParent := filepath.Join(preInner, "created_mid")
		mappedSub := filepath.Join(createdParent, "mapped_sub")
		overlapDir := filepath.Join(f.dst, "overlap_vendor", "overlap_app")

		rs := buildRepoArchiveWithFiles(t, f.gooDir, goolib.PkgSpec{
			Name: "A", Arch: "noarch", Version: "1",
			Files: map[string]string{
				"sub":     mappedSub,
				"rootdir": prePayloadRoot,
				"cfg.txt": filepath.Join(overlapDir, "cfg.txt"),
				"appdir":  overlapDir,
			},
		}, map[string]string{
			"sub/tool.exe":   "tool",
			"rootdir/r.txt":  "root-file",
			"cfg.txt":        "cfg",
			"appdir/lib.dll": "lib",
		})
		publishRepoSpec(f, rs)

		if err := f.install(t, "A"); err != nil {
			t.Fatalf("install(A): %v", err)
		}
		ins := f.rows(t)["A"].InstalledFiles
		for _, preParent := range []string{f.dst, preOuter, preInner} {
			if _, ok := ins[preParent]; ok {
				t.Errorf("pre-existing parent %q recorded in InstalledFiles: %v", preParent, ins)
			}
		}
		for _, recordedDir := range []string{
			createdParent,
			mappedSub,
			prePayloadRoot,
			filepath.Join(f.dst, "overlap_vendor"),
			overlapDir,
		} {
			if sum, ok := ins[recordedDir]; !ok || sum != "" {
				t.Errorf("InstalledFiles[%q] = (%q, %t), want (\"\", true)", recordedDir, sum, ok)
			}
		}

		f.remove(t, "A")
		for _, gone := range []string{createdParent, mappedSub, prePayloadRoot, filepath.Join(f.dst, "overlap_vendor"), overlapDir} {
			if _, err := os.Stat(gone); !os.IsNotExist(err) {
				t.Errorf("Stat(%q) after remove = %v, want removed", gone, err)
			}
		}
		for _, keptPre := range []string{f.dst, preOuter, preInner} {
			if _, err := os.Stat(keptPre); err != nil {
				t.Errorf("Stat(pre-existing parent %q) after remove = %v, want kept", keptPre, err)
			}
		}

		// Installing beneath a pre-existing symlinked directory records only
		// newly created child directories, never the symlink or its target.
		realTarget := filepath.Join(f.dst, "real_target_dir")
		if err := os.MkdirAll(realTarget, 0755); err != nil {
			t.Fatal(err)
		}
		symParent := filepath.Join(f.dst, "sym_parent_dir")
		if err := os.Symlink(realTarget, symParent); err == nil {
			createdUnderSym := filepath.Join(symParent, "created_sub")
			rsSym := buildRepoArchiveWithFiles(t, f.gooDir, goolib.PkgSpec{
				Name: "SymPkg", Arch: "noarch", Version: "1",
				Files: map[string]string{"s.txt": filepath.Join(createdUnderSym, "s.txt")},
			}, map[string]string{"s.txt": "under-symlink"})
			publishRepoSpec(f, rsSym)
			if err := f.install(t, "SymPkg"); err != nil {
				t.Fatalf("install(SymPkg): %v", err)
			}
			symRow := f.rows(t)["SymPkg"]
			for _, unrecorded := range []string{symParent, realTarget} {
				if _, ok := symRow.InstalledFiles[unrecorded]; ok {
					t.Errorf("SymPkg InstalledFiles unexpectedly recorded %q: %v", unrecorded, symRow.InstalledFiles)
				}
			}
			if sum, ok := symRow.InstalledFiles[createdUnderSym]; !ok || sum != "" {
				t.Errorf("SymPkg InstalledFiles[%q] = (%q, %t), want (\"\", true)", createdUnderSym, sum, ok)
			}
			if ok, err := verify.Files(symRow); err != nil || !ok {
				t.Errorf("verify.Files(SymPkg) = (%t, %v), want (true, nil)", ok, err)
			}
			f.remove(t, "SymPkg")
			if _, err := os.Stat(createdUnderSym); !os.IsNotExist(err) {
				t.Errorf("Stat(%q) after removing SymPkg = %v, want removed", createdUnderSym, err)
			}
			if lfi, err := os.Lstat(symParent); err != nil || lfi.Mode()&os.ModeSymlink == 0 {
				t.Errorf("Lstat(%q) after removing SymPkg = %v, %v; want symlink preserved", symParent, lfi, err)
			}
		}
	})

	t.Run("carry_forward_across_upgrade_dropped_payload_dir_from_disk_recovery_and_reinstall", func(t *testing.T) {
		// 1. Upgrade via FromRepo + dropped non-empty payload directory.
		f := newRelFixture(t, nil, nil)
		upOuter := filepath.Join(f.dst, "up_vendor")
		upInner := filepath.Join(upOuter, "up_suite")
		droppedPayloadDir := filepath.Join(f.dst, "dropped_payload_v1")

		rsV1 := buildRepoArchiveWithFiles(t, f.gooDir, goolib.PkgSpec{
			Name: "A", Arch: "noarch", Version: "1",
			Files: map[string]string{
				"v1.txt": filepath.Join(upInner, "v1", "v1.txt"),
				"pdir":   droppedPayloadDir,
			},
		}, map[string]string{
			"v1.txt":     "v1",
			"pdir/p.txt": "payload-v1",
		})
		publishRepoSpec(f, rsV1)
		if err := f.install(t, "A"); err != nil {
			t.Fatalf("install(A v1): %v", err)
		}

		// Place a temporary untracked file inside droppedPayloadDir so it
		// survives v1->v2 upgrade cleanup and carries forward into v2.
		blocker := filepath.Join(droppedPayloadDir, "blocker.tmp")
		if err := os.WriteFile(blocker, []byte("keep-for-now"), 0644); err != nil {
			t.Fatal(err)
		}

		rsV2 := buildRepoArchiveWithFiles(t, f.gooDir, goolib.PkgSpec{
			Name: "A", Arch: "noarch", Version: "2",
			Files: map[string]string{
				"v2.txt": filepath.Join(upInner, "v2", "v2.txt"),
			},
		}, map[string]string{"v2.txt": "v2"})
		publishRepoSpec(f, rsV2)
		if err := f.install(t, "A"); err != nil {
			t.Fatalf("upgrade install(A v2): %v", err)
		}

		v2Files := f.rows(t)["A"].InstalledFiles
		for _, carried := range []string{upOuter, upInner, droppedPayloadDir} {
			if sum, ok := v2Files[carried]; !ok || sum != "" {
				t.Errorf("A v2 InstalledFiles[%q] = (%q, %t), want carried forward as \"\"", carried, sum, ok)
			}
		}
		if _, err := os.Stat(filepath.Join(upInner, "v1")); !os.IsNotExist(err) {
			t.Errorf("v1 empty dir %q still exists after upgrade: %v", filepath.Join(upInner, "v1"), err)
		}

		// Once blocker is removed, uninstalling A v2 removes both the carried
		// created parents and the carried dropped payload directory.
		if err := os.Remove(blocker); err != nil {
			t.Fatal(err)
		}
		f.removeAllPkg(t, "A")
		for _, gone := range []string{upOuter, upInner, droppedPayloadDir} {
			if _, err := os.Stat(gone); !os.IsNotExist(err) {
				t.Errorf("Stat(%q) after removing A v2 = %v, want removed", gone, err)
			}
		}

		// 2. FromDisk carry-forward + Reinstall carry-forward.
		fdOuter := filepath.Join(f.dst, "fd_vendor")
		fdInner := filepath.Join(fdOuter, "fd_app")
		rsFD1 := buildRepoArchiveWithFiles(t, f.gooDir, goolib.PkgSpec{
			Name: "FD", Arch: "noarch", Version: "1",
			Files: map[string]string{"f1.txt": filepath.Join(fdInner, "f1.txt")},
		}, map[string]string{"f1.txt": "fd1"})
		rsFD2 := buildRepoArchiveWithFiles(t, f.gooDir, goolib.PkgSpec{
			Name: "FD", Arch: "noarch", Version: "2",
			Files: map[string]string{"f1.txt": filepath.Join(fdInner, "f1.txt")},
		}, map[string]string{"f1.txt": "fd2"})

		ins := New(f.cache, f.db, Options{})
		if err := ins.FromDisk(filepath.Join(f.gooDir, rsFD1.Source), false); err != nil {
			t.Fatalf("FromDisk(FD v1): %v", err)
		}
		if err := ins.FromDisk(filepath.Join(f.gooDir, rsFD2.Source), true); err != nil {
			t.Fatalf("FromDisk(FD v2): %v", err)
		}
		if err := Reinstall(t.Context(), f.rows(t)["FD"], false, false, f.downloader, f.db); err != nil {
			t.Fatalf("Reinstall(FD v2): %v", err)
		}
		fdFiles := f.rows(t)["FD"].InstalledFiles
		for _, carried := range []string{fdOuter, fdInner} {
			if sum, ok := fdFiles[carried]; !ok || sum != "" {
				t.Errorf("FD InstalledFiles[%q] after FromDisk+Reinstall = (%q, %t), want (\"\", true)", carried, sum, ok)
			}
		}
		f.removeAllPkg(t, "FD")
		for _, gone := range []string{fdInner, fdOuter} {
			if _, err := os.Stat(gone); !os.IsNotExist(err) {
				t.Errorf("Stat(%q) after removing FD = %v, want removed", gone, err)
			}
		}

		// 3. Replacement recovery carry-forward from captured victim row.
		recOuter := filepath.Join(f.dst, "rec_vendor")
		recInner := filepath.Join(recOuter, "rec_app")
		rsV := buildRepoArchiveWithFiles(t, f.gooDir, goolib.PkgSpec{
			Name: "V", Arch: "noarch", Version: "1",
			Files: map[string]string{"v.txt": filepath.Join(recInner, "v.txt")},
		}, map[string]string{"v.txt": "from V"})
		rsQ := buildRepoArchiveWithFiles(t, f.gooDir, goolib.PkgSpec{
			Name: "Q", Arch: "noarch", Version: "1",
			Files: map[string]string{"q.txt": filepath.Join(recInner, "q.txt")},
		}, map[string]string{"q.txt": "from Q"})
		rsR := buildRepoArchiveWithFiles(t, f.gooDir, goolib.PkgSpec{
			Name: "R", Arch: "noarch", Version: "1",
			Replaces: []string{"V"},
			Install:  failingScript,
			Files:    map[string]string{"r.txt": filepath.Join(f.dst, "r.txt")},
		}, map[string]string{"r.txt": "from R"})
		publishRepoSpec(f, rsV)
		publishRepoSpec(f, rsQ)
		publishRepoSpec(f, rsR)

		if err := f.install(t, "V"); err != nil {
			t.Fatalf("install(V): %v", err)
		}
		if err := f.install(t, "Q"); err != nil {
			t.Fatalf("install(Q): %v", err)
		}
		// R removes V (leaving recOuter/recInner non-empty due to Q), then
		// fails in R's install script and recovers V.
		if err := f.install(t, "R"); err == nil {
			t.Fatal("install(R) = nil, want failingScript error")
		}
		recoveredV := f.rows(t)["V"].InstalledFiles
		for _, carried := range []string{recOuter, recInner} {
			if sum, ok := recoveredV[carried]; !ok || sum != "" {
				t.Errorf("recovered V InstalledFiles[%q] = (%q, %t), want carried from captured row", carried, sum, ok)
			}
		}
		f.removeAllPkg(t, "Q")
		f.removeAllPkg(t, "V")
		for _, gone := range []string{recInner, recOuter} {
			if _, err := os.Stat(gone); !os.IsNotExist(err) {
				t.Errorf("Stat(%q) after removing Q and recovered V = %v, want removed", gone, err)
			}
		}

		// 4. Upgrade carries forward an empty dropped directory that is co-owned
		// by another installed row while deleting empty unowned dropped
		// directories and legacy empty-checksum regular files.
		coEmptyDir := filepath.Join(f.dst, "co_owned_empty_upgrade_dir")
		unownedEmptyDir := filepath.Join(f.dst, "unowned_empty_upgrade_dir")
		legacyEmptyFile := filepath.Join(f.dst, "legacy_empty_upgrade.dat")

		rsCoB := buildRepoArchiveWithFiles(t, f.gooDir, goolib.PkgSpec{
			Name: "CoB", Arch: "noarch", Version: "1",
			Files: map[string]string{"b.txt": filepath.Join(f.dst, "cob_keep.txt")},
		}, map[string]string{"b.txt": "cob"})
		rsCoA1 := buildRepoArchiveWithFiles(t, f.gooDir, goolib.PkgSpec{
			Name: "CoA", Arch: "noarch", Version: "1",
			Files: map[string]string{"a1.txt": filepath.Join(unownedEmptyDir, "a1.txt")},
		}, map[string]string{"a1.txt": "coa1"})
		publishRepoSpec(f, rsCoB)
		publishRepoSpec(f, rsCoA1)
		if err := f.install(t, "CoB"); err != nil {
			t.Fatalf("install(CoB): %v", err)
		}
		if err := f.install(t, "CoA"); err != nil {
			t.Fatalf("install(CoA v1): %v", err)
		}
		if err := os.MkdirAll(coEmptyDir, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(legacyEmptyFile, []byte("legacy-file"), 0644); err != nil {
			t.Fatal(err)
		}

		rowCoB := f.rows(t)["CoB"]
		rowCoB.InstalledFiles[coEmptyDir] = ""
		if err := f.db.AddPkg(rowCoB); err != nil {
			t.Fatalf("AddPkg(CoB): %v", err)
		}
		rowCoA1 := f.rows(t)["CoA"]
		rowCoA1.InstalledFiles[coEmptyDir] = ""
		rowCoA1.InstalledFiles[legacyEmptyFile] = ""
		if err := f.db.AddPkg(rowCoA1); err != nil {
			t.Fatalf("AddPkg(CoA v1): %v", err)
		}

		rsCoA2 := buildRepoArchiveWithFiles(t, f.gooDir, goolib.PkgSpec{
			Name: "CoA", Arch: "noarch", Version: "2",
			Files: map[string]string{"a2.txt": filepath.Join(f.dst, "coa2.txt")},
		}, map[string]string{"a2.txt": "coa2"})
		publishRepoSpec(f, rsCoA2)

		upgradeLogs := testutil.CaptureLogs(t)
		if err := f.install(t, "CoA"); err != nil {
			t.Fatalf("upgrade install(CoA v2): %v", err)
		}

		wantKeptUpgrade := fmt.Sprintf("file_kept op=upgrade pkg=CoA.noarch.1 path=%q owners=CoB.noarch", coEmptyDir)
		if !strings.Contains(upgradeLogs.String(), wantKeptUpgrade) {
			t.Errorf("upgrade logs missing %q; got:\n%s", wantKeptUpgrade, upgradeLogs.String())
		}
		if _, err := os.Stat(coEmptyDir); err != nil {
			t.Errorf("Stat(coEmptyDir %q) after CoA upgrade = %v, want kept on disk", coEmptyDir, err)
		}
		for _, gone := range []string{unownedEmptyDir, legacyEmptyFile} {
			if _, err := os.Stat(gone); !os.IsNotExist(err) {
				t.Errorf("Stat(%q) after CoA upgrade = %v, want deleted", gone, err)
			}
		}
		coA2Files := f.rows(t)["CoA"].InstalledFiles
		if sum, ok := coA2Files[coEmptyDir]; !ok || sum != "" {
			t.Errorf("CoA v2 InstalledFiles[%q] = (%q, %t), want carried forward as \"\"", coEmptyDir, sum, ok)
		}
		for _, omitted := range []string{unownedEmptyDir, legacyEmptyFile} {
			if _, ok := coA2Files[omitted]; ok {
				t.Errorf("CoA v2 InstalledFiles unexpectedly carried %q: %v", omitted, coA2Files)
			}
		}

		// Removing CoB first keeps coEmptyDir because CoA v2 carried it forward;
		// removing CoA v2 afterwards deletes coEmptyDir.
		f.removeAllPkg(t, "CoB")
		if _, err := os.Stat(coEmptyDir); err != nil {
			t.Errorf("Stat(%q) after removing CoB = %v, want kept while CoA v2 owns it", coEmptyDir, err)
		}
		f.removeAllPkg(t, "CoA")
		if _, err := os.Stat(coEmptyDir); !os.IsNotExist(err) {
			t.Errorf("Stat(%q) after removing last owner CoA v2 = %v, want removed", coEmptyDir, err)
		}
	})
}

func (f *relFixture) removeAllPkg(t *testing.T, name string) {
	t.Helper()
	st, ok := f.rows(t)[name]
	if !ok {
		t.Fatalf("package %q is not installed", name)
	}
	pi := goolib.PackageInfo{Name: st.PackageSpec.Name, Arch: st.PackageSpec.Arch, Ver: st.PackageSpec.Version}
	if err := remove.All(t.Context(), pi, remove.DepMap{}, false, f.downloader, f.db); err != nil {
		t.Fatalf("remove(%s): %v", pi, err)
	}
}

// TestPartialMkdirAllFailureRollsBackCreatedAncestors verifies that when
// directory creation fails part-way through a multi-level missing ancestor
// chain, every outer directory that MkdirAll created before failing is recorded
// for undo and removed during rollback.
func TestPartialMkdirAllFailureRollsBackCreatedAncestors(t *testing.T) {
	t.Run("deepest_of_three_missing_levels_component_too_long", func(t *testing.T) {
		settings.Initialize(t.TempDir(), false)
		db, err := googetdb.NewDB(settings.DBFile())
		if err != nil {
			t.Fatalf("googetdb.NewDB: %v", err)
		}
		t.Cleanup(func() { _ = db.Close() })

		dstRoot := t.TempDir()
		goo := filepath.Join(t.TempDir(), "partial.goo")
		writeGoo(t, goo, map[string]string{
			"lone.txt":    "payload",
			"pkgdir/item": "dir-payload",
		})

		tooLongComponent := strings.Repeat("x", 300)
		for _, tc := range []struct {
			name  string
			files map[string]string
		}{
			{
				name: "ensure_parents_three_levels",
				files: map[string]string{
					"lone.txt": filepath.Join(dstRoot, "lvl1", "lvl2", tooLongComponent, "lone.txt"),
				},
			},
			{
				name: "ensure_package_dir_three_levels",
				files: map[string]string{
					"pkgdir": filepath.Join(dstRoot, "dir1", "dir2", tooLongComponent),
				},
			},
		} {
			t.Run(tc.name, func(t *testing.T) {
				ps := goolib.PkgSpec{
					Name:    "partial",
					Arch:    "noarch",
					Version: "1.0.0",
					Files:   tc.files,
				}
				if _, err := installPkg(goo, &ps, Options{}, goolib.InstallerContext{}, db, nil); err == nil {
					t.Fatal("installPkg() = nil, want error at deepest directory level")
				}
				if diff := cmp.Diff(map[string]string{}, tree(t, dstRoot)); diff != "" {
					t.Errorf("dstRoot after rollback (-want +got):\n%s", diff)
				}
			})
		}
	})

	t.Run("total_path_length_overflow_rolls_back_all_created_outer_levels", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("POSIX PATH_MAX overflow check runs on Unix hosts.")
		}
		settings.Initialize(t.TempDir(), false)
		db, err := googetdb.NewDB(settings.DBFile())
		if err != nil {
			t.Fatalf("googetdb.NewDB: %v", err)
		}
		t.Cleanup(func() { _ = db.Close() })

		dstRoot := t.TempDir()
		goo := filepath.Join(t.TempDir(), "overflow.goo")
		writeGoo(t, goo, map[string]string{
			"lone.txt":    "payload",
			"pkgdir/item": "dir-payload",
		})

		parts := []string{dstRoot, "outer1", "outer2"}
		for range 22 {
			parts = append(parts, "seg_"+strings.Repeat("a", 200))
		}
		deepTarget := filepath.Join(parts...)

		for _, tc := range []struct {
			name  string
			files map[string]string
		}{
			{
				name:  "lone_file_parents",
				files: map[string]string{"lone.txt": filepath.Join(deepTarget, "lone.txt")},
			},
			{
				name:  "package_dir_parents",
				files: map[string]string{"pkgdir": deepTarget},
			},
		} {
			t.Run(tc.name, func(t *testing.T) {
				ps := goolib.PkgSpec{
					Name:    "overflow",
					Arch:    "noarch",
					Version: "1.0.0",
					Files:   tc.files,
				}
				if _, err := installPkg(goo, &ps, Options{}, goolib.InstallerContext{}, db, nil); err == nil {
					t.Fatal("installPkg() = nil, want ENAMETOOLONG error")
				}
				entries, readErr := os.ReadDir(dstRoot)
				if readErr != nil {
					t.Fatalf("ReadDir(%q): %v", dstRoot, readErr)
				}
				if len(entries) != 0 {
					t.Errorf("dstRoot entries after rollback = %d (%q...), want 0 leaked directories", len(entries), entries[0].Name())
				}
			})
		}
	})
}

// TestInstallScriptRemovingCreatedParentLeavesItUnrecordedAndVerifiable
// verifies that when a created parent directory is removed or replaced with a
// symbolic link before commit, recordDirs omits it from InstalledFiles and
// verify.Files succeeds.
func TestInstallScriptRemovingCreatedParentLeavesItUnrecordedAndVerifiable(t *testing.T) {
	t.Run("deleted_created_directory_is_not_recorded_and_verify_passes", func(t *testing.T) {
		root := t.TempDir()
		survivingParent := filepath.Join(root, "surviving")
		survivingFile := filepath.Join(survivingParent, "keep.txt")
		deletedParent := filepath.Join(root, "transient", "nested")

		var u undo
		if err := u.ensureParents(survivingParent); err != nil {
			t.Fatalf("ensureParents(survivingParent): %v", err)
		}
		if err := u.ensureParents(deletedParent); err != nil {
			t.Fatalf("ensureParents(deletedParent): %v", err)
		}
		if err := os.WriteFile(survivingFile, []byte("ok"), 0644); err != nil {
			t.Fatal(err)
		}
		// Simulate an install script removing the transient created parent
		// tree before commit.
		if err := os.RemoveAll(filepath.Join(root, "transient")); err != nil {
			t.Fatal(err)
		}

		insFiles := map[string]string{
			survivingFile: checksumOf(t, survivingFile),
		}
		u.recordDirs(insFiles)
		u.commit()

		want := map[string]string{
			survivingParent: "",
			survivingFile:   checksumOf(t, survivingFile),
		}
		if diff := cmp.Diff(want, insFiles); diff != "" {
			t.Errorf("insFiles after recordDirs (-want +got):\n%s", diff)
		}

		ps := client.PackageState{
			PackageSpec:    &goolib.PkgSpec{Name: "ScriptPkg", Arch: "noarch", Version: "1.0.0"},
			InstalledFiles: insFiles,
		}
		ok, err := verify.Files(ps)
		if err != nil || !ok {
			t.Errorf("verify.Files() = (%t, %v), want (true, nil)", ok, err)
		}

		// Verify recordDirs handles nil maps, skips file changes, and
		// deduplicates equivalent path keys via PathSet.Add.
		u.recordDirs(nil)
		dupDir := filepath.Join(root, "dup_dir")
		alreadyListedDir := filepath.Join(root, "already_listed")
		for _, d := range []string{dupDir, alreadyListedDir} {
			if err := os.MkdirAll(d, 0755); err != nil {
				t.Fatal(err)
			}
		}
		regularFile := filepath.Join(root, "not_a_dir.txt")
		if err := os.WriteFile(regularFile, []byte("f"), 0644); err != nil {
			t.Fatal(err)
		}
		u2 := undo{
			changes: []change{
				{path: regularFile, dir: false},
				{path: dupDir, dir: true},
				{path: dupDir + string(filepath.Separator) + ".", dir: true},
				{path: alreadyListedDir, dir: true},
			},
		}
		uncleanedAlready := alreadyListedDir + string(filepath.Separator) + "."
		gotDedup := map[string]string{uncleanedAlready: ""}
		u2.recordDirs(gotDedup)
		wantDedup := map[string]string{
			uncleanedAlready: "",
			dupDir:           "",
		}
		if diff := cmp.Diff(wantDedup, gotDedup); diff != "" {
			t.Errorf("recordDirs deduplication mismatch (-want +got):\n%s", diff)
		}
	})

	t.Run("install_script_relocates_created_parent_to_symlink_before_commit", func(t *testing.T) {
		if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
			t.Skip("POSIX shell install script runs on Linux and macOS.")
		}
		settings.Initialize(t.TempDir(), false)
		db, err := googetdb.NewDB(settings.DBFile())
		if err != nil {
			t.Fatalf("googetdb.NewDB: %v", err)
		}
		t.Cleanup(func() { _ = db.Close() })

		dstRoot := t.TempDir()
		realDir := filepath.Join(dstRoot, "real_store")
		if err := os.MkdirAll(realDir, 0755); err != nil {
			t.Fatal(err)
		}
		createdOuter := filepath.Join(dstRoot, "transient_outer")
		createdInner := filepath.Join(createdOuter, "transient_inner")
		installedTool := filepath.Join(createdInner, "tool.bin")

		script := fmt.Sprintf("#!/bin/sh\nset -e\nmv %q %q\nrmdir %q %q\nln -s %q %q\nln -s %q %q\n",
			installedTool, filepath.Join(realDir, "tool.bin"),
			createdInner, createdOuter,
			realDir, createdOuter,
			realDir, filepath.Join(realDir, "transient_inner"),
		)
		spec := goolib.PkgSpec{
			Name:    "RelocatePkg",
			Arch:    "noarch",
			Version: "1.0.0",
			Install: goolib.ExecFile{Path: "relocate.sh"},
			Files:   map[string]string{"tool.bin": installedTool},
		}

		gooPath := filepath.Join(t.TempDir(), "RelocatePkg.noarch.1.0.0.goo")
		writeGooArchiveWithEntries(t, gooPath, spec, []archiveEntry{
			{name: "relocate.sh", mode: 0755, body: script},
			{name: "tool.bin", mode: 0755, body: "binary-payload"},
		})

		ins := New(t.TempDir(), db, Options{})
		if err := ins.FromDisk(gooPath, false); err != nil {
			t.Fatalf("FromDisk(RelocatePkg): %v", err)
		}

		got, err := db.FetchPkg(goolib.PackageInfo{Name: "RelocatePkg", Arch: "noarch"})
		if err != nil {
			t.Fatalf("FetchPkg(RelocatePkg): %v", err)
		}
		for _, removedDir := range []string{createdOuter, createdInner} {
			if _, ok := got.InstalledFiles[removedDir]; ok {
				t.Errorf("InstalledFiles recorded %q even though install script removed the directory: %v", removedDir, got.InstalledFiles)
			}
		}
		ok, err := verify.Files(got)
		if err != nil || !ok {
			t.Errorf("verify.Files(RelocatePkg) = (%t, %v), want (true, nil)", ok, err)
		}
	})
}

// TestBackwardCompatibilityFrozen07fd257UninstallLoopWithRecordedParents
// verifies that running GooGet 3.4.2 (commit 07fd257)'s reverse-lexical
// directory removal loop over a package row with recorded parent directories
// deletes only empty directories and leaves parents shared with other files
// intact.
func TestBackwardCompatibilityFrozen07fd257UninstallLoopWithRecordedParents(t *testing.T) {
	f := newRelFixture(t, nil, nil)
	sharedVendor := filepath.Join(f.dst, "vendor")
	sharedSuite := filepath.Join(sharedVendor, "suite")
	appADir := filepath.Join(sharedSuite, "app_a")
	appASub := filepath.Join(appADir, "bin")
	fileA := filepath.Join(appASub, "a.exe")

	rsA := buildRepoArchiveWithFiles(t, f.gooDir, goolib.PkgSpec{
		Name: "A", Arch: "noarch", Version: "1",
		Files: map[string]string{"a.exe": fileA},
	}, map[string]string{"a.exe": "payload A"})
	publishRepoSpec(f, rsA)
	if err := f.install(t, "A"); err != nil {
		t.Fatalf("install(A): %v", err)
	}

	// Place another package's file under sharedSuite so sharedSuite and
	// sharedVendor remain non-empty while appADir and appASub become empty.
	otherFile := filepath.Join(sharedSuite, "other_pkg.dll")
	if err := os.WriteFile(otherFile, []byte("from other pkg"), 0644); err != nil {
		t.Fatal(err)
	}

	rowA := f.rows(t)["A"]
	frozen07fd257UninstallFilesAndDirs(rowA.InstalledFiles)

	for _, emptyRemoved := range []string{fileA, appASub, appADir} {
		if _, err := os.Stat(emptyRemoved); !os.IsNotExist(err) {
			t.Errorf("Stat(%q) after frozen 07fd257 uninstall = %v, want removed", emptyRemoved, err)
		}
	}
	for _, preserved := range []string{otherFile, sharedSuite, sharedVendor, f.dst} {
		if _, err := os.Stat(preserved); err != nil {
			t.Errorf("Stat(%q) after frozen 07fd257 uninstall = %v, want preserved", preserved, err)
		}
	}
}
