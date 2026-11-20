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
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/googet/v2/client"
	"github.com/google/googet/v2/goolib"
)

// cacheFiles returns the names of the archives in f's cache.
func (f *relFixture) cacheFiles(t *testing.T) []string {
	t.Helper()
	m, err := filepath.Glob(filepath.Join(f.cache, "*.goo"))
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, p := range m {
		out = append(out, filepath.Base(p))
	}
	return out
}

// TestFailedDownloadChangesNothing checks that when a package's archive can't
// be downloaded, nothing is installed, not even dependencies whose archives
// were downloaded first; that those archives stay cached; and that a second
// attempt reuses them without downloading them again or growing the cache.
func TestFailedDownloadChangesNothing(t *testing.T) {
	f := newRelFixture(t, nil, []goolib.PkgSpec{
		{Name: "T", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"D": "1", "E": "1"}},
		{Name: "D", Arch: "noarch", Version: "1"},
		{Name: "E", Arch: "noarch", Version: "1"},
	})
	f.unpublish(t, "T")
	for attempt := 1; attempt <= 2; attempt++ {
		if err := f.install(t, "T"); err == nil || !strings.Contains(err.Error(), "unexpected status") {
			t.Fatalf("attempt %d: install T = %v, want a download error", attempt, err)
		}
		if got := f.installed(t); len(got) != 0 {
			t.Errorf("attempt %d: installed packages = %v, want none", attempt, got)
		}
		for _, name := range []string{"D", "E"} {
			if f.fileExists(name) {
				t.Errorf("attempt %d: %s's file is installed", attempt, name)
			}
		}
		if diff := cmp.Diff([]string{"D.noarch.1.goo", "E.noarch.1.goo", "T.noarch.1.goo"}, f.cacheFiles(t)); diff != "" {
			t.Errorf("attempt %d: cached archives (-want +got):\n%s", attempt, diff)
		}
		for _, archive := range []string{"D.noarch.1.goo", "E.noarch.1.goo"} {
			if got := f.getCount(archive); got != 1 {
				t.Errorf("attempt %d: %s downloaded %d times, want 1", attempt, archive, got)
			}
		}
	}
}

// TestCorruptDependencyArchiveChangesNothing checks that a dependency whose
// archive fails its checksum stops the install before anything changes, and
// that the bad download isn't kept.
func TestCorruptDependencyArchiveChangesNothing(t *testing.T) {
	f := newRelFixture(t, nil, []goolib.PkgSpec{
		{Name: "T", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"D": "1", "E": "1"}},
		{Name: "D", Arch: "noarch", Version: "1"},
		{Name: "E", Arch: "noarch", Version: "1"},
	})
	f.corrupt(t, "E")
	if err := f.install(t, "T"); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("install T = %v, want a checksum error", err)
	}
	if got := f.installed(t); len(got) != 0 {
		t.Errorf("installed packages = %v, want none", got)
	}
	if diff := cmp.Diff([]string{"D.noarch.1.goo"}, f.cacheFiles(t)); diff != "" {
		t.Errorf("cached archives (-want +got):\n%s", diff)
	}
}

