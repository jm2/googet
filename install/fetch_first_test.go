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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/googet/v2/client"
	"github.com/google/googet/v2/goolib"
	"github.com/google/googet/v2/testutil"
)

// emptySHA256 is the checksum of an empty file.
var emptySHA256 = func() string {
	sum := sha256.Sum256(nil)
	return hex.EncodeToString(sum[:])
}()

// treeDigest returns the SHA-256 of every regular file under dir, keyed by
// its slash-separated path relative to dir.
func treeDigest(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		out[filepath.ToSlash(rel)] = checksumOf(t, path)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// hostState is everything an install can change on the host: the database
// rows, the installed files and the archive cache.
type hostState struct {
	rows  []string
	dst   map[string]string
	cache map[string]string
}

// hostState captures f's database rows, installed files and cache.
func (f *relFixture) hostState(t *testing.T) hostState {
	t.Helper()
	state, err := f.db.FetchPkgs("")
	if err != nil {
		t.Fatal(err)
	}
	var rows []string
	for _, st := range state {
		b, err := json.Marshal(st)
		if err != nil {
			t.Fatal(err)
		}
		rows = append(rows, string(b))
	}
	return hostState{rows: rows, dst: treeDigest(t, f.dst), cache: treeDigest(t, f.cache)}
}

// totalGets returns how many archive requests the repo has served.
func (f *relFixture) totalGets() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.gets {
		n += c
	}
	return n
}

// installWith installs the latest available version of name with opts.
func (f *relFixture) installWith(t *testing.T, opts Options, name string) error {
	t.Helper()
	return New(f.cache, f.db, opts).FromRepo(t.Context(), goolib.PackageInfo{Name: name}, "", f.rm, corpusArchs, f.downloader)
}

// checkOnlyArchivesAdded fails t unless after equals before, except for
// archives added to the cache whose checksum the repo publishes for that file
// name and for the empty files named in partial, which a failed download
// leaves behind.
func checkOnlyArchivesAdded(t *testing.T, f *relFixture, before, after hostState, partial ...string) {
	t.Helper()
	if diff := cmp.Diff(before.rows, after.rows); diff != "" {
		t.Errorf("database rows changed (-before +after):\n%s", diff)
	}
	if diff := cmp.Diff(before.dst, after.dst); diff != "" {
		t.Errorf("installed files changed (-before +after):\n%s", diff)
	}
	published := map[string]string{}
	for _, r := range f.rm {
		for _, rs := range r.Packages {
			published[rs.Source] = rs.Checksum
		}
	}
	for name, sum := range before.cache {
		if after.cache[name] != sum {
			t.Errorf("cached file %s changed or was deleted", name)
		}
	}
	for name, sum := range after.cache {
		if _, ok := before.cache[name]; ok {
			continue
		}
		switch {
		case slices.Contains(partial, name):
			if sum != emptySHA256 {
				t.Errorf("cached file %s: want the empty file a failed download leaves", name)
			}
		case published[name] != sum:
			t.Errorf("cache gained %s, which isn't a verified archive", name)
		}
	}
}

