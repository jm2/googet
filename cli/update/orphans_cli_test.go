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
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/googet/v2/goolib"
	"github.com/google/googet/v2/testutil"
	"github.com/google/subcommands"
)

// TestUpdateCmdOrphanFlagRegistrationAndValidation checks registration,
// default value, usage text, and invalid-input rejection for
// -orphaned_dependents on updateCmd.
func TestUpdateCmdOrphanFlagRegistrationAndValidation(t *testing.T) {
	var cmd updateCmd
	if !strings.Contains(cmd.Usage(), "-orphaned_dependents fail|remove") {
		t.Errorf("Usage() = %q, want -orphaned_dependents fail|remove", cmd.Usage())
	}

	fs := flag.NewFlagSet("update", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	cmd.SetFlags(fs)

	odFlag := fs.Lookup("orphaned_dependents")
	if odFlag == nil {
		t.Fatal("updateCmd did not register -orphaned_dependents")
	}
	if odFlag.DefValue != "fail" {
		t.Errorf("-orphaned_dependents default = %q, want fail", odFlag.DefValue)
	}
	forceFlag := fs.Lookup("force")
	if forceFlag == nil || !strings.Contains(forceFlag.Usage, "-orphaned_dependents") {
		t.Errorf("-force usage = %v, want mention of -orphaned_dependents", forceFlag)
	}

	for _, bad := range []string{"-orphaned_dependents=bogus", "-orphaned_dependents=", "-orphaned_dependents=   "} {
		if _, err := testutil.RunCmd(t, &updateCmd{}, bad); err == nil {
			t.Errorf("RunCmd(%q) succeeded, want parse error", bad)
		}
	}
}

// TestUpdateCmdOrphanedDependentsMatrix verifies the full flag x googet.conf
// precedence matrix on googet update: when effective mode is fail, an
// orphaning upgrade is skipped with a structured warning and exit 0 while
// other packages update; when effective mode is remove, the cascade runs and
// upgrades the replacing package; -force never alters orphan policy.
func TestUpdateCmdOrphanedDependentsMatrix(t *testing.T) {
	type confCase struct {
		name       string
		conf       string
		confRemove bool
	}
	confs := []confCase{
		{name: "absent", conf: "", confRemove: false},
		{name: "fail", conf: "orphaneddependents: fail\n", confRemove: false},
		{name: "remove", conf: "orphaneddependents: remove\n", confRemove: true},
		{name: "invalid", conf: "orphaneddependents: bogus\n", confRemove: false},
	}
	type flagCase struct {
		name       string
		arg        string
		set        bool
		flagRemove bool
	}
	flags := []flagCase{
		{name: "unset", set: false},
		{name: "fail", arg: "-orphaned_dependents=fail", set: true, flagRemove: false},
		{name: "remove", arg: "-orphaned_dependents=remove", set: true, flagRemove: true},
	}

	for _, cc := range confs {
		for _, fc := range flags {
			for _, force := range []bool{false, true} {
				wantRemove := cc.confRemove
				if fc.set {
					wantRemove = fc.flagRemove
				}
				name := "conf=" + cc.name + "/flag=" + fc.name
				if force {
					name += "/force"
				}
				t.Run(name, func(t *testing.T) {
					logs := testutil.CaptureLogs(t)
					_, repoURL := testutil.SetupCLIRepo(t, cc.conf,
						[]goolib.PkgSpec{
							{Name: "Other", Arch: "noarch", Version: "1"},
							{Name: "P", Arch: "noarch", Version: "1"},
							{Name: "D", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"P": "1"}},
							{Name: "R", Arch: "noarch", Version: "1"},
						},
						[]goolib.PkgSpec{
							{Name: "Other", Arch: "noarch", Version: "2"},
							{Name: "P", Arch: "noarch", Version: "1"},
							{Name: "D", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"P": "1"}},
							{Name: "R", Arch: "noarch", Version: "2", Replaces: []string{"P"}},
						})
					args := []string{"-sources=" + repoURL}
					if fc.set {
						args = append(args, fc.arg)
					}
					if force {
						args = append(args, "-force")
					}

					status, err := testutil.RunCmd(t, &updateCmd{}, args...)
					if err != nil {
						t.Fatalf("RunCmd(%v) unexpected flag error: %v", args, err)
					}
					if status != subcommands.ExitSuccess {
						t.Fatalf("status = %v, want ExitSuccess; logs:\n%s", status, logs.String())
					}
					if wantRemove {
						if diff := cmp.Diff([]string{"Other.noarch.2", "R.noarch.2"}, testutil.InstalledNames(t)); diff != "" {
							t.Errorf("installed (-want +got):\n%s", diff)
						}
					} else {
						wantLog := `dependency_unsatisfied op=replace mode=fail result=skipped dependent=D.noarch.1 requirement="P >= 1" removed=P.noarch.1 replacer=R.noarch.2`
						if !strings.Contains(logs.String(), "WARN :") || !strings.Contains(logs.String(), wantLog) {
							t.Errorf("logs missing skipped orphan warning %q:\n%s", wantLog, logs.String())
						}
						if diff := cmp.Diff([]string{"D.noarch.1", "Other.noarch.2", "P.noarch.1", "R.noarch.1"}, testutil.InstalledNames(t)); diff != "" {
							t.Errorf("installed (-want +got):\n%s", diff)
						}
					}
				})
			}
		}
	}
}

