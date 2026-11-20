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

package remove

import (
	"bytes"
	"flag"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/google/googet/v2/client"
	"github.com/google/googet/v2/googetdb"
	"github.com/google/googet/v2/goolib"
	"github.com/google/googet/v2/remove"
	"github.com/google/googet/v2/settings"
	"github.com/google/googet/v2/testutil"
	"github.com/google/subcommands"
)

func setupRemoveDB(t *testing.T, confirm bool, specs []goolib.PkgSpec) (*googetdb.GooDB, *client.Downloader) {
	t.Helper()
	settings.Initialize(t.TempDir(), confirm)
	db, err := googetdb.NewDB(settings.DBFile())
	if err != nil {
		t.Fatalf("googetdb.NewDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	downloader, err := client.NewDownloader("")
	if err != nil {
		t.Fatalf("client.NewDownloader: %v", err)
	}
	gooDir, logDir := t.TempDir(), t.TempDir()
	var state client.GooGetState
	for _, spec := range specs {
		rs := testutil.GenGoo(t, gooDir, logDir, spec)
		state = append(state, client.PackageState{
			PackageSpec: rs.PackageSpec,
			LocalPath:   filepath.Join(gooDir, rs.PackageSpec.String()+".goo"),
			Checksum:    rs.Checksum,
		})
	}
	if err := db.WriteStateToDB(state); err != nil {
		t.Fatalf("db.WriteStateToDB: %v", err)
	}
	return db, downloader
}

func installedNames(t *testing.T, db *googetdb.GooDB) []string {
	t.Helper()
	state, err := db.FetchPkgs("")
	if err != nil {
		t.Fatalf("db.FetchPkgs: %v", err)
	}
	var out []string
	for _, ps := range state {
		if ps.PackageSpec != nil {
			out = append(out, ps.PackageSpec.String())
		}
	}
	slices.Sort(out)
	return out
}

func withStdin(t *testing.T, input string, fn func()) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	if _, err := w.WriteString(input); err != nil {
		t.Fatalf("WriteString: %v", err)
	}
	w.Close()
	oldStdin := os.Stdin
	os.Stdin = r
	defer func() {
		os.Stdin = oldStdin
		r.Close()
	}()
	fn()
}

func hasWarningLog(buf *bytes.Buffer, want string) bool {
	for _, line := range strings.Split(buf.String(), "\n") {
		if strings.HasPrefix(line, "WARN :") && strings.Contains(line, want) {
			return true
		}
	}
	return false
}

// TestRemoveListsProvidesDependentsWithoutRemovingThem checks that removing a
// provider keeps the removal set equal to remove.EnumerateDeps while listing
// Provides-only dependents on stdout and in a structured warning log line.
func TestRemoveListsProvidesDependentsWithoutRemovingThem(t *testing.T) {
	logs := testutil.CaptureLogs(t)
	db, downloader := setupRemoveDB(t, false, []goolib.PkgSpec{
		{Name: "P", Arch: "noarch", Version: "1", Provides: []string{"virt=2"}},
		{Name: "D", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"P": "1"}, Provides: []string{"subvirt"}},
		{Name: "Q", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"virt": "2"}},
		{Name: "R", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"subvirt": ""}},
	})

	_, wantDL, err := remove.EnumerateDeps(goolib.PackageInfo{Name: "P", Arch: "noarch"}, db)
	if err != nil {
		t.Fatalf("EnumerateDeps: %v", err)
	}
	if diff := cmp.Diff([]string{"D.noarch 1", "P.noarch 1"}, wantDL, cmpopts.SortSlices(func(a, b string) bool { return a < b })); diff != "" {
		t.Fatalf("EnumerateDeps mismatch (-want +got):\n%s", diff)
	}

	cmd := removeCmd{}
	stdout := captureStdout(func() {
		if err := cmd.removeOne(t.Context(), "P", downloader, db); err != nil {
			t.Fatalf("removeOne: %v", err)
		}
	})

	// Q and R depend only on provided names, so neither is removed.
	if diff := cmp.Diff([]string{"Q.noarch.1", "R.noarch.1"}, installedNames(t, db)); diff != "" {
		t.Errorf("installed state after remove (-want +got):\n%s", diff)
	}

	for _, want := range []string{
		"The following packages will have an unsatisfied dependency (not removed):",
		"  Q.noarch.1 (requires virt >= 2, provided by P.noarch.1)",
		"  R.noarch.1 (requires subvirt, provided by D.noarch.1)",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout missing %q:\n%s", want, stdout)
		}
	}

	for _, wantLog := range []string{
		`dependency_unsatisfied op=remove result=listed dependent=Q.noarch.1 requirement="virt >= 2" removed=P.noarch.1`,
		`dependency_unsatisfied op=remove result=listed dependent=R.noarch.1 requirement="subvirt" removed=D.noarch.1`,
	} {
		if !hasWarningLog(logs, wantLog) {
			t.Errorf("logs missing warning %q; got:\n%s", wantLog, logs.String())
		}
	}
}