// TestPlanningFailureChangesNothing checks that an install that fails while
// it is planned downloads nothing and leaves the database, the installed
// files and the cache exactly as they were, on the first attempt and on a
// second one.
func TestPlanningFailureChangesNothing(t *testing.T) {
	forwardConflict := struct{ installed, available []goolib.PkgSpec }{
		installed: []goolib.PkgSpec{{Name: "X", Arch: "noarch", Version: "1"}},
		available: []goolib.PkgSpec{
			{Name: "A", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"B": "1", "C": "1"}},
			{Name: "B", Arch: "noarch", Version: "1"},
			{Name: "C", Arch: "noarch", Version: "1", Conflicts: []string{"X"}},
		},
	}
	for _, tc := range []struct {
		name      string
		installed []goolib.PkgSpec
		available []goolib.PkgSpec
		// target is the package to install; the default is A.
		target    string
		mode      ConflictMode
		wantErr   string
		wantErrIs error
		wantLog   string
	}{
		{
			name: "unresolvable dependency after a resolvable one",
			available: []goolib.PkgSpec{
				{Name: "A", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"B": "1", "Z": "1"}},
				{Name: "B", Arch: "noarch", Version: "1"},
			},
			wantErr: "no package found satisfying Z",
		},
		{
			name: "dependency cycle",
			available: []goolib.PkgSpec{
				{Name: "A", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"B": "1"}},
				{Name: "B", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"A": "1"}},
			},
			wantErr: "dependency cycle",
		},
		{name: "forward conflict in warn mode", installed: forwardConflict.installed, available: forwardConflict.available, mode: ConflictWarn, wantErr: "conflict with installed package or provider: X"},
		{name: "forward conflict in error mode", installed: forwardConflict.installed, available: forwardConflict.available, mode: ConflictError, wantErr: "conflict with installed package or provider: X"},
		{name: "forward conflict in ignore mode", installed: forwardConflict.installed, available: forwardConflict.available, mode: ConflictIgnore, wantErr: "conflict with installed package or provider: X"},
		{
			name:      "reverse conflict in error mode",
			installed: []goolib.PkgSpec{{Name: "X", Arch: "noarch", Version: "1", Conflicts: []string{"C"}}},
			available: []goolib.PkgSpec{
				{Name: "A", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"B": "1", "C": "1"}},
				{Name: "B", Arch: "noarch", Version: "1"},
				{Name: "C", Arch: "noarch", Version: "1"},
			},
			mode:      ConflictError,
			wantErrIs: ErrPackageConflict,
			wantLog:   "ERROR: package_conflict direction=reverse mode=error result=failed installed=X.noarch.1",
		},
		{
			name: "orphaned dependent in default fail mode",
			installed: []goolib.PkgSpec{
				{Name: "P", Arch: "noarch", Version: "1"},
				{Name: "D", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"P": "1"}},
			},
			available: []goolib.PkgSpec{
				{Name: "B", Arch: "noarch", Version: "1"},
				{Name: "R", Arch: "noarch", Version: "1", Replaces: []string{"P"}, PkgDependencies: map[string]string{"B": "1"}},
			},
			target:    "R",
			wantErr:   "D.noarch.1 (requires P >= 1, broken by removing P.noarch.1 for R.noarch.1)",
			wantErrIs: ErrOrphanedDependents,
		},
		{
			name: "replacement whose dependents form a cycle",
			installed: []goolib.PkgSpec{
				{Name: "P", Arch: "noarch", Version: "1"},
				{Name: "A", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"P": "1", "B": "1"}},
				{Name: "B", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"A": "1"}},
			},
			available: []goolib.PkgSpec{
				{Name: "D", Arch: "noarch", Version: "1"},
				{Name: "R", Arch: "noarch", Version: "1", Replaces: []string{"P"}, Conflicts: []string{"P"}, PkgDependencies: map[string]string{"D": "1"}},
			},
			target:  "R",
			wantErr: "depend on each other in a cycle",
			wantLog: "INFO: Replacement simulation for R.noarch.1 did not complete; deferring conflict checks until after replacements",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRelFixture(t, tc.installed, tc.available)
			target := "A"
			if tc.target != "" {
				target = tc.target
			}
			before := f.hostState(t)
			for attempt := 1; attempt <= 2; attempt++ {
				logs := testutil.CaptureLogs(t)
				err := f.installWith(t, Options{PackageConflicts: tc.mode}, target)
				switch {
				case err == nil:
					t.Fatalf("attempt %d: install %s succeeded, want an error", attempt, target)
				case tc.wantErrIs != nil && !errors.Is(err, tc.wantErrIs):
					t.Fatalf("attempt %d: install %s = %v, want errors.Is %v", attempt, target, err, tc.wantErrIs)
				case !strings.Contains(err.Error(), tc.wantErr):
					t.Fatalf("attempt %d: install %s = %v, want an error containing %q", attempt, target, err, tc.wantErr)
				}
				if diff := cmp.Diff(before, f.hostState(t), cmp.AllowUnexported(hostState{})); diff != "" {
					t.Errorf("attempt %d: host state changed (-before +after):\n%s", attempt, diff)
				}
				if n := f.totalGets(); n != 0 {
					t.Errorf("attempt %d: %d archives downloaded, want none", attempt, n)
				}
				var lines []string
				for _, l := range parseLogs(logs.String()) {
					lines = append(lines, l.level+": "+l.text)
					if !isPlanningLog(l.text) {
						t.Errorf("attempt %d: log line %q isn't one that planning writes", attempt, l.text)
					}
				}
				if tc.wantLog != "" && !slices.ContainsFunc(lines, func(l string) bool { return strings.HasPrefix(l, tc.wantLog) }) {
					t.Errorf("attempt %d: logs = %q, want a line starting with %q", attempt, lines, tc.wantLog)
				}
			}
		})
	}
}

