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
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/google/googet/v2/client"
	"github.com/google/googet/v2/goolib"
	"github.com/google/googet/v2/settings"
	"github.com/google/googet/v2/testutil"
)

// failingScript is an install or uninstall command that doesn't exist, so
// running it fails.
var failingScript = goolib.ExecFile{Path: "missing-script.exe"}

// replaceFixture returns a fixture with P installed, D installed and
// depending on P, and R available, replacing P, with replacer's other fields.
func replaceFixture(t *testing.T, replacer goolib.PkgSpec, installed ...goolib.PkgSpec) *relFixture {
	t.Helper()
	replacer.Name, replacer.Arch, replacer.Version, replacer.Replaces = "R", "noarch", "1", []string{"P"}
	if installed == nil {
		installed = []goolib.PkgSpec{
			{Name: "P", Arch: "noarch", Version: "1"},
			{Name: "D", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"P": "1"}},
		}
	}
	return newRelFixture(t, installed, []goolib.PkgSpec{replacer})
}

// rows returns every database row by name.
func (f *relFixture) rows(t *testing.T) map[string]client.PackageState {
	t.Helper()
	state, err := f.db.FetchPkgs("")
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]client.PackageState{}
	for _, ps := range state {
		out[ps.PackageSpec.Name] = ps
	}
	return out
}

// runWithHook installs name with FromRepo's executor, calling hook at each
// apply stage.
func (f *relFixture) runWithHook(t *testing.T, ctx context.Context, opts Options, name string, hook func(applyStage, string) error) error {
	t.Helper()
	ri := &repoInstall{Installer: New(f.cache, f.db, opts), rm: f.rm, archs: corpusArchs, downloader: f.downloader, stageHook: hook}
	return ri.fromRepo(ctx, goolib.PackageInfo{Name: name}, "")
}

// ignoreReinstalled ignores the fields of a database row that reinstalling
// the package rewrites.
var ignoreReinstalled = cmpopts.IgnoreFields(client.PackageState{}, "InstalledFiles", "InstalledApp")

// TestFailedReplacerReinstallsReplacedPackages checks that when the replacing
// package's install script fails, the packages its replacement removed are
// installed again, in the reverse of the order they were removed in, with
// their database rows and files back, and that the error reports both.
func TestFailedReplacerReinstallsReplacedPackages(t *testing.T) {
	f := replaceFixture(t, goolib.PkgSpec{Install: failingScript})
	before := f.rows(t)
	logs := testutil.CaptureLogs(t)
	var err error
	out := captureStdout(t, func() { err = f.install(t, "R") })
	if err == nil || !strings.Contains(err.Error(), "missing-script.exe") || !strings.Contains(err.Error(), "reinstalled the packages that installing R.noarch.1 had removed: P.noarch.1, D.noarch.1") {
		t.Fatalf("install R = %v, want the install script's error and the reinstalled packages", err)
	}
	if diff := cmp.Diff([]string{"D.noarch.1", "P.noarch.1"}, f.installed(t)); diff != "" {
		t.Errorf("installed packages (-want +got):\n%s", diff)
	}
	after := f.rows(t)
	for _, name := range []string{"P", "D"} {
		if diff := cmp.Diff(before[name], after[name], ignoreReinstalled); diff != "" {
			t.Errorf("%s's row after recovery (-before +after):\n%s", name, diff)
		}
		if diff := cmp.Diff(before[name].InstalledFiles, after[name].InstalledFiles); diff != "" {
			t.Errorf("%s's installed files after recovery (-before +after):\n%s", name, diff)
		}
		if !f.fileExists(name) {
			t.Errorf("%s's file is missing after recovery", name)
		}
	}
	if f.fileExists("R") {
		t.Errorf("R's file is installed")
	}
	record := "replace_record replacer=R.noarch.1 rows=D.noarch.1,P.noarch.1 archives=" + before["D"].LocalPath + "," + before["P"].LocalPath
	if !strings.Contains(out, record) || !strings.Contains(logs.String(), record) {
		t.Errorf("console output %q and logs %q, want both to contain %q", out, logs, record)
	}
	if !strings.Contains(logs.String(), "replace_recovery replacer=R.noarch.1 reinstalled=P.noarch.1,D.noarch.1 failed=0") {
		t.Errorf("logs missing the recovery line; got:\n%s", logs)
	}
}