// TestRemoveSecondProviderSuppressesListing checks that a dependent satisfied
// by another remaining provider is not listed on stdout or in the log.
func TestRemoveSecondProviderSuppressesListing(t *testing.T) {
	logs := testutil.CaptureLogs(t)
	db, downloader := setupRemoveDB(t, false, []goolib.PkgSpec{
		{Name: "P1", Arch: "noarch", Version: "1", Provides: []string{"virt=2"}},
		{Name: "P2", Arch: "noarch", Version: "1", Provides: []string{"virt=2"}},
		{Name: "Q", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"virt": "2"}},
	})

	cmd := removeCmd{}
	stdout := captureStdout(func() {
		if err := cmd.removeOne(t.Context(), "P1", downloader, db); err != nil {
			t.Fatalf("removeOne: %v", err)
		}
	})

	if diff := cmp.Diff([]string{"P2.noarch.1", "Q.noarch.1"}, installedNames(t, db)); diff != "" {
		t.Errorf("installed state (-want +got):\n%s", diff)
	}
	if strings.Contains(stdout, "unsatisfied dependency") {
		t.Errorf("unexpected unsatisfied-dependency section in stdout:\n%s", stdout)
	}
	if strings.Contains(logs.String(), "dependency_unsatisfied") {
		t.Errorf("unexpected dependency_unsatisfied log line:\n%s", logs.String())
	}
}