// TestFetchFailureLeavesOnlyVerifiedArchives checks that an install that
// fails while it fetches archives changes nothing but the cache, which only
// gains the archives verified before the failure, and that a second attempt
// downloads none of those again and leaves the cache as the first one did.
func TestFetchFailureLeavesOnlyVerifiedArchives(t *testing.T) {
	type fetchCase struct {
		name      string
		installed []goolib.PkgSpec
		available []goolib.PkgSpec
		opts      Options
		sabotage  func(t *testing.T, f *relFixture)
		wantErr   string
		// partial lists the empty files a failed download leaves in the
		// cache.
		partial []string
		// onceEach lists the archives downloaded once on the first attempt
		// and never again.
		onceEach []string
		// never lists the archives never requested.
		never []string
	}
	fourDownloads := []goolib.PkgSpec{
		{Name: "T", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"D": "1", "E": "1", "F": "1"}},
		{Name: "D", Arch: "noarch", Version: "1"},
		{Name: "E", Arch: "noarch", Version: "1"},
		{Name: "F", Arch: "noarch", Version: "1"},
	}
	// The archives are fetched in install order: the dependencies in sorted
	// order, then T.
	fetchOrder := []string{"D", "E", "F", "T"}
	var cases []fetchCase
	for k, failing := range fetchOrder {
		var before, after []string
		for _, name := range fetchOrder[:k] {
			before = append(before, name+".noarch.1.goo")
		}
		for _, name := range fetchOrder[k+1:] {
			after = append(after, name+".noarch.1.goo")
		}
		cases = append(cases, fetchCase{
			name:      fmt.Sprintf("download %d of 4 fails", k+1),
			available: fourDownloads,
			sabotage:  func(t *testing.T, f *relFixture) { f.unpublish(t, failing) },
			wantErr:   "unexpected status",
			partial:   []string{failing + ".noarch.1.goo"},
			onceEach:  before,
			never:     after,
		})
	}
	for _, tc := range append(cases, []fetchCase{
		{
			name: "an archive fails its checksum",
			available: []goolib.PkgSpec{
				{Name: "T", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"D": "1", "E": "1", "F": "1"}},
				{Name: "D", Arch: "noarch", Version: "1"},
				{Name: "E", Arch: "noarch", Version: "1"},
				{Name: "F", Arch: "noarch", Version: "1"},
			},
			sabotage: func(t *testing.T, f *relFixture) { f.corrupt(t, "E") },
			wantErr:  "checksum doesn't match",
			onceEach: []string{"D.noarch.1.goo"},
			never:    []string{"F.noarch.1.goo", "T.noarch.1.goo"},
		},
		{
			name: "a replaced package's archive is missing and gone from the repo",
			installed: []goolib.PkgSpec{
				{Name: "P", Arch: "noarch", Version: "1"},
				{Name: "Q", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"P": "1"}},
			},
			available: []goolib.PkgSpec{
				{Name: "T", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"D": "1"}, Replaces: []string{"P"}},
				{Name: "D", Arch: "noarch", Version: "1"},
			},
			opts: Options{OrphanedDependents: OrphanRemove},
			sabotage: func(t *testing.T, f *relFixture) {
				for _, dir := range []string{f.cache, f.gooDir} {
					if err := os.Remove(filepath.Join(dir, "P.noarch.1.goo")); err != nil {
						t.Fatal(err)
					}
				}
			},
			wantErr:  "cannot install T.noarch.1: installing it removes P.noarch.1, which has no usable cached archive",
			partial:  []string{"P.noarch.1.goo"},
			onceEach: []string{"D.noarch.1.goo", "T.noarch.1.goo"},
			never:    []string{"Q.noarch.1.goo"},
		},
	}...) {
		t.Run(tc.name, func(t *testing.T) {
			f := newRelFixture(t, tc.installed, tc.available)
			tc.sabotage(t, f)
			before := f.hostState(t)
			var first hostState
			for attempt := 1; attempt <= 2; attempt++ {
				if err := f.installWith(t, tc.opts, "T"); err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("attempt %d: install T = %v, want an error containing %q", attempt, err, tc.wantErr)
				}
				after := f.hostState(t)
				checkOnlyArchivesAdded(t, f, before, after, tc.partial...)
				if attempt == 1 {
					first = after
				} else if diff := cmp.Diff(first.cache, after.cache); diff != "" {
					t.Errorf("the second attempt changed the cache (-first +second):\n%s", diff)
				}
				for _, a := range tc.onceEach {
					if n := f.getCount(a); n != 1 {
						t.Errorf("attempt %d: %s downloaded %d times, want 1", attempt, a, n)
					}
				}
				for _, a := range tc.never {
					if n := f.getCount(a); n != 0 {
						t.Errorf("attempt %d: %s downloaded %d times, want 0", attempt, a, n)
					}
				}
			}
		})
	}
}

