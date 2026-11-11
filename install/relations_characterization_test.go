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

// The tests in this file pin how install and remove handle Conflicts,
// Replaces, Provides and installed files today. Each test says whether the
// behaviour it pins is intended, so the test guards against regressions, or
// known to be wrong, in which case the comment says what a fix should assert
// instead. A change that fixes a known-wrong behaviour should update the
// matching test, so that the change in semantics is visible in review.

import (
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/googet/v2/client"
	"github.com/google/googet/v2/googetdb"
	"github.com/google/googet/v2/goolib"
	"github.com/google/googet/v2/priority"
	"github.com/google/googet/v2/remove"
	"github.com/google/googet/v2/settings"
	"github.com/google/googet/v2/testutil"
)

// relFixture is a test repository served over HTTP plus a database holding
// packages that are already installed. Packages passed to newRelFixture each
// install one file, named after the package, into dst; packages installed
// with installArchive install whatever their archive holds.
type relFixture struct {
	db         *googetdb.GooDB
	rm         client.RepoMap
	gooDir     string
	cache      string
	dst        string
	downloader *client.Downloader
}

// newRelFixture installs the installed packages by writing their files and
// database rows directly, and publishes the available packages in a repo.
func newRelFixture(t *testing.T, installed, available []goolib.PkgSpec) *relFixture {
	t.Helper()
	settings.Initialize(t.TempDir(), false)
	db, err := googetdb.NewDB(settings.DBFile())
	if err != nil {
		t.Fatalf("googetdb.NewDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	downloader, err := client.NewDownloader("")
	if err != nil {
		t.Fatalf("client.NewDownloader: %v", err)
	}
	f := &relFixture{db: db, gooDir: t.TempDir(), cache: t.TempDir(), dst: t.TempDir(), downloader: downloader}
	srv := testutil.ServeGoo(t, f.gooDir)
	t.Cleanup(srv.Close)

	var state client.GooGetState
	for _, ps := range installed {
		rs := testutil.GenGoo(t, f.gooDir, f.dst, ps)
		local := filepath.Join(f.cache, rs.Source)
		copyFile(t, filepath.Join(f.gooDir, rs.Source), local)
		file := filepath.Join(f.dst, ps.Name)
		if err := os.WriteFile(file, []byte(rs.PackageSpec.String()), 0644); err != nil {
			t.Fatal(err)
		}
		state = append(state, client.PackageState{
			SourceRepo:     srv.URL,
			DownloadURL:    srv.URL + "/" + rs.Source,
			Checksum:       rs.Checksum,
			LocalPath:      local,
			PackageSpec:    rs.PackageSpec,
			InstalledFiles: map[string]string{file: checksumOf(t, file)},
		})
	}
	if err := db.WriteStateToDB(state); err != nil {
		t.Fatalf("WriteStateToDB: %v", err)
	}

	var specs []goolib.RepoSpec
	for _, ps := range available {
		specs = append(specs, testutil.GenGoo(t, f.gooDir, f.dst, ps))
	}
	f.rm = client.RepoMap{srv.URL: client.Repo{Priority: priority.Default, Packages: specs}}
	return f
}

// archive returns the path of the served archive for the available package
// with the given name.
func (f *relFixture) archive(t *testing.T, name string) string {
	t.Helper()
	for _, r := range f.rm {
		for _, rs := range r.Packages {
			if rs.PackageSpec.Name == name {
				return filepath.Join(f.gooDir, rs.Source)
			}
		}
	}
	t.Fatalf("package %q is not available", name)
	return ""
}

// unpublish stops serving the archive of an available package, so the repo
// index still lists it but downloading it fails.
func (f *relFixture) unpublish(t *testing.T, name string) {
	t.Helper()
	if err := os.Remove(f.archive(t, name)); err != nil {
		t.Fatal(err)
	}
}

// corrupt replaces the archive of an available package, so downloading it
// fails the checksum check.
func (f *relFixture) corrupt(t *testing.T, name string) {
	t.Helper()
	if err := os.WriteFile(f.archive(t, name), []byte("corrupt"), 0644); err != nil {
		t.Fatal(err)
	}
}

// install installs the latest available version of name, as googet install
// does.
func (f *relFixture) install(t *testing.T, name string) error {
	t.Helper()
	pi := goolib.PackageInfo{Name: name}
	return FromRepo(t.Context(), pi, "", f.cache, f.rm, []string{"noarch", "x86_64"}, false, false, f.downloader, f.db)
}

// installArchive installs version 1 of a package whose archive holds
// contents and whose spec maps archive paths to destinations as files does,
// and records it in the database. It returns the package's installed files.
func (f *relFixture) installArchive(t *testing.T, name string, contents, files map[string]string) map[string]string {
	t.Helper()
	goo := filepath.Join(f.cache, name+".noarch.1.goo")
	writeGoo(t, goo, contents)
	ps := &goolib.PkgSpec{Name: name, Arch: "noarch", Version: "1", Files: files}
	installed, err := installPkg(goo, ps, false, policy{}, f.db, func(insFiles map[string]string) error {
		return f.db.AddPkg(client.PackageState{LocalPath: goo, PackageSpec: ps, InstalledFiles: insFiles})
	})
	if err != nil {
		t.Fatalf("installPkg(%s): %v", name, err)
	}
	return installed
}

// remove removes version 1 of the noarch package name, without dependents.
func (f *relFixture) remove(t *testing.T, name string) {
	t.Helper()
	pi := goolib.PackageInfo{Name: name, Arch: "noarch", Ver: "1"}
	if err := remove.All(t.Context(), pi, remove.DepMap{}, false, f.downloader, f.db); err != nil {
		t.Fatalf("remove %s: %v", name, err)
	}
}

// installed returns name.arch.version for every package in the database.
func (f *relFixture) installed(t *testing.T) []string {
	t.Helper()
	state, err := f.db.FetchPkgs("")
	if err != nil {
		t.Fatalf("FetchPkgs: %v", err)
	}
	var got []string
	for _, ps := range state {
		got = append(got, ps.PackageSpec.String())
	}
	slices.Sort(got)
	return got
}

// fileExists reports whether the file installed by package name is on disk.
// It only applies to packages passed to newRelFixture, which install one
// file named after the package.
func (f *relFixture) fileExists(name string) bool {
	_, err := os.Stat(filepath.Join(f.dst, name))
	return err == nil
}

func copyFile(t *testing.T, src, dst string) {
	t.Helper()
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, b, 0644); err != nil {
		t.Fatal(err)
	}
}

func checksumOf(t *testing.T, path string) string {
	t.Helper()
	fh, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer fh.Close()
	return goolib.Checksum(fh)
}

// TestCharacterizeReplaceRemovesDependents pins that replacing a package
// also removes the packages that depend on it, even when the replacing
// package provides the name they depend on.
//
// Known wrong: a fix should keep D installed, because R provides P.
func TestCharacterizeReplaceRemovesDependents(t *testing.T) {
	f := newRelFixture(t,
		[]goolib.PkgSpec{
			{Name: "P", Arch: "noarch", Version: "1"},
			{Name: "D", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"P": "1"}},
		},
		[]goolib.PkgSpec{
			{Name: "R", Arch: "noarch", Version: "1", Replaces: []string{"P"}, Provides: []string{"P=1"}},
		})
	if err := f.install(t, "R"); err != nil {
		t.Fatalf("install R: %v", err)
	}
	if diff := cmp.Diff([]string{"R.noarch.1"}, f.installed(t)); diff != "" {
		t.Errorf("installed packages after replace (-want +got):\n%s", diff)
	}
	if f.fileExists("D") {
		t.Errorf("D's file still exists; today the replace cascade uninstalls D")
	}
}