// TestRemoveDryRunAndPromptListProvidesDependents checks that both -dry_run and
// interactive confirmation prompts show Provides-only dependents without
// mutating state when dry-run or declined.
func TestRemoveDryRunAndPromptListProvidesDependents(t *testing.T) {
	t.Run("dry run", func(t *testing.T) {
		logs := testutil.CaptureLogs(t)
		db, downloader := setupRemoveDB(t, false, []goolib.PkgSpec{
			{Name: "P", Arch: "noarch", Version: "1", Provides: []string{"virt=1"}},
			{Name: "Q", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"virt": "1"}},
		})
		cmd := removeCmd{dryRun: true}
		stdout := captureStdout(func() {
			if err := cmd.removeOne(t.Context(), "P", downloader, db); err != nil {
				t.Fatalf("removeOne(dryRun): %v", err)
			}
		})
		for _, want := range []string{
			"Dry run: The following packages would be removed:",
			"The following packages will have an unsatisfied dependency (not removed):",
			"  Q.noarch.1 (requires virt >= 1, provided by P.noarch.1)",
		} {
			if !strings.Contains(stdout, want) {
				t.Errorf("dry-run stdout missing %q:\n%s", want, stdout)
			}
		}
		if diff := cmp.Diff([]string{"P.noarch.1", "Q.noarch.1"}, installedNames(t, db)); diff != "" {
			t.Errorf("dry-run modified DB (-want +got):\n%s", diff)
		}
		if strings.Contains(logs.String(), "dependency_unsatisfied") {
			t.Errorf("dry-run emitted unexpected log line:\n%s", logs.String())
		}
	})

	t.Run("prompt declined", func(t *testing.T) {
		logs := testutil.CaptureLogs(t)
		db, downloader := setupRemoveDB(t, true, []goolib.PkgSpec{
			{Name: "P", Arch: "noarch", Version: "1", Provides: []string{"virt=1"}},
			{Name: "Q", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"virt": "1"}},
		})
		cmd := removeCmd{}
		var stdout string
		withStdin(t, "n\n", func() {
			stdout = captureStdout(func() {
				if err := cmd.removeOne(t.Context(), "P", downloader, db); err != nil {
					t.Fatalf("removeOne(declined): %v", err)
				}
			})
		})
		for _, want := range []string{
			"The following packages will be removed:",
			"The following packages will have an unsatisfied dependency (not removed):",
			"  Q.noarch.1 (requires virt >= 1, provided by P.noarch.1)",
			"canceling removal...",
		} {
			if !strings.Contains(stdout, want) {
				t.Errorf("declined prompt stdout missing %q:\n%s", want, stdout)
			}
		}
		if diff := cmp.Diff([]string{"P.noarch.1", "Q.noarch.1"}, installedNames(t, db)); diff != "" {
			t.Errorf("declined prompt modified DB (-want +got):\n%s", diff)
		}
		if strings.Contains(logs.String(), "dependency_unsatisfied") {
			t.Errorf("declined prompt emitted unexpected log line:\n%s", logs.String())
		}
	})

	t.Run("prompt accepted", func(t *testing.T) {
		logs := testutil.CaptureLogs(t)
		db, downloader := setupRemoveDB(t, true, []goolib.PkgSpec{
			{Name: "P", Arch: "noarch", Version: "1", Provides: []string{"virt=1"}},
			{Name: "Q", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"virt": "1"}},
		})
		cmd := removeCmd{}
		var stdout string
		withStdin(t, "y\n", func() {
			stdout = captureStdout(func() {
				if err := cmd.removeOne(t.Context(), "P", downloader, db); err != nil {
					t.Fatalf("removeOne(accepted): %v", err)
				}
			})
		})
		if count := strings.Count(stdout, "The following packages will have an unsatisfied dependency (not removed):"); count != 1 {
			t.Errorf("unsatisfied header printed %d times, want 1:\n%s", count, stdout)
		}
		if diff := cmp.Diff([]string{"Q.noarch.1"}, installedNames(t, db)); diff != "" {
			t.Errorf("accepted prompt DB (-want +got):\n%s", diff)
		}
		wantLog := `dependency_unsatisfied op=remove result=listed dependent=Q.noarch.1 requirement="virt >= 1" removed=P.noarch.1`
		if !hasWarningLog(logs, wantLog) {
			t.Errorf("logs missing warning %q; got:\n%s", wantLog, logs.String())
		}
	})
}

// TestRemoveMultipleRemovedProvidersFormatsStructuredLogWithoutSpace checks
// that when multiple packages in the removal cascade provided a requirement,
// stdout separates them with ", " for readability while the unquoted removed=
// field in the structured warning log joins them with "," without whitespace.
func TestRemoveMultipleRemovedProvidersFormatsStructuredLogWithoutSpace(t *testing.T) {
	logs := testutil.CaptureLogs(t)
	db, downloader := setupRemoveDB(t, false, []goolib.PkgSpec{
		{Name: "P", Arch: "noarch", Version: "1", Provides: []string{"virt=2"}},
		{Name: "D", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"P": "1"}, Provides: []string{"virt=3"}},
		{Name: "Q", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"virt": "2"}},
	})

	cmd := removeCmd{}
	stdout := captureStdout(func() {
		if err := cmd.removeOne(t.Context(), "P", downloader, db); err != nil {
			t.Fatalf("removeOne: %v", err)
		}
	})

	if diff := cmp.Diff([]string{"Q.noarch.1"}, installedNames(t, db)); diff != "" {
		t.Errorf("installed state (-want +got):\n%s", diff)
	}
	wantStdout := "  Q.noarch.1 (requires virt >= 2, provided by D.noarch.1, P.noarch.1)"
	if !strings.Contains(stdout, wantStdout) {
		t.Errorf("stdout missing %q:\n%s", wantStdout, stdout)
	}
	wantLog := `dependency_unsatisfied op=remove result=listed dependent=Q.noarch.1 requirement="virt >= 2" removed=D.noarch.1,P.noarch.1`
	if !hasWarningLog(logs, wantLog) {
		t.Errorf("logs missing whitespace-free removed= field %q; got:\n%s", wantLog, logs.String())
	}
}

