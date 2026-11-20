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
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/googet/v2/goolib"
	"github.com/google/googet/v2/testutil"
	"github.com/google/subcommands"
)

// fileExists reports whether the file that package name installs into dst
// is there.
func fileExists(dst, name string) bool {
	_, err := os.Stat(filepath.Join(dst, name))
	return err == nil
}

// TestUpdateCmdFollowsRename checks the life cycle of a package renamed with
// a version reset: update leaves it alone by default, moves it to its
// successor with -follow_replaces or followreplaces in googet.conf, removing
// the old package's files and row, and finds nothing more to do on the next
// run.
func TestUpdateCmdFollowsRename(t *testing.T) {
	installed := []goolib.PkgSpec{{Name: "P", Arch: "noarch", Version: "99.0.0"}}
	repo := []goolib.PkgSpec{
		{Name: "P", Arch: "noarch", Version: "99.0.0"},
		{Name: "R", Arch: "noarch", Version: "1.36.0", Replaces: []string{"P"}, Provides: []string{"P"}},
	}
	t.Run("flag", func(t *testing.T) {
		dst, repoURL := testutil.SetupCLIRepo(t, "", installed, repo)
		status, out := runUpdate(t, "-sources="+repoURL)
		if status != subcommands.ExitSuccess || !strings.Contains(out, "No updates available for any installed packages.") {
			t.Fatalf("default update = %v, %q; want success with no updates", status, out)
		}
		if diff := cmp.Diff([]string{"P.noarch.99.0.0"}, testutil.InstalledNames(t)); diff != "" {
			t.Errorf("after default update, installed (-want +got):\n%s", diff)
		}

		status, out = runUpdate(t, "-sources="+repoURL, "-follow_replaces")
		if status != subcommands.ExitSuccess {
			t.Fatalf("update -follow_replaces = %v; output:\n%s", status, out)
		}
		wantLine := "  P.noarch, 99.0.0 --> R.noarch, 1.36.0 from " + repoURL + " (replaces P.noarch.99.0.0)\n"
		if !strings.Contains(out, wantLine) {
			t.Errorf("output missing %q:\n%s", wantLine, out)
		}
		if diff := cmp.Diff([]string{"R.noarch.1.36.0"}, testutil.InstalledNames(t)); diff != "" {
			t.Errorf("after update -follow_replaces, installed (-want +got):\n%s", diff)
		}
		if fileExists(dst, "P") || !fileExists(dst, "R") {
			t.Errorf("files: P exists %t, R exists %t; want only R", fileExists(dst, "P"), fileExists(dst, "R"))
		}

		status, out = runUpdate(t, "-sources="+repoURL, "-follow_replaces")
		if status != subcommands.ExitSuccess || !strings.Contains(out, "No updates available for any installed packages.") {
			t.Errorf("second update -follow_replaces = %v, %q; want success with no updates", status, out)
		}
		if diff := cmp.Diff([]string{"R.noarch.1.36.0"}, testutil.InstalledNames(t)); diff != "" {
			t.Errorf("after second run, installed (-want +got):\n%s", diff)
		}
	})
	for _, tc := range []struct {
		name string
		args []string
		want []string
	}{
		{name: "conf", want: []string{"R.noarch.1.36.0"}},
		{name: "conf overridden by flag", args: []string{"-follow_replaces=false"}, want: []string{"P.noarch.99.0.0"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, repoURL := testutil.SetupCLIRepo(t, "followreplaces: true\n", installed, repo)
			if status, out := runUpdate(t, append([]string{"-sources=" + repoURL}, tc.args...)...); status != subcommands.ExitSuccess {
				t.Fatalf("update %q = %v; output:\n%s", tc.args, status, out)
			}
			if diff := cmp.Diff(tc.want, testutil.InstalledNames(t)); diff != "" {
				t.Errorf("installed (-want +got):\n%s", diff)
			}
		})
	}
}