// TestCharacterizeReplaceBeforeDownload pins that the replaced package is
// uninstalled, and its cached archive deleted, before the replacing package
// is downloaded, so a failed download leaves neither package installed.
//
// Known wrong: a fix should download and verify R before removing anything,
// leaving P installed with its file and cached archive.
func TestCharacterizeReplaceBeforeDownload(t *testing.T) {
	for _, tc := range []struct {
		name     string
		sabotage func(f *relFixture, t *testing.T, name string)
		wantErr  string
	}{
		{name: "archive missing", sabotage: (*relFixture).unpublish, wantErr: "unexpected status"},
		{name: "checksum mismatch", sabotage: (*relFixture).corrupt, wantErr: "checksum"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRelFixture(t,
				[]goolib.PkgSpec{{Name: "P", Arch: "noarch", Version: "1"}},
				[]goolib.PkgSpec{{Name: "R", Arch: "noarch", Version: "1", Replaces: []string{"P"}}})
			tc.sabotage(f, t, "R")
			p, err := f.db.FetchPkg(goolib.PackageInfo{Name: "P", Arch: "noarch"})
			if err != nil {
				t.Fatalf("FetchPkg(P): %v", err)
			}
			if _, err := os.Stat(p.LocalPath); err != nil {
				t.Fatalf("P's cached archive before install: %v", err)
			}
			if err := f.install(t, "R"); err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("install R = %v, want an error containing %q", err, tc.wantErr)
			}
			if got := f.installed(t); len(got) != 0 {
				t.Errorf("installed packages after failed replace = %v, want none (today P is removed first)", got)
			}
			if f.fileExists("P") {
				t.Errorf("P's file still exists; today P is uninstalled before R is downloaded")
			}
			if _, err := os.Stat(p.LocalPath); !os.IsNotExist(err) {
				t.Errorf("P's cached archive: Stat error = %v, want it deleted (today P's cache is removed with P)", err)
			}
		})
	}
}

