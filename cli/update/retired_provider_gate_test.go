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
	"bytes"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/googet/v2/client"
	"github.com/google/googet/v2/goolib"
	"github.com/google/googet/v2/install"
	"github.com/google/googet/v2/priority"
	"github.com/google/googet/v2/testutil"
)

// TestRetiredProviderGate checks how update treats an installed package P
// that no repository offers for its arch any more, when the package the
// Provides fallback would pick declares Replaces for P: the replacement must
// pass the replacement rules, and an installed package is never moved to an
// unchecked provider.
func TestRetiredProviderGate(t *testing.T) {
	p := spec("P.x86_64.1.0.0")
	for _, tc := range []struct {
		name  string
		state client.GooGetState
		rm    client.RepoMap
		cfg   updateConfig
		want  []string
	}{
		{
			name:  "provider without Replaces keeps the legacy fallback",
			state: stateOf(p),
			rm:    stable(spec("R.x86_64.2.0.0", prov("P"))),
			want:  []string{"Upgrade P.x86_64.1.0.0 -> R.x86_64.2.0.0 from stable"},
		},
		{
			name:  "legacy pick without Replaces wins over a lower provider that replaces",
			state: stateOf(p),
			rm:    stable(spec("R1.x86_64.3.0.0", prov("P")), spec("R2.x86_64.2.0.0", repl("P"), prov("P"))),
			want:  []string{"Upgrade P.x86_64.1.0.0 -> R1.x86_64.3.0.0 from stable"},
		},
		{
			name:  "single replacing provider is scheduled as a replacement",
			state: stateOf(p),
			rm:    stable(spec("R.x86_64.2.0.0", repl("P"), prov("P"))),
			want:  []string{"Replace P.x86_64.1.0.0 -> R.x86_64.2.0.0 from stable replaced=[P.x86_64.1.0.0] removed=[P.x86_64.1.0.0]"},
		},
		{
			name:  "two replacing providers at the best priority are ambiguous",
			state: stateOf(p),
			rm:    stable(spec("R2.x86_64.3.0.0", repl("P"), prov("P")), spec("R1.x86_64.2.0.0", repl("P"), prov("P"))),
			want:  []string{"skip P.x86_64.1.0.0 reason=ambiguous_replacers candidates=[R1.x86_64.2.0.0,R2.x86_64.3.0.0]"},
		},
		{
			name:  "replacing provider at a higher priority is unique",
			state: stateOf(p),
			rm: client.RepoMap{
				"stable": repoOf(priority.Default, spec("R2.x86_64.3.0.0", repl("P"), prov("P"))),
				"canary": repoOf(priority.Canary, spec("R1.x86_64.2.0.0", repl("P"), prov("P"))),
			},
			want: []string{"Replace P.x86_64.1.0.0 -> R1.x86_64.2.0.0 from canary replaced=[P.x86_64.1.0.0] removed=[P.x86_64.1.0.0]"},
		},
		{
			name:  "versions, archs and repositories of one name count once",
			state: stateOf(p),
			rm: client.RepoMap{
				"a": repoOf(priority.Default, spec("R.x86_64.1.0.0", repl("P"), prov("P")), spec("R.x86_64.2.0.0", repl("P"), prov("P")), spec("R.noarch.2.0.0", repl("P"), prov("P"))),
				"b": repoOf(priority.Default, spec("R.x86_64.2.0.0", repl("P"), prov("P")), spec("R.noarch.1.5.0", repl("P"), prov("P"))),
			},
			want: []string{"Replace P.x86_64.1.0.0 -> R.noarch.2.0.0 from a replaced=[P.x86_64.1.0.0] removed=[P.x86_64.1.0.0]"},
		},
		{
			name:  "superseded version that replaced P doesn't count",
			state: stateOf(p),
			rm:    stable(spec("X.x86_64.1.0.0", repl("P"), prov("P")), spec("X.x86_64.2.0.0", prov("P"))),
			want:  []string{"Upgrade P.x86_64.1.0.0 -> X.x86_64.2.0.0 from stable"},
		},
		{
			name:  "own version for another arch at a higher priority pins P",
			state: stateOf(p),
			rm: client.RepoMap{
				"stable": repoOf(priority.Default, spec("R.x86_64.2.0.0", repl("P"), prov("P"))),
				"pin":    repoOf(priority.Pin, spec("P.noarch.0.5.0")),
			},
			want: []string{"skip P.x86_64.1.0.0 reason=pinned_priority replacer=R.x86_64.2.0.0"},
		},
		{
			name:  "replacing provider below the default priority is pinned",
			state: stateOf(p),
			rm:    client.RepoMap{"low": repoOf(400, spec("R.x86_64.2.0.0", repl("P"), prov("P")))},
			want:  []string{"skip P.x86_64.1.0.0 reason=pinned_priority replacer=R.x86_64.2.0.0"},
		},
		{
			name:  "version reset at a higher priority is scheduled",
			state: stateOf(p),
			rm:    client.RepoMap{"canary": repoOf(priority.Canary, spec("R.x86_64.0.9.0", repl("P"), prov("P")))},
			want:  []string{"Replace P.x86_64.1.0.0 -> R.x86_64.0.9.0 from canary replaced=[P.x86_64.1.0.0] removed=[P.x86_64.1.0.0]"},
		},
		{
			name:  "version reset at the default priority is not newer",
			state: stateOf(p),
			rm:    stable(spec("R.x86_64.0.9.0", repl("P"), prov("P"))),
		},
		{
			name:  "LockArch keeps a locked noarch replacer out of the race",
			state: stateOf(spec("P.x86_64.1.0.0", locked)),
			rm:    stable(spec("R1.x86_64.2.0.0", repl("P"), prov("P")), spec("R2.noarch.2.0.0", locked, repl("P"), prov("P"))),
			want:  []string{"Replace P.x86_64.1.0.0 -> R1.x86_64.2.0.0 from stable replaced=[P.x86_64.1.0.0] removed=[P.x86_64.1.0.0]"},
		},
		{
			name:  "unlocked noarch replacer competes",
			state: stateOf(spec("P.x86_64.1.0.0", locked)),
			rm:    stable(spec("R1.x86_64.2.0.0", repl("P"), prov("P")), spec("R2.noarch.2.0.0", repl("P"), prov("P"))),
			want:  []string{"skip P.x86_64.1.0.0 reason=ambiguous_replacers candidates=[R1.x86_64.2.0.0,R2.noarch.2.0.0]"},
		},
		{
			name:  "replacer for another arch doesn't compete",
			state: stateOf(spec("P.x86_32.1.0.0")),
			rm:    stable(spec("R1.x86_32.2.0.0", repl("P"), prov("P")), spec("R2.x86_64.2.0.0", repl("P"), prov("P"))),
			want:  []string{"Replace P.x86_32.1.0.0 -> R1.x86_32.2.0.0 from stable replaced=[P.x86_32.1.0.0] removed=[P.x86_32.1.0.0]"},
		},
		{
			name:  "noarch replacer alone is not reached without following Replaces",
			state: stateOf(p),
			rm:    stable(spec("R.noarch.2.0.0", repl("P"), prov("P"))),
		},
		{
			name:  "orphaning replacement is skipped and P stays",
			state: stateOf(p, spec("D.noarch.1.0.0", dep("P", "1.0.0"))),
			rm:    stable(spec("R.x86_64.2.0.0", repl("P"), prov("P=0.5.0")), spec("D.noarch.1.0.0", dep("P", "1.0.0"))),
			want:  []string{"skip P.x86_64.1.0.0 reason=orphaned_dependents replacer=R.x86_64.2.0.0"},
		},
		{
			name:  "orphaning replacement under OrphanRemove removes the dependent",
			state: stateOf(p, spec("D.noarch.1.0.0", dep("P", "1.0.0"))),
			rm:    stable(spec("R.x86_64.2.0.0", repl("P"), prov("P=0.5.0")), spec("D.noarch.1.0.0", dep("P", "1.0.0"))),
			cfg:   updateConfig{archs: planArchs, opts: install.Options{OrphanedDependents: install.OrphanRemove}},
			want:  []string{"Replace P.x86_64.1.0.0 -> R.x86_64.2.0.0 from stable replaced=[P.x86_64.1.0.0] removed=[D.noarch.1.0.0, P.x86_64.1.0.0]"},
		},
		{
			name:  "replacement that fails to plan is skipped and P stays",
			state: stateOf(p, spec("Other.noarch.1.0.0")),
			rm:    stable(spec("R.x86_64.2.0.0", repl("P"), prov("P"), conf("Other")), spec("Other.noarch.1.0.0")),
			want:  []string{"skip P.x86_64.1.0.0 reason=plan_rejected replacer=R.x86_64.2.0.0"},
		},
		{
			name:  "package that a dependency of the replacer replaces counts as replaced",
			state: stateOf(p, spec("Q.x86_64.1.0.0")),
			rm:    stable(spec("R.x86_64.2.0.0", repl("P"), prov("P"), dep("N", "1.0.0")), spec("N.x86_64.1.0.0", repl("Q"))),
			want:  []string{"Replace P.x86_64.1.0.0 -> R.x86_64.2.0.0 from stable replaced=[Q.x86_64.1.0.0, P.x86_64.1.0.0] removed=[Q.x86_64.1.0.0, P.x86_64.1.0.0]"},
		},
		{
			name:  "provider whose best version dropped Provides leaves P with a skip",
			state: stateOf(p),
			rm:    stable(spec("L.x86_64.2.0.0", repl("P"), prov("P")), spec("L.x86_64.3.0.0", repl("P"))),
			want:  []string{"skip P.x86_64.1.0.0 reason=plan_rejected replacer=L.x86_64.2.0.0"},
		},
		{
			name:  "provider whose best version dropped Replaces leaves P with a skip",
			state: stateOf(p),
			rm:    stable(spec("L.x86_64.2.0.0", repl("P"), prov("P")), spec("L.noarch.3.0.0", prov("P"))),
			want:  []string{"skip P.x86_64.1.0.0 reason=plan_rejected replacer=L.x86_64.2.0.0"},
		},
		{
			name:  "provider for an arch that isn't installable leaves P with a skip",
			state: stateOf(p),
			rm:    stable(spec("L.x86_64.2.0.0", repl("P"), prov("P"))),
			cfg:   updateConfig{archs: []string{"noarch", "x86_32"}},
			want:  []string{"skip P.x86_64.1.0.0 reason=plan_rejected replacer=L.x86_64.2.0.0"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := tc.cfg
			if cfg.archs == nil {
				cfg = gateCfg
			}
			got := summarize(planUpdates(tc.state, tc.rm, cfg))
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("planUpdates (-want +got):\n%s", diff)
			}
		})
	}
}

