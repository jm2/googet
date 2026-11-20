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
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/googet/v2/client"
	"github.com/google/googet/v2/googetdb"
	"github.com/google/googet/v2/goolib"
	"github.com/google/googet/v2/settings"
	"github.com/google/googet/v2/testutil"
)

// scriptedPkg builds a .goo archive in gooDir whose payload writes payloadPath
// and whose optional install and uninstall shell scripts append lines to a log.
func scriptedPkg(t *testing.T, gooDir, payloadPath string, ps goolib.PkgSpec, installScript, uninstallScript string) goolib.RepoSpec {
	t.Helper()
	ps.Files = map[string]string{"payload": payloadPath}
	if installScript != "" {
		ps.Install = goolib.ExecFile{Path: "install.sh"}
	}
	if uninstallScript != "" {
		ps.Uninstall = goolib.ExecFile{Path: "uninstall.sh"}
	}
	specBytes, err := json.Marshal(ps)
	if err != nil {
		t.Fatal(err)
	}
	archiveName := ps.String() + ".goo"
	f, err := os.Create(filepath.Join(gooDir, archiveName))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	h := sha256.New()
	gw := gzip.NewWriter(io.MultiWriter(h, f))
	tw := tar.NewWriter(gw)
	modTime := time.Now()
	entries := []struct {
		name string
		mode int64
		body []byte
	}{
		{name: "payload", mode: 0644, body: []byte(ps.String())},
		{name: ps.Name + ".pkgspec", mode: 0644, body: specBytes},
	}
	if installScript != "" {
		entries = append(entries, struct {
			name string
			mode int64
			body []byte
		}{name: "install.sh", mode: 0755, body: []byte(installScript)})
	}
	if uninstallScript != "" {
		entries = append(entries, struct {
			name string
			mode int64
			body []byte
		}{name: "uninstall.sh", mode: 0755, body: []byte(uninstallScript)})
	}
	for _, e := range entries {
		if err := tw.WriteHeader(&tar.Header{Name: e.name, Mode: e.mode, Size: int64(len(e.body)), ModTime: modTime}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(e.body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	return goolib.RepoSpec{
		Checksum:    fmt.Sprintf("%x", h.Sum(nil)),
		Source:      archiveName,
		PackageSpec: &ps,
	}
}

// TestRecoveryExactReverseScriptOrderAndStrictConflicts checks that uninstall
// scripts run in removal order and recovery install scripts run in the exact
// reverse removal order with file-conflict overwriting enabled even when
// StrictConflicts is set.
func TestRecoveryExactReverseScriptOrderAndStrictConflicts(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("The packages' install scripts are POSIX shell scripts.")
	}
	t.Cleanup(settings.Snapshot())
	rootDir := t.TempDir()
	settings.Initialize(rootDir, false)
	db, err := googetdb.NewDB(settings.DBFile())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	gooDir := t.TempDir()
	cacheDir := t.TempDir()
	dstDir := t.TempDir()
	orderLog := filepath.Join(t.TempDir(), "events.log")
	sharedFile := filepath.Join(dstDir, "shared")

	srv := testutil.ServeGoo(t, gooDir)
	t.Cleanup(srv.Close)
	downloader, err := client.NewDownloader("")
	if err != nil {
		t.Fatal(err)
	}

	logScript := func(tag string) string {
		return fmt.Sprintf("#!/bin/sh\necho %s >> %q\n", tag, orderLog)
	}

	// Unrelated package U also records sharedFile so StrictConflicts would
	// block reinstalling victims unless recovery sets Force: true.
	if err := os.WriteFile(sharedFile, []byte("initial"), 0644); err != nil {
		t.Fatal(err)
	}
	state := client.GooGetState{{
		PackageSpec:    &goolib.PkgSpec{Name: "U", Arch: "noarch", Version: "1"},
		InstalledFiles: map[string]string{sharedFile: "hash-u"},
	}}

	victims := []goolib.PkgSpec{
		{Name: "P", Arch: "noarch", Version: "1"},
		{Name: "D", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"P": "1"}},
		{Name: "E", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"D": "1"}},
		{Name: "Q", Arch: "noarch", Version: "1"},
	}
	for _, ps := range victims {
		targetFile := filepath.Join(dstDir, ps.Name)
		if ps.Name == "P" {
			targetFile = sharedFile
		}
		rs := scriptedPkg(t, gooDir, targetFile, ps, logScript("install:"+ps.Name), logScript("uninstall:"+ps.Name))
		local := filepath.Join(cacheDir, rs.Source)
		b, readErr := os.ReadFile(filepath.Join(gooDir, rs.Source))
		if readErr != nil {
			t.Fatal(readErr)
		}
		if writeErr := os.WriteFile(local, b, 0644); writeErr != nil {
			t.Fatal(writeErr)
		}
		if writeErr := os.WriteFile(targetFile, []byte(ps.String()), 0644); writeErr != nil {
			t.Fatal(writeErr)
		}
		state = append(state, client.PackageState{
			SourceRepo:     srv.URL,
			DownloadURL:    srv.URL + "/" + rs.Source,
			Checksum:       rs.Checksum,
			LocalPath:      local,
			PackageSpec:    rs.PackageSpec,
			InstalledFiles: map[string]string{targetFile: checksumOf(t, targetFile)},
		})
	}
	if err := db.WriteStateToDB(state); err != nil {
		t.Fatal(err)
	}

	replacer := scriptedPkg(t, gooDir, filepath.Join(dstDir, "R"),
		goolib.PkgSpec{Name: "R", Arch: "noarch", Version: "1", Replaces: []string{"P", "Q"}},
		"#!/bin/sh\necho install:R >> \""+orderLog+"\"\nexit 1\n", "")
	rm := client.RepoMap{srv.URL: client.Repo{Packages: []goolib.RepoSpec{replacer}}}

	err = New(cacheDir, db, Options{StrictConflicts: true, OrphanedDependents: OrphanRemove}).FromRepo(t.Context(), goolib.PackageInfo{Name: "R"}, "", rm, corpusArchs, downloader)
	if err == nil || !strings.Contains(err.Error(), "reinstalled the packages that installing R.noarch.1 had removed: P.noarch.1, D.noarch.1, Q.noarch.1, E.noarch.1") {
		t.Fatalf("FromRepo(R) = %v, want script error with P, D, Q, E reinstalled in reverse removal order", err)
	}

	gotBytes, err := os.ReadFile(orderLog)
	if err != nil {
		t.Fatal(err)
	}
	gotOrder := strings.Split(strings.TrimSpace(string(gotBytes)), "\n")
	wantOrder := []string{
		"uninstall:E",
		"uninstall:Q",
		"uninstall:D",
		"uninstall:P",
		"install:R",
		"install:P",
		"install:D",
		"install:Q",
		"install:E",
	}
	if diff := cmp.Diff(wantOrder, gotOrder); diff != "" {
		t.Errorf("script execution sequence (-want +got):\n%s", diff)
	}
	if b, err := os.ReadFile(sharedFile); err != nil || string(b) != "P.noarch.1" {
		t.Errorf("shared file content = %q (%v), want %q", b, err, "P.noarch.1")
	}

	// Also verify file-overlap failure before R's install script runs under StrictConflicts.
	overlapReplacer := scriptedPkg(t, gooDir, sharedFile,
		goolib.PkgSpec{Name: "R2", Arch: "noarch", Version: "1", Replaces: []string{"P", "Q"}},
		"#!/bin/sh\necho install:R2 >> \""+orderLog+"\"\n", "")
	rm2 := client.RepoMap{srv.URL: client.Repo{Packages: []goolib.RepoSpec{overlapReplacer}}}
	err = New(cacheDir, db, Options{StrictConflicts: true, OrphanedDependents: OrphanRemove}).FromRepo(t.Context(), goolib.PackageInfo{Name: "R2"}, "", rm2, corpusArchs, downloader)
	if err == nil || !strings.Contains(err.Error(), "file conflict:") || !strings.Contains(err.Error(), "reinstalled the packages that installing R2.noarch.1 had removed: P.noarch.1, D.noarch.1, Q.noarch.1, E.noarch.1") {
		t.Fatalf("FromRepo(R2) = %v, want StrictConflicts file-conflict error and full recovery", err)
	}
	if b, err := os.ReadFile(sharedFile); err != nil || string(b) != "P.noarch.1" {
		t.Errorf("shared file content after file-conflict recovery = %q (%v), want %q", b, err, "P.noarch.1")
	}
}

