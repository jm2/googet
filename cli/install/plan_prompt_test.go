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

// dryRun runs installFromRepo for name with -dry_run against a fresh
// database holding installed and a repository holding available, and returns
// the console output, the error and whether the database changed.
func dryRun(t *testing.T, installed client.GooGetState, available []goolib.PkgSpec, name string) (string, error, bool) {
	t.Helper()
	settings.Initialize(t.TempDir(), false)
	db, err := googetdb.NewDB(settings.DBFile())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.WriteStateToDB(installed); err != nil {
		t.Fatal(err)
	}
	before, err := db.FetchPkgs("")
	if err != nil {
		t.Fatal(err)
	}
	downloader, err := client.NewDownloader("")
	if err != nil {
		t.Fatal(err)
	}
	i := newTestInstaller(db, t.TempDir(), downloader, false, true)
	gooDir := t.TempDir()
	srv := testutil.ServeGoo(t, gooDir)
	t.Cleanup(srv.Close)
	var specs []goolib.RepoSpec
	for _, ps := range available {
		specs = append(specs, testutil.GenGoo(t, gooDir, t.TempDir(), ps))
	}
	i.repoMap = client.RepoMap{srv.URL: client.Repo{Packages: specs}}
	var runErr error
	out := captureStdout(func() { runErr = i.installFromRepo(context.Background(), name, []string{"noarch"}) })
	after, err := db.FetchPkgs("")
	if err != nil {
		t.Fatal(err)
	}
	return out, runErr, len(before) != len(after)
}

// TestInstallDryRunShowsRemovals checks that a dry run lists the installed
// packages a replacement removes, and why, without changing anything.
func TestInstallDryRunShowsRemovals(t *testing.T) {
	out, err, changed := dryRun(t,
		client.GooGetState{
			{PackageSpec: &goolib.PkgSpec{Name: "P", Arch: "noarch", Version: "1"}},
			{PackageSpec: &goolib.PkgSpec{Name: "D", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"P": "1"}}},
		},
		[]goolib.PkgSpec{{Name: "R", Arch: "noarch", Version: "1", Replaces: []string{"P"}}},
		"R")
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	want := strings.Join([]string{
		"The following packages will be installed:",
		"  R.noarch.1",
		"The following packages will be removed:",
		"  D.noarch.1 (depends on a package R.noarch.1 replaces)",
		"  P.noarch.1 (replaced by R.noarch.1)",
		"",
		"Dry run: Would install R.noarch.1 and its dependencies if not already installed.",
	}, "\n")
	if !strings.Contains(out, want) {
		t.Errorf("dry run output = %q, want it to contain %q", out, want)
	}
	if changed {
		t.Errorf("dry run changed the database")
	}
}

// TestInstallDryRunListsOnlyWhatIsInstalled checks that a dependency that an
// installed version already satisfies isn't listed, even when a repository
// has a newer version of it.
func TestInstallDryRunListsOnlyWhatIsInstalled(t *testing.T) {
	out, err, _ := dryRun(t,
		client.GooGetState{{PackageSpec: &goolib.PkgSpec{Name: "B", Arch: "noarch", Version: "1"}}},
		[]goolib.PkgSpec{
			{Name: "A", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"B": "1"}},
			{Name: "B", Arch: "noarch", Version: "2"},
		},
		"A")
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if !strings.Contains(out, "  A.noarch.1\n") || strings.Contains(out, "B.noarch.2") {
		t.Errorf("dry run output = %q, want A.noarch.1 listed and B.noarch.2 not", out)
	}
}

// TestInstallDryRunReportsConflict checks that a dry run fails with the
// error the install would fail with.
func TestInstallDryRunReportsConflict(t *testing.T) {
	_, err, changed := dryRun(t,
		client.GooGetState{{PackageSpec: &goolib.PkgSpec{Name: "C", Arch: "noarch", Version: "1"}}},
		[]goolib.PkgSpec{{Name: "A", Arch: "noarch", Version: "1", Conflicts: []string{"C"}}},
		"A")
	if want := "conflict with installed package or provider: C"; err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("dry run = %v, want an error containing %q", err, want)
	}
	if changed {
		t.Errorf("dry run changed the database")
	}
}

// TestInstallInstalledDependencyMissingFromRepos checks that installing a
// package whose dependency is installed but offered by no repository
// succeeds without a prompt.
func TestInstallInstalledDependencyMissingFromRepos(t *testing.T) {
	_, repoURL := testutil.SetupCLIRepo(t, "",
		[]goolib.PkgSpec{{Name: "B", Arch: "noarch", Version: "1"}},
		[]goolib.PkgSpec{{Name: "A", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"B": "1"}}})
	status, err := testutil.RunCmd(t, &installCmd{}, "-sources="+repoURL, "A")
	if err != nil || status != subcommands.ExitSuccess {
		t.Fatalf("Execute = (%v, %v), want (ExitSuccess, nil)", status, err)
	}
	if diff := cmp.Diff([]string{"A.noarch.1", "B.noarch.1"}, testutil.InstalledNames(t)); diff != "" {
		t.Errorf("installed (-want +got):\n%s", diff)
	}
}

// TestDryRunExitCodes checks that an install dry run exits with failure
// exactly when the install would fail, and changes nothing either way.
func TestDryRunExitCodes(t *testing.T) {
	for _, tc := range []struct {
		name      string
		conf      string
		installed []goolib.PkgSpec
		available []goolib.PkgSpec
		want      subcommands.ExitStatus
	}{
		{
			name:      "install that would succeed",
			available: []goolib.PkgSpec{{Name: "A", Arch: "noarch", Version: "1"}},
			want:      subcommands.ExitSuccess,
		},
		{
			name:      "install that would conflict",
			installed: []goolib.PkgSpec{{Name: "C", Arch: "noarch", Version: "1"}},
			available: []goolib.PkgSpec{{Name: "A", Arch: "noarch", Version: "1", Conflicts: []string{"C"}}},
			want:      subcommands.ExitFailure,
		},
		{
			name:      "install that an installed package conflicts with, under packageconflicts: error",
			conf:      "packageconflicts: error\n",
			installed: []goolib.PkgSpec{{Name: "X", Arch: "noarch", Version: "1", Conflicts: []string{"A"}}},
			available: []goolib.PkgSpec{{Name: "A", Arch: "noarch", Version: "1"}},
			want:      subcommands.ExitFailure,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, repoURL := testutil.SetupCLIRepo(t, tc.conf, tc.installed, tc.available)
			status, err := testutil.RunCmd(t, &installCmd{}, "-sources="+repoURL, "-dry_run", "A")
			if err != nil || status != tc.want {
				t.Errorf("Execute = (%v, %v), want (%v, nil)", status, err, tc.want)
			}
			var want []string
			for _, ps := range tc.installed {
				want = append(want, ps.String())
			}
			if diff := cmp.Diff(want, testutil.InstalledNames(t)); diff != "" {
				t.Errorf("installed (-want +got):\n%s", diff)
			}
		})
	}
}
