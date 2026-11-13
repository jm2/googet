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
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/googet/v2/cli/internal/policy"
	"github.com/google/googet/v2/client"
	"github.com/google/googet/v2/googetdb"
	"github.com/google/googet/v2/goolib"
	"github.com/google/googet/v2/install"
	"github.com/google/googet/v2/settings"
	"github.com/google/googet/v2/testutil"
	"github.com/google/subcommands"
)

// withStdin redirects os.Stdin to read input for the duration of fn.
func withStdin(t *testing.T, input string, fn func()) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.WriteString(input); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	old := os.Stdin
	os.Stdin = r
	defer func() {
		os.Stdin = old
		r.Close()
	}()
	fn()
}

// TestInstallConfirmationPrompt checks that interactive installs render the
// full plan (ordered installs with version changes, removals with reasons, and
// conflict warnings) in the confirmation prompt, leave everything untouched
// with zero archive downloads when declined, and apply the plan when accepted.
func TestInstallConfirmationPrompt(t *testing.T) {
	runCase := func(t *testing.T, answer string) (out string, err error, gets int32, names []string) {
		t.Helper()
		t.Cleanup(settings.Snapshot())
		rootDir := t.TempDir()
		if err := os.WriteFile(filepath.Join(rootDir, "googet.conf"), []byte("orphaneddependents: remove\n"), 0644); err != nil {
			t.Fatal(err)
		}
		settings.Initialize(rootDir, true)

		db, err := googetdb.NewDB(settings.DBFile())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { db.Close() })

		dstDir := t.TempDir()
		gooDir := t.TempDir()
		cacheDir := t.TempDir()

		var archiveGets atomic.Int32
		baseHandler := testutil.GooHandler(t, gooDir)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, ".goo") {
				archiveGets.Add(1)
			}
			baseHandler.ServeHTTP(w, r)
		}))
		t.Cleanup(srv.Close)

		seedInstalled := func(ps goolib.PkgSpec) client.PackageState {
			rs := testutil.GenGoo(t, gooDir, dstDir, ps)
			local := filepath.Join(cacheDir, rs.Source)
			b, readErr := os.ReadFile(filepath.Join(gooDir, rs.Source))
			if readErr != nil {
				t.Fatal(readErr)
			}
			if writeErr := os.WriteFile(local, b, 0644); writeErr != nil {
				t.Fatal(writeErr)
			}
			installedFile := filepath.Join(dstDir, ps.Name)
			if writeErr := os.WriteFile(installedFile, []byte(rs.PackageSpec.String()), 0644); writeErr != nil {
				t.Fatal(writeErr)
			}
			f, openErr := os.Open(installedFile)
			if openErr != nil {
				t.Fatal(openErr)
			}
			sum := goolib.Checksum(f)
			f.Close()
			return client.PackageState{
				SourceRepo:     srv.URL,
				DownloadURL:    srv.URL + "/" + rs.Source,
				Checksum:       rs.Checksum,
				LocalPath:      local,
				PackageSpec:    rs.PackageSpec,
				InstalledFiles: map[string]string{installedFile: sum},
			}
		}

		initial := client.GooGetState{
			seedInstalled(goolib.PkgSpec{Name: "A", Arch: "noarch", Version: "1"}),
			seedInstalled(goolib.PkgSpec{Name: "P", Arch: "noarch", Version: "1"}),
			seedInstalled(goolib.PkgSpec{Name: "D", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"P": "1"}}),
			seedInstalled(goolib.PkgSpec{Name: "X", Arch: "noarch", Version: "1", Conflicts: []string{"B"}}),
		}
		if err := db.WriteStateToDB(initial); err != nil {
			t.Fatal(err)
		}

		var available []goolib.RepoSpec
		for _, ps := range []goolib.PkgSpec{
			{Name: "A", Arch: "noarch", Version: "2", PkgDependencies: map[string]string{"B": "1"}, Replaces: []string{"P"}},
			{Name: "B", Arch: "noarch", Version: "1"},
		} {
			available = append(available, testutil.GenGoo(t, gooDir, dstDir, ps))
		}

		downloader, err := client.NewDownloader("")
		if err != nil {
			t.Fatal(err)
		}
		var flags policy.Flags
		ins := &installer{
			db:           db,
			cache:        cacheDir,
			downloader:   downloader,
			repoMap:      client.RepoMap{srv.URL: client.Repo{Packages: available}},
			pkgInstaller: install.New(cacheDir, db, flags.Options(false, false)),
			confirm:      true,
		}

		withStdin(t, answer, func() {
			out = captureStdout(func() {
				err = ins.installFromRepo(context.Background(), "A", []string{"noarch"})
			})
		})
		return out, err, archiveGets.Load(), testutil.InstalledNames(t)
	}

	wantPrompt := strings.Join([]string{
		"The following packages will be installed:",
		"  B.noarch.1",
		"  A.noarch.2 (upgrade from 1)",
		"The following packages will be removed:",
		"  D.noarch.1 (depends on a package A.noarch.2 replaces)",
		"  P.noarch.1 (replaced by A.noarch.2)",
		`Warning: installed package X.noarch.1 conflicts with B.noarch.1 (entry "B"); installing anyway (package_conflicts=warn)`,
		"Do you wish to install A.noarch.2 and all dependencies? (y/N): ",
	}, "\n")

	t.Run("declined_prompt_changes_nothing", func(t *testing.T) {
		out, err, gets, names := runCase(t, "n\n")
		if err != nil {
			t.Fatalf("installFromRepo(decline) unexpected error: %v", err)
		}
		if !strings.Contains(out, wantPrompt) || !strings.Contains(out, "canceling install...") {
			t.Errorf("prompt output = %q, want %q and cancellation message", out, wantPrompt)
		}
		if gets != 0 {
			t.Errorf("archive downloads on declined prompt = %d, want 0", gets)
		}
		if diff := cmp.Diff([]string{"A.noarch.1", "D.noarch.1", "P.noarch.1", "X.noarch.1"}, names); diff != "" {
			t.Errorf("installed packages after decline (-want +got):\n%s", diff)
		}
	})

	t.Run("accepted_prompt_executes_plan", func(t *testing.T) {
		out, err, gets, names := runCase(t, "yes\n")
		if err != nil {
			t.Fatalf("installFromRepo(accept) unexpected error: %v", err)
		}
		if !strings.Contains(out, wantPrompt) {
			t.Errorf("prompt output = %q, want %q", out, wantPrompt)
		}
		if gets != 2 {
			t.Errorf("archive downloads on accepted prompt = %d, want 2", gets)
		}
		if diff := cmp.Diff([]string{"A.noarch.2", "B.noarch.1", "X.noarch.1"}, names); diff != "" {
			t.Errorf("installed packages after accept (-want +got):\n%s", diff)
		}
	})
}