// TestFailedUninstallReinstallsEarlierRemovals checks that when uninstalling
// one replaced package fails, the packages removed before it are installed
// again, and the one that failed stays installed.
func TestFailedUninstallReinstallsEarlierRemovals(t *testing.T) {
	f := replaceFixture(t, goolib.PkgSpec{}, goolib.PkgSpec{Name: "P", Arch: "noarch", Version: "1", Uninstall: failingScript},
		goolib.PkgSpec{Name: "D", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"P": "1"}})
	err := f.install(t, "R")
	if err == nil || !strings.Contains(err.Error(), "missing-script.exe") || !strings.Contains(err.Error(), "had removed: D.noarch.1") {
		t.Fatalf("install R = %v, want P's uninstall error and D reinstalled", err)
	}
	if diff := cmp.Diff([]string{"D.noarch.1", "P.noarch.1"}, f.installed(t)); diff != "" {
		t.Errorf("installed packages (-want +got):\n%s", diff)
	}
	for _, name := range []string{"P", "D"} {
		if !f.fileExists(name) {
			t.Errorf("%s's file is missing", name)
		}
	}
}

// TestRecoveryFailureKeepsArchiveAndReportsIt checks that a package that
// can't be reinstalled is reported, with the path of its kept archive, that
// the other removed packages are still reinstalled, and that the original
// error is kept.
func TestRecoveryFailureKeepsArchiveAndReportsIt(t *testing.T) {
	f := replaceFixture(t, goolib.PkgSpec{Install: failingScript})
	pArchive := f.rows(t)["P"].LocalPath
	err := f.runWithHook(t, t.Context(), Options{}, "R", func(stage applyStage, step string) error {
		if stage == stageAfterUninstall && step == "P.noarch.1" {
			// Make P's kept archive unusable.
			return os.WriteFile(pArchive, []byte("corrupt"), 0644)
		}
		return nil
	})
	for _, want := range []string{"missing-script.exe", fmt.Sprintf("couldn't reinstall P.noarch.1, which installing R.noarch.1 removed (archive %q)", pArchive), "had removed: D.noarch.1"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("install R = %v, want an error containing %q", err, want)
		}
	}
	if diff := cmp.Diff([]string{"D.noarch.1"}, f.installed(t)); diff != "" {
		t.Errorf("installed packages (-want +got):\n%s", diff)
	}
	if _, statErr := os.Stat(pArchive); statErr != nil {
		t.Errorf("P's archive %s: %v, want it kept", pArchive, statErr)
	}
}

// TestSuccessfulReplaceDeletesKeptArchives checks that once the replacing
// package is recorded, the archives kept for the packages it replaced are
// deleted.
func TestSuccessfulReplaceDeletesKeptArchives(t *testing.T) {
	f := replaceFixture(t, goolib.PkgSpec{})
	before := f.rows(t)
	if err := f.install(t, "R"); err != nil {
		t.Fatalf("install R: %v", err)
	}
	if diff := cmp.Diff([]string{"R.noarch.1"}, f.installed(t)); diff != "" {
		t.Errorf("installed packages (-want +got):\n%s", diff)
	}
	for _, name := range []string{"P", "D"} {
		if _, err := os.Stat(before[name].LocalPath); !os.IsNotExist(err) {
			t.Errorf("%s's archive: Stat error = %v, want it deleted", name, err)
		}
	}
}

// TestDatabaseOnlyRecoveryRestoresRows checks that a database-only install
// that fails after removing replaced packages writes their rows back exactly.
func TestDatabaseOnlyRecoveryRestoresRows(t *testing.T) {
	f := replaceFixture(t, goolib.PkgSpec{})
	before := f.rows(t)
	stop := errors.New("stop")
	err := f.runWithHook(t, t.Context(), Options{DBOnly: true}, "R", func(stage applyStage, _ string) error {
		if stage == stageAfterReplace {
			return stop
		}
		return nil
	})
	if !errors.Is(err, stop) {
		t.Fatalf("install R = %v, want the injected error", err)
	}
	if diff := cmp.Diff(before, f.rows(t), cmpopts.IgnoreFields(client.PackageState{}, "InstalledApp")); diff != "" {
		t.Errorf("rows after recovery (-before +after):\n%s", diff)
	}
}

