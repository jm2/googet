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
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/googet/v2/client"
	"github.com/google/googet/v2/goolib"
	"github.com/google/googet/v2/settings"
	"github.com/google/googet/v2/testutil"
)

// TestReverseReplacesMatchingPure checks which Replaces entries of installed
// packages reverseConflicts reports against an incoming package: entries
// match the incoming package's own name, arch and version only, never its
// Provides, and an entry naming it with an unparseable version is reported
// as unparseable.
func TestReverseReplacesMatchingPure(t *testing.T) {
	p := func(arch, ver string, provides ...string) *goolib.PkgSpec {
		return &goolib.PkgSpec{Name: "P", Arch: arch, Version: ver, Provides: provides}
	}
	r := func(replaces ...string) *goolib.PkgSpec {
		return &goolib.PkgSpec{Name: "R", Arch: "noarch", Version: "1.0.0", Replaces: replaces}
	}
	type hit struct {
		Kind        reverseKind
		Installed   string
		Entry       string
		Unparseable bool
	}
	for _, tc := range []struct {
		name      string
		installed []*goolib.PkgSpec
		incoming  *goolib.PkgSpec
		ex        exclusions
		want      []hit
	}{
		{
			name:      "bare name matches",
			installed: []*goolib.PkgSpec{r("P")},
			incoming:  p("noarch", "1.0.0"),
			want:      []hit{{revReplaces, "R.noarch.1.0.0", "P", false}},
		},
		{
			name:      "incoming Provides never matches",
			installed: []*goolib.PkgSpec{r("P", "virt")},
			incoming:  &goolib.PkgSpec{Name: "Q", Arch: "noarch", Version: "1.0.0", Provides: []string{"P", "virt=2.0.0"}},
		},
		{
			name:      "arch qualifier mismatch does not match",
			installed: []*goolib.PkgSpec{r("P.x86_64")},
			incoming:  p("noarch", "1.0.0"),
		},
		{
			name:      "arch qualifier match",
			installed: []*goolib.PkgSpec{r("P.x86_64")},
			incoming:  p("x86_64", "1.0.0"),
			want:      []hit{{revReplaces, "R.noarch.1.0.0", "P.x86_64", false}},
		},
		{
			name:      "below minimum version does not match",
			installed: []*goolib.PkgSpec{r("P.noarch.2.0.0")},
			incoming:  p("noarch", "1.9.9"),
		},
		{
			name:      "at minimum version matches",
			installed: []*goolib.PkgSpec{r("P.noarch.2.0.0")},
			incoming:  p("noarch", "2.0.0"),
			want:      []hit{{revReplaces, "R.noarch.1.0.0", "P.noarch.2.0.0", false}},
		},
		{
			name:      "above minimum version matches",
			installed: []*goolib.PkgSpec{r("P.noarch.2.0.0")},
			incoming:  p("noarch", "3.0.0"),
			want:      []hit{{revReplaces, "R.noarch.1.0.0", "P.noarch.2.0.0", false}},
		},
		{
			name:      "unparseable entry version naming the package is unparseable",
			installed: []*goolib.PkgSpec{r("P.noarch.notaver")},
			incoming:  p("noarch", "1.0.0"),
			want:      []hit{{revReplaces, "R.noarch.1.0.0", "P.noarch.notaver", true}},
		},
		{
			name:      "unparseable incoming version against bare entry is unparseable",
			installed: []*goolib.PkgSpec{r("P")},
			incoming:  p("noarch", "notaver"),
			want:      []hit{{revReplaces, "R.noarch.1.0.0", "P", true}},
		},
		{
			name:      "unparseable entry naming another package is ignored",
			installed: []*goolib.PkgSpec{r("Q.noarch.notaver")},
			incoming:  p("noarch", "1.0.0"),
		},
		{
			name:      "own name.arch row is excluded on an in-place upgrade",
			installed: []*goolib.PkgSpec{{Name: "P", Arch: "x86_64", Version: "1.0.0", Replaces: []string{"P"}}},
			incoming:  p("x86_64", "2.0.0"),
			ex:        selfExclusion(p("x86_64", "2.0.0")),
		},
		{
			name:      "another arch of the same name is still reported",
			installed: []*goolib.PkgSpec{{Name: "P", Arch: "x86_64", Version: "1.0.0", Replaces: []string{"P"}}},
			incoming:  p("x86_32", "2.0.0"),
			ex:        selfExclusion(p("x86_32", "2.0.0")),
			want:      []hit{{revReplaces, "P.x86_64.1.0.0", "P", false}},
		},
		{
			name:      "excluded victim row is not reported",
			installed: []*goolib.PkgSpec{r("P")},
			incoming:  p("noarch", "1.0.0"),
			ex:        exclusions{victims: map[string]bool{"R.noarch": true}},
		},
		{
			name:      "Conflicts hit comes before Replaces hit of the same package",
			installed: []*goolib.PkgSpec{{Name: "R", Arch: "noarch", Version: "1.0.0", Conflicts: []string{"P"}, Replaces: []string{"P"}}},
			incoming:  p("noarch", "1.0.0"),
			want: []hit{
				{revConflict, "R.noarch.1.0.0", "P", false},
				{revReplaces, "R.noarch.1.0.0", "P", false},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got []hit
			for _, h := range reverseConflicts(tc.incoming, tc.installed, tc.ex) {
				got = append(got, hit{h.kind, h.installed.String(), h.entry, h.unparseable})
			}
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("reverseConflicts hits (-want +got):\n%s", diff)
			}
		})
	}
}