// TestUpdateCmdSplitVersusSuccessor checks that update never installs
// packages that replace P without providing it, and moves P to the single
// package that replaces and provides it, logging the split packages it
// ignored.
func TestUpdateCmdSplitVersusSuccessor(t *testing.T) {
	splits := []goolib.PkgSpec{
		{Name: "S1", Arch: "noarch", Version: "1.0.0", Replaces: []string{"P"}},
		{Name: "S2", Arch: "noarch", Version: "1.0.0", Replaces: []string{"P"}},
		{Name: "S3", Arch: "noarch", Version: "1.0.0", Replaces: []string{"P"}},
	}
	installed := []goolib.PkgSpec{{Name: "P", Arch: "noarch", Version: "1.0.0"}}
	pub := goolib.PkgSpec{Name: "P", Arch: "noarch", Version: "1.0.0"}
	successor := goolib.PkgSpec{Name: "R", Arch: "noarch", Version: "2.0.0", Replaces: []string{"P"}, Provides: []string{"P"}}
	for _, tc := range []struct {
		name     string
		repo     []goolib.PkgSpec
		args     []string
		want     []string
		wantOut  string
		wantLogs string
	}{
		{
			name:    "split packages with -follow_replaces",
			repo:    append([]goolib.PkgSpec{pub}, splits...),
			args:    []string{"-follow_replaces"},
			want:    []string{"P.noarch.1.0.0"},
			wantOut: "  P.noarch, 1.0.0: not replaced (missing_provides: S1.noarch.1.0.0, S2.noarch.1.0.0, S3.noarch.1.0.0)\n",
		},
		{
			name: "split packages once P is retired",
			repo: splits,
			want: []string{"P.noarch.1.0.0"},
		},
		{
			name:     "successor and split packages with -follow_replaces",
			repo:     append([]goolib.PkgSpec{pub, successor}, splits...),
			args:     []string{"-follow_replaces"},
			want:     []string{"R.noarch.2.0.0"},
			wantLogs: "update_replace_split_ignored package=P.noarch.1.0.0 replacer=R.noarch.2.0.0 ignored=S1.noarch.1.0.0,S2.noarch.1.0.0,S3.noarch.1.0.0",
		},
		{
			name:     "successor and split packages once P is retired",
			repo:     append([]goolib.PkgSpec{successor}, splits...),
			want:     []string{"R.noarch.2.0.0"},
			wantLogs: "update_replace_split_ignored package=P.noarch.1.0.0 replacer=R.noarch.2.0.0 ignored=S1.noarch.1.0.0,S2.noarch.1.0.0,S3.noarch.1.0.0",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs := testutil.CaptureLogs(t)
			_, repoURL := testutil.SetupCLIRepo(t, "", installed, tc.repo)
			status, out := runUpdate(t, append([]string{"-sources=" + repoURL}, tc.args...)...)
			if status != subcommands.ExitSuccess {
				t.Fatalf("update = %v; output:\n%s", status, out)
			}
			if diff := cmp.Diff(tc.want, testutil.InstalledNames(t)); diff != "" {
				t.Errorf("installed (-want +got):\n%s", diff)
			}
			if !strings.Contains(out, tc.wantOut) {
				t.Errorf("output missing %q:\n%s", tc.wantOut, out)
			}
			if !strings.Contains(logs.String(), tc.wantLogs) {
				t.Errorf("logs missing %q:\n%s", tc.wantLogs, logs.String())
			}
		})
	}
}