// TestRetiredProviderGateCycles checks that a gated replacement is skipped
// when another package would replace the replacer in turn: P's own version
// for another arch, an installed package, or a chain of replacing packages
// leading back to P or around a loop, while an acyclic chain is allowed.
func TestRetiredProviderGateCycles(t *testing.T) {
	p := spec("P.x86_64.1.0.0")
	r := spec("R.x86_64.2.0.0", repl("P"), prov("P"))
	for _, tc := range []struct {
		name  string
		state client.GooGetState
		rm    client.RepoMap
		want  []string
	}{
		{
			name:  "P for another arch replaces the replacer",
			state: stateOf(p),
			rm:    stable(r, spec("P.noarch.2.0.0", repl("R"), prov("R"))),
			want:  []string{"skip P.x86_64.1.0.0 reason=replaces_cycle replacer=R.x86_64.2.0.0"},
		},
		{
			name:  "installed package replaces the replacer",
			state: stateOf(p, spec("Q.noarch.1.0.0", repl("R"))),
			rm:    stable(r),
			want:  []string{"skip P.x86_64.1.0.0 reason=replaces_cycle replacer=R.x86_64.2.0.0"},
		},
		{
			name:  "three-node chain leads back to P",
			state: stateOf(p),
			rm:    stable(r, spec("S.noarch.1.0.0", repl("R"), prov("R")), spec("T.noarch.1.0.0", repl("S"), prov("S")), spec("P.noarch.3.0.0", repl("T"), prov("T"))),
			want:  []string{"skip P.x86_64.1.0.0 reason=replaces_cycle replacer=R.x86_64.2.0.0"},
		},
		{
			name:  "loop beyond the replacer",
			state: stateOf(p),
			rm:    stable(r, spec("S.noarch.1.0.0", repl("R", "T"), prov("R", "T")), spec("T.noarch.1.0.0", repl("S"), prov("S"))),
			want:  []string{"skip P.x86_64.1.0.0 reason=replaces_cycle replacer=R.x86_64.2.0.0"},
		},
		{
			name:  "acyclic chain is allowed",
			state: stateOf(p),
			rm:    stable(r, spec("S.noarch.1.0.0", repl("R"), prov("R"))),
			want:  []string{"Replace P.x86_64.1.0.0 -> R.x86_64.2.0.0 from stable replaced=[P.x86_64.1.0.0] removed=[P.x86_64.1.0.0]"},
		},
		{
			name:  "chain link without Provides doesn't count",
			state: stateOf(p),
			rm:    stable(r, spec("S.noarch.1.0.0", repl("R")), spec("P.noarch.3.0.0", repl("S"), prov("S"))),
			want:  []string{"Replace P.x86_64.1.0.0 -> R.x86_64.2.0.0 from stable replaced=[P.x86_64.1.0.0] removed=[P.x86_64.1.0.0]"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := summarize(planUpdates(tc.state, tc.rm, gateCfg))
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("planUpdates (-want +got):\n%s", diff)
			}
		})
	}
}