// TestReverseReplacesModes checks how FromRepo and FromDisk handle installing
// a package that an installed package replaces, under each package conflict
// mode: warn installs with a structured warning and a console warning, error
// fails with ErrPackageConflict and changes nothing, and ignore installs with
// an informational log line only.
func TestReverseReplacesModes(t *testing.T) {
	installedR := goolib.PkgSpec{Name: "R", Arch: "noarch", Version: "1.0.0", Replaces: []string{"P"}}
	incomingP := goolib.PkgSpec{Name: "P", Arch: "noarch", Version: "1.0.0"}
	for _, tc := range []struct {
		mode       ConflictMode
		wantResult string
		wantTag    string
		wantWarn   bool
		wantErr    bool
		wantState  []string
	}{
		{mode: ConflictWarn, wantResult: "warned", wantTag: "WARN : ", wantWarn: true, wantState: []string{"P.noarch.1.0.0", "R.noarch.1.0.0"}},
		{mode: ConflictError, wantResult: "failed", wantTag: "ERROR: ", wantErr: true, wantState: []string{"R.noarch.1.0.0"}},
		{mode: ConflictIgnore, wantResult: "ignored", wantTag: "INFO : ", wantState: []string{"P.noarch.1.0.0", "R.noarch.1.0.0"}},
	} {
		for _, via := range []string{"FromRepo", "FromDisk"} {
			t.Run(tc.mode.String()+"/"+via, func(t *testing.T) {
				logs := testutil.CaptureLogs(t)
				f := newRelFixture(t, []goolib.PkgSpec{installedR}, []goolib.PkgSpec{incomingP})
				ins := New(f.cache, f.db, Options{PackageConflicts: tc.mode})
				var err error
				out := captureConsole(t, func() {
					if via == "FromRepo" {
						err = ins.FromRepo(t.Context(), goolib.PackageInfo{Name: "P"}, "", f.rm, []string{"noarch"}, f.downloader)
						return
					}
					dir := t.TempDir()
					rs := testutil.GenGoo(t, dir, f.dst, incomingP)
					err = ins.FromDisk(filepath.Join(dir, rs.Source), false)
				})
				if tc.wantErr {
					want := `package conflict: cannot install P.noarch.1.0.0: installed package R.noarch.1.0.0 replaces it (entry "P"; package conflict mode is error)`
					if !errors.Is(err, ErrPackageConflict) || err.Error() != want {
						t.Errorf("%s = %v, want ErrPackageConflict %q", via, err, want)
					}
				} else if err != nil {
					t.Fatalf("%s: %v", via, err)
				}
				if diff := cmp.Diff(tc.wantState, f.installed(t)); diff != "" {
					t.Errorf("installed packages (-want +got):\n%s", diff)
				}
				wantLine := conflictLine(revReplaces, tc.mode, tc.wantResult, &installedR, "P", &incomingP)
				var lines int
				for _, line := range strings.Split(logs.String(), "\n") {
					if strings.Contains(line, wantLine) {
						lines++
						if !strings.HasPrefix(line, tc.wantTag) {
							t.Errorf("log line %q has the wrong level, want prefix %q", line, tc.wantTag)
						}
					}
				}
				if lines != 1 {
					t.Errorf("count of %q = %d, want 1; logs:\n%s", wantLine, lines, logs.String())
				}
				if strings.Contains(logs.String(), "direction=reverse ") {
					t.Errorf("Replaces hit logged as a reverse Conflicts hit; logs:\n%s", logs.String())
				}
				wantStdout := `Warning: installed package R.noarch.1.0.0 replaces P.noarch.1.0.0 (entry "P"); installing anyway (package_conflicts=warn)`
				if got := strings.Contains(out, wantStdout); got != tc.wantWarn {
					t.Errorf("stdout contains %q = %t, want %t; stdout:\n%s", wantStdout, got, tc.wantWarn, out)
				}
			})
		}
	}
}