// TestDBOnlyReplaceLeavesReplacedArchives checks that a database-only install
// that replaces a package neither downloads nor deletes the archives of the
// packages it removes.
func TestDBOnlyReplaceLeavesReplacedArchives(t *testing.T) {
	f := newRelFixture(t,
		[]goolib.PkgSpec{
			{Name: "P", Arch: "noarch", Version: "1"},
			{Name: "Q", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"P": "1"}},
		},
		[]goolib.PkgSpec{{Name: "R", Arch: "noarch", Version: "1", Replaces: []string{"P"}}})
	before := f.hostState(t)
	if err := f.installWith(t, Options{DBOnly: true, OrphanedDependents: OrphanRemove}, "R"); err != nil {
		t.Fatalf("database-only install of R: %v", err)
	}
	if diff := cmp.Diff([]string{"R.noarch.1"}, f.installed(t)); diff != "" {
		t.Errorf("installed packages (-want +got):\n%s", diff)
	}
	after := f.hostState(t)
	for _, a := range []string{"P.noarch.1.goo", "Q.noarch.1.goo"} {
		if after.cache[a] != before.cache[a] {
			t.Errorf("replaced package archive %s changed or was deleted", a)
		}
		if n := f.getCount(a); n != 0 {
			t.Errorf("%s downloaded %d times, want 0", a, n)
		}
	}
	if diff := cmp.Diff(before.dst, after.dst); diff != "" {
		t.Errorf("a database-only install changed installed files (-before +after):\n%s", diff)
	}
}

