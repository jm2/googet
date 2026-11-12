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
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/googet/v2/goolib"
)

// addInstalledFile records an extra file, which exists on disk, as installed
// by the installed package name.arch, and returns its path and the package's
// cached archive.
func addInstalledFile(t *testing.T, f *relFixture, name, arch string) (file, archive string) {
	t.Helper()
	ps, err := f.db.FetchPkg(goolib.PackageInfo{Name: name, Arch: arch})
	if err != nil || ps.PackageSpec == nil {
		t.Fatalf("FetchPkg(%s.%s) = %+v, %v", name, arch, ps, err)
	}
	file = filepath.Join(f.dst, name+"-"+arch+"-extra")
	if err := os.WriteFile(file, []byte("extra"), 0644); err != nil {
		t.Fatal(err)
	}
	ps.InstalledFiles[file] = checksumOf(t, file)
	if err := f.db.AddPkg(ps); err != nil {
		t.Fatal(err)
	}
	return file, ps.LocalPath
}

// TestInstallOtherArchKeepsInstalledArch checks that installing one arch of a
// package leaves another installed arch's files and cached archive alone.
func TestInstallOtherArchKeepsInstalledArch(t *testing.T) {
	f := newRelFixture(t,
		[]goolib.PkgSpec{{Name: "foo", Arch: "x86_64", Version: "1"}},
		[]goolib.PkgSpec{{Name: "foo", Arch: "noarch", Version: "2"}})
	file, archive := addInstalledFile(t, f, "foo", "x86_64")
	if err := f.install(t, "foo"); err != nil {
		t.Fatalf("install foo: %v", err)
	}
	if diff := cmp.Diff([]string{"foo.noarch.2", "foo.x86_64.1"}, f.installed(t)); diff != "" {
		t.Errorf("installed packages (-want +got):\n%s", diff)
	}
	if _, err := os.Stat(file); err != nil {
		t.Errorf("foo.x86_64's file %s: %v, want it kept", file, err)
	}
	if _, err := os.Stat(archive); err != nil {
		t.Errorf("foo.x86_64's cached archive %s: %v, want it kept", archive, err)
	}
}

// TestUpgradeCleansOldVersion checks that upgrading a package still deletes
// the old version's files that the new version doesn't ship, and its cached
// archive.
func TestUpgradeCleansOldVersion(t *testing.T) {
	f := newRelFixture(t,
		[]goolib.PkgSpec{{Name: "foo", Arch: "noarch", Version: "1"}},
		[]goolib.PkgSpec{{Name: "foo", Arch: "noarch", Version: "2"}})
	file, archive := addInstalledFile(t, f, "foo", "noarch")
	if err := f.install(t, "foo"); err != nil {
		t.Fatalf("install foo: %v", err)
	}
	if diff := cmp.Diff([]string{"foo.noarch.2"}, f.installed(t)); diff != "" {
		t.Errorf("installed packages (-want +got):\n%s", diff)
	}
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Errorf("old version's dropped file %s: Stat error = %v, want it deleted", file, err)
	}
	if _, err := os.Stat(archive); !os.IsNotExist(err) {
		t.Errorf("old version's cached archive %s: Stat error = %v, want it deleted", archive, err)
	}
	if !f.fileExists("foo") {
		t.Errorf("the file foo.noarch.2 ships is missing")
	}
}

// TestDowngradeCleansNewerVersion checks that installing an older version of
// an installed package still deletes the newer version's files that the
// older one doesn't ship, and its cached archive.
func TestDowngradeCleansNewerVersion(t *testing.T) {
	f := newRelFixture(t,
		[]goolib.PkgSpec{{Name: "foo", Arch: "noarch", Version: "2"}},
		[]goolib.PkgSpec{{Name: "foo", Arch: "noarch", Version: "1"}})
	file, archive := addInstalledFile(t, f, "foo", "noarch")
	if err := f.installVersion(t, "foo", "noarch", "1"); err != nil {
		t.Fatalf("install foo.noarch.1: %v", err)
	}
	if diff := cmp.Diff([]string{"foo.noarch.1"}, f.installed(t)); diff != "" {
		t.Errorf("installed packages (-want +got):\n%s", diff)
	}
	for _, p := range []string{file, archive} {
		if fileThere(p) {
			t.Errorf("%s is still there after the downgrade, want it deleted", p)
		}
	}
	if !f.fileExists("foo") {
		t.Errorf("the file foo.noarch.1 ships is missing")
	}
}