// TestReverseReplacesInPlanWarnings checks that Installer.Plan reports the
// console warning for an allowed reverse Replaces hit and writes nothing
// itself.
func TestReverseReplacesInPlanWarnings(t *testing.T) {
	f := newRelFixture(t,
		[]goolib.PkgSpec{{Name: "R", Arch: "noarch", Version: "1.0.0", Replaces: []string{"P"}}},
		[]goolib.PkgSpec{{Name: "P", Arch: "noarch", Version: "1.0.0"}})
	logs := testutil.CaptureLogs(t)
	var p *Plan
	var err error
	out := captureConsole(t, func() {
		p, err = New(f.cache, f.db, Options{}).Plan(goolib.PackageInfo{Name: "P"}, "", f.rm, []string{"noarch"})
	})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if logs.Len() != 0 || out != "" {
		t.Errorf("Plan wrote log lines %q and console output %q, want none", logs.String(), out)
	}
	want := "The following packages will be installed:\n  P.noarch.1.0.0\n" +
		`Warning: installed package R.noarch.1.0.0 replaces P.noarch.1.0.0 (entry "P"); installing anyway (package_conflicts=warn)` + "\n"
	if diff := cmp.Diff(want, p.String()); diff != "" {
		t.Errorf("String() (-want +got):\n%s", diff)
	}
}

// TestReverseReplacesUnparseableNeverFails checks that a Replaces entry
// naming the incoming package with a version that can't be compared logs a
// result=unparseable warning in every mode and never fails the install.
func TestReverseReplacesUnparseableNeverFails(t *testing.T) {
	for _, mode := range []ConflictMode{ConflictWarn, ConflictError, ConflictIgnore} {
		t.Run(mode.String(), func(t *testing.T) {
			logs := testutil.CaptureLogs(t)
			f := newRelFixture(t,
				[]goolib.PkgSpec{{Name: "R", Arch: "noarch", Version: "1.0.0", Replaces: []string{"P.noarch.notaver"}}},
				[]goolib.PkgSpec{{Name: "P", Arch: "noarch", Version: "1.0.0"}})
			ins := New(f.cache, f.db, Options{PackageConflicts: mode})
			if err := ins.FromRepo(t.Context(), goolib.PackageInfo{Name: "P"}, "", f.rm, []string{"noarch"}, f.downloader); err != nil {
				t.Fatalf("FromRepo under %s: %v", mode, err)
			}
			want := fmt.Sprintf(`package_conflict direction=reverse_replaces mode=%s result=unparseable installed=R.noarch.1.0.0 entry="P.noarch.notaver" incoming=P.noarch.1.0.0`, mode)
			var lines int
			for _, line := range strings.Split(logs.String(), "\n") {
				if strings.Contains(line, want) {
					lines++
					if !strings.HasPrefix(line, "WARN : ") {
						t.Errorf("log line %q is not a warning", line)
					}
				}
			}
			if lines != 1 {
				t.Errorf("count of %q = %d, want 1; logs:\n%s", want, lines, logs.String())
			}
			if diff := cmp.Diff([]string{"P.noarch.1.0.0", "R.noarch.1.0.0"}, f.installed(t)); diff != "" {
				t.Errorf("installed packages (-want +got):\n%s", diff)
			}
		})
	}
}