// TestPlanningOutputPrecedesExecution checks that an install writes every log
// line and console message that planning produces, with the same text and in
// the same order as buildPlan reports them, before anything is downloaded or
// installed, in each conflict mode.
func TestPlanningOutputPrecedesExecution(t *testing.T) {
	installed := []goolib.PkgSpec{
		{Name: "P", Arch: "noarch", Version: "1"},
		{Name: "X", Arch: "noarch", Version: "1", Conflicts: []string{"E"}},
	}
	available := []goolib.PkgSpec{
		{Name: "T", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"D": "1", "E": "1"}, Replaces: []string{"P"}, Conflicts: []string{"P"}},
		{Name: "D", Arch: "noarch", Version: "1"},
		{Name: "E", Arch: "noarch", Version: "1"},
	}
	for _, tc := range []struct {
		mode    ConflictMode
		wantErr bool
		// wantLine is a planning log line the mode must produce.
		wantLine string
	}{
		{mode: ConflictWarn, wantLine: "WARN: package_conflict direction=reverse mode=warn result=warned installed=X.noarch.1"},
		{mode: ConflictIgnore, wantLine: "INFO: package_conflict direction=reverse mode=ignore result=ignored installed=X.noarch.1"},
		{mode: ConflictError, wantErr: true, wantLine: "ERROR: package_conflict direction=reverse mode=error result=failed installed=X.noarch.1"},
	} {
		t.Run(tc.mode.String(), func(t *testing.T) {
			f := newRelFixture(t, installed, available)
			state, err := f.db.FetchPkgs("")
			if err != nil {
				t.Fatal(err)
			}
			want := runPlan(corpusCase{Target: "T", Mode: tc.mode}, f.rm, state).Events
			logs := testutil.CaptureLogs(t)
			stdout := captureStdout(t, func() { err = f.installWith(t, Options{PackageConflicts: tc.mode}, "T") })
			if (err != nil) != tc.wantErr {
				t.Fatalf("install T = %v, want error: %t", err, tc.wantErr)
			}
			var got []string
			firstExec := -1
			for i, l := range parseLogs(logs.String()) {
				if !isPlanningLog(l.text) {
					if firstExec < 0 {
						firstExec = i
					}
					continue
				}
				if firstExec >= 0 {
					t.Errorf("planning log line %q comes after execution log lines", l.text)
				}
				got = append(got, l.level+": "+l.text)
			}
			if tc.wantErr && firstExec >= 0 {
				t.Errorf("a failed plan still wrote execution log lines")
			}
			lines := strings.Split(strings.TrimSuffix(stdout, "\n"), "\n")
			firstDone := slices.IndexFunc(lines, func(l string) bool { return strings.HasPrefix(l, "Installation of ") })
			for i, l := range lines {
				if !isPlanningOutput(l) {
					continue
				}
				if firstDone >= 0 && i > firstDone {
					t.Errorf("planning console message %q comes after an installation finished", l)
				}
				got = append(got, "STDOUT: "+l)
			}
			if diff := cmp.Diff(want, got); diff != "" {
				t.Errorf("planning output (-buildPlan +install):\n%s", diff)
			}
			if !slices.ContainsFunc(got, func(l string) bool { return strings.HasPrefix(l, tc.wantLine) }) {
				t.Errorf("planning output = %q, want a line starting with %q", got, tc.wantLine)
			}
			if !slices.Contains(got, `INFO: conflict "P" with P.noarch.1 skipped: installing T.noarch.1 removes it`) {
				t.Errorf("planning output = %q, want the line for the conflict the replacement clears", got)
			}
		})
	}
}