// TestRetiredProviderGateCoexistence checks the gate when the replacer is
// already installed: at the replacer's exact name and arch only, an installed
// replacer with a pending update is replaced once, whichever order the
// installed packages come in, and one at its best version leaves P alone.
func TestRetiredProviderGateCoexistence(t *testing.T) {
	p := spec("P.x86_64.1.0.0")
	for _, tc := range []struct {
		name      string
		installed []*goolib.PkgSpec
		rm        client.RepoMap
		want      []string
		wantNotes []string
	}{
		{
			name:      "installed replacer with a pending update replaces P once",
			installed: []*goolib.PkgSpec{p, spec("R.x86_64.1.0.0")},
			rm:        stable(spec("R.x86_64.2.0.0", repl("P"), prov("P"))),
			want:      []string{"Replace P.x86_64.1.0.0 -> R.x86_64.2.0.0 from stable replaced=[P.x86_64.1.0.0] removed=[P.x86_64.1.0.0]"},
		},
		{
			name:      "orphaning update of an installed replacer leaves P and updates the replacer in place",
			installed: []*goolib.PkgSpec{p, spec("R.x86_64.1.0.0"), spec("D.noarch.1.0.0", dep("P", "1.0.0"))},
			rm:        stable(spec("R.x86_64.2.0.0", repl("P"), prov("P=0.5.0")), spec("D.noarch.1.0.0", dep("P", "1.0.0"))),
			want: []string{
				"skip P.x86_64.1.0.0 reason=orphaned_dependents replacer=R.x86_64.2.0.0",
				"Upgrade R.x86_64.1.0.0 -> R.x86_64.2.0.0 from stable",
			},
		},
		{
			name:      "installed replacer at its best version leaves P",
			installed: []*goolib.PkgSpec{p, spec("R.x86_64.2.0.0", repl("P"), prov("P"))},
			rm:        stable(spec("R.x86_64.2.0.0", repl("P"), prov("P"))),
			wantNotes: []string{"R.x86_64 - 2.0.0 already installed", "R.x86_64 - highest priority version already installed"},
		},
		{
			name:      "replacer installed for another arch doesn't block the replacement",
			installed: []*goolib.PkgSpec{p, spec("R.x86_32.2.0.0")},
			rm:        stable(spec("R.x86_64.2.0.0", repl("P"), prov("P")), spec("R.x86_32.2.0.0")),
			want:      []string{"Replace P.x86_64.1.0.0 -> R.x86_64.2.0.0 from stable replaced=[P.x86_64.1.0.0] removed=[P.x86_64.1.0.0]"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, order := range []string{"forward", "reversed"} {
				installed := slices.Clone(tc.installed)
				if order == "reversed" {
					slices.Reverse(installed)
				}
				plan := planUpdates(stateOf(installed...), tc.rm, gateCfg)
				if diff := cmp.Diff(tc.want, summarize(plan)); diff != "" {
					t.Errorf("%s: planUpdates (-want +got):\n%s", order, diff)
				}
				if tc.wantNotes != nil {
					got := notes(plan)
					slices.Sort(got)
					if diff := cmp.Diff(tc.wantNotes, got); diff != "" {
						t.Errorf("%s: notes (-want +got):\n%s", order, diff)
					}
				}
			}
		})
	}
}

