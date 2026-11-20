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
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/googet/v2/goolib"
	"github.com/google/googet/v2/settings"
	"github.com/google/googet/v2/testutil"
	"github.com/google/subcommands"
)

// TestInstallCmdOrphanFlagRegistrationAndValidation checks registration,
// default value, usage text, and invalid-input rejection for
// -orphaned_dependents on installCmd.
func TestInstallCmdOrphanFlagRegistrationAndValidation(t *testing.T) {
	var cmd installCmd
	if !strings.Contains(cmd.Usage(), "-orphaned_dependents fail|remove") {
		t.Errorf("Usage() = %q, want -orphaned_dependents fail|remove", cmd.Usage())
	}

	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	cmd.SetFlags(fs)

	odFlag := fs.Lookup("orphaned_dependents")
	if odFlag == nil {
		t.Fatal("installCmd did not register -orphaned_dependents")
	}
	if odFlag.DefValue != "fail" {
		t.Errorf("-orphaned_dependents default = %q, want fail", odFlag.DefValue)
	}
	forceFlag := fs.Lookup("force")
	if forceFlag == nil || !strings.Contains(forceFlag.Usage, "-orphaned_dependents") {
		t.Errorf("-force usage = %v, want mention of -orphaned_dependents", forceFlag)
	}

	for _, bad := range []string{"-orphaned_dependents=bogus", "-orphaned_dependents=", "-orphaned_dependents=   "} {
		if _, err := testutil.RunCmd(t, &installCmd{}, bad, "A"); err == nil {
			t.Errorf("RunCmd(%q) succeeded, want parse error", bad)
		}
	}
}

// TestInstallCmdOrphanedDependentsMatrix verifies the full flag x googet.conf
// precedence matrix on googet install, including -force isolation and CLI
// remediation hints.
func TestInstallCmdOrphanedDependentsMatrix(t *testing.T) {
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
							{Name: "P", Arch: "noarch", Version: "1"},
							{Name: "D", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"P": "1"}},
						},
						[]goolib.PkgSpec{
							{Name: "R", Arch: "noarch", Version: "1", Replaces: []string{"P"}},
						})
					args := []string{"-sources=" + repoURL}
					if fc.set {
						args = append(args, fc.arg)
					}
					if force {
						args = append(args, "-force")
					}
					args = append(args, "R")

					status, err := testutil.RunCmd(t, &installCmd{}, args...)
					if err != nil {
						t.Fatalf("RunCmd(%v) unexpected flag error: %v", args, err)
					}
					if wantRemove {
						if status != subcommands.ExitSuccess {
							t.Fatalf("status = %v, want ExitSuccess; logs:\n%s", status, logs.String())
						}
						if diff := cmp.Diff([]string{"R.noarch.1"}, testutil.InstalledNames(t)); diff != "" {
							t.Errorf("installed (-want +got):\n%s", diff)
						}
					} else {
						if status != subcommands.ExitFailure {
							t.Fatalf("status = %v, want ExitFailure", status)
						}
						if !strings.Contains(logs.String(), "(pass -orphaned_dependents=remove to remove them)") {
							t.Errorf("logs missing remediation hint:\n%s", logs.String())
						}
						if diff := cmp.Diff([]string{"D.noarch.1", "P.noarch.1"}, testutil.InstalledNames(t)); diff != "" {
							t.Errorf("installed (-want +got):\n%s", diff)
						}
					}
				})
			}
		}
	}
}