// TestDivergenceStopsWithoutFurtherChanges checks that when the installed
// packages stop matching the plan, the install logs plan_divergence at error
// level, naming the step and the check, and changes nothing after the point
// where the divergence appeared, except that packages the failing step had
// already removed are reinstalled. Steps completed before it stay.
func TestDivergenceStopsWithoutFurtherChanges(t *testing.T) {
	for _, tc := range []struct {
		name      string
		available []goolib.PkgSpec
		target    string
		// at is the stage, and step, at which the installed packages change.
		at     applyStage
		atStep string
		change client.PackageState
		check  string
		want   []string
		// recovered is set when the step had removed packages before the
		// divergence, which are reinstalled.
		recovered bool
	}{
		{
			name: "removal set changes after an earlier step",
			available: []goolib.PkgSpec{
				{Name: "T", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"D": "1"}, Replaces: []string{"P"}},
				{Name: "D", Arch: "noarch", Version: "1"},
			},
			target: "T",
			at:     stageCommitted,
			atStep: "D.noarch.1",
			change: client.PackageState{PackageSpec: &goolib.PkgSpec{Name: "Q", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"P": "1"}}},
			check:  "removal-set",
			want:   []string{"D.noarch.1", "P.noarch.1", "Q.noarch.1"},
		},
		{
			name:      "conflict reappears after replacing",
			available: []goolib.PkgSpec{{Name: "R", Arch: "noarch", Version: "1", Conflicts: []string{"P"}, Replaces: []string{"P"}}},
			target:    "R",
			at:        stageAfterReplace,
			atStep:    "R.noarch.1",
			change:    client.PackageState{PackageSpec: &goolib.PkgSpec{Name: "P", Arch: "x86_64", Version: "1"}},
			check:     "conflict-recheck",
			want:      []string{"P.noarch.1", "P.x86_64.1"},
			recovered: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRelFixture(t, []goolib.PkgSpec{{Name: "P", Arch: "noarch", Version: "1"}}, tc.available)
			ri := &repoInstall{Installer: New(f.cache, f.db, Options{}), rm: f.rm, archs: corpusArchs, downloader: f.downloader}
			var atChange hostState
			ri.stageHook = func(stage applyStage, step string) error {
				if stage != tc.at || step != tc.atStep {
					return nil
				}
				if err := f.db.AddPkg(tc.change); err != nil {
					return err
				}
				atChange = f.hostState(t)
				return nil
			}
			logs := testutil.CaptureLogs(t)
			err := ri.fromRepo(t.Context(), goolib.PackageInfo{Name: tc.target}, "")
			if !errors.Is(err, errPlanDivergence) {
				t.Fatalf("install %s = %v, want errPlanDivergence", tc.target, err)
			}
			if atChange.rows == nil {
				t.Fatalf("the installed packages were never changed")
			}
			if tc.recovered {
				if !f.fileExists("P") {
					t.Errorf("P's file is missing after recovery")
				}
			} else if diff := cmp.Diff(atChange, f.hostState(t), cmp.AllowUnexported(hostState{})); diff != "" {
				t.Errorf("host state changed after the divergence (-at divergence +after):\n%s", diff)
			}
			if diff := cmp.Diff(tc.want, f.installed(t)); diff != "" {
				t.Errorf("installed packages (-want +got):\n%s", diff)
			}
			wantLog := "plan_divergence step=" + tc.target + ".noarch.1 check=" + tc.check + ": "
			if !slices.ContainsFunc(parseLogs(logs.String()), func(l liveLine) bool {
				return l.level == "ERROR" && strings.HasPrefix(l.text, wantLog)
			}) {
				t.Errorf("logs:\n%s\nwant an error line starting with %q", logs, wantLog)
			}
		})
	}
}

// TestReplaceOfPackageInstalledEarlierInRun checks a replacement that removes
// a package that an earlier step of the same install installed: the install
// succeeds without needing that package's archive before it runs, and each
// archive is downloaded once.
func TestReplaceOfPackageInstalledEarlierInRun(t *testing.T) {
	for _, dbOnly := range []bool{false, true} {
		t.Run(map[bool]string{false: "full", true: "database only"}[dbOnly], func(t *testing.T) {
			f := newRelFixture(t, nil, []goolib.PkgSpec{
				{Name: "T", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"A": "1", "B": "1"}},
				{Name: "A", Arch: "noarch", Version: "1"},
				{Name: "B", Arch: "noarch", Version: "1", Replaces: []string{"A"}},
			})
			if err := f.installWith(t, Options{DBOnly: dbOnly}, "T"); err != nil {
				t.Fatalf("install T: %v", err)
			}
			if diff := cmp.Diff([]string{"B.noarch.1", "T.noarch.1"}, f.installed(t)); diff != "" {
				t.Errorf("installed packages (-want +got):\n%s", diff)
			}
			if !dbOnly && f.fileExists("A") {
				t.Errorf("A's file is still installed after B replaced it")
			}
			for _, a := range []string{"A.noarch.1.goo", "B.noarch.1.goo", "T.noarch.1.goo"} {
				if n := f.getCount(a); n != 1 {
					t.Errorf("%s downloaded %d times, want 1", a, n)
				}
			}
		})
	}
}

