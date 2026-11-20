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

package update

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/googet/v2/client"
	"github.com/google/googet/v2/googetdb"
	"github.com/google/googet/v2/goolib"
	"github.com/google/googet/v2/settings"
	"github.com/google/googet/v2/testutil"
	"github.com/google/subcommands"
)

// setupGoopackCLIRepo initializes a temporary GooGet root, seeds initialState
// on disk and in SQLite, and serves goopack-built repoPkgs from an httptest
// /repo/index server.
func setupGoopackCLIRepo(t *testing.T, goopackBin, confYAML string, initialState, repoPkgs []goolib.PkgSpec) (dstDir, repoURL string) {
	t.Helper()
	rootDir := t.TempDir()
	dstDir = t.TempDir()
	gooDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(rootDir, "cache"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(rootDir, "repos"), 0755); err != nil {
		t.Fatal(err)
	}

	t.Cleanup(settings.Snapshot())
	fullConf := "allowunsafeurl: true\narchs: [noarch, x86_64]\n" + confYAML
	if err := os.WriteFile(filepath.Join(rootDir, "googet.conf"), []byte(fullConf), 0644); err != nil {
		t.Fatal(err)
	}
	settings.Initialize(rootDir, false)

	srv := testutil.ServeGoo(t, gooDir)
	t.Cleanup(srv.Close)
	repoURL = srv.URL + "/repo"

	db, err := googetdb.NewDB(settings.DBFile())
	if err != nil {
		t.Fatal(err)
	}
	var state client.GooGetState
	for _, ps := range initialState {
		rs := testutil.GenGooWithGoopack(t, goopackBin, gooDir, dstDir, ps)
		local := filepath.Join(rootDir, "cache", rs.Source)
		b, err := os.ReadFile(filepath.Join(gooDir, rs.Source))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(local, b, 0644); err != nil {
			t.Fatal(err)
		}
		file := filepath.Join(dstDir, ps.Name)
		if err := os.WriteFile(file, []byte(rs.PackageSpec.String()), 0644); err != nil {
			t.Fatal(err)
		}
		fh, err := os.Open(file)
		if err != nil {
			t.Fatal(err)
		}
		sum := goolib.Checksum(fh)
		fh.Close()
		state = append(state, client.PackageState{
			SourceRepo:     repoURL,
			DownloadURL:    srv.URL + "/" + rs.Source,
			Checksum:       rs.Checksum,
			LocalPath:      local,
			PackageSpec:    rs.PackageSpec,
			InstalledFiles: map[string]string{file: sum},
		})
	}
	if err := db.WriteStateToDB(state); err != nil {
		t.Fatal(err)
	}
	db.Close()

	var specs []goolib.RepoSpec
	for _, ps := range repoPkgs {
		specs = append(specs, testutil.GenGooWithGoopack(t, goopackBin, gooDir, dstDir, ps))
	}
	if err := os.MkdirAll(filepath.Join(gooDir, "repo"), 0755); err != nil {
		t.Fatal(err)
	}
	idx, err := json.Marshal(specs)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gooDir, "repo", "index"), idx, 0644); err != nil {
		t.Fatal(err)
	}
	return dstDir, repoURL
}