// TestRecoveryBoundaryAcrossAllApplyStages checks every pre-install failure
// point against the post-install roll-forward boundary.
func TestRecoveryBoundaryAcrossAllApplyStages(t *testing.T) {
	injected := errors.New("injected stage error")
	tests := []struct {
		name          string
		stage         applyStage
		step          string
		sabotageFetch bool
		wantInstalled []string
	}{
		{
			name:          "before_first_uninstall_changes_nothing",
			stage:         stageBeforeUninstall,
			step:          "D.noarch.1",
			wantInstalled: []string{"D.noarch.1", "P.noarch.1"},
		},
		{
			name:          "before_second_uninstall_recovers_first",
			stage:         stageBeforeUninstall,
			step:          "P.noarch.1",
			wantInstalled: []string{"D.noarch.1", "P.noarch.1"},
		},
		{
			name:          "after_first_uninstall_recovers_first",
			stage:         stageAfterUninstall,
			step:          "D.noarch.1",
			wantInstalled: []string{"D.noarch.1", "P.noarch.1"},
		},
		{
			name:          "after_last_uninstall_recovers_all",
			stage:         stageAfterUninstall,
			step:          "P.noarch.1",
			wantInstalled: []string{"D.noarch.1", "P.noarch.1"},
		},
		{
			name:          "after_replace_recovers_all",
			stage:         stageAfterReplace,
			step:          "R.noarch.1",
			wantInstalled: []string{"D.noarch.1", "P.noarch.1"},
		},
		{
			name:          "replacer_redownload_failure_before_install_recovers_all",
			stage:         stageAfterReplace,
			step:          "R.noarch.1",
			sabotageFetch: true,
			wantInstalled: []string{"D.noarch.1", "P.noarch.1"},
		},
		{
			name:          "after_install_rolls_back_and_recovers_all",
			stage:         stageAfterInstall,
			step:          "R.noarch.1",
			wantInstalled: []string{"D.noarch.1", "P.noarch.1"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := replaceFixture(t, goolib.PkgSpec{})
			before := f.rows(t)
			err := f.runWithHook(t, t.Context(), Options{OrphanedDependents: OrphanRemove}, "R", func(stage applyStage, step string) error {
				if stage != tc.stage || step != tc.step {
					return nil
				}
				if tc.sabotageFetch {
					if err := os.Remove(filepath.Join(f.cache, "R.noarch.1.goo")); err != nil {
						return err
					}
					f.unpublish(t, "R")
					return nil
				}
				return injected
			})
			if err == nil {
				t.Fatal("runWithHook succeeded, want error")
			}
			if !tc.sabotageFetch && !errors.Is(err, injected) {
				t.Errorf("runWithHook error = %v, want errors.Is(injected)", err)
			}
			if diff := cmp.Diff(tc.wantInstalled, f.installed(t)); diff != "" {
				t.Errorf("installed (-want +got):\n%s", diff)
			}
			for _, v := range []string{"P", "D"} {
				if !f.fileExists(v) {
					t.Errorf("%s file missing after recovery", v)
				}
			}
			if f.fileExists("R") {
				t.Errorf("R file present after recovery")
			}
			for _, v := range []string{"P", "D"} {
				if _, statErr := os.Stat(before[v].LocalPath); statErr != nil {
					t.Errorf("%s archive Stat = %v, want kept after recovery", v, statErr)
				}
			}
		})
	}
}