// TestCharacterizeSelfConflictBlocksUpgrade pins that a package that both
// provides and conflicts with the same name can still upgrade, because the
// installed version of the incoming package is excluded from its own conflict
// check.
//
// Intended: installing A 2 overwrites A 1, so A 1 does not block the upgrade.
func TestCharacterizeSelfConflictBlocksUpgrade(t *testing.T) {
	spec := func(version string) goolib.PkgSpec {
		return goolib.PkgSpec{Name: "A", Arch: "noarch", Version: version, Provides: []string{"X"}, Conflicts: []string{"X"}}
	}
	f := newRelFixture(t, []goolib.PkgSpec{spec("1")}, []goolib.PkgSpec{spec("2")})
	if err := f.install(t, "A"); err != nil {
		t.Fatalf("install A 2: %v", err)
	}
	if diff := cmp.Diff([]string{"A.noarch.2"}, f.installed(t)); diff != "" {
		t.Errorf("installed packages (-want +got):\n%s", diff)
	}
}

// TestCharacterizeConflictsOneWay pins that only the incoming package's
// Conflicts are checked, so installing a package that an installed package
// conflicts with succeeds.
//
// Known wrong: a fix should detect the conflict when B is installed. If the
// fix makes it an error, installing B should fail here; if it only warns by
// default, this test should also assert the warning.
func TestCharacterizeConflictsOneWay(t *testing.T) {
	f := newRelFixture(t,
		[]goolib.PkgSpec{{Name: "A", Arch: "noarch", Version: "1", Conflicts: []string{"B"}}},
		[]goolib.PkgSpec{{Name: "B", Arch: "noarch", Version: "1"}})
	if err := f.install(t, "B"); err != nil {
		t.Fatalf("install B: %v, want success (today conflicts are one-way)", err)
	}
	if diff := cmp.Diff([]string{"A.noarch.1", "B.noarch.1"}, f.installed(t)); diff != "" {
		t.Errorf("installed packages (-want +got):\n%s", diff)
	}
}

// TestCharacterizeOverwrittenFileDeletedOnRemove pins that when B overwrites
// a file that A installed, removing A deletes the file even though B's
// database row still lists it.
//
// Known wrong: a fix should keep the file, because B owns it.
func TestCharacterizeOverwrittenFileDeletedOnRemove(t *testing.T) {
	f := newRelFixture(t, nil, nil)
	shared := filepath.Join(f.dst, "shared")
	f.installArchive(t, "A", map[string]string{"shared": "from A"}, map[string]string{"shared": shared})
	f.installArchive(t, "B", map[string]string{"shared": "from B"}, map[string]string{"shared": shared})
	if b, err := os.ReadFile(shared); err != nil || string(b) != "from B" {
		t.Fatalf("shared file before removing A = %q, %v; want %q", b, err, "from B")
	}

	f.remove(t, "A")
	if _, err := os.Stat(shared); !os.IsNotExist(err) {
		t.Errorf("shared file: Stat error = %v, want it deleted with A (today removal ignores ownership)", err)
	}
	b, err := f.db.FetchPkg(goolib.PackageInfo{Name: "B", Arch: "noarch"})
	if err != nil {
		t.Fatalf("FetchPkg(B): %v", err)
	}
	if _, ok := b.InstalledFiles[shared]; !ok {
		t.Errorf("B's InstalledFiles = %v, want it to still list %s", b.InstalledFiles, shared)
	}
}

// TestCharacterizeCreatedParentDirsNotRecorded pins that parent directories
// that install creates above a package's destination aren't recorded in its
// InstalledFiles, so removing the package leaves them behind.
//
// The package ships a directory rather than a lone file. Install creates the
// missing parents of a lone file with the file's own mode (for example 0644),
// which on Linux and macOS leaves them untraversable and makes non-root
// installs into nested destinations fail. That is a separate bug, not pinned
// here.
//
// Known wrong: a fix should record the created parents and remove them, when
// they are empty, along with the package.
func TestCharacterizeCreatedParentDirsNotRecorded(t *testing.T) {
	f := newRelFixture(t, nil, nil)
	parent := filepath.Join(f.dst, "vendor", "app")
	target := filepath.Join(parent, "bin")

	files := f.installArchive(t, "A", map[string]string{"bin/tool": "from A"}, map[string]string{"bin": target})
	want := []string{target, filepath.Join(target, "tool")}
	if diff := cmp.Diff(want, slices.Sorted(maps.Keys(files))); diff != "" {
		t.Errorf("InstalledFiles (-want +got):\n%s", diff)
	}

	f.remove(t, "A")
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Errorf("%s: Stat error = %v, want it removed with A", target, err)
	}
	if _, err := os.Stat(parent); err != nil {
		t.Errorf("created parent directory: Stat error = %v, want it left behind (today parents aren't recorded)", err)
	}
}