// TestEndToEndUpdateFollowReplacesWithGoopack verifies googet update
// -follow_replaces end to end against goopack-built archives when installed P
// is in range, when installed P is already out of range, and when a newer P
// outside the Replaces bound is offered.
func TestEndToEndUpdateFollowReplacesWithGoopack(t *testing.T) {
	goopackBin := testutil.BuildGoopack(t)
	replacer := goolib.PkgSpec{
		Name:      "R",
		Arch:      "noarch",
		Version:   "2.0.0",
		Conflicts: []string{"P<2.0.0"},
		Replaces:  []string{"P<2.0.0"},
		Provides:  []string{"P=2.0.0"},
	}

	t.Run("in_range_replaced_and_second_run_schedules_nothing", func(t *testing.T) {
		installed := []goolib.PkgSpec{{Name: "P", Arch: "noarch", Version: "1.0.0"}}
		repo := []goolib.PkgSpec{
			{Name: "P", Arch: "noarch", Version: "1.0.0"},
			replacer,
		}
		dst, repoURL := setupGoopackCLIRepo(t, goopackBin, "", installed, repo)

		status, out := runUpdate(t, "-sources="+repoURL, "-follow_replaces")
		if status != subcommands.ExitSuccess {
			t.Fatalf("update -follow_replaces = %v; output:\n%s", status, out)
		}
		if !strings.Contains(out, "P.noarch, 1.0.0 --> R.noarch, 2.0.0") {
			t.Errorf("output missing replacement line:\n%s", out)
		}
		if diff := cmp.Diff([]string{"R.noarch.2.0.0"}, testutil.InstalledNames(t)); diff != "" {
			t.Errorf("installed after first run (-want +got):\n%s", diff)
		}
		if fileExists(dst, "P") || !fileExists(dst, "R") {
			t.Errorf("files on disk: P=%t R=%t, want P=false R=true", fileExists(dst, "P"), fileExists(dst, "R"))
		}

		status, out = runUpdate(t, "-sources="+repoURL, "-follow_replaces")
		if status != subcommands.ExitSuccess || !strings.Contains(out, "No updates available for any installed packages.") {
			t.Errorf("second update -follow_replaces = %v, %q; want success with no updates", status, out)
		}
		if diff := cmp.Diff([]string{"R.noarch.2.0.0"}, testutil.InstalledNames(t)); diff != "" {
			t.Errorf("installed after second run (-want +got):\n%s", diff)
		}
	})

	t.Run("out_of_range_installed_takes_normal_upgrade_with_no_skip", func(t *testing.T) {
		logs := testutil.CaptureLogs(t)
		installed := []goolib.PkgSpec{{Name: "P", Arch: "noarch", Version: "2.5.0"}}
		repo := []goolib.PkgSpec{
			{Name: "P", Arch: "noarch", Version: "3.0.0"},
			replacer,
		}
		dst, repoURL := setupGoopackCLIRepo(t, goopackBin, "", installed, repo)

		status, out := runUpdate(t, "-sources="+repoURL, "-follow_replaces")
		if status != subcommands.ExitSuccess {
			t.Fatalf("update -follow_replaces = %v; output:\n%s", status, out)
		}
		if strings.Contains(out, "successor_out_of_range") || strings.Contains(logs.String(), "update_replace_skipped") {
			t.Errorf("unexpected skip output or log for out-of-range installed P:\nout:\n%s\nlogs:\n%s", out, logs.String())
		}
		if diff := cmp.Diff([]string{"P.noarch.3.0.0"}, testutil.InstalledNames(t)); diff != "" {
			t.Errorf("installed (-want +got):\n%s", diff)
		}
		if !fileExists(dst, "P") || fileExists(dst, "R") {
			t.Errorf("files on disk: P=%t R=%t, want P=true R=false", fileExists(dst, "P"), fileExists(dst, "R"))
		}
	})

	t.Run("out_of_range_successor_skips_in_dry_run_and_upgrades_in_place_live", func(t *testing.T) {
		installed := []goolib.PkgSpec{{Name: "P", Arch: "noarch", Version: "1.0.0"}}
		repo := []goolib.PkgSpec{
			{Name: "P", Arch: "noarch", Version: "2.0.0"},
			replacer,
		}
		dst, repoURL := setupGoopackCLIRepo(t, goopackBin, "", installed, repo)

		dryStatus, dryOut := runUpdate(t, "-sources="+repoURL, "-dry_run", "-follow_replaces")
		if dryStatus != subcommands.ExitSuccess {
			t.Fatalf("dry-run update = %v; output:\n%s", dryStatus, dryOut)
		}
		if !strings.Contains(dryOut, "successor_out_of_range") || !strings.Contains(dryOut, dryRunLine) {
			t.Errorf("dry-run output missing successor_out_of_range or dry-run trailer:\n%s", dryOut)
		}
		if diff := cmp.Diff([]string{"P.noarch.1.0.0"}, testutil.InstalledNames(t)); diff != "" {
			t.Fatalf("dry-run mutated installed state (-want +got):\n%s", diff)
		}

		logs := testutil.CaptureLogs(t)
		liveStatus, liveOut := runUpdate(t, "-sources="+repoURL, "-follow_replaces")
		if liveStatus != subcommands.ExitSuccess {
			t.Fatalf("live update = %v; output:\n%s", liveStatus, liveOut)
		}
		if !strings.Contains(liveOut, "successor_out_of_range") || !strings.Contains(logs.String(), "successor_out_of_range") {
			t.Errorf("live output/logs missing successor_out_of_range:\nout:\n%s\nlogs:\n%s", liveOut, logs.String())
		}
		if diff := cmp.Diff([]string{"P.noarch.2.0.0"}, testutil.InstalledNames(t)); diff != "" {
			t.Errorf("installed after live update (-want +got):\n%s", diff)
		}
		if !fileExists(dst, "P") || fileExists(dst, "R") {
			t.Errorf("files on disk: P=%t R=%t, want P=true R=false", fileExists(dst, "P"), fileExists(dst, "R"))
		}
	})
}

