//go:build linux

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

package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/googet/v2/client"
	"github.com/google/googet/v2/googetdb"
	"github.com/google/googet/v2/goolib"
	"github.com/google/googet/v2/testutil"
)

// googetBinary builds the googet CLI binary into a test-scoped temporary
// directory that is automatically cleaned up when t completes.
func googetBinary(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "googet")
	cmd := exec.Command("go", "build", "-o", bin, ".")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build googet: %v\n%s", err, out)
	}
	return bin
}

// buildSubprocessGoo writes a .goo archive with an optional uninstall script.
func buildSubprocessGoo(t *testing.T, gooDir, dstDir string, ps goolib.PkgSpec, uninstallScript string) goolib.RepoSpec {
	t.Helper()
	ps.Files = map[string]string{ps.Name: filepath.Join(dstDir, ps.Name)}
	if uninstallScript != "" {
		ps.Uninstall = goolib.ExecFile{Path: "uninstall.sh"}
	}
	specJSON, err := json.Marshal(ps)
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
	now := time.Now()

	files := []struct {
		name string
		mode int64
		body []byte
	}{
		{name: ps.Name, mode: 0644, body: []byte(ps.String())},
		{name: ps.Name + ".pkgspec", mode: 0644, body: specJSON},
	}
	if uninstallScript != "" {
		files = append(files, struct {
			name string
			mode int64
			body []byte
		}{name: "uninstall.sh", mode: 0755, body: []byte(uninstallScript)})
	}
	for _, entry := range files {
		if err := tw.WriteHeader(&tar.Header{Name: entry.name, Mode: entry.mode, Size: int64(len(entry.body)), ModTime: now}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(entry.body); err != nil {
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

// TestSubprocessKillAndInterruptDuringReplace verifies that sending a real
// SIGKILL or SIGINT to a running googet subprocess while it uninstalls a
// replaced package leaves the SQLite database consistent, prints the
// replace_record line with valid kept archive paths, and permits clean
// recovery either by rerunning "googet install R" or by manually installing
// the kept archives named on the replace_record line.
func TestSubprocessKillAndInterruptDuringReplace(t *testing.T) {
	bin := googetBinary(t)

	for _, sigCase := range []struct {
		name     string
		sig      syscall.Signal
		recovery string
	}{
		{name: "sigkill_then_rerun_replacer", sig: syscall.SIGKILL, recovery: "rerun"},
		{name: "sigkill_then_manual_archive_recovery", sig: syscall.SIGKILL, recovery: "manual"},
		{name: "sigint_then_manual_archive_recovery", sig: syscall.SIGINT, recovery: "manual"},
	} {
		t.Run(sigCase.name, func(t *testing.T) {
			rootDir := t.TempDir()
			dstDir := t.TempDir()
			gooDir := t.TempDir()
			ctrlDir := t.TempDir()
			readyFile := filepath.Join(ctrlDir, "in_uninstall")
			releaseFile := filepath.Join(ctrlDir, "release_uninstall")
			t.Cleanup(func() { _ = os.WriteFile(releaseFile, []byte("done"), 0644) })

			for _, sub := range []string{"cache", "repos"} {
				if err := os.MkdirAll(filepath.Join(rootDir, sub), 0755); err != nil {
					t.Fatal(err)
				}
			}
			conf := "allowunsafeurl: true\narchs: [noarch, x86_64]\n"
			if err := os.WriteFile(filepath.Join(rootDir, "googet.conf"), []byte(conf), 0644); err != nil {
				t.Fatal(err)
			}

			srv := testutil.ServeGoo(t, gooDir)
			t.Cleanup(srv.Close)
			repoURL := srv.URL + "/repo"

			// D uninstalls immediately; P signals readyFile and waits until
			// releaseFile appears, allowing the test to deliver SIGKILL or SIGINT
			// deterministically after D is removed and while P is mid-uninstall.
			slowUninstall := fmt.Sprintf("#!/bin/sh\nif [ ! -f %q ]; then\n  touch %q\n  i=0\n  while [ ! -f %q ] && [ $i -lt 400 ]; do\n    sleep 0.02\n    i=$((i+1))\n  done\nfi\n", releaseFile, readyFile, releaseFile)

			pRS := buildSubprocessGoo(t, gooDir, dstDir, goolib.PkgSpec{Name: "P", Arch: "noarch", Version: "1"}, slowUninstall)
			dRS := buildSubprocessGoo(t, gooDir, dstDir, goolib.PkgSpec{Name: "D", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"P": "1"}}, "")
			rRS := buildSubprocessGoo(t, gooDir, dstDir, goolib.PkgSpec{Name: "R", Arch: "noarch", Version: "1", Replaces: []string{"P"}}, "")

			dbPath := filepath.Join(rootDir, "googet.db")
			db, err := googetdb.NewDB(dbPath)
			if err != nil {
				t.Fatal(err)
			}
			var seedState client.GooGetState
			for _, rs := range []goolib.RepoSpec{pRS, dRS} {
				localArchive := filepath.Join(rootDir, "cache", rs.Source)
				raw, readErr := os.ReadFile(filepath.Join(gooDir, rs.Source))
				if readErr != nil {
					t.Fatal(readErr)
				}
				if writeErr := os.WriteFile(localArchive, raw, 0644); writeErr != nil {
					t.Fatal(writeErr)
				}
				payload := filepath.Join(dstDir, rs.PackageSpec.Name)
				if writeErr := os.WriteFile(payload, []byte(rs.PackageSpec.String()), 0644); writeErr != nil {
					t.Fatal(writeErr)
				}
				f, openErr := os.Open(payload)
				if openErr != nil {
					t.Fatal(openErr)
				}
				sum := goolib.Checksum(f)
				f.Close()
				seedState = append(seedState, client.PackageState{
					SourceRepo:     repoURL,
					DownloadURL:    srv.URL + "/" + rs.Source,
					Checksum:       rs.Checksum,
					LocalPath:      localArchive,
					PackageSpec:    rs.PackageSpec,
					InstalledFiles: map[string]string{payload: sum},
				})
			}
			if err := db.WriteStateToDB(seedState); err != nil {
				t.Fatal(err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}

			if err := os.MkdirAll(filepath.Join(gooDir, "repo"), 0755); err != nil {
				t.Fatal(err)
			}
			idxBytes, err := json.Marshal([]goolib.RepoSpec{rRS})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(gooDir, "repo", "index"), idxBytes, 0644); err != nil {
				t.Fatal(err)
			}

			var stdout, stderr bytes.Buffer
			cmd := exec.Command(bin, "-root="+rootDir, "-noconfirm", "-system_log=false", "-progress=false", "install", "-sources="+repoURL, "R")
			cmd.Stdout = &stdout
			cmd.Stderr = &stderr
			if err := cmd.Start(); err != nil {
				t.Fatalf("Start googet: %v", err)
			}

			deadline := time.Now().Add(10 * time.Second)
			for {
				if _, err := os.Stat(readyFile); err == nil {
					break
				}
				if time.Now().After(deadline) {
					_ = cmd.Process.Kill()
					t.Fatalf("timed out waiting for victim uninstall script; stdout=%q stderr=%q", stdout.String(), stderr.String())
				}
				time.Sleep(10 * time.Millisecond)
			}

			if err := cmd.Process.Signal(sigCase.sig); err != nil {
				t.Fatalf("Signal(%v): %v", sigCase.sig, err)
			}
			waitErr := cmd.Wait()
			// Release the child shell loop immediately so no background script lingers.
			if err := os.WriteFile(releaseFile, []byte("done"), 0644); err != nil {
				t.Fatal(err)
			}
			if waitErr == nil {
				t.Fatalf("expected non-zero exit on %v, got nil", sigCase.sig)
			}

			dArchive := filepath.Join(rootDir, "cache", "D.noarch.1.goo")
			pArchive := filepath.Join(rootDir, "cache", "P.noarch.1.goo")
			wantRecord := "replace_record replacer=R.noarch.1 rows=D.noarch.1,P.noarch.1 archives=" + dArchive + "," + pArchive
			if !strings.Contains(stdout.String(), wantRecord) {
				t.Fatalf("stdout = %q, want %q", stdout.String(), wantRecord)
			}
			logContent, err := os.ReadFile(filepath.Join(rootDir, "googet.log"))
			if err != nil {
				t.Fatalf("ReadFile(googet.log): %v", err)
			}
			if !strings.Contains(string(logContent), wantRecord) {
				t.Errorf("googet.log missing %q; got:\n%s", wantRecord, logContent)
			}

			// Verify SQLite integrity and consistent row state (D gone, P still present).
			rawSQL, err := sql.Open("sqlite", dbPath)
			if err != nil {
				t.Fatal(err)
			}
			var integrity string
			if err := rawSQL.QueryRow(`PRAGMA integrity_check`).Scan(&integrity); err != nil || integrity != "ok" {
				rawSQL.Close()
				t.Fatalf("SQLite integrity_check = %q (%v), want ok", integrity, err)
			}
			rawSQL.Close()

			checkDB, err := googetdb.NewDB(dbPath)
			if err != nil {
				t.Fatal(err)
			}
			afterCrash, err := checkDB.FetchPkgs("")
			if err != nil {
				t.Fatal(err)
			}
			checkDB.Close()
			var crashNames []string
			for _, st := range afterCrash {
				crashNames = append(crashNames, st.PackageSpec.String())
			}
			if diff := cmp.Diff([]string{"P.noarch.1"}, crashNames); diff != "" {
				t.Errorf("installed rows after kill (-want +got):\n%s", diff)
			}

			// Both kept archives exist intact on disk.
			for _, archive := range []string{dArchive, pArchive} {
				if fi, statErr := os.Stat(archive); statErr != nil || fi.Size() == 0 {
					t.Fatalf("kept archive %s Stat = (%v, %v), want non-empty file", archive, fi, statErr)
				}
			}

			if sigCase.recovery == "rerun" {
				rerun := exec.Command(bin, "-root="+rootDir, "-noconfirm", "-system_log=false", "-progress=false", "install", "-sources="+repoURL, "R")
				if out, err := rerun.CombinedOutput(); err != nil {
					t.Fatalf("rerun install R failed: %v\n%s", err, out)
				}
				finalDB, err := googetdb.NewDB(dbPath)
				if err != nil {
					t.Fatal(err)
				}
				finalState, err := finalDB.FetchPkgs("")
				finalDB.Close()
				if err != nil {
					t.Fatal(err)
				}
				var gotFinal []string
				for _, st := range finalState {
					gotFinal = append(gotFinal, st.PackageSpec.String())
				}
				if diff := cmp.Diff([]string{"R.noarch.1"}, gotFinal); diff != "" {
					t.Errorf("installed after rerun (-want +got):\n%s", diff)
				}
			} else {
				// Parse the archive paths directly from the printed replace_record line
				// and manually reinstall the already-removed package D.
				var recordArchives []string
				for _, line := range strings.Split(stdout.String(), "\n") {
					if _, rest, ok := strings.Cut(line, "archives="); ok {
						recordArchives = strings.Split(strings.TrimSpace(rest), ",")
						break
					}
				}
				if len(recordArchives) != 2 {
					t.Fatalf("parsed archives = %v, want 2 paths", recordArchives)
				}
				manual := exec.Command(bin, "-root="+rootDir, "-noconfirm", "-system_log=false", "-progress=false", "install", recordArchives[0])
				if out, err := manual.CombinedOutput(); err != nil {
					t.Fatalf("manual install from kept archive %s failed: %v\n%s", recordArchives[0], err, out)
				}
				finalDB, err := googetdb.NewDB(dbPath)
				if err != nil {
					t.Fatal(err)
				}
				finalState, err := finalDB.FetchPkgs("")
				finalDB.Close()
				if err != nil {
					t.Fatal(err)
				}
				var gotFinal []string
				for _, st := range finalState {
					gotFinal = append(gotFinal, st.PackageSpec.String())
				}
				slices.Sort(gotFinal)
				if diff := cmp.Diff([]string{"D.noarch.1", "P.noarch.1"}, gotFinal); diff != "" {
					t.Errorf("installed after manual archive recovery (-want +got):\n%s", diff)
				}
			}
		})
	}
}