// TestRetiredProviderGateMultiVictim checks that one replacing package that
// replaces two retired packages is scheduled once, whichever order the
// installed packages come in and whichever of them it provides.
func TestRetiredProviderGateMultiVictim(t *testing.T) {
	p1, p2 := spec("P1.x86_64.1.0.0"), spec("P2.x86_64.1.0.0")
	for _, tc := range []struct {
		name     string
		provides []string
		want     map[string][]string
	}{
		{
			name:     "provides one of them",
			provides: []string{"P2"},
			want: map[string][]string{
				"forward":  {"Replace P2.x86_64.1.0.0 -> R.x86_64.2.0.0 from stable replaced=[P1.x86_64.1.0.0, P2.x86_64.1.0.0] removed=[P1.x86_64.1.0.0, P2.x86_64.1.0.0]"},
				"reversed": {"Replace P2.x86_64.1.0.0 -> R.x86_64.2.0.0 from stable replaced=[P1.x86_64.1.0.0, P2.x86_64.1.0.0] removed=[P1.x86_64.1.0.0, P2.x86_64.1.0.0]"},
			},
		},
		{
			name:     "provides both",
			provides: []string{"P1", "P2"},
			want: map[string][]string{
				"forward":  {"Replace P1.x86_64.1.0.0 -> R.x86_64.2.0.0 from stable replaced=[P1.x86_64.1.0.0, P2.x86_64.1.0.0] removed=[P1.x86_64.1.0.0, P2.x86_64.1.0.0]"},
				"reversed": {"Replace P2.x86_64.1.0.0 -> R.x86_64.2.0.0 from stable replaced=[P1.x86_64.1.0.0, P2.x86_64.1.0.0] removed=[P1.x86_64.1.0.0, P2.x86_64.1.0.0]"},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rm := stable(spec("R.x86_64.2.0.0", repl("P1", "P2"), prov(tc.provides...)))
			for order, state := range map[string]client.GooGetState{"forward": stateOf(p1, p2), "reversed": stateOf(p2, p1)} {
				if diff := cmp.Diff(tc.want[order], summarize(planUpdates(state, rm, gateCfg))); diff != "" {
					t.Errorf("%s: planUpdates (-want +got):\n%s", order, diff)
				}
			}
		})
	}
}