// TestEndToEndUpdateOldClientCoexistenceWithGoopack checks googet update on a
// state where a 3.4.x client left R (Conflicts/Replaces P<2.0.0) and P 1.0.0
// installed side by side: default update with R at its current version leaves P
// intact while updating unrelated packages, -follow_replaces suppresses in-range
// P when R is current (unless an out-of-range P 2.0.0 successor is offered), and
// -follow_replaces removes P when R itself updates.
func TestEndToEndUpdateOldClientCoexistenceWithGoopack(t *testing.T) {
	goopackBin := testutil.BuildGoopack(t)
	coexistingP := goolib.PkgSpec{Name: "P", Arch: "noarch", Version: "1.0.0"}
	coexistingR := goolib.PkgSpec{
		Name:      "R",
		Arch:      "noarch",
		Version:   "2.0.0",
		Conflicts: []string{"P<2.0.0"},
		Replaces:  []string{"P<2.0.0"},
		Provides:  []string{"P=2.0.0"},
	}
	unrelatedU1 := goolib.PkgSpec{Name: "U", Arch: "noarch", Version: "1.0.0"}
	unrelatedU2 := goolib.PkgSpec{Name: "U", Arch: "noarch", Version: "2.0.0"}
	successorP := goolib.PkgSpec{Name: "P", Arch: "noarch", Version: "2.0.0"}
	updatedR := goolib.PkgSpec{
		Name:      "R",
		Arch:      "noarch",
		Version:   "2.1.0",
		Conflicts: []string{"P<2.0.0"},
		Replaces:  []string{"P<2.0.0"},
		Provides:  []string{"P=2.1.0"},
	}

	t.Run("default_update_with_current_replacer_keeps_coexisting_victim_intact", func(t *testing.T) {
		dst, repoURL := setupGoopackCLIRepo(t, goopackBin, "",
			[]goolib.PkgSpec{coexistingP, coexistingR, unrelatedU1},
			[]goolib.PkgSpec{coexistingP, coexistingR, unrelatedU2},
		)
		status, out := runUpdate(t, "-sources="+repoURL)
		if status != subcommands.ExitSuccess {
			t.Fatalf("default update = %v; output:\n%s", status, out)
		}
		if diff := cmp.Diff([]string{"P.noarch.1.0.0", "R.noarch.2.0.0", "U.noarch.2.0.0"}, testutil.InstalledNames(t)); diff != "" {
			t.Errorf("installed after default update (-want +got):\n%s", diff)
		}
		if !fileExists(dst, "P") || !fileExists(dst, "R") || !fileExists(dst, "U") {
			t.Errorf("files on disk: P=%t R=%t U=%t, want all true", fileExists(dst, "P"), fileExists(dst, "R"), fileExists(dst, "U"))
		}
	})

	t.Run("follow_replaces_with_current_replacer_and_out_of_range_successor_upgrades_victim", func(t *testing.T) {
		logs := testutil.CaptureLogs(t)
		dst, repoURL := setupGoopackCLIRepo(t, goopackBin, "",
			[]goolib.PkgSpec{coexistingP, coexistingR},
			[]goolib.PkgSpec{successorP, coexistingR},
		)
		status, out := runUpdate(t, "-sources="+repoURL, "-follow_replaces")
		if status != subcommands.ExitSuccess {
			t.Fatalf("update -follow_replaces = %v; output:\n%s", status, out)
		}
		if !strings.Contains(out, "successor_out_of_range") || strings.Contains(logs.String(), "update_replace_coexisting_current") {
			t.Errorf("expected successor_out_of_range without coexisting suppression:\nout:\n%s\nlogs:\n%s", out, logs.String())
		}
		if diff := cmp.Diff([]string{"P.noarch.2.0.0", "R.noarch.2.0.0"}, testutil.InstalledNames(t)); diff != "" {
			t.Errorf("installed (-want +got):\n%s", diff)
		}
		if !fileExists(dst, "P") || !fileExists(dst, "R") {
			t.Errorf("files on disk: P=%t R=%t, want both true", fileExists(dst, "P"), fileExists(dst, "R"))
		}
	})

	t.Run("follow_replaces_removes_coexisting_victim_when_replacer_updates", func(t *testing.T) {
		dst, repoURL := setupGoopackCLIRepo(t, goopackBin, "",
			[]goolib.PkgSpec{coexistingP, coexistingR},
			[]goolib.PkgSpec{coexistingP, updatedR},
		)
		status, out := runUpdate(t, "-sources="+repoURL, "-follow_replaces")
		if status != subcommands.ExitSuccess {
			t.Fatalf("update -follow_replaces = %v; output:\n%s", status, out)
		}
		if diff := cmp.Diff([]string{"R.noarch.2.1.0"}, testutil.InstalledNames(t)); diff != "" {
			t.Errorf("installed after update -follow_replaces (-want +got):\n%s", diff)
		}
		if fileExists(dst, "P") || !fileExists(dst, "R") {
			t.Errorf("files on disk: P=%t R=%t, want P=false R=true", fileExists(dst, "P"), fileExists(dst, "R"))
		}
	})
}