// TestUnusableReplacedArchiveChangesNothing checks that replacing a package
// whose cached archive is gone and can't be downloaded again fails before
// anything changes, naming the package and how to get past it.
func TestUnusableReplacedArchiveChangesNothing(t *testing.T) {
	f := newRelFixture(t,
		[]goolib.PkgSpec{{Name: "P", Arch: "noarch", Version: "1"}},
		[]goolib.PkgSpec{{Name: "R", Arch: "noarch", Version: "1", Replaces: []string{"P"}}})
	p, err := f.db.FetchPkg(goolib.PackageInfo{Name: "P", Arch: "noarch"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(p.LocalPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(f.gooDir, filepath.Base(p.LocalPath))); err != nil {
		t.Fatal(err)
	}
	err = f.install(t, "R")
	for _, want := range []string{
		"cannot install R.noarch.1: installing it removes P.noarch.1, which has no usable cached archive to uninstall it with:",
		"restore the archive at " + p.LocalPath,
		`remove P.noarch first with "googet remove -db_only P.noarch", which doesn't run its uninstall script`,
	} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("install R = %v, want an error containing %q", err, want)
		}
	}
	if diff := cmp.Diff([]string{"P.noarch.1"}, f.installed(t)); diff != "" {
		t.Errorf("installed packages (-want +got):\n%s", diff)
	}
	if !f.fileExists("P") {
		t.Errorf("P's file was removed")
	}

	// When multiple architectures of P are installed and only P.x86_64 is
	// selected by Replaces, the remediation hint targets P.x86_64 specifically
	// rather than arch-less P.
	fMulti := newRelFixture(t,
		[]goolib.PkgSpec{
			{Name: "P", Arch: "noarch", Version: "1"},
			{Name: "P", Arch: "x86_64", Version: "3"},
		},
		[]goolib.PkgSpec{{Name: "R", Arch: "noarch", Version: "1", Replaces: []string{"P.x86_64.2"}}})
	px, err := fMulti.db.FetchPkg(goolib.PackageInfo{Name: "P", Arch: "x86_64"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(px.LocalPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(fMulti.gooDir, filepath.Base(px.LocalPath))); err != nil {
		t.Fatal(err)
	}
	err = fMulti.install(t, "R")
	wantMulti := `remove P.x86_64 first with "googet remove -db_only P.x86_64", which doesn't run its uninstall script`
	if err == nil || !strings.Contains(err.Error(), wantMulti) {
		t.Errorf("install R (multi-arch missing archive) = %v, want substring %q", err, wantMulti)
	}
}

// TestDBOnlyReplaceNeedsNoReplacedArchive checks that a database-only install
// doesn't need the archives of the packages it replaces, since it runs no
// uninstall scripts.
func TestDBOnlyReplaceNeedsNoReplacedArchive(t *testing.T) {
	f := newRelFixture(t,
		[]goolib.PkgSpec{{Name: "P", Arch: "noarch", Version: "1"}},
		[]goolib.PkgSpec{{Name: "R", Arch: "noarch", Version: "1", Replaces: []string{"P"}}})
	p, err := f.db.FetchPkg(goolib.PackageInfo{Name: "P", Arch: "noarch"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(p.LocalPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(f.gooDir, filepath.Base(p.LocalPath))); err != nil {
		t.Fatal(err)
	}
	if err := New(f.cache, f.db, Options{DBOnly: true}).FromRepo(t.Context(), goolib.PackageInfo{Name: "R"}, "", f.rm, corpusArchs, f.downloader); err != nil {
		t.Fatalf("database-only install of R: %v", err)
	}
	if diff := cmp.Diff([]string{"R.noarch.1"}, f.installed(t)); diff != "" {
		t.Errorf("installed packages (-want +got):\n%s", diff)
	}
	if got := f.getCount(filepath.Base(p.LocalPath)); got != 0 {
		t.Errorf("P's archive downloaded %d times, want 0", got)
	}
}

// TestCachedArchiveIsNotDownloadedAgain checks that an archive already in
// the cache with the right checksum is used without downloading it.
func TestCachedArchiveIsNotDownloadedAgain(t *testing.T) {
	f := newRelFixture(t, nil, []goolib.PkgSpec{{Name: "A", Arch: "noarch", Version: "1"}})
	copyFile(t, f.archive(t, "A"), filepath.Join(f.cache, "A.noarch.1.goo"))
	if err := f.install(t, "A"); err != nil {
		t.Fatalf("install A: %v", err)
	}
	if got := f.getCount("A.noarch.1.goo"); got != 0 {
		t.Errorf("A's archive downloaded %d times, want 0", got)
	}
}

// TestReinstallThatReplacesItself checks a package that lists its own name in
// Replaces and is installed again at the same version: removing the
// installed copy deletes the archive the install fetched, so the install
// fetches it again.
func TestReinstallThatReplacesItself(t *testing.T) {
	spec := goolib.PkgSpec{Name: "X", Arch: "noarch", Version: "2", Replaces: []string{"X"}}
	f := newRelFixture(t, []goolib.PkgSpec{spec}, []goolib.PkgSpec{spec})
	if err := f.install(t, "X"); err != nil {
		t.Fatalf("install X: %v", err)
	}
	if diff := cmp.Diff([]string{"X.noarch.2"}, f.installed(t)); diff != "" {
		t.Errorf("installed packages (-want +got):\n%s", diff)
	}
	if !f.fileExists("X") {
		t.Errorf("X's file is missing")
	}
}

// TestPlanningOutputComesFirst checks that the console messages for every
// package an install resolves come before any package is installed.
func TestPlanningOutputComesFirst(t *testing.T) {
	f := newRelFixture(t, nil, []goolib.PkgSpec{
		{Name: "A", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"B": "1", "C": "1"}},
		{Name: "B", Arch: "noarch", Version: "1"},
		{Name: "C", Arch: "noarch", Version: "1"},
	})
	var err error
	out := captureStdout(t, func() { err = f.install(t, "A") })
	if err != nil {
		t.Fatalf("install A: %v", err)
	}
	var got []string
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		if strings.HasPrefix(l, "Installing ") || strings.HasPrefix(l, "Installation of ") {
			got = append(got, l)
		}
	}
	want := []string{
		"Installing A.. and dependencies...",
		"Installing B.noarch.1 and dependencies...",
		"Installing C.noarch.1 and dependencies...",
		"Installation of B.noarch.1 and all dependencies completed",
		"Installation of C.noarch.1 and all dependencies completed",
		"Installation of A.noarch.1 and all dependencies completed",
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("console messages (-want +got):\n%s", diff)
	}
}

// TestApplyStopsWhenRemovalsDiverge checks that if the installed packages
// change after an install is planned so that a replacement would remove
// packages the plan didn't expect, the install stops before removing
// anything.
func TestApplyStopsWhenRemovalsDiverge(t *testing.T) {
	f := newRelFixture(t,
		[]goolib.PkgSpec{{Name: "P", Arch: "noarch", Version: "1"}},
		[]goolib.PkgSpec{{Name: "R", Arch: "noarch", Version: "1", Replaces: []string{"P"}}})
	ri := &repoInstall{Installer: New(f.cache, f.db, Options{}), rm: f.rm, archs: corpusArchs, downloader: f.downloader}
	ri.stageHook = func(stage applyStage, _ string) error {
		if stage != stageFetched {
			return nil
		}
		// Another package that depends on P appears.
		return f.db.AddPkg(client.PackageState{PackageSpec: &goolib.PkgSpec{Name: "D", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"P": "1"}}})
	}
	err := ri.fromRepo(t.Context(), goolib.PackageInfo{Name: "R"}, "")
	if !errors.Is(err, errPlanDivergence) {
		t.Fatalf("install R = %v, want errPlanDivergence", err)
	}
	if diff := cmp.Diff([]string{"D.noarch.1", "P.noarch.1"}, f.installed(t)); diff != "" {
		t.Errorf("installed packages (-want +got):\n%s", diff)
	}
	if !f.fileExists("P") {
		t.Errorf("P's file was removed")
	}
}

// TestApplyStages checks the order in which an install reaches each apply
// stage, which tests use to inject failures.
func TestApplyStages(t *testing.T) {
	f := newRelFixture(t,
		[]goolib.PkgSpec{{Name: "P", Arch: "noarch", Version: "1"}},
		[]goolib.PkgSpec{
			{Name: "R", Arch: "noarch", Version: "1", Replaces: []string{"P"}, PkgDependencies: map[string]string{"D": "1"}},
			{Name: "D", Arch: "noarch", Version: "1"},
		})
	var got []string
	ri := &repoInstall{Installer: New(f.cache, f.db, Options{}), rm: f.rm, archs: corpusArchs, downloader: f.downloader}
	names := map[applyStage]string{stageFetched: "fetched", stageBeforeReplace: "before-replace", stageBeforeUninstall: "before-uninstall", stageAfterUninstall: "after-uninstall", stageAfterReplace: "after-replace", stageAfterInstall: "after-install", stageAddPkg: "add-pkg", stageCommitted: "committed"}
	ri.stageHook = func(stage applyStage, step string) error {
		got = append(got, strings.TrimSpace(names[stage]+" "+step))
		return nil
	}
	if err := ri.fromRepo(t.Context(), goolib.PackageInfo{Name: "R"}, ""); err != nil {
		t.Fatalf("install R: %v", err)
	}
	want := []string{
		"fetched",
		"after-install D.noarch.1", "add-pkg D.noarch.1", "committed D.noarch.1",
		"before-replace R.noarch.1", "before-uninstall P.noarch.1", "after-uninstall P.noarch.1", "after-replace R.noarch.1",
		"after-install R.noarch.1", "add-pkg R.noarch.1", "committed R.noarch.1",
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("stages (-want +got):\n%s", diff)
	}
	if want := []string{"D.noarch.1", "R.noarch.1"}; !slices.Equal(f.installed(t), want) {
		t.Errorf("installed packages = %v, want %v", f.installed(t), want)
	}
}

// TestApplyStopsWhenConflictReappears checks that a conflict the plan
// expected the replacement to clear, but that the installed packages show
// again once it has run, stops the install before the package is installed
// and reinstalls the package the replacement removed.
func TestApplyStopsWhenConflictReappears(t *testing.T) {
	f := newRelFixture(t,
		[]goolib.PkgSpec{{Name: "P", Arch: "noarch", Version: "1"}},
		[]goolib.PkgSpec{{Name: "R", Arch: "noarch", Version: "1", Conflicts: []string{"P"}, Replaces: []string{"P"}}})
	ri := &repoInstall{Installer: New(f.cache, f.db, Options{}), rm: f.rm, archs: corpusArchs, downloader: f.downloader}
	ri.stageHook = func(stage applyStage, _ string) error {
		if stage != stageAfterReplace {
			return nil
		}
		// Another arch of P appears.
		return f.db.AddPkg(client.PackageState{PackageSpec: &goolib.PkgSpec{Name: "P", Arch: "x86_64", Version: "1"}})
	}
	err := ri.fromRepo(t.Context(), goolib.PackageInfo{Name: "R"}, "")
	if !errors.Is(err, errPlanDivergence) || !strings.Contains(err.Error(), "conflict with installed package or provider: P") {
		t.Fatalf("install R = %v, want errPlanDivergence naming the conflict with P", err)
	}
	// The replaced P.noarch is reinstalled.
	if diff := cmp.Diff([]string{"P.noarch.1", "P.x86_64.1"}, f.installed(t)); diff != "" {
		t.Errorf("installed packages (-want +got):\n%s", diff)
	}
}
