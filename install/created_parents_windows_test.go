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

package install

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/googet/v2/googetdb"
	"github.com/google/googet/v2/goolib"
	"github.com/google/googet/v2/oswrap"
	"github.com/google/googet/v2/remove"
	"github.com/google/googet/v2/settings"
	"github.com/google/googet/v2/verify"
)

// TestWindowsCreatedParentsCaseVariantCarryAndOwnership verifies on Windows
// NTFS that a carried parent directory whose casing differs from a newly
// created child directory is removed deepest-first without leaking the parent,
// and that a created parent owned by another row through a case variant is
// preserved on removal.
func TestWindowsCreatedParentsCaseVariantCarryAndOwnership(t *testing.T) {
	t.Run("carried_parent_with_different_case_from_new_child_removes_cleanly", func(t *testing.T) {
		f := newRelFixture(t, nil, nil)

		// Version 1 creates Uppercase "Vendor"; version 2 installs under
		// lowercase "vendor\app" so carry-forward holds "Vendor" alongside
		// "vendor\app". Depth-first deletion must remove "vendor\app" before
		// "Vendor" even though 'v' > 'V' in byte order.
		v1Parent := filepath.Join(f.dst, "Vendor")
		v1File := filepath.Join(v1Parent, "v1.dll")
		v2Child := filepath.Join(f.dst, "vendor", "app")
		v2File := filepath.Join(v2Child, "v2.dll")

		rsV1 := buildRepoArchiveWithFiles(t, f.gooDir, goolib.PkgSpec{
			Name: "A", Arch: "noarch", Version: "1",
			Files: map[string]string{"v1.dll": v1File},
		}, map[string]string{"v1.dll": "from v1"})
		publishRepoSpec(f, rsV1)
		if err := f.install(t, "A"); err != nil {
			t.Fatalf("install(A v1): %v", err)
		}

		rsV2 := buildRepoArchiveWithFiles(t, f.gooDir, goolib.PkgSpec{
			Name: "A", Arch: "noarch", Version: "2",
			Files: map[string]string{"v2.dll": v2File},
		}, map[string]string{"v2.dll": "from v2"})
		publishRepoSpec(f, rsV2)
		if err := f.install(t, "A"); err != nil {
			t.Fatalf("upgrade install(A v2): %v", err)
		}

		pi := goolib.PackageInfo{Name: "A", Arch: "noarch", Ver: "2"}
		if err := remove.All(t.Context(), pi, remove.DepMap{}, false, f.downloader, f.db); err != nil {
			t.Fatalf("remove(A v2): %v", err)
		}
		if _, err := oswrap.Stat(v1Parent); !os.IsNotExist(err) {
			t.Errorf("Stat(%q) after removing A v2 = %v, want whole created chain removed", v1Parent, err)
		}
	})

	t.Run("parent_owned_through_case_variant_is_kept", func(t *testing.T) {
		f := newRelFixture(t, nil, nil)
		upperParent := filepath.Join(f.dst, "SharedParent")
		lowerParent := filepath.Join(f.dst, "sharedparent")

		rsA := buildRepoArchiveWithFiles(t, f.gooDir, goolib.PkgSpec{
			Name: "A", Arch: "noarch", Version: "1",
			Files: map[string]string{"a.dll": filepath.Join(upperParent, "sub_a", "a.dll")},
		}, map[string]string{"a.dll": "from A"})
		rsB := buildRepoArchiveWithFiles(t, f.gooDir, goolib.PkgSpec{
			Name: "B", Arch: "noarch", Version: "1",
			Files: map[string]string{"bdir": lowerParent},
		}, map[string]string{"bdir/b.dll": "from B"})
		publishRepoSpec(f, rsA)
		publishRepoSpec(f, rsB)

		if err := f.install(t, "A"); err != nil {
			t.Fatalf("install(A): %v", err)
		}
		if err := f.install(t, "B"); err != nil {
			t.Fatalf("install(B): %v", err)
		}
		if err := oswrap.Remove(filepath.Join(lowerParent, "b.dll")); err != nil {
			t.Fatal(err)
		}

		f.remove(t, "A")
		if _, err := oswrap.Stat(upperParent); err != nil {
			t.Errorf("Stat(%q) after removing A = %v, want kept while B owns case variant", upperParent, err)
		}
	})
}

