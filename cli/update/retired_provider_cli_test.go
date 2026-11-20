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
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/googet/v2/googetdb"
	"github.com/google/googet/v2/goolib"
	"github.com/google/googet/v2/settings"
	"github.com/google/googet/v2/testutil"
	"github.com/google/subcommands"
)

// sourceRepos returns the repository each installed package came from,
// keyed by its name.arch.version.
func sourceRepos(t *testing.T) map[string]string {
	t.Helper()
	db, err := googetdb.NewDB(settings.DBFile())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	state, err := db.FetchPkgs("")
	if err != nil {
		t.Fatal(err)
	}
	out := make(map[string]string)
	for _, ps := range state {
		out[ps.PackageSpec.String()] = ps.SourceRepo
	}
	return out
}

// TestUpdateCmdInstallsFromPlannedRepo checks that update installs each
// package from the repository its plan printed, even when another repository
// that sorts first offers the same name, arch and version but only the
// printed one is admissible.
func TestUpdateCmdInstallsFromPlannedRepo(t *testing.T) {
	installed := []goolib.PkgSpec{{Name: "P", Arch: "x86_64", Version: "1.0.0", LockArch: true}}
	dst, _ := testutil.SetupCLIRepo(t, "", installed, nil)
	urls := serveRepos(t, dst, map[string][]goolib.PkgSpec{
		"a": {
			{Name: "R", Arch: "x86_64", Version: "2.0.0", Replaces: []string{"P"}, Provides: []string{"P"}},
			{Name: "R", Arch: "noarch", Version: "2.0.0", LockArch: true, Replaces: []string{"P"}, Provides: []string{"P"}},
		},
		"b": {
			{Name: "R", Arch: "noarch", Version: "2.0.0", Replaces: []string{"P"}, Provides: []string{"P"}},
		},
	})
	status, out := runUpdate(t, "-sources="+urls["a"]+","+urls["b"])
	if status != subcommands.ExitSuccess {
		t.Fatalf("update = %v; output:\n%s", status, out)
	}
	wantLine := "  P.x86_64, 1.0.0 --> R.noarch, 2.0.0 from " + urls["b"] + " (replaces P.x86_64.1.0.0)\n"
	if !strings.Contains(out, wantLine) {
		t.Errorf("output missing %q:\n%s", wantLine, out)
	}
	if diff := cmp.Diff(map[string]string{"R.noarch.2.0.0": urls["b"]}, sourceRepos(t)); diff != "" {
		t.Errorf("installed packages and their repositories (-want +got):\n%s", diff)
	}
}

// TestUpdateCmdDryRunMatchesLiveSkips checks that a dry run prints the same
// plan, skipped replacements included, as the live run that follows it, that
// the dry run changes nothing, and that the live run leaves each package
// whose replacement it skipped installed.
func TestUpdateCmdDryRunMatchesLiveSkips(t *testing.T) {
	for _, tc := range []struct {
		name      string
		installed []goolib.PkgSpec
		repo      []goolib.PkgSpec
		wantSkip  string
		wantAfter []string
	}{
		{
			name:      "provider whose best version dropped Provides",
			installed: []goolib.PkgSpec{{Name: "P", Arch: "x86_64", Version: "1.0.0"}},
			repo: []goolib.PkgSpec{
				{Name: "L", Arch: "x86_64", Version: "2.0.0", Replaces: []string{"P"}, Provides: []string{"P"}},
				{Name: "L", Arch: "x86_64", Version: "3.0.0", Replaces: []string{"P"}},
			},
			wantSkip:  "  P.x86_64, 1.0.0: not replaced by L.x86_64.2.0.0 (plan_rejected: ",
			wantAfter: []string{"P.x86_64.1.0.0"},
		},
		{
			name: "replacement that installs again what an earlier one removes",
			installed: []goolib.PkgSpec{
				{Name: "P1", Arch: "x86_64", Version: "1.0.0"},
				{Name: "P2", Arch: "x86_64", Version: "1.0.0"},
				{Name: "Q", Arch: "x86_64", Version: "1.0.0"},
			},
			repo: []goolib.PkgSpec{
				{Name: "R1", Arch: "x86_64", Version: "2.0.0", Replaces: []string{"P1", "Q"}, Provides: []string{"P1"}},
				{Name: "R2", Arch: "x86_64", Version: "2.0.0", Replaces: []string{"P2"}, Provides: []string{"P2"}, PkgDependencies: map[string]string{"Q": "1.0.0"}},
				{Name: "Q", Arch: "x86_64", Version: "1.0.0"},
			},
			wantSkip:  "  P2.x86_64, 1.0.0: not replaced by R2.x86_64.2.0.0 (plan_rejected: ",
			wantAfter: []string{"P2.x86_64.1.0.0", "R1.x86_64.2.0.0"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, repoURL := testutil.SetupCLIRepo(t, "", tc.installed, tc.repo)
			before := testutil.InstalledNames(t)
			status, dry := runUpdate(t, "-sources="+repoURL, "-dry_run")
			if status != subcommands.ExitSuccess {
				t.Fatalf("update -dry_run = %v; output:\n%s", status, dry)
			}
			if !strings.Contains(dry, tc.wantSkip) {
				t.Errorf("dry run output missing %q:\n%s", tc.wantSkip, dry)
			}
			if diff := cmp.Diff(before, testutil.InstalledNames(t)); diff != "" {
				t.Errorf("dry run changed the installed packages (-before +after):\n%s", diff)
			}
			status, live := runUpdate(t, "-sources="+repoURL)
			if status != subcommands.ExitSuccess {
				t.Fatalf("update = %v; output:\n%s", status, live)
			}
			plan := strings.TrimSuffix(dry, dryRunLine)
			if !strings.HasPrefix(live, plan) {
				t.Errorf("live output doesn't start with the dry run's plan\ndry run:\n%s\nlive:\n%s", dry, live)
			}
			if diff := cmp.Diff(tc.wantAfter, testutil.InstalledNames(t)); diff != "" {
				t.Errorf("after the live run, installed (-want +got):\n%s", diff)
			}
		})
	}
}