// TestRecordFailureRollsBackReplacerAndRecovers checks that when the
// replacing package's database row can't be written, even after a second
// attempt, its files are rolled back and the packages it replaced are
// installed again, and that no unrecorded replace_record line is printed.
func TestRecordFailureRollsBackReplacerAndRecovers(t *testing.T) {
	f := replaceFixture(t, goolib.PkgSpec{})
	before := f.rows(t)
	full := errors.New("disk full")
	attempts := 0
	var err error
	out := captureStdout(t, func() {
		err = f.runWithHook(t, t.Context(), Options{}, "R", func(stage applyStage, step string) error {
			if stage == stageAddPkg && step == "R.noarch.1" {
				attempts++
				return full
			}
			return nil
		})
	})
	if !errors.Is(err, full) || !strings.Contains(err.Error(), "reinstalled the packages that installing R.noarch.1 had removed: P.noarch.1, D.noarch.1") {
		t.Fatalf("install R = %v, want the write error and both replaced packages reinstalled", err)
	}
	if attempts != 2 {
		t.Errorf("recording R was attempted %d times, want 2", attempts)
	}
	if diff := cmp.Diff(before, f.rows(t), cmpopts.IgnoreFields(client.PackageState{}, "InstalledApp")); diff != "" {
		t.Errorf("rows after recovery (-before +after):\n%s", diff)
	}
	if f.fileExists("R") {
		t.Errorf("R's file is still on disk; it should have been rolled back")
	}
	for _, name := range []string{"P", "D"} {
		if !f.fileExists(name) {
			t.Errorf("%s's file is missing after recovery", name)
		}
	}
	if strings.Contains(out, "result=unrecorded") {
		t.Errorf("console output %q, want no unrecorded replace_record line", out)
	}
}

