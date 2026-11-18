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
	"runtime"
	"testing"

	"github.com/google/googet/v2/googetdb"
	"github.com/google/googet/v2/goolib"
	"github.com/google/googet/v2/oswrap"
	"github.com/google/googet/v2/settings"
)

func expectedEffectivePerm(t *testing.T, requested os.FileMode) os.FileMode {
	t.Helper()
	probe := filepath.Join(t.TempDir(), "perm_probe")
	if err := os.Mkdir(probe, requested); err != nil {
		t.Fatalf("Mkdir(%q, %04o): %v", probe, requested, err)
	}
	fi, err := os.Stat(probe)
	if err != nil {
		t.Fatalf("Stat(%q): %v", probe, err)
	}
	return fi.Mode().Perm()
}

// TestCreatedParentDirectoriesUse0755WhilePackageDirKeepsArchiveMode verifies
// that non-root installs of a lone non-executable file into nested missing
// directories succeed because missing ancestors are created with mode 0755
// (&^ umask), and that installing a package directory with mode 0700 under
// missing ancestors applies 0755 (&^ umask) to the missing ancestors while
// preserving 0700 (&^ umask) on the package's own directory.
func TestCreatedParentDirectoriesUse0755WhilePackageDirKeepsArchiveMode(t *testing.T) {
	t.Run("lone_file_into_nested_missing_ancestors_succeeds_for_non_root", func(t *testing.T) {
		settings.Initialize(t.TempDir(), false)
		db, err := googetdb.NewDB(settings.DBFile())
		if err != nil {
			t.Fatalf("googetdb.NewDB: %v", err)
		}
		t.Cleanup(func() { _ = db.Close() })

		dstRoot := t.TempDir()
		targetFile := filepath.Join(dstRoot, "a", "b", "file.cfg")
		spec := goolib.PkgSpec{
			Name:    "LoneFilePkg",
			Arch:    "noarch",
			Version: "1.0.0",
			Files:   map[string]string{"file.cfg": targetFile},
		}

		gooPath := filepath.Join(t.TempDir(), "LoneFilePkg.noarch.1.0.0.goo")
		// Use mode 0600 (no execute bits anywhere) so pre-fix code passing
		// fi.Mode() to MkdirAll fails to traverse a/ when creating a/b/.
		writeGooArchiveWithEntries(t, gooPath, spec, []archiveEntry{
			{name: "file.cfg", mode: 0600, body: "nested-config"},
		})

		ins := New(t.TempDir(), db, Options{})
		if err := ins.FromDisk(gooPath, false); err != nil {
			t.Fatalf("FromDisk(LoneFilePkg) into nested missing parents failed: %v", err)
		}

		gotBytes, err := os.ReadFile(targetFile)
		if err != nil || string(gotBytes) != "nested-config" {
			t.Fatalf("ReadFile(%q) = %q, %v; want %q", targetFile, gotBytes, err, "nested-config")
		}

		if runtime.GOOS != "windows" {
			wantParentPerm := expectedEffectivePerm(t, 0755)
			for _, ancestor := range []string{
				filepath.Join(dstRoot, "a"),
				filepath.Join(dstRoot, "a", "b"),
			} {
				fi, err := os.Stat(ancestor)
				if err != nil {
					t.Fatalf("Stat(%q): %v", ancestor, err)
				}
				if gotPerm := fi.Mode().Perm(); gotPerm != wantParentPerm {
					t.Errorf("ancestor %q perm = %04o, want %04o (0755 &^ umask)", ancestor, gotPerm, wantParentPerm)
				}
			}
		}
	})

	t.Run("package_directory_keeps_0700_archive_mode_under_0755_ancestors", func(t *testing.T) {
		srcRoot := t.TempDir()
		srcPkgDir := filepath.Join(srcRoot, "private_dir")
		srcSubDir := filepath.Join(srcPkgDir, "inner")
		if err := os.MkdirAll(srcSubDir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(srcPkgDir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(srcSubDir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(srcSubDir, "secret.txt"), []byte("top-secret"), 0600); err != nil {
			t.Fatal(err)
		}

		dstRoot := t.TempDir()
		anc1 := filepath.Join(dstRoot, "anc1")
		anc2 := filepath.Join(anc1, "anc2")
		dstPkgDir := filepath.Join(anc2, "private_dir")

		insFiles := make(map[string]string)
		var u undo
		if err := oswrap.Walk(srcPkgDir, makeInstallFunction(srcPkgDir, dstPkgDir, insFiles, Options{}, conflictMap{}, &u)); err != nil {
			t.Fatalf("Walk(makeInstallFunction) failed: %v", err)
		}
		u.commit()

		if got, err := os.ReadFile(filepath.Join(dstPkgDir, "inner", "secret.txt")); err != nil || string(got) != "top-secret" {
			t.Fatalf("installed secret.txt = %q, %v; want %q", got, err, "top-secret")
		}

		if runtime.GOOS != "windows" {
			wantAncestorPerm := expectedEffectivePerm(t, 0755)
			wantPkgPerm := expectedEffectivePerm(t, 0700)

			for _, ancestor := range []string{anc1, anc2} {
				fi, err := os.Stat(ancestor)
				if err != nil {
					t.Fatalf("Stat(%q): %v", ancestor, err)
				}
				if gotPerm := fi.Mode().Perm(); gotPerm != wantAncestorPerm {
					t.Errorf("created ancestor %q perm = %04o, want %04o", ancestor, gotPerm, wantAncestorPerm)
				}
			}
			for _, pkgDir := range []string{dstPkgDir, filepath.Join(dstPkgDir, "inner")} {
				fi, err := os.Stat(pkgDir)
				if err != nil {
					t.Fatalf("Stat(%q): %v", pkgDir, err)
				}
				if gotPerm := fi.Mode().Perm(); gotPerm != wantPkgPerm {
					t.Errorf("package directory %q perm = %04o, want %04o", pkgDir, gotPerm, wantPkgPerm)
				}
			}
		}
	})

	t.Run("filesystem_root_target_short_circuits_without_creating_changes", func(t *testing.T) {
		root := filepath.VolumeName(t.TempDir()) + string(os.PathSeparator)
		var u undo
		if err := u.ensureParents(root); err != nil {
			t.Fatalf("ensureParents(%q): %v", root, err)
		}
		if err := u.ensurePackageDir(root, 0755); err != nil {
			t.Fatalf("ensurePackageDir(%q): %v", root, err)
		}
		if len(u.changes) != 0 {
			t.Errorf("undo changes after root calls = %v, want empty", u.changes)
		}
	})

	t.Run("regular_file_blocking_directory_creation_fails_without_recording_changes", func(t *testing.T) {
		blocker := filepath.Join(t.TempDir(), "blocker.file")
		if err := os.WriteFile(blocker, []byte("keep-file"), 0644); err != nil {
			t.Fatal(err)
		}
		var u undo
		if err := u.ensurePackageDir(blocker, 0755); err == nil {
			t.Errorf("ensurePackageDir(regular file %q) = nil, want error", blocker)
		}
		if err := u.ensureParents(blocker); err == nil {
			t.Errorf("ensureParents(regular file %q) = nil, want error", blocker)
		}
		if err := u.ensureParents(filepath.Join(blocker, "child")); err == nil {
			t.Errorf("ensureParents(under regular file %q) = nil, want error", blocker)
		}
		if err := u.ensurePackageDir(filepath.Join(blocker, "child"), 0755); err == nil {
			t.Errorf("ensurePackageDir(under regular file %q) = nil, want error", blocker)
		}
		if len(u.changes) != 0 {
			t.Errorf("undo changes after blocked mkdir calls = %v, want empty", u.changes)
		}
		if got, err := os.ReadFile(blocker); err != nil || string(got) != "keep-file" {
			t.Errorf("blocker file after failed mkdir calls = %q, %v; want %q", got, err, "keep-file")
		}
	})

	t.Run("symlinked_directory_short_circuits_while_dangling_symlink_fails", func(t *testing.T) {
		base := t.TempDir()
		realDir := filepath.Join(base, "real_dir")
		if err := os.MkdirAll(realDir, 0755); err != nil {
			t.Fatal(err)
		}
		symDir := filepath.Join(base, "sym_dir")
		if err := os.Symlink(realDir, symDir); err != nil {
			t.Skipf("os.Symlink unavailable in test environment: %v", err)
		}
		danglingLink := filepath.Join(base, "dangling_link")
		if err := os.Symlink(filepath.Join(base, "missing_target"), danglingLink); err != nil {
			t.Fatalf("os.Symlink(dangling): %v", err)
		}

		var u undo
		if err := u.ensureParents(symDir); err != nil {
			t.Fatalf("ensureParents(symlinked dir %q): %v", symDir, err)
		}
		if err := u.ensurePackageDir(symDir, 0700); err != nil {
			t.Fatalf("ensurePackageDir(symlinked dir %q): %v", symDir, err)
		}
		if len(u.changes) != 0 {
			t.Errorf("undo changes for existing symlinked dir = %v, want empty", u.changes)
		}
		if lfi, err := oswrap.Lstat(symDir); err != nil || lfi.Mode()&os.ModeSymlink == 0 {
			t.Errorf("Lstat(%q) mode = %v, err = %v; want symbolic link preserved", symDir, lfi, err)
		}

		if err := u.ensureParents(danglingLink); err == nil {
			t.Errorf("ensureParents(dangling symlink %q) = nil, want error", danglingLink)
		}
		if err := u.ensurePackageDir(danglingLink, 0755); err == nil {
			t.Errorf("ensurePackageDir(dangling symlink %q) = nil, want error", danglingLink)
		}
		if len(u.changes) != 0 {
			t.Errorf("undo changes after dangling symlink failures = %v, want empty", u.changes)
		}

		// Installing a lone 0600 file beneath symDir creates the missing child
		// directory with mode 0755 (&^ umask) while preserving symDir as a
		// symbolic link.
		settings.Initialize(t.TempDir(), false)
		db, err := googetdb.NewDB(settings.DBFile())
		if err != nil {
			t.Fatalf("googetdb.NewDB: %v", err)
		}
		t.Cleanup(func() { _ = db.Close() })

		createdChild := filepath.Join(symDir, "nested_child")
		targetFile := filepath.Join(createdChild, "app.cfg")
		spec := goolib.PkgSpec{
			Name:    "SymlinkParentPkg",
			Arch:    "noarch",
			Version: "1.0.0",
			Files:   map[string]string{"app.cfg": targetFile},
		}
		gooPath := filepath.Join(t.TempDir(), "SymlinkParentPkg.noarch.1.0.0.goo")
		writeGooArchiveWithEntries(t, gooPath, spec, []archiveEntry{
			{name: "app.cfg", mode: 0600, body: "under-symlink"},
		})

		ins := New(t.TempDir(), db, Options{})
		if err := ins.FromDisk(gooPath, false); err != nil {
			t.Fatalf("FromDisk(SymlinkParentPkg): %v", err)
		}
		if got, err := os.ReadFile(targetFile); err != nil || string(got) != "under-symlink" {
			t.Fatalf("ReadFile(%q) = %q, %v; want %q", targetFile, got, err, "under-symlink")
		}
		if runtime.GOOS != "windows" {
			wantPerm := expectedEffectivePerm(t, 0755)
			fi, err := os.Stat(createdChild)
			if err != nil {
				t.Fatalf("Stat(%q): %v", createdChild, err)
			}
			if gotPerm := fi.Mode().Perm(); gotPerm != wantPerm {
				t.Errorf("created child under symlink %q perm = %04o, want %04o", createdChild, gotPerm, wantPerm)
			}
		}
	})
}