// TestInstallConfirmationFailsBeforePromptingOnInvalidPlan checks that an
// interactive install whose plan fails returns the error before asking for
// confirmation.
func TestInstallConfirmationFailsBeforePromptingOnInvalidPlan(t *testing.T) {
	t.Cleanup(settings.Snapshot())
	settings.Initialize(t.TempDir(), true)
	db, err := googetdb.NewDB(settings.DBFile())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.WriteStateToDB(client.GooGetState{
		{PackageSpec: &goolib.PkgSpec{Name: "C", Arch: "noarch", Version: "1"}},
	}); err != nil {
		t.Fatal(err)
	}
	downloader, err := client.NewDownloader("")
	if err != nil {
		t.Fatal(err)
	}
	gooDir := t.TempDir()
	srv := testutil.ServeGoo(t, gooDir)
	t.Cleanup(srv.Close)
	rs := testutil.GenGoo(t, gooDir, t.TempDir(), goolib.PkgSpec{Name: "A", Arch: "noarch", Version: "1", Conflicts: []string{"C"}})
	var flags policy.Flags
	ins := &installer{
		db:           db,
		cache:        t.TempDir(),
		downloader:   downloader,
		repoMap:      client.RepoMap{srv.URL: client.Repo{Packages: []goolib.RepoSpec{rs}}},
		pkgInstaller: install.New(t.TempDir(), db, flags.Options(false, false)),
		confirm:      true,
	}

	var runErr error
	var out string
	withStdin(t, "y\n", func() {
		out = captureStdout(func() {
			runErr = ins.installFromRepo(context.Background(), "A", []string{"noarch"})
		})
	})
	if runErr == nil || !strings.Contains(runErr.Error(), "conflict with installed package or provider: C") {
		t.Fatalf("installFromRepo = %v, want conflict error", runErr)
	}
	if strings.Contains(out, "Do you wish to install") {
		t.Errorf("stdout = %q, want no confirmation prompt when planning fails", out)
	}
}