// TestUpdateCmdOrphanGatedReplacement checks that a replacement that would
// orphan a dependent is skipped at planning time under the default policy,
// so that P and its sibling still update in place and update exits 0, and
// that -orphaned_dependents=remove removes the dependent with P and says so
// apart from the replaced package.
func TestUpdateCmdOrphanGatedReplacement(t *testing.T) {
	installed := []goolib.PkgSpec{
		{Name: "D", Arch: "noarch", Version: "1.0.0", PkgDependencies: map[string]string{"P": "1.0.0"}},
		{Name: "P", Arch: "noarch", Version: "1.0.0"},
		{Name: "S", Arch: "noarch", Version: "1.0.0"},
	}
	repo := []goolib.PkgSpec{
		{Name: "D", Arch: "noarch", Version: "1.0.0", PkgDependencies: map[string]string{"P": "1.0.0"}},
		{Name: "P", Arch: "noarch", Version: "1.5.0"},
		{Name: "S", Arch: "noarch", Version: "1.5.0"},
		{Name: "R", Arch: "noarch", Version: "2.0.0", Replaces: []string{"P"}, Provides: []string{"P=0.5.0"}},
	}
	t.Run("fail", func(t *testing.T) {
		logs := testutil.CaptureLogs(t)
		_, repoURL := testutil.SetupCLIRepo(t, "", installed, repo)
		status, out := runUpdate(t, "-sources="+repoURL, "-follow_replaces")
		if status != subcommands.ExitSuccess {
			t.Fatalf("update = %v; output:\n%s", status, out)
		}
		if diff := cmp.Diff([]string{"D.noarch.1.0.0", "P.noarch.1.5.0", "S.noarch.1.5.0"}, testutil.InstalledNames(t)); diff != "" {
			t.Errorf("installed (-want +got):\n%s", diff)
		}
		for _, want := range []string{
			"update_replace_skipped package=P.noarch.1.0.0 replacer=R.noarch.2.0.0 reason=orphaned_dependents",
			`dependency_unsatisfied op=replace mode=fail result=skipped dependent=D.noarch.1.0.0 requirement="P >= 1.0.0" removed=P.noarch.1.0.0 replacer=R.noarch.2.0.0`,
		} {
			if !strings.Contains(logs.String(), want) {
				t.Errorf("logs missing %q:\n%s", want, logs.String())
			}
		}
		if want := "  P.noarch, 1.0.0: not replaced by R.noarch.2.0.0 (orphaned_dependents: "; !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	})
	t.Run("remove", func(t *testing.T) {
		_, repoURL := testutil.SetupCLIRepo(t, "", installed, repo)
		status, out := runUpdate(t, "-sources="+repoURL, "-follow_replaces", "-orphaned_dependents=remove")
		if status != subcommands.ExitSuccess {
			t.Fatalf("update = %v; output:\n%s", status, out)
		}
		if diff := cmp.Diff([]string{"R.noarch.2.0.0", "S.noarch.1.5.0"}, testutil.InstalledNames(t)); diff != "" {
			t.Errorf("installed (-want +got):\n%s", diff)
		}
		want := "  P.noarch, 1.0.0 --> R.noarch, 2.0.0 from " + repoURL + " (replaces P.noarch.1.0.0; removes orphaned D.noarch.1.0.0)\n"
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	})
}