// TestRetiredProviderGateSequentialConflicts checks that each gated
// replacement is planned over the state the earlier ones leave, so that a
// replacement conflicting with an earlier one is skipped at planning time.
func TestRetiredProviderGateSequentialConflicts(t *testing.T) {
	state := stateOf(spec("P1.x86_64.1.0.0"), spec("P2.x86_64.1.0.0"))
	rm := stable(
		spec("R1.x86_64.2.0.0", repl("P1"), prov("P1"), conf("R2")),
		spec("R2.x86_64.2.0.0", repl("P2"), prov("P2"), conf("R1")),
	)
	want := []string{
		"Replace P1.x86_64.1.0.0 -> R1.x86_64.2.0.0 from stable replaced=[P1.x86_64.1.0.0] removed=[P1.x86_64.1.0.0]",
		"skip P2.x86_64.1.0.0 reason=plan_rejected replacer=R2.x86_64.2.0.0",
	}
	if diff := cmp.Diff(want, summarize(planUpdates(state, rm, gateCfg))); diff != "" {
		t.Errorf("planUpdates (-want +got):\n%s", diff)
	}
}

// TestPlanUpdatesIsPure checks that planning writes nothing to the log or
// the console and leaves the installed state it is given unchanged.
func TestPlanUpdatesIsPure(t *testing.T) {
	mk := func() client.GooGetState {
		return stateOf(spec("P.x86_64.1.0.0"), spec("D.noarch.1.0.0", dep("P", "1.0.0")), spec("Q.noarch.1.0.0"))
	}
	rm := stable(spec("R.x86_64.2.0.0", repl("P"), prov("P=0.5.0")), spec("Q.noarch.2.0.0"), spec("D.noarch.1.0.0", dep("P", "1.0.0")))
	logs := testutil.CaptureLogs(t)
	state := mk()
	out := captureStdout(func() {
		planUpdates(state, rm, updateConfig{archs: planArchs, opts: install.Options{OrphanedDependents: install.OrphanRemove}})
		planUpdates(state, rm, gateCfg)
	})
	if logs.Len() != 0 || out != "" {
		t.Errorf("planUpdates wrote log lines %q and console output %q, want none", logs.String(), out)
	}
	if diff := cmp.Diff(mk(), state); diff != "" {
		t.Errorf("planUpdates modified its input state (-before +after):\n%s", diff)
	}
}