// TestDryRunMultiPackagePartialFailure checks that a dry run with multiple
// arguments exits with failure when any argument fails planning, while still
// changing nothing in the database.
func TestDryRunMultiPackagePartialFailure(t *testing.T) {
	_, repoURL := testutil.SetupCLIRepo(t, "",
		[]goolib.PkgSpec{{Name: "C", Arch: "noarch", Version: "1"}},
		[]goolib.PkgSpec{
			{Name: "Good", Arch: "noarch", Version: "1"},
			{Name: "Bad", Arch: "noarch", Version: "1", Conflicts: []string{"C"}},
		})
	status, err := testutil.RunCmd(t, &installCmd{}, "-sources="+repoURL, "-dry_run", "Good", "Bad")
	if err != nil || status != subcommands.ExitFailure {
		t.Errorf("Execute(Good, Bad) = (%v, %v), want (ExitFailure, nil)", status, err)
	}
	if diff := cmp.Diff([]string{"C.noarch.1"}, testutil.InstalledNames(t)); diff != "" {
		t.Errorf("installed (-want +got):\n%s", diff)
	}
}

// TestUnattendedInstallLogsStructuredConflictAndPlansOnce checks that a
// non-interactive (-noconfirm) repository install executes FromRepo directly
// rather than running a silent pre-plan first, preserving structured
// package_conflict failure logs and planning only once.
func TestUnattendedInstallLogsStructuredConflictAndPlansOnce(t *testing.T) {
	t.Run("structured_reverse_conflict_failure_lines_logged", func(t *testing.T) {
		logs := testutil.CaptureLogs(t)
		_, repoURL := testutil.SetupCLIRepo(t, "",
			[]goolib.PkgSpec{
				{Name: "A", Arch: "noarch", Version: "1", Conflicts: []string{"B"}},
				{Name: "C", Arch: "noarch", Version: "1", Conflicts: []string{"B"}},
			},
			[]goolib.PkgSpec{{Name: "B", Arch: "noarch", Version: "1"}})

		status, err := testutil.RunCmd(t, &installCmd{}, "-sources="+repoURL, "-package_conflicts=error", "B")
		if err != nil || status != subcommands.ExitFailure {
			t.Fatalf("Execute = (%v, %v), want (ExitFailure, nil)", status, err)
		}
		for _, want := range []string{
			`package_conflict direction=reverse mode=error result=failed installed=A.noarch.1 entry="B" incoming=B.noarch.1`,
			`package_conflict direction=reverse mode=error result=failed installed=C.noarch.1 entry="B" incoming=B.noarch.1`,
			"pass -package_conflicts=warn to install anyway",
		} {
			if !strings.Contains(logs.String(), want) {
				t.Errorf("logs missing %q; got:\n%s", want, logs.String())
			}
		}
	})

	t.Run("unattended_success_plans_once", func(t *testing.T) {
		logs := testutil.CaptureLogs(t)
		_, repoURL := testutil.SetupCLIRepo(t, "",
			[]goolib.PkgSpec{{Name: "X", Arch: "noarch", Version: "1", Conflicts: []string{"B"}}},
			[]goolib.PkgSpec{
				{Name: "A", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"B": "1"}},
				{Name: "B", Arch: "noarch", Version: "1"},
				// Unparseable sibling version logs an error each time FindRepoLatest scans B.
				{Name: "B", Arch: "noarch", Version: "not-a-semver"},
			})

		status, err := testutil.RunCmd(t, &installCmd{}, "-sources="+repoURL, "A")
		if err != nil || status != subcommands.ExitSuccess {
			t.Fatalf("Execute = (%v, %v), want (ExitSuccess, nil)", status, err)
		}
		const badVerMsg = "Error comparing versions not-a-semver vs 1"
		if got := strings.Count(logs.String(), badVerMsg); got != 1 {
			t.Errorf("unattended install logged %q %d times, want 1; logs:\n%s", badVerMsg, got, logs.String())
		}
	})
}
