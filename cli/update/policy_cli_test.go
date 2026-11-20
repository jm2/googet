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
	"flag"
	"io"
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

// TestUpdateCmdFlagRegistrationAndValidation checks -package_conflicts flag
// registration and parse-error rejection on updateCmd.
func TestUpdateCmdFlagRegistrationAndValidation(t *testing.T) {
	var cmd updateCmd
	fs := flag.NewFlagSet("update", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	cmd.SetFlags(fs)

	pcFlag := fs.Lookup("package_conflicts")
	if pcFlag == nil {
		t.Fatal("updateCmd did not register -package_conflicts")
	}
	if pcFlag.DefValue != "warn" {
		t.Errorf("-package_conflicts default = %q, want warn", pcFlag.DefValue)
	}

	for _, bad := range []string{"-package_conflicts=bogus", "-package_conflicts=", "-package_conflicts=   "} {
		if _, err := testutil.RunCmd(t, &updateCmd{}, bad); err == nil {
			t.Errorf("RunCmd(%q) succeeded, want parse error", bad)
		}
	}
}

// TestUpdateCmdConflictPolicyAndSelfHealing verifies flag-over-conf precedence,
// CLI remediation hints, multi-package partial failure and self-healing across
// runs, upgrade-time Conflicts+Replaces replacement, and StrictConflicts +
// -force preservation.
func TestUpdateCmdConflictPolicyAndSelfHealing(t *testing.T) {
	t.Run("flag warn overrides conf error across multi-package update", func(t *testing.T) {
		_, repoURL := testutil.SetupCLIRepo(t, "packageconflicts: error\n",
			[]goolib.PkgSpec{
				{Name: "B", Arch: "noarch", Version: "1"},
				{Name: "C", Arch: "noarch", Version: "1"},
				{Name: "Z", Arch: "noarch", Version: "1", Conflicts: []string{"B"}},
			},
			[]goolib.PkgSpec{
				{Name: "B", Arch: "noarch", Version: "2"},
				{Name: "C", Arch: "noarch", Version: "2"},
				{Name: "Z", Arch: "noarch", Version: "2"},
			})
		status, err := testutil.RunCmd(t, &updateCmd{}, "-sources="+repoURL, "-package_conflicts=warn")
		if err != nil || status != subcommands.ExitSuccess {
			t.Fatalf("Execute(warn) = (%v, %v), want (ExitSuccess, nil)", status, err)
		}
		if diff := cmp.Diff([]string{"B.noarch.2", "C.noarch.2", "Z.noarch.2"}, testutil.InstalledNames(t)); diff != "" {
			t.Errorf("installed (-want +got):\n%s", diff)
		}
	})

	t.Run("flag error overrides conf warn with partial progress and next-run self-healing", func(t *testing.T) {
		logs := testutil.CaptureLogs(t)
		_, repoURL := testutil.SetupCLIRepo(t, "packageconflicts: warn\n",
			[]goolib.PkgSpec{
				{Name: "B", Arch: "noarch", Version: "1"},
				{Name: "C", Arch: "noarch", Version: "1"},
				{Name: "Z", Arch: "noarch", Version: "1", Conflicts: []string{"B"}},
			},
			[]goolib.PkgSpec{
				{Name: "B", Arch: "noarch", Version: "2"},
				{Name: "C", Arch: "noarch", Version: "2"},
				{Name: "Z", Arch: "noarch", Version: "2"},
			})
		// First run: B is attempted before Z upgrades, so B fails while C and
		// Z (which drops Conflicts: ["B"] in v2) succeed.
		status, err := testutil.RunCmd(t, &updateCmd{}, "-sources="+repoURL, "-package_conflicts=error")
		if err != nil || status != subcommands.ExitFailure {
			t.Fatalf("first Execute(error) = (%v, %v), want (ExitFailure, nil)", status, err)
		}
		if !strings.Contains(logs.String(), "pass -package_conflicts=warn to install anyway") {
			t.Errorf("update CLI error log missing -package_conflicts=warn hint; logs:\n%s", logs.String())
		}
		if diff := cmp.Diff([]string{"B.noarch.1", "C.noarch.2", "Z.noarch.2"}, testutil.InstalledNames(t)); diff != "" {
			t.Fatalf("installed after first run (-want +got):\n%s", diff)
		}

		// Second run: Z.noarch.2 no longer conflicts with B, so B heals.
		status, err = testutil.RunCmd(t, &updateCmd{}, "-sources="+repoURL, "-package_conflicts=error")
		if err != nil || status != subcommands.ExitSuccess {
			t.Fatalf("second Execute(error) = (%v, %v), want (ExitSuccess, nil)", status, err)
		}
		if diff := cmp.Diff([]string{"B.noarch.2", "C.noarch.2", "Z.noarch.2"}, testutil.InstalledNames(t)); diff != "" {
			t.Errorf("installed after second run (-want +got):\n%s", diff)
		}
	})

	t.Run("upgraded package adding Conflicts and Replaces replaces installed target", func(t *testing.T) {
		_, repoURL := testutil.SetupCLIRepo(t, "packageconflicts: error\n",
			[]goolib.PkgSpec{
				{Name: "R", Arch: "noarch", Version: "1"},
				{Name: "X", Arch: "noarch", Version: "1"},
			},
			[]goolib.PkgSpec{
				{Name: "R", Arch: "noarch", Version: "2", Conflicts: []string{"X"}, Replaces: []string{"X"}},
				{Name: "X", Arch: "noarch", Version: "1"},
			})
		status, err := testutil.RunCmd(t, &updateCmd{}, "-sources="+repoURL)
		if err != nil || status != subcommands.ExitSuccess {
			t.Fatalf("Execute = (%v, %v), want (ExitSuccess, nil)", status, err)
		}
		if diff := cmp.Diff([]string{"R.noarch.2"}, testutil.InstalledNames(t)); diff != "" {
			t.Errorf("installed (-want +got):\n%s", diff)
		}
	})

	t.Run("strictconflicts enforced and overridden by -force on update", func(t *testing.T) {
		dstDir, repoURL := testutil.SetupCLIRepo(t, "strictconflicts: true\n",
			[]goolib.PkgSpec{{Name: "B", Arch: "noarch", Version: "1"}},
			[]goolib.PkgSpec{{Name: "B", Arch: "noarch", Version: "2"}})
		// Add package A owning the destination file that B writes.
		shared := filepath.Join(dstDir, "B")
		db, err := googetdb.NewDB(settings.DBFile())
		if err != nil {
			t.Fatal(err)
		}
		state, err := db.FetchPkgs("")
		if err != nil {
			t.Fatal(err)
		}
		state = append(state, client.PackageState{
			PackageSpec:    &goolib.PkgSpec{Name: "A", Arch: "noarch", Version: "1"},
			InstalledFiles: map[string]string{shared: "nonemptyhash"},
		})
		if err := db.WriteStateToDB(state); err != nil {
			t.Fatal(err)
		}
		db.Close()

		status, err := testutil.RunCmd(t, &updateCmd{}, "-sources="+repoURL)
		if err != nil || status != subcommands.ExitFailure {
			t.Fatalf("unforced strictconflicts update = (%v, %v), want (ExitFailure, nil)", status, err)
		}
		status, err = testutil.RunCmd(t, &updateCmd{}, "-sources="+repoURL, "-force")
		if err != nil || status != subcommands.ExitSuccess {
			t.Fatalf("forced strictconflicts update = (%v, %v), want (ExitSuccess, nil)", status, err)
		}
		if diff := cmp.Diff([]string{"A.noarch.1", "B.noarch.2"}, testutil.InstalledNames(t)); diff != "" {
			t.Errorf("installed (-want +got):\n%s", diff)
		}
	})
}