// TestRemoveExecuteMultiArgAndRemainingBelowMinProvider checks end-to-end
// Execute across multiple package arguments and verifies that arch-qualified
// requirements and remaining providers below the required minimum version are
// formatted and reported accurately.
func TestRemoveExecuteMultiArgAndRemainingBelowMinProvider(t *testing.T) {
	t.Run("multi_arg_execute_lists_when_last_provider_is_removed", func(t *testing.T) {
		logs := testutil.CaptureLogs(t)
		settings.Initialize(t.TempDir(), false)
		db, err := googetdb.NewDB(settings.DBFile())
		if err != nil {
			t.Fatalf("googetdb.NewDB: %v", err)
		}
		if err := db.WriteStateToDB(client.GooGetState{
			{PackageSpec: &goolib.PkgSpec{Name: "P1", Arch: "noarch", Version: "1", Provides: []string{"virt=2"}}},
			{PackageSpec: &goolib.PkgSpec{Name: "P2", Arch: "noarch", Version: "1", Provides: []string{"virt=2"}}},
			{PackageSpec: &goolib.PkgSpec{Name: "Q", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"virt": "2"}}},
		}); err != nil {
			t.Fatalf("db.WriteStateToDB: %v", err)
		}
		if err := db.Close(); err != nil {
			t.Fatalf("db.Close: %v", err)
		}

		cmd := removeCmd{}
		fs := flag.NewFlagSet("remove", flag.ContinueOnError)
		cmd.SetFlags(fs)
		if err := fs.Parse([]string{"-db_only", "P1", "P2"}); err != nil {
			t.Fatalf("fs.Parse: %v", err)
		}

		var status subcommands.ExitStatus
		stdout := captureStdout(func() {
			status = cmd.Execute(t.Context(), fs)
		})
		if status != subcommands.ExitSuccess {
			t.Fatalf("Execute status = %v, want %v", status, subcommands.ExitSuccess)
		}

		reopened, err := googetdb.NewDB(settings.DBFile())
		if err != nil {
			t.Fatalf("googetdb.NewDB reopen: %v", err)
		}
		defer reopened.Close()
		if diff := cmp.Diff([]string{"Q.noarch.1"}, installedNames(t, reopened)); diff != "" {
			t.Errorf("installed state (-want +got):\n%s", diff)
		}
		if count := strings.Count(stdout, "The following packages will have an unsatisfied dependency (not removed):"); count != 1 {
			t.Errorf("unsatisfied header count = %d, want 1:\n%s", count, stdout)
		}
		wantStdout := "  Q.noarch.1 (requires virt >= 2, provided by P2.noarch.1)"
		if !strings.Contains(stdout, wantStdout) {
			t.Errorf("stdout missing %q:\n%s", wantStdout, stdout)
		}
		wantLog := `dependency_unsatisfied op=remove result=listed dependent=Q.noarch.1 requirement="virt >= 2" removed=P2.noarch.1`
		if !hasWarningLog(logs, wantLog) {
			t.Errorf("logs missing warning %q; got:\n%s", wantLog, logs.String())
		}
	})

	t.Run("arch_qualified_requirement_and_below_minimum_remaining_provider", func(t *testing.T) {
		logs := testutil.CaptureLogs(t)
		db, downloader := setupRemoveDB(t, false, []goolib.PkgSpec{
			{Name: "P1", Arch: "noarch", Version: "1", Provides: []string{"virt=2", "archvirt=1"}},
			{Name: "P2", Arch: "noarch", Version: "1", Provides: []string{"virt=1"}},
			{Name: "Q", Arch: "x86_64", Version: "1", PkgDependencies: map[string]string{
				"virt.x86_64":     "2",
				"archvirt.x86_64": "",
			}},
		})

		cmd := removeCmd{}
		stdout := captureStdout(func() {
			if err := cmd.removeOne(t.Context(), "P1", downloader, db); err != nil {
				t.Fatalf("removeOne: %v", err)
			}
		})
		for _, want := range []string{
			"  Q.x86_64.1 (requires archvirt.x86_64, provided by P1.noarch.1)",
			"  Q.x86_64.1 (requires virt.x86_64 >= 2, provided by P1.noarch.1)",
		} {
			if !strings.Contains(stdout, want) {
				t.Errorf("stdout missing %q:\n%s", want, stdout)
			}
		}
		for _, wantLog := range []string{
			`dependency_unsatisfied op=remove result=listed dependent=Q.x86_64.1 requirement="archvirt.x86_64" removed=P1.noarch.1`,
			`dependency_unsatisfied op=remove result=listed dependent=Q.x86_64.1 requirement="virt.x86_64 >= 2" removed=P1.noarch.1`,
		} {
			if !hasWarningLog(logs, wantLog) {
				t.Errorf("logs missing warning %q; got:\n%s", wantLog, logs.String())
			}
		}
	})
}