// TestUpdateCmdDryRunParity checks that googet update -dry_run prints
// exactly the plan a live run prints, replacements and every kind of skipped
// replacement included, exits 0 and changes nothing.
func TestUpdateCmdDryRunParity(t *testing.T) {
	pkg := func(name, ver string, mods ...func(*goolib.PkgSpec)) goolib.PkgSpec {
		ps := goolib.PkgSpec{Name: name, Arch: "noarch", Version: ver}
		for _, m := range mods {
			m(&ps)
		}
		return ps
	}
	rp := func(e ...string) func(*goolib.PkgSpec) { return func(ps *goolib.PkgSpec) { ps.Replaces = e } }
	pv := func(e ...string) func(*goolib.PkgSpec) { return func(ps *goolib.PkgSpec) { ps.Provides = e } }
	cf := func(e ...string) func(*goolib.PkgSpec) { return func(ps *goolib.PkgSpec) { ps.Conflicts = e } }
	dp := func(name, ver string) func(*goolib.PkgSpec) {
		return func(ps *goolib.PkgSpec) { ps.PkgDependencies = map[string]string{name: ver} }
	}
	installed := []goolib.PkgSpec{
		pkg("A", "1.0.0"),
		pkg("C", "1.0.0"),
		pkg("DF", "1.0.0", dp("F", "1.0.0")),
		pkg("E", "1.0.0", rp("RE"), pv("RE")),
		pkg("F", "1.0.0"),
		pkg("G", "1.0.0"),
		pkg("H1", "1.0.0"),
		pkg("H2", "1.0.0"),
		pkg("O", "1.0.0"),
		pkg("U", "1.0.0"),
	}
	repo := []goolib.PkgSpec{
		pkg("A", "1.0.0"), pkg("SA", "1.0.0", rp("A")),
		pkg("C", "1.0.0"), pkg("RC1", "1.0.0", rp("C"), pv("C")), pkg("RC2", "1.0.0", rp("C"), pv("C")),
		pkg("DF", "1.0.0", dp("F", "1.0.0")),
		pkg("E", "1.0.0", rp("RE"), pv("RE")), pkg("RE", "1.0.0", rp("E"), pv("E")),
		pkg("F", "1.0.0"), pkg("RF", "1.0.0", rp("F"), pv("F=0.5.0")),
		pkg("G", "1.0.0"), pkg("RG", "1.0.0", rp("G"), pv("G"), cf("O")),
		pkg("H1", "1.0.0"), pkg("H2", "1.0.0"), pkg("RH", "2.0.0", rp("H1", "H2"), pv("H1", "H2")),
		pkg("O", "1.0.0"),
		pkg("U", "2.0.0"),
	}
	logs := testutil.CaptureLogs(t)
	_, repoURL := testutil.SetupCLIRepo(t, "", installed, repo)
	before := testutil.InstalledNames(t)

	status, dry := runUpdate(t, "-sources="+repoURL, "-follow_replaces", "-dry_run")
	if status != subcommands.ExitSuccess {
		t.Fatalf("update -dry_run = %v; output:\n%s", status, dry)
	}
	if diff := cmp.Diff(before, testutil.InstalledNames(t)); diff != "" {
		t.Fatalf("update -dry_run changed the installed packages (-before +after):\n%s", diff)
	}
	if !strings.HasSuffix(dry, dryRunLine) {
		t.Fatalf("dry run output doesn't end with %q:\n%s", dryRunLine, dry)
	}
	for _, want := range []string{
		"  A.noarch, 1.0.0: not replaced (missing_provides: SA.noarch.1.0.0)\n",
		"  C.noarch, 1.0.0: not replaced (ambiguous_replacers: RC1.noarch.1.0.0, RC2.noarch.1.0.0)\n",
		"  E.noarch, 1.0.0: not replaced by RE.noarch.1.0.0 (replaces_cycle)\n",
		"  F.noarch, 1.0.0: not replaced by RF.noarch.1.0.0 (orphaned_dependents: ",
		"  G.noarch, 1.0.0: not replaced by RG.noarch.1.0.0 (plan_rejected: ",
		"  H1.noarch, 1.0.0 --> RH.noarch, 2.0.0 from " + repoURL + " (replaces H1.noarch.1.0.0, H2.noarch.1.0.0)\n",
		"  U.noarch, 1.0.0 --> U.noarch, 2.0.0 from " + repoURL + "\n",
	} {
		if !strings.Contains(dry, want) {
			t.Errorf("dry run output missing %q:\n%s", want, dry)
		}
	}

	status, live := runUpdate(t, "-sources="+repoURL, "-follow_replaces")
	if status != subcommands.ExitSuccess {
		t.Fatalf("update = %v; output:\n%s\nlogs:\n%s", status, live, logs.String())
	}
	if plan := strings.TrimSuffix(dry, dryRunLine); !strings.HasPrefix(live, plan) {
		t.Errorf("live run doesn't start with the dry run's plan.\ndry run:\n%s\nlive run:\n%s", plan, live)
	}
	want := []string{"A.noarch.1.0.0", "C.noarch.1.0.0", "DF.noarch.1.0.0", "E.noarch.1.0.0", "F.noarch.1.0.0", "G.noarch.1.0.0", "O.noarch.1.0.0", "RH.noarch.2.0.0", "U.noarch.2.0.0"}
	if diff := cmp.Diff(want, testutil.InstalledNames(t)); diff != "" {
		t.Errorf("after the live run, installed (-want +got):\n%s", diff)
	}
}