// TestUpdateCmdNestedDependencyOrphanSkippedAndHardErrorsStillFail checks that
// an orphan arising inside a nested dependency's replacement is skipped with
// exit 0 while other packages update, whereas a hard error in the same or
// another package is never skipped and exits with ExitFailure.
func TestUpdateCmdNestedDependencyOrphanSkippedAndHardErrorsStillFail(t *testing.T) {
	t.Run("orphan in nested dependency step skips top-level upgrade with exit 0", func(t *testing.T) {
		logs := testutil.CaptureLogs(t)
		_, repoURL := testutil.SetupCLIRepo(t, "",
			[]goolib.PkgSpec{
				{Name: "A", Arch: "noarch", Version: "1"},
				{Name: "Other", Arch: "noarch", Version: "1"},
				{Name: "P", Arch: "noarch", Version: "1"},
				{Name: "D", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"P": "1"}},
			},
			[]goolib.PkgSpec{
				{Name: "A", Arch: "noarch", Version: "2", PkgDependencies: map[string]string{"Sub": "1"}},
				{Name: "Sub", Arch: "noarch", Version: "1", Replaces: []string{"P"}},
				{Name: "Other", Arch: "noarch", Version: "2"},
				{Name: "P", Arch: "noarch", Version: "1"},
				{Name: "D", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"P": "1"}},
			})

		status, err := testutil.RunCmd(t, &updateCmd{}, "-sources="+repoURL)
		if err != nil || status != subcommands.ExitSuccess {
			t.Fatalf("Execute = (%v, %v), want (ExitSuccess, nil); logs:\n%s", status, err, logs.String())
		}
		wantLog := `dependency_unsatisfied op=replace mode=fail result=skipped dependent=D.noarch.1 requirement="P >= 1" removed=P.noarch.1 replacer=Sub.noarch.1`
		if !strings.Contains(logs.String(), wantLog) {
			t.Errorf("logs missing nested skipped orphan warning %q:\n%s", wantLog, logs.String())
		}
		if diff := cmp.Diff([]string{"A.noarch.1", "D.noarch.1", "Other.noarch.2", "P.noarch.1"}, testutil.InstalledNames(t)); diff != "" {
			t.Errorf("installed (-want +got):\n%s", diff)
		}
	})

	t.Run("later conflict in same upgrade plan wins over orphan and exits 1", func(t *testing.T) {
		logs := testutil.CaptureLogs(t)
		_, repoURL := testutil.SetupCLIRepo(t, "",
			[]goolib.PkgSpec{
				{Name: "A", Arch: "noarch", Version: "1"},
				{Name: "Blocker", Arch: "noarch", Version: "1"},
				{Name: "P", Arch: "noarch", Version: "1"},
				{Name: "D", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"P": "1"}},
			},
			[]goolib.PkgSpec{
				{Name: "A", Arch: "noarch", Version: "2", PkgDependencies: map[string]string{"B": "1", "C": "1"}},
				{Name: "B", Arch: "noarch", Version: "1", Replaces: []string{"P"}},
				{Name: "C", Arch: "noarch", Version: "1", Conflicts: []string{"Blocker"}},
				{Name: "Blocker", Arch: "noarch", Version: "1"},
				{Name: "P", Arch: "noarch", Version: "1"},
				{Name: "D", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"P": "1"}},
			})

		status, err := testutil.RunCmd(t, &updateCmd{}, "-sources="+repoURL)
		if err != nil || status != subcommands.ExitFailure {
			t.Fatalf("Execute = (%v, %v), want (ExitFailure, nil); logs:\n%s", status, err, logs.String())
		}
		if strings.Contains(logs.String(), "result=skipped") {
			t.Errorf("hard conflict upgrade was falsely logged as skipped:\n%s", logs.String())
		}
		if !strings.Contains(logs.String(), "Error updating noarch A 2") {
			t.Errorf("logs missing Error updating A 2:\n%s", logs.String())
		}
	})

	t.Run("update -dry_run stays exit 0 even when repo contains orphaning upgrade", func(t *testing.T) {
		_, repoURL := testutil.SetupCLIRepo(t, "",
			[]goolib.PkgSpec{
				{Name: "P", Arch: "noarch", Version: "1"},
				{Name: "D", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"P": "1"}},
				{Name: "R", Arch: "noarch", Version: "1"},
			},
			[]goolib.PkgSpec{
				{Name: "P", Arch: "noarch", Version: "1"},
				{Name: "D", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"P": "1"}},
				{Name: "R", Arch: "noarch", Version: "2", Replaces: []string{"P"}},
			})

		status, err := testutil.RunCmd(t, &updateCmd{}, "-sources="+repoURL, "-dry_run")
		if err != nil || status != subcommands.ExitSuccess {
			t.Fatalf("update -dry_run = (%v, %v), want (ExitSuccess, nil)", status, err)
		}
	})
}