// TestRemoveCorruptRemainingVersionLogsWarningWithoutBlockingRemoval checks
// that a version comparison error in providesOrphans logs a warning and still
// lets the requested package removal succeed.
func TestRemoveCorruptRemainingVersionLogsWarningWithoutBlockingRemoval(t *testing.T) {
	logs := testutil.CaptureLogs(t)
	settings.Initialize(t.TempDir(), false)
	db, err := googetdb.NewDB(settings.DBFile())
	if err != nil {
		t.Fatalf("googetdb.NewDB: %v", err)
	}
	defer db.Close()
	if err := db.WriteStateToDB(client.GooGetState{
		{PackageSpec: &goolib.PkgSpec{Name: "P", Arch: "noarch", Version: "1", Provides: []string{"virt=2"}}},
		{PackageSpec: &goolib.PkgSpec{Name: "virt", Arch: "x86_64", Version: "notaver"}},
		{PackageSpec: &goolib.PkgSpec{Name: "Q", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"virt": "2"}}},
	}); err != nil {
		t.Fatalf("db.WriteStateToDB: %v", err)
	}
	downloader, err := client.NewDownloader("")
	if err != nil {
		t.Fatalf("client.NewDownloader: %v", err)
	}

	cmd := removeCmd{dbOnly: true}
	_ = captureStdout(func() {
		if err := cmd.removeOne(t.Context(), "P", downloader, db); err != nil {
			t.Fatalf("removeOne: %v", err)
		}
	})

	if diff := cmp.Diff([]string{"Q.noarch.1", "virt.x86_64.notaver"}, installedNames(t, db)); diff != "" {
		t.Errorf("installed state (-want +got):\n%s", diff)
	}
	if !hasWarningLog(logs, `Checking unsatisfied dependencies for "P":`) {
		t.Errorf("expected unsatisfied-dependency check warning in logs, got:\n%s", logs.String())
	}
}