// TestRenderReplaceAndSkip checks how renderUpdates writes replacements and
// skipped replacements to the user and the log: a replacement names the
// packages it replaces and, separately, the dependents removed with them,
// and each skip carries its reason, replacer, candidates and error.
func TestRenderReplaceAndSkip(t *testing.T) {
	p := spec("P.x86_64.1.0.0")
	r := spec("R.x86_64.2.0.0")
	orphanErr := &install.OrphanedDependentsError{Dependents: []install.OrphanedDependent{{
		Package:    goolib.PackageInfo{Name: "D", Arch: "noarch", Ver: "1.0.0"},
		Removed:    []goolib.PackageInfo{{Name: "P", Arch: "x86_64", Ver: "1.0.0"}},
		ReplacedBy: goolib.PackageInfo{Name: "R", Arch: "x86_64", Ver: "2.0.0"},
	}}}
	var plan updatePlan
	plan.addAction(updateAction{op: opReplace, from: p, to: r, repo: "stable",
		replaced: []goolib.PackageInfo{{Name: "P", Arch: "x86_64", Ver: "1.0.0"}},
		removed:  []goolib.PackageInfo{{Name: "D", Arch: "noarch", Ver: "1.0.0"}, {Name: "P", Arch: "x86_64", Ver: "1.0.0"}},
	})
	plan.addAction(updateAction{op: opReplace, from: p, to: r, repo: "stable",
		replaced: []goolib.PackageInfo{{Name: "P", Arch: "x86_64", Ver: "1.0.0"}},
		removed:  []goolib.PackageInfo{{Name: "P", Arch: "x86_64", Ver: "1.0.0"}},
	})
	plan.addAction(updateAction{op: opUpgrade, from: spec("Q.noarch.1.0.0"), to: spec("Q.noarch.2.0.0"), repo: "stable"})
	plan.addSkip(replaceSkip{pkg: p, reason: reasonAmbiguous, candidates: []*goolib.PkgSpec{spec("R1.x86_64.2.0.0"), spec("R2.x86_64.2.0.0")}})
	plan.addSkip(replaceSkip{pkg: p, replacer: r, reason: reasonOrphans, err: orphanErr})
	plan.addSkip(replaceSkip{pkg: p, replacer: r, reason: reasonCycle})
	plan.addSkip(replaceSkip{pkg: p, replacer: r, reason: reasonRejected, err: errors.New("boom")})

	logs := testutil.CaptureLogs(t)
	var w bytes.Buffer
	renderUpdates(&w, plan)
	wantOut := strings.Join([]string{
		"Searching for available updates...",
		"  P.x86_64, 1.0.0 --> R.x86_64, 2.0.0 from stable (replaces P.x86_64.1.0.0; removes orphaned D.noarch.1.0.0)",
		"  P.x86_64, 1.0.0 --> R.x86_64, 2.0.0 from stable (replaces P.x86_64.1.0.0)",
		"  Q.noarch, 1.0.0 --> Q.noarch, 2.0.0 from stable",
		"  P.x86_64, 1.0.0: not replaced (ambiguous_replacers: R1.x86_64.2.0.0, R2.x86_64.2.0.0)",
		"  P.x86_64, 1.0.0: not replaced by R.x86_64.2.0.0 (orphaned_dependents: " + orphanErr.Error() + ")",
		"  P.x86_64, 1.0.0: not replaced by R.x86_64.2.0.0 (replaces_cycle)",
		"  P.x86_64, 1.0.0: not replaced by R.x86_64.2.0.0 (plan_rejected: boom)",
		"",
	}, "\n")
	if diff := cmp.Diff(wantOut, w.String()); diff != "" {
		t.Errorf("output (-want +got):\n%s", diff)
	}
	for _, want := range []string{
		"INFO : ", "Replace for package P.x86_64, 1.0.0 installed and R.x86_64, 2.0.0 available from stable (replaces P.x86_64.1.0.0; removes orphaned D.noarch.1.0.0).",
		"Upgrade for package Q.noarch, 1.0.0 installed and Q.noarch, 2.0.0 available from stable.",
		"update_replace_skipped package=P.x86_64.1.0.0 replacer=none reason=ambiguous_replacers candidates=R1.x86_64.2.0.0,R2.x86_64.2.0.0",
		"update_replace_skipped package=P.x86_64.1.0.0 replacer=R.x86_64.2.0.0 reason=replaces_cycle",
		`update_replace_skipped package=P.x86_64.1.0.0 replacer=R.x86_64.2.0.0 reason=plan_rejected err="boom"`,
		"dependency_unsatisfied op=replace mode=fail result=skipped dependent=D.noarch.1.0.0",
	} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("logs missing %q; logs:\n%s", want, logs.String())
		}
	}
	if got := strings.Count(logs.String(), "WARN : "); got != 5 {
		t.Errorf("warning count = %d, want 5 (four skips and one orphaned dependent); logs:\n%s", got, logs.String())
	}
}