// TestUpdateCmdSelfOrphanProvidesOnlyAndMixedHardFailure checks that
// Provides-only dependents do not skip an update, that a self-orphaning
// upgrade is treated as a hard error rather than skipped, and that a hard
// failure on one package still exits non-zero when another package in the same
// update pass is skipped for orphaned dependents.
func TestUpdateCmdSelfOrphanProvidesOnlyAndMixedHardFailure(t *testing.T) {
	t.Run("provides_only_orphan_installs_upgrade_and_lists_dependent", func(t *testing.T) {
		logs := testutil.CaptureLogs(t)
		_, repoURL := testutil.SetupCLIRepo(t, "",
			[]goolib.PkgSpec{
				{Name: "P", Arch: "noarch", Version: "1", Provides: []string{"virt=2"}},
				{Name: "Q", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"virt": "1"}},
				{Name: "R", Arch: "noarch", Version: "1"},
			},
			[]goolib.PkgSpec{
				{Name: "P", Arch: "noarch", Version: "1", Provides: []string{"virt=2"}},
				{Name: "Q", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"virt": "1"}},
				{Name: "R", Arch: "noarch", Version: "2", Replaces: []string{"P"}},
			})

		status, err := testutil.RunCmd(t, &updateCmd{}, "-sources="+repoURL)
		if err != nil || status != subcommands.ExitSuccess {
			t.Fatalf("Execute = (%v, %v), want (ExitSuccess, nil); logs:\n%s", status, err, logs.String())
		}
		if strings.Contains(logs.String(), "result=skipped") {
			t.Errorf("Provides-only orphan was unexpectedly skipped:\n%s", logs.String())
		}
		wantListed := `dependency_unsatisfied op=replace result=listed dependent=Q.noarch.1 requirement="virt >= 1" removed=P.noarch.1 replacer=R.noarch.2`
		if !strings.Contains(logs.String(), wantListed) {
			t.Errorf("logs missing listed warning %q:\n%s", wantListed, logs.String())
		}
		if diff := cmp.Diff([]string{"Q.noarch.1", "R.noarch.2"}, testutil.InstalledNames(t)); diff != "" {
			t.Errorf("installed (-want +got):\n%s", diff)
		}
	})

	t.Run("self_orphaning_upgrade_is_not_skipped_and_exits_failure", func(t *testing.T) {
		logs := testutil.CaptureLogs(t)
		_, repoURL := testutil.SetupCLIRepo(t, "",
			[]goolib.PkgSpec{
				{Name: "P", Arch: "noarch", Version: "1"},
				{Name: "R", Arch: "noarch", Version: "1"},
			},
			[]goolib.PkgSpec{
				{Name: "P", Arch: "noarch", Version: "1"},
				{Name: "R", Arch: "noarch", Version: "2", Replaces: []string{"P"}, PkgDependencies: map[string]string{"P": "1"}},
			})

		status, err := testutil.RunCmd(t, &updateCmd{}, "-sources="+repoURL)
		if err != nil || status != subcommands.ExitFailure {
			t.Fatalf("Execute = (%v, %v), want (ExitFailure, nil); logs:\n%s", status, err, logs.String())
		}
		if strings.Contains(logs.String(), "result=skipped") {
			t.Errorf("self-orphaning upgrade was falsely skipped:\n%s", logs.String())
		}
		if !strings.Contains(logs.String(), "Error updating noarch R 2") {
			t.Errorf("logs missing Error updating R 2:\n%s", logs.String())
		}
		if diff := cmp.Diff([]string{"P.noarch.1", "R.noarch.1"}, testutil.InstalledNames(t)); diff != "" {
			t.Errorf("installed (-want +got):\n%s", diff)
		}
	})

	t.Run("hard_failure_on_earlier_package_plus_later_orphan_skip_preserves_failure", func(t *testing.T) {
		logs := testutil.CaptureLogs(t)
		_, repoURL := testutil.SetupCLIRepo(t, "",
			[]goolib.PkgSpec{
				{Name: "A", Arch: "noarch", Version: "1"},
				{Name: "Blocker", Arch: "noarch", Version: "1"},
				{Name: "Other", Arch: "noarch", Version: "1"},
				{Name: "P", Arch: "noarch", Version: "1"},
				{Name: "D", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"P": "1"}},
				{Name: "R", Arch: "noarch", Version: "1"},
			},
			[]goolib.PkgSpec{
				{Name: "A", Arch: "noarch", Version: "2", Conflicts: []string{"Blocker"}},
				{Name: "Blocker", Arch: "noarch", Version: "1"},
				{Name: "Other", Arch: "noarch", Version: "2"},
				{Name: "P", Arch: "noarch", Version: "1"},
				{Name: "D", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"P": "1"}},
				{Name: "R", Arch: "noarch", Version: "2", Replaces: []string{"P"}},
			})

		status, err := testutil.RunCmd(t, &updateCmd{}, "-sources="+repoURL)
		if err != nil || status != subcommands.ExitFailure {
			t.Fatalf("Execute = (%v, %v), want (ExitFailure, nil); logs:\n%s", status, err, logs.String())
		}
		if !strings.Contains(logs.String(), "Error updating noarch A 2") {
			t.Errorf("logs missing Error updating A 2:\n%s", logs.String())
		}
		if !strings.Contains(logs.String(), "result=skipped dependent=D.noarch.1") {
			t.Errorf("logs missing skipped orphan warning for R 2:\n%s", logs.String())
		}
		if diff := cmp.Diff([]string{"A.noarch.1", "Blocker.noarch.1", "D.noarch.1", "Other.noarch.2", "P.noarch.1", "R.noarch.1"}, testutil.InstalledNames(t)); diff != "" {
			t.Errorf("installed (-want +got):\n%s", diff)
		}
	})
}