// TestOldClientUnpackDirCommitAndRecoveryLifecycle checks that rows written
// by older clients (LocalPath empty, UnpackDir set) have UnpackDir+".goo"
// deleted at commit, used to reinstall the package when recording the
// replacing package fails, and redownloaded on cancelled-context recovery
// while keeping LocalPath empty.
func TestOldClientUnpackDirCommitAndRecoveryLifecycle(t *testing.T) {
	t.Run("commit_deletes_derived_unpack_dir_archive", func(t *testing.T) {
		f := replaceFixture(t, goolib.PkgSpec{}, goolib.PkgSpec{Name: "P", Arch: "noarch", Version: "1"})
		var derived string
		rewriteRow(t, f, "P", func(ps *client.PackageState) {
			ps.UnpackDir = strings.TrimSuffix(ps.LocalPath, ".goo")
			derived = ps.UnpackDir + ".goo"
			ps.LocalPath = ""
		})
		if err := f.install(t, "R"); err != nil {
			t.Fatalf("install R: %v", err)
		}
		if _, err := os.Stat(derived); !os.IsNotExist(err) {
			t.Errorf("derived archive %s Stat = %v, want deleted at commit", derived, err)
		}
	})

	t.Run("record_failure_recovers_from_derived_archive", func(t *testing.T) {
		f := replaceFixture(t, goolib.PkgSpec{}, goolib.PkgSpec{Name: "P", Arch: "noarch", Version: "1"})
		var derived string
		rewriteRow(t, f, "P", func(ps *client.PackageState) {
			ps.UnpackDir = strings.TrimSuffix(ps.LocalPath, ".goo")
			derived = ps.UnpackDir + ".goo"
			ps.LocalPath = ""
		})
		dbErr := errors.New("db write failed")
		var err error
		out := captureStdout(t, func() {
			err = f.runWithHook(t, t.Context(), Options{}, "R", func(stage applyStage, step string) error {
				if stage == stageAddPkg && step == "R.noarch.1" {
					return dbErr
				}
				return nil
			})
		})
		if !errors.Is(err, dbErr) || !strings.Contains(err.Error(), "reinstalled the packages that installing R.noarch.1 had removed: P.noarch.1") {
			t.Errorf("err = %v, want the write error and P reinstalled", err)
		}
		if strings.Contains(out, "result=unrecorded") {
			t.Errorf("stdout = %q, want no unrecorded replace_record line", out)
		}
		if diff := cmp.Diff([]string{"P.noarch.1"}, f.installed(t)); diff != "" {
			t.Errorf("installed (-want +got):\n%s", diff)
		}
		if _, statErr := os.Stat(derived); statErr != nil {
			t.Errorf("derived archive %s Stat = %v, want kept", derived, statErr)
		}
	})

	t.Run("cancelled_context_redownloads_derived_archive_and_preserves_empty_local_path", func(t *testing.T) {
		f := replaceFixture(t, goolib.PkgSpec{Install: failingScript}, goolib.PkgSpec{Name: "P", Arch: "noarch", Version: "1"})
		var derived string
		rewriteRow(t, f, "P", func(ps *client.PackageState) {
			ps.UnpackDir = strings.TrimSuffix(ps.LocalPath, ".goo")
			derived = ps.UnpackDir + ".goo"
			ps.LocalPath = ""
		})
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		err := f.runWithHook(t, ctx, Options{}, "R", func(stage applyStage, step string) error {
			if stage == stageAfterUninstall && step == "P.noarch.1" {
				cancel()
				return os.Remove(derived)
			}
			return nil
		})
		if err == nil || !strings.Contains(err.Error(), "had removed: P.noarch.1") {
			t.Fatalf("install R = %v, want P recovered", err)
		}
		got := f.rows(t)["P"]
		if got.LocalPath != "" || got.UnpackDir+"."+"goo" != derived {
			t.Errorf("restored row = %+v, want empty LocalPath and preserved UnpackDir", got)
		}
	})
}