// TestUpgradeOfOneArchCleansOnlyThatArch checks that upgrading one installed
// arch of a package cleans up that arch's old version only, leaving another
// installed arch's files and archive alone.
func TestUpgradeOfOneArchCleansOnlyThatArch(t *testing.T) {
	f := newRelFixture(t,
		[]goolib.PkgSpec{{Name: "foo", Arch: "noarch", Version: "1"}, {Name: "foo", Arch: "x86_64", Version: "1"}},
		[]goolib.PkgSpec{{Name: "foo", Arch: "x86_64", Version: "2"}})
	noarchFile, noarchArchive := addInstalledFile(t, f, "foo", "noarch")
	oldFile, oldArchive := addInstalledFile(t, f, "foo", "x86_64")
	if err := f.installVersion(t, "foo", "x86_64", "2"); err != nil {
		t.Fatalf("install foo.x86_64.2: %v", err)
	}
	if diff := cmp.Diff([]string{"foo.noarch.1", "foo.x86_64.2"}, f.installed(t)); diff != "" {
		t.Errorf("installed packages (-want +got):\n%s", diff)
	}
	for _, p := range []string{noarchFile, noarchArchive} {
		if !fileThere(p) {
			t.Errorf("foo.noarch's %s was deleted, want it kept", p)
		}
	}
	for _, p := range []string{oldFile, oldArchive} {
		if fileThere(p) {
			t.Errorf("foo.x86_64.1's %s is still there, want it deleted", p)
		}
	}
}

// TestReinstallSameVersionCleanup pins what installing the installed version
// again from a repository cleans up: the install's own files stay, a file
// the package no longer ships is deleted, and so is the cached archive, which
// is the archive just installed, so the recorded archive path is left
// dangling. Keeping the archive is a deliberate change that must update this
// test.
func TestReinstallSameVersionCleanup(t *testing.T) {
	spec := goolib.PkgSpec{Name: "foo", Arch: "noarch", Version: "1"}
	f := newRelFixture(t, []goolib.PkgSpec{spec}, []goolib.PkgSpec{spec})
	file, archive := addInstalledFile(t, f, "foo", "noarch")
	if err := f.installVersion(t, "foo", "noarch", "1"); err != nil {
		t.Fatalf("install foo.noarch.1: %v", err)
	}
	if diff := cmp.Diff([]string{"foo.noarch.1"}, f.installed(t)); diff != "" {
		t.Errorf("installed packages (-want +got):\n%s", diff)
	}
	if !f.fileExists("foo") {
		t.Errorf("foo's file is missing after reinstalling it")
	}
	if fileThere(file) {
		t.Errorf("%s, which foo.noarch.1 doesn't ship, is still there", file)
	}
	if fileThere(archive) {
		t.Errorf("the cached archive %s is still there; reinstalling deleted it until now", archive)
	}
}

// installVersion installs name.arch.ver from the fixture's repository, as
// googet install does once it has resolved the version.
func (f *relFixture) installVersion(t *testing.T, name, arch, ver string) error {
	t.Helper()
	var repo string
	for u := range f.rm {
		repo = u
	}
	pi := goolib.PackageInfo{Name: name, Arch: arch, Ver: ver}
	return FromRepo(t.Context(), pi, repo, f.cache, f.rm, corpusArchs, false, false, f.downloader, f.db)
}

// fileThere reports whether a file exists at path.
func fileThere(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