// TestWindowsCreatedParentsLongPathAndPartialFailureRollback verifies on
// Windows NTFS that created ancestors exceeding MAX_PATH (260 characters)
// succeed through oswrap's extended-length path handling, record and remove
// cleanly, and roll back completely when creation fails part-way.
func TestWindowsCreatedParentsLongPathAndPartialFailureRollback(t *testing.T) {
	t.Run("long_path_ancestors_created_recorded_verified_and_removed", func(t *testing.T) {
		f := newRelFixture(t, nil, nil)
		seg := strings.Repeat("long_segment_", 6) // 78 chars per segment.
		longParent := filepath.Join(f.dst, seg+"1", seg+"2", seg+"3", seg+"4")
		targetFile := filepath.Join(longParent, "deep.dll")
		if len(targetFile) <= 260 {
			t.Fatalf("len(targetFile) = %d, want > 260 (MAX_PATH)", len(targetFile))
		}

		rs := buildRepoArchiveWithFiles(t, f.gooDir, goolib.PkgSpec{
			Name: "LongPkg", Arch: "noarch", Version: "1",
			Files: map[string]string{"deep.dll": targetFile},
		}, map[string]string{"deep.dll": "extended-length-payload"})
		publishRepoSpec(f, rs)

		if err := f.install(t, "LongPkg"); err != nil {
			t.Fatalf("install(LongPkg): %v", err)
		}
		row := f.rows(t)["LongPkg"]
		if _, ok := row.InstalledFiles[longParent]; !ok {
			t.Errorf("LongPkg InstalledFiles missing long-path parent %q", longParent)
		}
		ok, err := verify.Files(row)
		if err != nil || !ok {
			t.Errorf("verify.Files(LongPkg) = (%t, %v), want (true, nil)", ok, err)
		}

		f.remove(t, "LongPkg")
		outer := filepath.Join(f.dst, seg+"1")
		if _, err := oswrap.Stat(outer); !os.IsNotExist(err) {
			t.Errorf("Stat(outer long-path ancestor %q) after remove = %v, want removed", outer, err)
		}
	})

	t.Run("long_path_partial_failure_rolls_back_all_created_ancestors", func(t *testing.T) {
		settings.Initialize(t.TempDir(), false)
		db, err := googetdb.NewDB(settings.DBFile())
		if err != nil {
			t.Fatalf("googetdb.NewDB: %v", err)
		}
		t.Cleanup(func() { _ = db.Close() })

		dstRoot := t.TempDir()
		goo := filepath.Join(t.TempDir(), "long_fail.goo")
		writeGoo(t, goo, map[string]string{"item.dll": "payload"})

		seg := strings.Repeat("deep_anc_", 10) // 90 chars per segment.
		overMaxComp := strings.Repeat("z", 300)
		for _, leaf := range []string{overMaxComp, "invalid|ntfs|leaf"} {
			target := filepath.Join(dstRoot, seg+"a", seg+"b", seg+"c", leaf, "item.dll")
			ps := goolib.PkgSpec{
				Name:    "FailLong",
				Arch:    "noarch",
				Version: "1.0.0",
				Files:   map[string]string{"item.dll": target},
			}
			if _, err := installPkg(goo, &ps, Options{}, goolib.InstallerContext{}, db, nil); err == nil {
				t.Fatalf("installPkg(leaf=%q) = nil, want directory creation error", leaf)
			}
			entries, err := os.ReadDir(dstRoot)
			if err != nil {
				t.Fatalf("ReadDir(%q): %v", dstRoot, err)
			}
			if len(entries) != 0 {
				t.Errorf("dstRoot entries after rollback (leaf=%q) = %d, want 0", leaf, len(entries))
			}
		}
	})
}

// TestWindowsDriveRootAndTopLevelDirDoNotPanic verifies on Windows that
// ensureParents and ensurePackageDir handle mounted and unmounted volume roots
// and top-level directory parents (where filepath.Dir(dir) is a drive root
// such as `C:\`) without panicking in oswrap.rootDir.
func TestWindowsDriveRootAndTopLevelDirDoNotPanic(t *testing.T) {
	volRoot := filepath.VolumeName(t.TempDir()) + `\`
	var u undo
	if err := u.ensureParents(volRoot); err != nil {
		t.Fatalf("ensureParents(%q): %v", volRoot, err)
	}
	if err := u.ensurePackageDir(volRoot, 0755); err != nil {
		t.Fatalf("ensurePackageDir(%q): %v", volRoot, err)
	}
	// Exercise an unmounted drive root if one exists: must return an error
	// cleanly rather than panicking out of range in oswrap.rootDir.
	for _, letter := range "ZYXWVUTSRQ" {
		probeRoot := string(letter) + `:\`
		if _, err := oswrap.Stat(probeRoot); err != nil {
			if err := u.ensureParents(probeRoot); err == nil {
				t.Errorf("ensureParents(unmounted %q) = nil, want error", probeRoot)
			}
			if err := u.ensurePackageDir(probeRoot, 0755); err == nil {
				t.Errorf("ensurePackageDir(unmounted %q) = nil, want error", probeRoot)
			}
			if err := u.ensurePackageDir(filepath.Join(probeRoot, "GooGetTopLevelProbe"), 0755); err == nil {
				t.Errorf("ensurePackageDir(under unmounted %q) = nil, want error", probeRoot)
			}
			break
		}
	}
	if len(u.changes) != 0 {
		t.Errorf("undo changes after drive root probes = %v, want empty", u.changes)
	}
}