// TestPrePhase3StateSchemaCompatibility checks that database rows and state
// arrays written after both recovery and commit decode strictly with no
// unknown JSON fields under the pre-Phase-3 PackageState layout.
func TestPrePhase3StateSchemaCompatibility(t *testing.T) {
	type prePhase3InstalledApp struct {
		Name string
		Reg  string
	}
	type prePhase3PackageState struct {
		SourceRepo     string
		DownloadURL    string
		Checksum       string
		LocalPath      string
		UnpackDir      string
		PackageSpec    *goolib.PkgSpec
		InstalledFiles map[string]string
		InstallDate    int64
		InstalledApp   prePhase3InstalledApp
	}

	checkStrictDecode := func(t *testing.T, f *relFixture) {
		t.Helper()
		rawDB, err := sql.Open("sqlite", settings.DBFile())
		if err != nil {
			t.Fatal(err)
		}
		defer rawDB.Close()

		rows, err := rawDB.Query(`SELECT pkg_json FROM InstalledPackages`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var count int
		for rows.Next() {
			count++
			var raw string
			if err := rows.Scan(&raw); err != nil {
				t.Fatal(err)
			}
			dec := json.NewDecoder(bytes.NewReader([]byte(raw)))
			dec.DisallowUnknownFields()
			var legacy prePhase3PackageState
			if err := dec.Decode(&legacy); err != nil {
				t.Errorf("SQLite row %s failed strict pre-Phase-3 decode: %v", raw, err)
			}
		}
		if count == 0 {
			t.Errorf("expected at least one installed row to decode")
		}

		// Also check legacy googet.state array JSON round-trip.
		state, err := f.db.FetchPkgs("")
		if err != nil {
			t.Fatal(err)
		}
		stateJSON, err := json.Marshal(state)
		if err != nil {
			t.Fatal(err)
		}
		dec := json.NewDecoder(bytes.NewReader(stateJSON))
		dec.DisallowUnknownFields()
		var legacyList []prePhase3PackageState
		if err := dec.Decode(&legacyList); err != nil {
			t.Errorf("googet.state JSON failed strict pre-Phase-3 decode: %v", err)
		}
	}

	t.Run("after_recovery", func(t *testing.T) {
		f := replaceFixture(t, goolib.PkgSpec{Install: failingScript})
		if err := f.installWith(t, Options{OrphanedDependents: OrphanRemove}, "R"); err == nil {
			t.Fatal("install R succeeded, want error")
		}
		checkStrictDecode(t, f)
	})

	t.Run("after_successful_replace_commit", func(t *testing.T) {
		f := replaceFixture(t, goolib.PkgSpec{})
		if err := f.installWith(t, Options{OrphanedDependents: OrphanRemove}, "R"); err != nil {
			t.Fatalf("install R: %v", err)
		}
		checkStrictDecode(t, f)
	})
}

// TestEarlierReplaceCommitPreservesArchiveNeededByLaterStep checks that when
// an earlier step replaces an installed package and a later step in the same
// plan installs that package again from the same cached archive, committing
// the earlier step does not delete the prefetched archive before the later
// step installs from it.
func TestEarlierReplaceCommitPreservesArchiveNeededByLaterStep(t *testing.T) {
	f := newRelFixture(t,
		[]goolib.PkgSpec{{Name: "P", Arch: "noarch", Version: "2"}},
		[]goolib.PkgSpec{
			{Name: "T", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"B": "1", "P": "2"}},
			{Name: "B", Arch: "noarch", Version: "1", Replaces: []string{"P"}},
			{Name: "P", Arch: "noarch", Version: "2"},
		})
	logs := testutil.CaptureLogs(t)
	err := f.runWithHook(t, t.Context(), Options{}, "T", func(stage applyStage, _ string) error {
		if stage == stageFetched {
			// Once fetch has verified every archive, apply must not need the
			// repository again.
			f.unpublish(t, "P")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("install T: %v", err)
	}
	if diff := cmp.Diff([]string{"B.noarch.1", "P.noarch.2", "T.noarch.1"}, f.installed(t)); diff != "" {
		t.Errorf("installed packages (-want +got):\n%s", diff)
	}
	if n := f.getCount("P.noarch.2.goo"); n != 0 {
		t.Errorf("P.noarch.2.goo downloaded %d times, want 0", n)
	}
	pRow := f.rows(t)["P"]
	if got := checksumOf(t, pRow.LocalPath); got != pRow.Checksum {
		t.Errorf("P's recorded archive checksum = %s, want %s", got, pRow.Checksum)
	}
	var reused int
	for _, l := range parseLogs(logs.String()) {
		if strings.HasPrefix(l.text, "using existing file: ") && strings.Contains(l.text, "P.noarch.2.goo") {
			reused++
		}
	}
	if reused != 1 {
		t.Errorf("P.noarch.2.goo logged as reused %d times, want 1", reused)
	}
}

// TestAllRecoveryReinstallsFailingReportsFailuresCleanly checks that when
// every removed package fails to reinstall during recovery, all of them are
// attempted, their kept archives are preserved and named, no false claim of
// reinstalled packages is formatted, and database-only recovery failures omit
// empty archive paths.
func TestAllRecoveryReinstallsFailingReportsFailuresCleanly(t *testing.T) {
	t.Run("both_victim_and_dependent_fail_to_reinstall", func(t *testing.T) {
		f := replaceFixture(t, goolib.PkgSpec{Install: failingScript})
		before := f.rows(t)
		logs := testutil.CaptureLogs(t)
		err := f.runWithHook(t, t.Context(), Options{OrphanedDependents: OrphanRemove}, "R", func(stage applyStage, _ string) error {
			if stage == stageAfterReplace {
				for _, name := range []string{"P", "D"} {
					if writeErr := os.WriteFile(before[name].LocalPath, []byte("corrupt"), 0644); writeErr != nil {
						return writeErr
					}
				}
			}
			return nil
		})
		if err == nil {
			t.Fatal("install R succeeded, want error")
		}
		for _, want := range []string{
			"missing-script.exe",
			fmt.Sprintf("couldn't reinstall P.noarch.1, which installing R.noarch.1 removed (archive %q)", before["P"].LocalPath),
			fmt.Sprintf("couldn't reinstall D.noarch.1, which installing R.noarch.1 removed (archive %q)", before["D"].LocalPath),
		} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("err = %q, want substring %q", err.Error(), want)
			}
		}
		if strings.Contains(err.Error(), "reinstalled the packages") {
			t.Errorf("err = %q, must not claim packages were reinstalled when all failed", err.Error())
		}
		if !strings.Contains(logs.String(), "replace_recovery replacer=R.noarch.1 reinstalled= failed=2") {
			t.Errorf("logs missing failed=2 recovery line; got:\n%s", logs.String())
		}
		for _, name := range []string{"P", "D"} {
			if _, statErr := os.Stat(before[name].LocalPath); statErr != nil {
				t.Errorf("%s archive Stat = %v, want kept", name, statErr)
			}
		}
	})

	t.Run("db_only_recovery_failure_omits_empty_archive_path", func(t *testing.T) {
		f := replaceFixture(t, goolib.PkgSpec{})
		stop := errors.New("stop")
		err := f.runWithHook(t, t.Context(), Options{DBOnly: true, OrphanedDependents: OrphanRemove}, "R", func(stage applyStage, _ string) error {
			if stage == stageAfterReplace {
				if closeErr := f.db.Close(); closeErr != nil {
					return closeErr
				}
				return stop
			}
			return nil
		})
		if err == nil || !errors.Is(err, stop) {
			t.Fatalf("install R = %v, want stop error", err)
		}
		for _, want := range []string{
			"couldn't reinstall P.noarch.1, which installing R.noarch.1 removed:",
			"couldn't reinstall D.noarch.1, which installing R.noarch.1 removed:",
		} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("err = %q, want substring %q", err.Error(), want)
			}
		}
		if strings.Contains(err.Error(), `(archive "")`) || strings.Contains(err.Error(), "reinstalled the packages") {
			t.Errorf("err = %q, want no empty archive path or false reinstalled claim", err.Error())
		}
	})
}