// TestReinstallThatReplacesItselfRecordsItsArchive checks that a package that
// replaces its own installed copy at the same version installs from the
// copy's archive, which removing the copy keeps and which is the same
// archive, without downloading it, and records an archive that exists.
func TestReinstallThatReplacesItselfRecordsItsArchive(t *testing.T) {
	spec := goolib.PkgSpec{Name: "X", Arch: "noarch", Version: "2", Replaces: []string{"X"}}
	f := newRelFixture(t, []goolib.PkgSpec{spec}, []goolib.PkgSpec{spec})
	if err := f.install(t, "X"); err != nil {
		t.Fatalf("install X: %v", err)
	}
	if n := f.getCount("X.noarch.2.goo"); n != 0 {
		t.Errorf("X's archive downloaded %d times, want 0", n)
	}
	row, err := f.db.FetchPkg(goolib.PackageInfo{Name: "X", Arch: "noarch"})
	if err != nil {
		t.Fatal(err)
	}
	if got := checksumOf(t, row.LocalPath); got != row.Checksum {
		t.Errorf("recorded archive %s has checksum %s, want %s", row.LocalPath, got, row.Checksum)
	}
}

// TestInstallFailureKeepsEarlierSteps checks what happens when a package's
// install script fails after earlier steps of the same install ran: the
// dependencies installed before it stay installed, and the packages it
// replaced are reinstalled.
func TestInstallFailureKeepsEarlierSteps(t *testing.T) {
	failing := goolib.ExecFile{Path: "missing-install-script.exe"}
	for _, tc := range []struct {
		name      string
		available []goolib.PkgSpec
		want      []string
		gone      []string
	}{
		{
			name: "dependency",
			available: []goolib.PkgSpec{
				{Name: "T", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"D": "1"}, Install: failing},
				{Name: "D", Arch: "noarch", Version: "1"},
			},
			want: []string{"D.noarch.1", "P.noarch.1"},
			gone: []string{"T"},
		},
		{
			name: "dependency and replacement",
			available: []goolib.PkgSpec{
				{Name: "T", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"D": "1"}, Replaces: []string{"P"}, Install: failing},
				{Name: "D", Arch: "noarch", Version: "1"},
			},
			want: []string{"D.noarch.1", "P.noarch.1"},
			gone: []string{"T"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRelFixture(t, []goolib.PkgSpec{{Name: "P", Arch: "noarch", Version: "1"}}, tc.available)
			err := f.install(t, "T")
			if err == nil || errors.Is(err, errPlanDivergence) {
				t.Fatalf("install T = %v, want the install script's error", err)
			}
			if diff := cmp.Diff(tc.want, f.installed(t)); diff != "" {
				t.Errorf("installed packages (-want +got):\n%s", diff)
			}
			if !f.fileExists("D") {
				t.Errorf("D's file is missing")
			}
			for _, name := range tc.gone {
				if f.fileExists(name) {
					t.Errorf("%s's file exists", name)
				}
			}
		})
	}
}

// rewriteRow replaces the database row of the installed package name.noarch
// with what edit makes of it.
func rewriteRow(t *testing.T, f *relFixture, name string, edit func(*client.PackageState)) client.PackageState {
	t.Helper()
	row, err := f.db.FetchPkg(goolib.PackageInfo{Name: name, Arch: "noarch"})
	if err != nil || row.PackageSpec == nil {
		t.Fatalf("FetchPkg(%s) = %+v, %v", name, row, err)
	}
	edit(&row)
	if err := f.db.AddPkg(row); err != nil {
		t.Fatal(err)
	}
	return row
}