// TestCharacterizeConflictsCheckedBeforeReplaces pins that a package that
// lists the same name in Conflicts and Replaces can't replace it, because
// conflicts are checked first.
//
// Known wrong: a fix should exempt replaced packages from the conflict check,
// so installing R replaces P.
func TestCharacterizeConflictsCheckedBeforeReplaces(t *testing.T) {
	f := newRelFixture(t,
		[]goolib.PkgSpec{{Name: "P", Arch: "noarch", Version: "1"}},
		[]goolib.PkgSpec{{Name: "R", Arch: "noarch", Version: "1", Conflicts: []string{"P"}, Replaces: []string{"P"}}})
	if err := f.install(t, "R"); err == nil || !strings.Contains(err.Error(), "conflict") {
		t.Errorf("install R = %v, want a conflict error (today Conflicts is checked before Replaces)", err)
	}
	if diff := cmp.Diff([]string{"P.noarch.1"}, f.installed(t)); diff != "" {
		t.Errorf("installed packages (-want +got):\n%s", diff)
	}
}

// TestCharacterizeReplaceDependentArchExact pins that an arch-less Replaces
// entry misses dependents that name the replaced package with an arch, so
// they stay installed with an unsatisfied dependency.
//
// Known wrong: a fix should treat D as a dependent of P.x86_64, whatever the
// fix then decides to do with it.
func TestCharacterizeReplaceDependentArchExact(t *testing.T) {
	f := newRelFixture(t,
		[]goolib.PkgSpec{
			{Name: "P", Arch: "x86_64", Version: "1"},
			{Name: "D", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"P.x86_64": "1"}},
		},
		[]goolib.PkgSpec{{Name: "R", Arch: "noarch", Version: "1", Replaces: []string{"P"}}})
	// Replace looks up P's dependents the same way.
	deps, _, err := remove.EnumerateDeps(goolib.PackageInfo{Name: "P"}, f.db)
	if err != nil {
		t.Fatalf("EnumerateDeps(P): %v", err)
	}
	if _, ok := deps["D.noarch"]; ok {
		t.Errorf("EnumerateDeps(P) = %v, want no D.noarch (today arch-qualified dependents are missed)", deps)
	}
	if err := f.install(t, "R"); err != nil {
		t.Fatalf("install R: %v", err)
	}
	if diff := cmp.Diff([]string{"D.noarch.1", "R.noarch.1"}, f.installed(t)); diff != "" {
		t.Errorf("installed packages after replace (-want +got):\n%s (today D is missed)", diff)
	}
}

// TestCharacterizeReplacesVersionIsMinimum pins that the version in a
// "name.arch.version" Replaces entry is a minimum, so it matches newer
// installed versions and not older ones.
//
// Intended: this is the legacy meaning of entries without an operator, and
// it should not change even if version operators are added.
func TestCharacterizeReplacesVersionIsMinimum(t *testing.T) {
	for _, tc := range []struct {
		installed    string
		wantReplaced bool
	}{
		{installed: "3.0.0", wantReplaced: true},
		{installed: "2.0.0", wantReplaced: true},
		{installed: "1.0.0", wantReplaced: false},
	} {
		t.Run(tc.installed, func(t *testing.T) {
			f := newRelFixture(t, []goolib.PkgSpec{{Name: "foo", Arch: "noarch", Version: tc.installed}}, nil)
			r := &goolib.PkgSpec{Name: "R", Arch: "noarch", Version: "1", Replaces: []string{"foo.noarch.2.0.0"}}
			if err := (&installer{dbOnly: true, downloader: f.downloader, db: f.db}).resolveReplacements(t.Context(), r); err != nil {
				t.Fatalf("resolveReplacements: %v", err)
			}
			if gotReplaced := len(f.installed(t)) == 0; gotReplaced != tc.wantReplaced {
				t.Errorf("foo %s replaced = %t, want %t", tc.installed, gotReplaced, tc.wantReplaced)
			}
		})
	}
}
