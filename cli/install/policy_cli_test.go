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
	"flag"
	"io"
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

// TestInstallCmdFlagRegistrationAndValidation checks -package_conflicts flag
// registration and parse-error rejection on installCmd.
func TestInstallCmdFlagRegistrationAndValidation(t *testing.T) {
	var cmd installCmd
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	cmd.SetFlags(fs)

	pcFlag := fs.Lookup("package_conflicts")
	if pcFlag == nil {
		t.Fatal("installCmd did not register -package_conflicts")
	}
	if pcFlag.DefValue != "warn" {
		t.Errorf("-package_conflicts default = %q, want warn", pcFlag.DefValue)
	}

	for _, bad := range []string{"-package_conflicts=bogus", "-package_conflicts=", "-package_conflicts=   "} {
		if _, err := testutil.RunCmd(t, &installCmd{}, bad, "A"); err == nil {
			t.Errorf("RunCmd(%q) succeeded, want parse error", bad)
		}
	}
}

// TestInstallCmdConflictPolicyEndToEnd verifies flag-over-conf precedence,
// CLI remediation hints, offline .goo installs, mixed .goo + repo arguments,
// and StrictConflicts + -force preservation through installCmd.Execute.
func TestInstallCmdConflictPolicyEndToEnd(t *testing.T) {
	t.Run("flag error overrides conf warn on repo install and logs CLI hint", func(t *testing.T) {
		logs := testutil.CaptureLogs(t)
		_, repoURL := testutil.SetupCLIRepo(t, "packageconflicts: warn\n",
			[]goolib.PkgSpec{{Name: "A", Arch: "noarch", Version: "1", Conflicts: []string{"B"}}},
			[]goolib.PkgSpec{{Name: "B", Arch: "noarch", Version: "1"}})
		status, err := testutil.RunCmd(t, &installCmd{}, "-sources="+repoURL, "-package_conflicts=error", "B")
		if err != nil || status != subcommands.ExitFailure {
			t.Fatalf("Execute = (%v, %v), want (ExitFailure, nil)", status, err)
		}
		if diff := cmp.Diff([]string{"A.noarch.1"}, testutil.InstalledNames(t)); diff != "" {
			t.Errorf("installed (-want +got):\n%s", diff)
		}
		if !strings.Contains(logs.String(), "pass -package_conflicts=warn to install anyway") {
			t.Errorf("CLI error log missing -package_conflicts=warn hint; logs:\n%s", logs.String())
		}
	})

	t.Run("flag warn overrides conf error on repo install", func(t *testing.T) {
		_, repoURL := testutil.SetupCLIRepo(t, "packageconflicts: error\n",
			[]goolib.PkgSpec{{Name: "A", Arch: "noarch", Version: "1", Conflicts: []string{"B"}}},
			[]goolib.PkgSpec{{Name: "B", Arch: "noarch", Version: "1"}})
		status, err := testutil.RunCmd(t, &installCmd{}, "-sources="+repoURL, "-package_conflicts=warn", "B")
		if err != nil || status != subcommands.ExitSuccess {
			t.Fatalf("Execute = (%v, %v), want (ExitSuccess, nil)", status, err)
		}
		if diff := cmp.Diff([]string{"A.noarch.1", "B.noarch.1"}, testutil.InstalledNames(t)); diff != "" {
			t.Errorf("installed (-want +got):\n%s", diff)
		}
	})

	t.Run("offline local .goo install with no repos honors -package_conflicts", func(t *testing.T) {
		logs := testutil.CaptureLogs(t)
		dstDir, _ := testutil.SetupCLIRepo(t, "packageconflicts: warn\n",
			[]goolib.PkgSpec{{Name: "A", Arch: "noarch", Version: "1", Conflicts: []string{"B"}}},
			nil)
		localDir := t.TempDir()
		rs := testutil.GenGoo(t, localDir, dstDir, goolib.PkgSpec{Name: "B", Arch: "noarch", Version: "1"})
		gooPath := filepath.Join(localDir, rs.Source)

		status, err := testutil.RunCmd(t, &installCmd{}, "-package_conflicts=error", gooPath)
		if err != nil || status != subcommands.ExitFailure {
			t.Fatalf("offline Execute(error) = (%v, %v), want (ExitFailure, nil)", status, err)
		}
		if !strings.Contains(logs.String(), "pass -package_conflicts=warn to install anyway") {
			t.Errorf("offline CLI error log missing -package_conflicts=warn hint; logs:\n%s", logs.String())
		}
		status, err = testutil.RunCmd(t, &installCmd{}, "-package_conflicts=warn", gooPath)
		if err != nil || status != subcommands.ExitSuccess {
			t.Fatalf("offline Execute(warn) = (%v, %v), want (ExitSuccess, nil)", status, err)
		}
		if diff := cmp.Diff([]string{"A.noarch.1", "B.noarch.1"}, testutil.InstalledNames(t)); diff != "" {
			t.Errorf("installed (-want +got):\n%s", diff)
		}
	})

	t.Run("mixed local .goo and repo arguments populate RepoMap", func(t *testing.T) {
		dstDir, repoURL := testutil.SetupCLIRepo(t, "",
			nil,
			[]goolib.PkgSpec{{Name: "RepoPkg", Arch: "noarch", Version: "1"}})
		localDir := t.TempDir()
		rs := testutil.GenGoo(t, localDir, dstDir, goolib.PkgSpec{Name: "LocalPkg", Arch: "noarch", Version: "1"})
		gooPath := filepath.Join(localDir, rs.Source)

		status, err := testutil.RunCmd(t, &installCmd{}, "-sources="+repoURL, gooPath, "RepoPkg")
		if err != nil || status != subcommands.ExitSuccess {
			t.Fatalf("mixed Execute = (%v, %v), want (ExitSuccess, nil)", status, err)
		}
		if diff := cmp.Diff([]string{"LocalPkg.noarch.1", "RepoPkg.noarch.1"}, testutil.InstalledNames(t)); diff != "" {
			t.Errorf("installed (-want +got):\n%s", diff)
		}
	})

	t.Run("strictconflicts enforced and overridden by -force without affecting -package_conflicts", func(t *testing.T) {
		dstDir, repoURL := testutil.SetupCLIRepo(t, "strictconflicts: true\n",
			nil,
			[]goolib.PkgSpec{{Name: "B", Arch: "noarch", Version: "1"}})
		// Seed package A owning the destination file that B installs.
		shared := filepath.Join(dstDir, "B")
		if err := os.WriteFile(shared, []byte("owned by A"), 0644); err != nil {
			t.Fatal(err)
		}
		db, err := googetdb.NewDB(settings.DBFile())
		if err != nil {
			t.Fatal(err)
		}
		if err := db.WriteStateToDB(client.GooGetState{{
			PackageSpec:    &goolib.PkgSpec{Name: "A", Arch: "noarch", Version: "1"},
			InstalledFiles: map[string]string{shared: "nonemptyhash"},
		}}); err != nil {
			t.Fatal(err)
		}
		db.Close()

		status, err := testutil.RunCmd(t, &installCmd{}, "-sources="+repoURL, "B")
		if err != nil || status != subcommands.ExitFailure {
			t.Fatalf("unforced strictconflicts Execute = (%v, %v), want (ExitFailure, nil)", status, err)
		}
		status, err = testutil.RunCmd(t, &installCmd{}, "-sources="+repoURL, "-force", "B")
		if err != nil || status != subcommands.ExitSuccess {
			t.Fatalf("forced strictconflicts Execute = (%v, %v), want (ExitSuccess, nil)", status, err)
		}
		if diff := cmp.Diff([]string{"A.noarch.1", "B.noarch.1"}, testutil.InstalledNames(t)); diff != "" {
			t.Errorf("installed (-want +got):\n%s", diff)
		}
	})
}