// TestReverseReplacesMutualVictimIsExempt checks that installing R, whose
// Replaces entry removes the installed P that in turn replaces R, logs no
// reverse Replaces hit in any mode because P is gone once R is installed.
func TestReverseReplacesMutualVictimIsExempt(t *testing.T) {
	for _, mode := range []ConflictMode{ConflictWarn, ConflictError, ConflictIgnore} {
		t.Run(mode.String(), func(t *testing.T) {
			logs := testutil.CaptureLogs(t)
			f := newRelFixture(t,
				[]goolib.PkgSpec{{Name: "P", Arch: "noarch", Version: "1.0.0", Replaces: []string{"R"}}},
				[]goolib.PkgSpec{{Name: "R", Arch: "noarch", Version: "1.0.0", Replaces: []string{"P"}}})
			ins := New(f.cache, f.db, Options{PackageConflicts: mode})
			if err := ins.FromRepo(t.Context(), goolib.PackageInfo{Name: "R"}, "", f.rm, []string{"noarch"}, f.downloader); err != nil {
				t.Fatalf("FromRepo(R) under %s: %v", mode, err)
			}
			if diff := cmp.Diff([]string{"R.noarch.1.0.0"}, f.installed(t)); diff != "" {
				t.Errorf("installed packages (-want +got):\n%s", diff)
			}
			if strings.Contains(logs.String(), "package_conflict") {
				t.Errorf("unexpected package_conflict log for a removed victim:\n%s", logs.String())
			}
		})
	}
}