// TestRecordingRetriedOnce checks that a replacing package whose database row
// fails to write once is recorded on the second attempt.
func TestRecordingRetriedOnce(t *testing.T) {
	f := replaceFixture(t, goolib.PkgSpec{})
	attempts := 0
	err := f.runWithHook(t, t.Context(), Options{}, "R", func(stage applyStage, step string) error {
		if stage == stageAddPkg && step == "R.noarch.1" {
			attempts++
			if attempts == 1 {
				return errors.New("busy")
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("install R: %v", err)
	}
	if diff := cmp.Diff([]string{"R.noarch.1"}, f.installed(t)); diff != "" {
		t.Errorf("installed packages (-want +got):\n%s", diff)
	}
}

// TestRecoveryIsPerStep checks that a failure in a later step doesn't put
// back packages that an earlier, completed step replaced, since the package
// that replaced them stays installed.
func TestRecoveryIsPerStep(t *testing.T) {
	f := newRelFixture(t,
		[]goolib.PkgSpec{{Name: "P", Arch: "noarch", Version: "1"}},
		[]goolib.PkgSpec{
			{Name: "T", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"D": "1"}, Install: failingScript},
			{Name: "D", Arch: "noarch", Version: "1", Replaces: []string{"P"}},
		})
	err := f.install(t, "T")
	if err == nil || strings.Contains(err.Error(), "reinstalled") {
		t.Fatalf("install T = %v, want the install script's error and nothing reinstalled", err)
	}
	if diff := cmp.Diff([]string{"D.noarch.1"}, f.installed(t)); diff != "" {
		t.Errorf("installed packages (-want +got):\n%s", diff)
	}
	if f.fileExists("P") {
		t.Errorf("P's file was put back although D, which replaced it, is installed")
	}
}

// TestRecoveryOfOlderClientRow checks that a replaced package that an older
// GooGet recorded with only an unpack directory is reinstalled from the
// archive next to that directory, and its row is written back as it was.
func TestRecoveryOfOlderClientRow(t *testing.T) {
	f := replaceFixture(t, goolib.PkgSpec{Install: failingScript}, goolib.PkgSpec{Name: "P", Arch: "noarch", Version: "1"})
	old := rewriteRow(t, f, "P", func(ps *client.PackageState) {
		ps.UnpackDir = strings.TrimSuffix(ps.LocalPath, ".goo")
		ps.LocalPath = ""
	})
	err := f.install(t, "R")
	if err == nil || !strings.Contains(err.Error(), "had removed: P.noarch.1") {
		t.Fatalf("install R = %v, want P reinstalled", err)
	}
	got := f.rows(t)["P"]
	if diff := cmp.Diff(old, got, ignoreReinstalled); diff != "" {
		t.Errorf("P's row after recovery (-before +after):\n%s", diff)
	}
	if got.LocalPath != "" {
		t.Errorf("P's LocalPath = %q, want it empty, as the older client recorded it", got.LocalPath)
	}
	if !f.fileExists("P") {
		t.Errorf("P's file is missing after recovery")
	}
}

// TestRecoveryRunsAfterCancellation checks that recovery still runs, and can
// download a replaced package's archive again, after the install's context
// is cancelled.
func TestRecoveryRunsAfterCancellation(t *testing.T) {
	f := replaceFixture(t, goolib.PkgSpec{Install: failingScript}, goolib.PkgSpec{Name: "P", Arch: "noarch", Version: "1"})
	pArchive := f.rows(t)["P"].LocalPath
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	err := f.runWithHook(t, ctx, Options{}, "R", func(stage applyStage, step string) error {
		if stage == stageAfterUninstall && step == "P.noarch.1" {
			cancel()
			// Recovery has to download P's archive again.
			return os.Remove(pArchive)
		}
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "had removed: P.noarch.1") {
		t.Fatalf("install R = %v, want P reinstalled", err)
	}
	if diff := cmp.Diff([]string{"P.noarch.1"}, f.installed(t)); diff != "" {
		t.Errorf("installed packages (-want +got):\n%s", diff)
	}
	if !f.fileExists("P") {
		t.Errorf("P's file is missing after recovery")
	}
}

// TestReplaceRecordAddsNothingToTheDatabase checks that a replacement that
// fails and recovers leaves the database with no new tables and rows that
// decode as package states without unknown fields, so older GooGet versions
// read it as before.
func TestReplaceRecordAddsNothingToTheDatabase(t *testing.T) {
	f := replaceFixture(t, goolib.PkgSpec{Install: failingScript})
	if err := f.install(t, "R"); err == nil {
		t.Fatalf("install R succeeded, want the install script's error")
	}
	db, err := sql.Open("sqlite", settings.DBFile())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tables, err := db.Query(`SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%'`)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for tables.Next() {
		var n string
		if err := tables.Scan(&n); err != nil {
			t.Fatal(err)
		}
		names = append(names, n)
	}
	tables.Close()
	if diff := cmp.Diff([]string{"InstalledPackages"}, names); diff != "" {
		t.Errorf("tables (-want +got):\n%s", diff)
	}
	rows, err := db.Query(`SELECT pkg_json FROM InstalledPackages`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			t.Fatal(err)
		}
		dec := json.NewDecoder(bytes.NewReader([]byte(raw)))
		dec.DisallowUnknownFields()
		var ps client.PackageState
		if err := dec.Decode(&ps); err != nil {
			t.Errorf("row %s doesn't decode strictly: %v", raw, err)
		}
	}
}

// TestRecoveryOfPackageInstalledEarlierInRun checks that a package that an
// earlier step of the same install installed, and that a later step replaces,
// is reinstalled from the archive that earlier step fetched when the later
// step fails.
func TestRecoveryOfPackageInstalledEarlierInRun(t *testing.T) {
	for _, dbOnly := range []bool{false, true} {
		t.Run(map[bool]string{false: "full", true: "database only"}[dbOnly], func(t *testing.T) {
			f := newRelFixture(t, nil, []goolib.PkgSpec{
				{Name: "T", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"A": "1", "B": "1"}},
				{Name: "A", Arch: "noarch", Version: "1"},
				{Name: "B", Arch: "noarch", Version: "1", Replaces: []string{"A"}},
			})
			stop := errors.New("stop")
			var err error
			out := captureStdout(t, func() {
				err = f.runWithHook(t, t.Context(), Options{DBOnly: dbOnly}, "T", func(stage applyStage, step string) error {
					if stage == stageAfterReplace && step == "B.noarch.1" {
						return stop
					}
					return nil
				})
			})
			wantRecord := "replace_record replacer=B.noarch.1 rows=A.noarch.1 archives="
			if !dbOnly {
				wantRecord += filepath.Join(f.cache, "A.noarch.1.goo")
			}
			if !strings.Contains(out, wantRecord+"\n") {
				t.Errorf("console output %q, want a line %q", out, wantRecord)
			}
			if !errors.Is(err, stop) || !strings.Contains(err.Error(), "had removed: A.noarch.1") {
				t.Fatalf("install T = %v, want the injected error and A reinstalled", err)
			}
			if diff := cmp.Diff([]string{"A.noarch.1"}, f.installed(t)); diff != "" {
				t.Errorf("installed packages (-want +got):\n%s", diff)
			}
			if !dbOnly && !f.fileExists("A") {
				t.Errorf("A's file is missing after recovery")
			}
			if n := f.getCount("A.noarch.1.goo"); n != 1 {
				t.Errorf("A's archive downloaded %d times, want 1", n)
			}
		})
	}
}