// TestReplacedArchiveOfOlderRows checks how an install finds the archive of
// a package it replaces when that package's row was written by a local
// install, which records no checksum or download URL, or by an older GooGet,
// which records only an unpack directory: the install succeeds while the
// archive is cached and otherwise fails before anything changes, naming
// where the archive should be.
func TestReplacedArchiveOfOlderRows(t *testing.T) {
	for _, tc := range []struct {
		name    string
		edit    func(*client.PackageState)
		missing bool
		wantErr string
	}{
		{
			name: "local install, archive cached",
			edit: func(r *client.PackageState) { r.Checksum, r.DownloadURL, r.SourceRepo = "", "", "" },
		},
		{
			name:    "local install, archive missing",
			edit:    func(r *client.PackageState) { r.Checksum, r.DownloadURL, r.SourceRepo = "", "", "" },
			missing: true,
			wantErr: "can not redownload P.noarch.1, DownloadURL not saved",
		},
		{
			name: "older GooGet, archive cached",
			edit: func(r *client.PackageState) { r.UnpackDir, r.LocalPath = strings.TrimSuffix(r.LocalPath, ".goo"), "" },
		},
		{
			name:    "older GooGet, archive missing and gone from the repo",
			edit:    func(r *client.PackageState) { r.UnpackDir, r.LocalPath = strings.TrimSuffix(r.LocalPath, ".goo"), "" },
			missing: true,
			wantErr: "error redownloading P.noarch.1",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRelFixture(t,
				[]goolib.PkgSpec{{Name: "P", Arch: "noarch", Version: "1"}},
				[]goolib.PkgSpec{{Name: "R", Arch: "noarch", Version: "1", Replaces: []string{"P"}}})
			archive := filepath.Join(f.cache, "P.noarch.1.goo")
			rewriteRow(t, f, "P", tc.edit)
			if tc.missing {
				for _, p := range []string{archive, filepath.Join(f.gooDir, "P.noarch.1.goo")} {
					if err := os.Remove(p); err != nil {
						t.Fatal(err)
					}
				}
			}
			before := f.hostState(t)
			err := f.install(t, "R")
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("install R: %v", err)
				}
				if diff := cmp.Diff([]string{"R.noarch.1"}, f.installed(t)); diff != "" {
					t.Errorf("installed packages (-want +got):\n%s", diff)
				}
				if f.fileExists("P") {
					t.Errorf("P's file is still installed after R replaced it")
				}
				if n := f.getCount("P.noarch.1.goo"); n != 0 {
					t.Errorf("P's archive downloaded %d times, want 0", n)
				}
				return
			}
			for _, want := range []string{
				"cannot install R.noarch.1: installing it removes P.noarch.1, which has no usable cached archive",
				tc.wantErr,
				"restore the archive at " + archive,
			} {
				if err == nil || !strings.Contains(err.Error(), want) {
					t.Errorf("install R = %v, want an error containing %q", err, want)
				}
			}
			var partial []string
			if !strings.Contains(tc.wantErr, "not saved") {
				partial = []string{"P.noarch.1.goo"}
			}
			checkOnlyArchivesAdded(t, f, before, f.hostState(t), partial...)
		})
	}
}

// TestArchiveDamagedAfterFetchIsFetchedAgain checks that an archive that
// changes between being fetched and being installed is verified again and
// downloaded once more, rather than installed as it is.
func TestArchiveDamagedAfterFetchIsFetchedAgain(t *testing.T) {
	f := newRelFixture(t, nil, []goolib.PkgSpec{{Name: "T", Arch: "noarch", Version: "1"}})
	ri := &repoInstall{Installer: New(f.cache, f.db, Options{}), rm: f.rm, archs: corpusArchs, downloader: f.downloader}
	archive := filepath.Join(f.cache, "T.noarch.1.goo")
	ri.stageHook = func(stage applyStage, _ string) error {
		if stage != stageFetched {
			return nil
		}
		return os.WriteFile(archive, []byte("damaged"), 0644)
	}
	if err := ri.fromRepo(t.Context(), goolib.PackageInfo{Name: "T"}, ""); err != nil {
		t.Fatalf("install T: %v", err)
	}
	if n := f.getCount("T.noarch.1.goo"); n != 2 {
		t.Errorf("T's archive downloaded %d times, want 2", n)
	}
	if !f.fileExists("T") {
		t.Errorf("T's file is missing")
	}
	row, err := f.db.FetchPkg(goolib.PackageInfo{Name: "T", Arch: "noarch"})
	if err != nil {
		t.Fatal(err)
	}
	if got := checksumOf(t, row.LocalPath); got != row.Checksum {
		t.Errorf("recorded archive %s has checksum %s, want %s", row.LocalPath, got, row.Checksum)
	}
}