// TestReverseReplacesSurvivingVictimRechecks checks the deferred reverse
// Replaces checks for an exempted victim that survives the replacement: the
// pre-replacement check fails in error mode only, the post-replacement check
// applies every mode, unparseable entries never fail, and a victim that is
// gone produces no events.
func TestReverseReplacesSurvivingVictimRechecks(t *testing.T) {
	r := &goolib.PkgSpec{Name: "R", Arch: "noarch", Version: "1.0.0", Replaces: []string{"P.noarch.2.0.0"}}
	survivor := func(replaces string) client.GooGetState {
		return client.GooGetState{
			{PackageSpec: &goolib.PkgSpec{Name: "P", Arch: "noarch", Version: "1.0.0", Replaces: []string{replaces}}},
		}
	}
	def := deferredChecks{rev: map[string]bool{"P.noarch": true}}

	t.Run("pre-replacement error mode fails on a survivor", func(t *testing.T) {
		sink := &recordingSink{}
		err := def.recheckBeforeReplacementsIn(r, ConflictError, survivor("R"), sink)
		want := `package conflict: cannot install R.noarch.1.0.0: installed package P.noarch.1.0.0 replaces it (entry "R"; package conflict mode is error)`
		if !errors.Is(err, ErrPackageConflict) || err.Error() != want {
			t.Fatalf("recheckBeforeReplacementsIn = %v, want %q", err, want)
		}
		wantEvents := []event{{levelError, `package_conflict direction=reverse_replaces mode=error result=failed installed=P.noarch.1.0.0 entry="R" incoming=R.noarch.1.0.0`}}
		if diff := cmp.Diff(wantEvents, sink.events, cmp.AllowUnexported(event{})); diff != "" {
			t.Errorf("events (-want +got):\n%s", diff)
		}
	})

	t.Run("pre-replacement warn mode defers to the post-replacement check", func(t *testing.T) {
		sink := &recordingSink{}
		if err := def.recheckBeforeReplacementsIn(r, ConflictWarn, survivor("R"), sink); err != nil || len(sink.events) != 0 {
			t.Errorf("recheckBeforeReplacementsIn = %v with events %v, want nil and none", err, sink.events)
		}
	})

	t.Run("pre-replacement error mode drops unparseable entries", func(t *testing.T) {
		sink := &recordingSink{}
		if err := def.recheckBeforeReplacementsIn(r, ConflictError, survivor("R.noarch.notaver"), sink); err != nil || len(sink.events) != 0 {
			t.Errorf("recheckBeforeReplacementsIn = %v with events %v, want nil and none", err, sink.events)
		}
	})

	t.Run("post-replacement warn mode warns on a survivor", func(t *testing.T) {
		sink := &recordingSink{}
		if err := def.recheckIn(r, ConflictWarn, survivor("R"), sink); err != nil {
			t.Fatalf("recheckIn: %v", err)
		}
		wantEvents := []event{
			{levelWarning, `package_conflict direction=reverse_replaces mode=warn result=warned installed=P.noarch.1.0.0 entry="R" incoming=R.noarch.1.0.0`},
			{levelStdout, "Warning: installed package P.noarch.1.0.0 replaces R.noarch.1.0.0 (entry \"R\"); installing anyway (package_conflicts=warn)\n"},
		}
		if diff := cmp.Diff(wantEvents, sink.events, cmp.AllowUnexported(event{})); diff != "" {
			t.Errorf("events (-want +got):\n%s", diff)
		}
	})

	t.Run("post-replacement error mode fails on a survivor", func(t *testing.T) {
		sink := &recordingSink{}
		if err := def.recheckIn(r, ConflictError, survivor("R"), sink); !errors.Is(err, ErrPackageConflict) {
			t.Errorf("recheckIn = %v, want ErrPackageConflict", err)
		}
	})

	t.Run("post-replacement unparseable entry warns without failing", func(t *testing.T) {
		sink := &recordingSink{}
		if err := def.recheckIn(r, ConflictError, survivor("R.noarch.notaver"), sink); err != nil {
			t.Fatalf("recheckIn: %v", err)
		}
		wantEvents := []event{{levelWarning, `package_conflict direction=reverse_replaces mode=error result=unparseable installed=P.noarch.1.0.0 entry="R.noarch.notaver" incoming=R.noarch.1.0.0`}}
		if diff := cmp.Diff(wantEvents, sink.events, cmp.AllowUnexported(event{})); diff != "" {
			t.Errorf("events (-want +got):\n%s", diff)
		}
	})

	t.Run("removed victim produces no events", func(t *testing.T) {
		sink := &recordingSink{}
		if err := def.recheckIn(r, ConflictError, nil, sink); err != nil || len(sink.events) != 0 {
			t.Errorf("recheckIn = %v with events %v, want nil and none", err, sink.events)
		}
	})
}