// TestUpdateCmdCoexistence checks update over a database in which P is
// installed alongside a package R that replaces and provides it.
func TestUpdateCmdCoexistence(t *testing.T) {
	installed := []goolib.PkgSpec{
		{Name: "P", Arch: "noarch", Version: "1.0.0"},
		{Name: "R", Arch: "noarch", Version: "2.0.0", Replaces: []string{"P"}, Provides: []string{"P"}},
	}
	r := func(ver string) goolib.PkgSpec {
		return goolib.PkgSpec{Name: "R", Arch: "noarch", Version: ver, Replaces: []string{"P"}, Provides: []string{"P"}}
	}
	for _, tc := range []struct {
		name     string
		repo     []goolib.PkgSpec
		args     []string
		want     []string
		wantLogs string
	}{
		{
			name:     "current replacer without following Replaces",
			repo:     []goolib.PkgSpec{{Name: "P", Arch: "noarch", Version: "1.5.0"}, r("2.0.0")},
			want:     []string{"P.noarch.1.5.0", "R.noarch.2.0.0"},
			wantLogs: `package_conflict direction=reverse_replaces mode=warn result=warned installed=R.noarch.2.0.0 entry="P" incoming=P.noarch.1.5.0`,
		},
		{
			name:     "current replacer with -follow_replaces",
			repo:     []goolib.PkgSpec{{Name: "P", Arch: "noarch", Version: "1.5.0"}, r("2.0.0")},
			args:     []string{"-follow_replaces"},
			want:     []string{"P.noarch.1.0.0", "R.noarch.2.0.0"},
			wantLogs: "update_replace_coexisting_current package=P.noarch.1.0.0 replacer=R.noarch.2.0.0",
		},
		{
			name: "replacer update with -follow_replaces",
			repo: []goolib.PkgSpec{{Name: "P", Arch: "noarch", Version: "1.5.0"}, r("2.1.0")},
			args: []string{"-follow_replaces"},
			want: []string{"R.noarch.2.1.0"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs := testutil.CaptureLogs(t)
			_, repoURL := testutil.SetupCLIRepo(t, "", installed, tc.repo)
			status, out := runUpdate(t, append([]string{"-sources=" + repoURL}, tc.args...)...)
			if status != subcommands.ExitSuccess {
				t.Fatalf("update = %v; output:\n%s\nlogs:\n%s", status, out, logs.String())
			}
			if diff := cmp.Diff(tc.want, testutil.InstalledNames(t)); diff != "" {
				t.Errorf("installed (-want +got):\n%s", diff)
			}
			if !strings.Contains(logs.String(), tc.wantLogs) {
				t.Errorf("logs missing %q:\n%s", tc.wantLogs, logs.String())
			}
		})
	}
}