// TestRetiredProviderGateNoDuplicateTarget checks that a package a scheduled
// replacement installs counts as installed for the Provides fallback of the
// other installed packages, so that update never installs it twice.
func TestRetiredProviderGateNoDuplicateTarget(t *testing.T) {
	state := stateOf(spec("P.x86_64.1.0.0"), spec("Q.x86_64.1.0.0"))
	rm := stable(spec("R.x86_64.2.0.0", repl("P"), prov("P", "Q")))
	for order, st := range map[string]client.GooGetState{"forward": state, "reversed": {state[1], state[0]}} {
		plan := planUpdates(st, rm, gateCfg)
		want := []string{"Replace P.x86_64.1.0.0 -> R.x86_64.2.0.0 from stable replaced=[P.x86_64.1.0.0] removed=[P.x86_64.1.0.0]"}
		if diff := cmp.Diff(want, summarize(plan)); diff != "" {
			t.Errorf("%s: planUpdates (-want +got):\n%s", order, diff)
		}
		if !slices.Contains(notes(plan), "R.x86_64 - 2.0.0 already installed") {
			t.Errorf("%s: notes = %q, want R reported as already installed for Q", order, notes(plan))
		}
	}
}

// TestRetiredProviderGateKeepsEarlierReplacement checks that a replacement
// never removes the package an earlier scheduled replacement installs, so
// that update doesn't install a package only to remove it again, whichever
// order the installed packages come in.
func TestRetiredProviderGateKeepsEarlierReplacement(t *testing.T) {
	p1, p2 := spec("P1.x86_64.1.0.0"), spec("P2.x86_64.1.0.0")
	rm := stable(
		spec("R1.x86_64.2.0.0", repl("P1", "R2"), prov("P1")),
		spec("R2.x86_64.2.0.0", repl("P2"), prov("P2")),
	)
	for _, tc := range []struct {
		order string
		state client.GooGetState
		want  []string
	}{
		{
			order: "forward",
			state: stateOf(p1, p2),
			want: []string{
				"Replace P1.x86_64.1.0.0 -> R1.x86_64.2.0.0 from stable replaced=[P1.x86_64.1.0.0] removed=[P1.x86_64.1.0.0]",
				"skip P2.x86_64.1.0.0 reason=replaces_cycle replacer=R2.x86_64.2.0.0",
			},
		},
		{
			order: "reversed",
			state: stateOf(p2, p1),
			want: []string{
				"Replace P2.x86_64.1.0.0 -> R2.x86_64.2.0.0 from stable replaced=[P2.x86_64.1.0.0] removed=[P2.x86_64.1.0.0]",
				"skip P1.x86_64.1.0.0 reason=plan_rejected replacer=R1.x86_64.2.0.0",
			},
		},
	} {
		plan := planUpdates(tc.state, rm, gateCfg)
		if diff := cmp.Diff(tc.want, summarize(plan)); diff != "" {
			t.Errorf("%s: planUpdates (-want +got):\n%s", tc.order, diff)
		}
		targets := make(map[string]bool)
		for _, a := range plan.actions() {
			targets[pkgKey(a.to)] = true
		}
		for _, a := range plan.actions() {
			for _, pi := range a.removed {
				if targets[pi.Name+"."+pi.Arch] {
					t.Errorf("%s: %s removes %s, which another action installs", tc.order, a.to, pi)
				}
			}
		}
	}
}