// TestRemoveSetMatchesEnumerateDepsProperty checks over random multi-arch,
// dotted-name, identity, and Provides dependency graphs that removeCmd removes
// the exact same set of packages as remove.EnumerateDeps.
func TestRemoveSetMatchesEnumerateDepsProperty(t *testing.T) {
	rng := rand.New(rand.NewPCG(313, 2026))
	names := []string{"A", "B", "C", "foo", "foo.bar"}
	archs := []string{"noarch", "x86_64"}
	vers := []string{"1", "2"}
	provs := []string{"virt", "virt=1", "virt=2", "sub.virt=1"}

	downloader, err := client.NewDownloader("")
	if err != nil {
		t.Fatalf("client.NewDownloader: %v", err)
	}

	for seed := range 80 {
		settings.Initialize(t.TempDir(), false)
		db, err := googetdb.NewDB(settings.DBFile())
		if err != nil {
			t.Fatalf("seed %d: googetdb.NewDB: %v", seed, err)
		}

		var state client.GooGetState
		for idx, n := range names {
			for _, a := range archs {
				if rng.IntN(2) == 0 {
					continue
				}
				deps := map[string]string{}
				// Only depend on earlier names so non-root cycles cannot form.
				for prev := range idx {
					if rng.IntN(3) == 0 {
						key := names[prev]
						if rng.IntN(3) == 0 {
							key += "." + archs[rng.IntN(len(archs))]
						}
						deps[key] = vers[rng.IntN(len(vers))]
					}
				}
				if rng.IntN(2) == 0 {
					deps["virt"] = vers[rng.IntN(len(vers))]
				}
				var pv []string
				if rng.IntN(2) == 0 {
					pv = append(pv, provs[rng.IntN(len(provs))])
				}
				ps := &goolib.PkgSpec{
					Name:            n,
					Arch:            a,
					Version:         vers[rng.IntN(len(vers))],
					PkgDependencies: deps,
					Provides:        pv,
				}
				state = append(state, client.PackageState{PackageSpec: ps})
			}
		}
		if len(state) == 0 {
			db.Close()
			continue
		}
		if err := db.WriteStateToDB(state); err != nil {
			db.Close()
			t.Fatalf("seed %d: WriteStateToDB: %v", seed, err)
		}

		chosen := state[rng.IntN(len(state))].PackageSpec
		target := chosen.Name + "." + chosen.Arch
		rootPI := goolib.PackageInfo{Name: chosen.Name, Arch: chosen.Arch}
		if rng.IntN(2) == 0 {
			target = state[rng.IntN(len(state))].PackageSpec.Name
			// archs is iterated in ascending order ("noarch", "x86_64") with at
			// most one row per (Name, Arch), so the first matching Name in state
			// equals SQLite's ORDER BY pkg_arch, pkg_ver pick independently of
			// lookupInstalledPkg.
			for _, st := range state {
				if st.PackageSpec.Name == target {
					rootPI = goolib.PackageInfo{Name: st.PackageSpec.Name, Arch: st.PackageSpec.Arch}
					break
				}
			}
		}
		enumDeps, _, err := remove.EnumerateDeps(rootPI, db)
		if err != nil {
			db.Close()
			t.Fatalf("seed %d: EnumerateDeps(%+v): %v", seed, rootPI, err)
		}
		var wantRemaining []string
		for _, st := range state {
			k := st.PackageSpec.Name + "." + st.PackageSpec.Arch
			if _, removed := enumDeps[k]; !removed {
				wantRemaining = append(wantRemaining, st.PackageSpec.String())
			}
		}
		slices.Sort(wantRemaining)

		cmd := removeCmd{dbOnly: true}
		_ = captureStdout(func() {
			if err := cmd.removeOne(t.Context(), target, downloader, db); err != nil {
				t.Fatalf("seed %d: removeOne(%q): %v", seed, target, err)
			}
		})
		gotRemaining := installedNames(t, db)
		db.Close()

		if diff := cmp.Diff(wantRemaining, gotRemaining); diff != "" {
			t.Fatalf("seed %d target %q: removal set diverged from EnumerateDeps (-wantRemaining +gotRemaining):\n%s", seed, target, diff)
		}
	}
}