// TestReverseReplacesVictimDeferredThroughPlan checks through buildPlan that
// the reverse Replaces hit of an exempted victim is dropped when the victim
// is removed, and reported once when a dependency moves the victim to a
// version that the replacement no longer removes.
func TestReverseReplacesVictimDeferredThroughPlan(t *testing.T) {
	installed := client.GooGetState{
		{PackageSpec: &goolib.PkgSpec{Name: "P", Arch: "noarch", Version: "2.0.0", Replaces: []string{"R"}}},
	}
	rm := func(rReplaces string, deps map[string]string, extra ...*goolib.PkgSpec) client.RepoMap {
		specs := []goolib.RepoSpec{{PackageSpec: &goolib.PkgSpec{Name: "R", Arch: "noarch", Version: "1.0.0", Replaces: []string{rReplaces}, PkgDependencies: deps}}}
		for _, e := range extra {
			specs = append(specs, goolib.RepoSpec{PackageSpec: e})
		}
		return client.RepoMap{"repo": client.Repo{Packages: specs}}
	}
	conflictEvents := func(sink *recordingSink) []string {
		var out []string
		for _, e := range sink.events {
			if strings.HasPrefix(e.text, "package_conflict ") {
				out = append(out, string(e.level)+" "+e.text)
			}
		}
		return out
	}
	finalNames := func(p *plan) []string {
		var out []string
		for _, row := range p.final {
			out = append(out, row.PackageSpec.String())
		}
		slices.Sort(out)
		return out
	}

	for _, mode := range []ConflictMode{ConflictWarn, ConflictError, ConflictIgnore} {
		t.Run("removed victim/"+mode.String(), func(t *testing.T) {
			sink := &recordingSink{}
			p, err := buildPlan(planTarget{pi: goolib.PackageInfo{Name: "R"}}, rm("P", nil), installed, []string{"noarch"}, planPolicy{conflicts: mode}, sink)
			if err != nil {
				t.Fatalf("buildPlan: %v", err)
			}
			if got := conflictEvents(sink); len(got) != 0 {
				t.Errorf("unexpected package_conflict events %q", got)
			}
			if diff := cmp.Diff([]string{"R.noarch.1.0.0"}, finalNames(p)); diff != "" {
				t.Errorf("final state (-want +got):\n%s", diff)
			}
		})
	}

	// D removes P 2.0.0 first, then E pulls in P 1.0.0, which R's versioned
	// Replaces entry no longer removes, so P 1.0.0 survives and still
	// replaces R.
	drift := rm("P.noarch.2.0.0", map[string]string{"D": "1.0.0", "E": "1.0.0"},
		&goolib.PkgSpec{Name: "D", Arch: "noarch", Version: "1.0.0", Replaces: []string{"P"}},
		&goolib.PkgSpec{Name: "E", Arch: "noarch", Version: "1.0.0", PkgDependencies: map[string]string{"P": "1.0.0"}},
		&goolib.PkgSpec{Name: "P", Arch: "noarch", Version: "1.0.0", Replaces: []string{"R"}},
	)
	t.Run("surviving victim/warn", func(t *testing.T) {
		sink := &recordingSink{}
		p, err := buildPlan(planTarget{pi: goolib.PackageInfo{Name: "R"}}, drift, installed, []string{"noarch"}, planPolicy{conflicts: ConflictWarn}, sink)
		if err != nil {
			t.Fatalf("buildPlan: %v", err)
		}
		want := []string{
			`WARNING package_conflict direction=reverse_replaces mode=warn result=warned installed=D.noarch.1.0.0 entry="P" incoming=P.noarch.1.0.0`,
			`WARNING package_conflict direction=reverse_replaces mode=warn result=warned installed=P.noarch.1.0.0 entry="R" incoming=R.noarch.1.0.0`,
		}
		if diff := cmp.Diff(want, conflictEvents(sink)); diff != "" {
			t.Errorf("package_conflict events (-want +got):\n%s", diff)
		}
		if diff := cmp.Diff([]string{"D.noarch.1.0.0", "E.noarch.1.0.0", "P.noarch.1.0.0", "R.noarch.1.0.0"}, finalNames(p)); diff != "" {
			t.Errorf("final state (-want +got):\n%s", diff)
		}
	})
	t.Run("surviving victim/error", func(t *testing.T) {
		sink := &recordingSink{}
		_, err := buildPlan(planTarget{pi: goolib.PackageInfo{Name: "R"}}, drift, installed, []string{"noarch"}, planPolicy{conflicts: ConflictError}, sink)
		if !errors.Is(err, ErrPackageConflict) || !strings.Contains(err.Error(), "cannot install P.noarch.1.0.0: installed package D.noarch.1.0.0 replaces it") {
			t.Errorf("buildPlan = %v, want ErrPackageConflict for P.noarch.1.0.0", err)
		}
	})
}

// TestReverseReplacesReinstallSkipsCheck checks that Reinstall, like it does
// for Conflicts, skips the reverse Replaces check even in error mode.
func TestReverseReplacesReinstallSkipsCheck(t *testing.T) {
	f := newRelFixture(t,
		[]goolib.PkgSpec{
			{Name: "P", Arch: "noarch", Version: "1.0.0"},
			{Name: "R", Arch: "noarch", Version: "1.0.0", Replaces: []string{"P"}},
		},
		nil)
	setPackageConflicts(t, "error")
	t.Cleanup(settings.Snapshot())
	ps, err := f.db.FetchPkg(goolib.PackageInfo{Name: "P", Arch: "noarch"})
	if err != nil {
		t.Fatal(err)
	}
	if err := Reinstall(t.Context(), ps, false, false, f.downloader, f.db); err != nil {
		t.Fatalf("Reinstall(P) under packageconflicts=error: %v", err)
	}
}