// TestUpdateCmdMultiVictimAndSequentialConflicts checks a live update that
// consolidates two packages into one, never reinstalling either, and one in
// which a second replacement conflicts with the first, so that it is skipped
// at planning time and its package updates in place.
func TestUpdateCmdMultiVictimAndSequentialConflicts(t *testing.T) {
	t.Run("consolidation", func(t *testing.T) {
		_, repoURL := testutil.SetupCLIRepo(t, "",
			[]goolib.PkgSpec{{Name: "P1", Arch: "noarch", Version: "1.0.0"}, {Name: "P2", Arch: "noarch", Version: "1.0.0"}},
			[]goolib.PkgSpec{
				{Name: "P1", Arch: "noarch", Version: "1.1.0"},
				{Name: "P2", Arch: "noarch", Version: "1.0.0"},
				{Name: "R", Arch: "noarch", Version: "2.0.0", Replaces: []string{"P1", "P2"}, Provides: []string{"P2"}},
			})
		status, out := runUpdate(t, "-sources="+repoURL, "-follow_replaces")
		if status != subcommands.ExitSuccess {
			t.Fatalf("update = %v; output:\n%s", status, out)
		}
		if diff := cmp.Diff([]string{"R.noarch.2.0.0"}, testutil.InstalledNames(t)); diff != "" {
			t.Errorf("installed (-want +got):\n%s", diff)
		}
		if strings.Contains(out, "P1.noarch.1.1.0") || strings.Contains(out, "missing_provides") {
			t.Errorf("output mentions an update of P1 or a skip:\n%s", out)
		}
	})
	t.Run("sequential conflicts", func(t *testing.T) {
		logs := testutil.CaptureLogs(t)
		_, repoURL := testutil.SetupCLIRepo(t, "",
			[]goolib.PkgSpec{{Name: "P1", Arch: "noarch", Version: "1.0.0"}, {Name: "P2", Arch: "noarch", Version: "1.0.0"}},
			[]goolib.PkgSpec{
				{Name: "P1", Arch: "noarch", Version: "1.0.0"},
				{Name: "P2", Arch: "noarch", Version: "1.1.0"},
				{Name: "R1", Arch: "noarch", Version: "2.0.0", Replaces: []string{"P1"}, Provides: []string{"P1"}, Conflicts: []string{"R2"}},
				{Name: "R2", Arch: "noarch", Version: "2.0.0", Replaces: []string{"P2"}, Provides: []string{"P2"}, Conflicts: []string{"R1"}},
			})
		_, dry := runUpdate(t, "-sources="+repoURL, "-follow_replaces", "-dry_run")
		status, live := runUpdate(t, "-sources="+repoURL, "-follow_replaces")
		if status != subcommands.ExitSuccess {
			t.Fatalf("update = %v; output:\n%s\nlogs:\n%s", status, live, logs.String())
		}
		if diff := cmp.Diff([]string{"P2.noarch.1.1.0", "R1.noarch.2.0.0"}, testutil.InstalledNames(t)); diff != "" {
			t.Errorf("installed (-want +got):\n%s", diff)
		}
		if want := "  P2.noarch, 1.0.0: not replaced by R2.noarch.2.0.0 (plan_rejected: "; !strings.Contains(live, want) {
			t.Errorf("output missing %q:\n%s", want, live)
		}
		if plan := strings.TrimSuffix(dry, dryRunLine); !strings.HasPrefix(live, plan) {
			t.Errorf("live run doesn't start with the dry run's plan.\ndry run:\n%s\nlive run:\n%s", plan, live)
		}
	})
}

// TestUpdateCmdGatedReplacementFailureExitsZero checks that when the
// replacement of a retired package fails to plan, update reports the skip,
// leaves the package installed and exits 0.
func TestUpdateCmdGatedReplacementFailureExitsZero(t *testing.T) {
	logs := testutil.CaptureLogs(t)
	_, repoURL := testutil.SetupCLIRepo(t, "",
		[]goolib.PkgSpec{{Name: "O", Arch: "noarch", Version: "1.0.0"}, {Name: "P", Arch: "noarch", Version: "1.0.0"}},
		[]goolib.PkgSpec{
			{Name: "O", Arch: "noarch", Version: "1.0.0"},
			{Name: "R", Arch: "noarch", Version: "2.0.0", Replaces: []string{"P"}, Provides: []string{"P"}, Conflicts: []string{"O"}},
		})
	status, out := runUpdate(t, "-sources="+repoURL)
	if status != subcommands.ExitSuccess {
		t.Fatalf("update = %v; output:\n%s", status, out)
	}
	if diff := cmp.Diff([]string{"O.noarch.1.0.0", "P.noarch.1.0.0"}, testutil.InstalledNames(t)); diff != "" {
		t.Errorf("installed (-want +got):\n%s", diff)
	}
	if want := "update_replace_skipped package=P.noarch.1.0.0 replacer=R.noarch.2.0.0 reason=plan_rejected"; !strings.Contains(logs.String(), want) {
		t.Errorf("logs missing %q:\n%s", want, logs.String())
	}
}