// TestRetiredProviderGateKeepsEarlierRemoval checks that a replacement never
// installs again, as a dependency, a package that an earlier scheduled
// replacement removes, even though the default package conflict policy only
// warns about installing a package that an installed package replaces.
func TestRetiredProviderGateKeepsEarlierRemoval(t *testing.T) {
	p1, p2, q := spec("P1.x86_64.1.0.0"), spec("P2.x86_64.1.0.0"), spec("Q.x86_64.1.0.0")
	rm := stable(
		spec("R1.x86_64.2.0.0", repl("P1", "Q"), prov("P1")),
		spec("R2.x86_64.2.0.0", repl("P2"), prov("P2"), dep("Q", "1.0.0")),
		spec("Q.x86_64.1.0.0"),
	)
	plan := planUpdates(stateOf(p1, p2, q), rm, gateCfg)
	want := []string{
		"Replace P1.x86_64.1.0.0 -> R1.x86_64.2.0.0 from stable replaced=[P1.x86_64.1.0.0, Q.x86_64.1.0.0] removed=[P1.x86_64.1.0.0, Q.x86_64.1.0.0]",
		"skip P2.x86_64.1.0.0 reason=plan_rejected replacer=R2.x86_64.2.0.0",
	}
	if diff := cmp.Diff(want, summarize(plan)); diff != "" {
		t.Fatalf("planUpdates (-want +got):\n%s", diff)
	}
	for _, e := range plan.entries {
		if e.skip == nil {
			continue
		}
		if e.skip.err == nil || !strings.Contains(e.skip.err.Error(), "installs Q.x86_64.1.0.0 again") {
			t.Errorf("skip error = %v, want one saying that R2 installs Q again", e.skip.err)
		}
	}
}