// TestInstallCmdInteractivePromptAndMultiArgOrphans checks interactive prompt
// behaviour on both fatal name-based orphans and non-fatal Provides-only
// dependents, as well as multi-package CLI execution when one argument fails
// with orphaned dependents.
func TestInstallCmdInteractivePromptAndMultiArgOrphans(t *testing.T) {
	t.Run("interactive_orphan_error_fails_before_prompting", func(t *testing.T) {
		logs := testutil.CaptureLogs(t)
		_, repoURL := testutil.SetupCLIRepo(t, "",
			[]goolib.PkgSpec{
				{Name: "P", Arch: "noarch", Version: "1"},
				{Name: "D", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"P": "1"}},
			},
			[]goolib.PkgSpec{
				{Name: "R", Arch: "noarch", Version: "1", Replaces: []string{"P"}},
			})
		settings.Confirm = true

		var status subcommands.ExitStatus
		var runErr error
		var stdout string
		withStdin(t, "yes\n", func() {
			stdout = captureStdout(func() {
				fs := flag.NewFlagSet("install", flag.ContinueOnError)
				fs.SetOutput(io.Discard)
				var cmd installCmd
				cmd.SetFlags(fs)
				runErr = fs.Parse([]string{"-sources=" + repoURL, "R"})
				if runErr == nil {
					status = cmd.Execute(t.Context(), fs)
				}
			})
		})
		if runErr != nil || status != subcommands.ExitFailure {
			t.Fatalf("Execute = (%v, %v), want (ExitFailure, nil)", status, runErr)
		}
		if strings.Contains(stdout, "Do you wish to install") {
			t.Errorf("stdout = %q, want no prompt when orphan check fails", stdout)
		}
		if !strings.Contains(logs.String(), "(pass -orphaned_dependents=remove to remove them)") {
			t.Errorf("logs missing orphan hint:\n%s", logs.String())
		}
		if diff := cmp.Diff([]string{"D.noarch.1", "P.noarch.1"}, testutil.InstalledNames(t)); diff != "" {
			t.Errorf("installed (-want +got):\n%s", diff)
		}
	})

	t.Run("interactive_provides_only_orphan_shown_in_prompt_and_kept", func(t *testing.T) {
		logs := testutil.CaptureLogs(t)
		_, repoURL := testutil.SetupCLIRepo(t, "",
			[]goolib.PkgSpec{
				{Name: "P", Arch: "noarch", Version: "1", Provides: []string{"virt=2"}},
				{Name: "Q", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"virt": "1"}},
			},
			[]goolib.PkgSpec{
				{Name: "R", Arch: "noarch", Version: "1", Replaces: []string{"P"}},
			})
		settings.Confirm = true

		wantSection := "The following packages will have an unsatisfied dependency (not removed):\n  Q.noarch.1 (requires virt >= 1, provided by P.noarch.1; replaced by R.noarch.1)\n"
		var status subcommands.ExitStatus
		var stdout string
		withStdin(t, "n\n", func() {
			stdout = captureStdout(func() {
				fs := flag.NewFlagSet("install", flag.ContinueOnError)
				fs.SetOutput(io.Discard)
				var cmd installCmd
				cmd.SetFlags(fs)
				if err := fs.Parse([]string{"-sources=" + repoURL, "R"}); err != nil {
					t.Fatal(err)
				}
				status = cmd.Execute(t.Context(), fs)
			})
		})
		if status != subcommands.ExitSuccess {
			t.Fatalf("decline status = %v, want ExitSuccess", status)
		}
		if !strings.Contains(stdout, wantSection) || !strings.Contains(stdout, "canceling install...") {
			t.Errorf("decline stdout = %q, want %q and cancellation", stdout, wantSection)
		}
		if diff := cmp.Diff([]string{"P.noarch.1", "Q.noarch.1"}, testutil.InstalledNames(t)); diff != "" {
			t.Errorf("installed after decline (-want +got):\n%s", diff)
		}

		withStdin(t, "yes\n", func() {
			stdout = captureStdout(func() {
				fs := flag.NewFlagSet("install", flag.ContinueOnError)
				fs.SetOutput(io.Discard)
				var cmd installCmd
				cmd.SetFlags(fs)
				if err := fs.Parse([]string{"-sources=" + repoURL, "R"}); err != nil {
					t.Fatal(err)
				}
				status = cmd.Execute(t.Context(), fs)
			})
		})
		if status != subcommands.ExitSuccess {
			t.Fatalf("accept status = %v, want ExitSuccess; logs:\n%s", status, logs.String())
		}
		if !strings.Contains(logs.String(), `dependency_unsatisfied op=replace result=listed dependent=Q.noarch.1 requirement="virt >= 1" removed=P.noarch.1 replacer=R.noarch.1`) {
			t.Errorf("logs missing structured listed warning:\n%s", logs.String())
		}
		if diff := cmp.Diff([]string{"Q.noarch.1", "R.noarch.1"}, testutil.InstalledNames(t)); diff != "" {
			t.Errorf("installed after accept (-want +got):\n%s", diff)
		}
	})

	t.Run("multi_arg_install_continues_to_clean_package_and_returns_failure", func(t *testing.T) {
		_, repoURL := testutil.SetupCLIRepo(t, "",
			[]goolib.PkgSpec{
				{Name: "P", Arch: "noarch", Version: "1"},
				{Name: "D", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"P": "1"}},
			},
			[]goolib.PkgSpec{
				{Name: "Bad", Arch: "noarch", Version: "1", Replaces: []string{"P"}},
				{Name: "Good", Arch: "noarch", Version: "1"},
			})

		status, err := testutil.RunCmd(t, &installCmd{}, "-sources="+repoURL, "Bad", "Good")
		if err != nil || status != subcommands.ExitFailure {
			t.Fatalf("Execute(Bad, Good) = (%v, %v), want (ExitFailure, nil)", status, err)
		}
		if diff := cmp.Diff([]string{"D.noarch.1", "Good.noarch.1", "P.noarch.1"}, testutil.InstalledNames(t)); diff != "" {
			t.Errorf("installed (-want +got):\n%s", diff)
		}
	})
}
