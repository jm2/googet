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
	"fmt"
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

// followCfg is the planner configuration of googet update -follow_replaces.
var followCfg = updateConfig{archs: planArchs, followReplaces: true}

// diags describes the diagnostics of plan, in order, one string each.
func diags(plan updatePlan) []string {
	var out []string
	for _, e := range plan.entries {
		if e.diag != nil {
			s := fmt.Sprintf("%s %s replacer=%s", e.diag.kind, e.diag.pkg, e.diag.replacer)
			if len(e.diag.others) > 0 {
				s += " others=" + joinSpecs(e.diag.others)
			}
			out = append(out, s)
		}
	}
	return out
}

// followCase is a planner scenario with the expected actions and skips, the
// expected diagnostics, and, for tests that also plan the installed packages
// in reverse order, the packages the actions install, sorted.
type followCase struct {
	name        string
	state       client.GooGetState
	rm          client.RepoMap
	cfg         updateConfig
	want        []string
	wantDiags   []string
	wantTargets []string
}

// config returns tc's planner configuration, which defaults to followCfg.
func (tc followCase) config() updateConfig {
	if tc.cfg.archs == nil {
		return followCfg
	}
	return tc.cfg
}

// run plans tc and compares the outcome with its expectations.
func (tc followCase) run(t *testing.T) {
	t.Helper()
	plan := planUpdates(tc.state, tc.rm, tc.config())
	if diff := cmp.Diff(tc.want, summarize(plan)); diff != "" {
		t.Errorf("planUpdates (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff(tc.wantDiags, diags(plan)); diff != "" {
		t.Errorf("diagnostics (-want +got):\n%s", diff)
	}
}

// TestFollowReplacesSplitPackages checks that packages that replace P
// without providing it never replace P: with followReplaces they are
// reported as a skip when nothing else qualifies and as a diagnostic when a
// single package replaces and provides P, and a package that replaces and
// provides P is never chosen among several.
func TestFollowReplacesSplitPackages(t *testing.T) {
	p := spec("P.x86_64.1.0.0")
	splits := []*goolib.PkgSpec{spec("S1.noarch.1.0.0", repl("P")), spec("S2.noarch.1.0.0", repl("P")), spec("S3.x86_64.1.0.0", repl("P"))}
	with := func(specs ...*goolib.PkgSpec) client.RepoMap {
		return stable(append(slices.Clone(splits), specs...)...)
	}
	const missing = "skip P.x86_64.1.0.0 reason=missing_provides candidates=[S1.noarch.1.0.0,S2.noarch.1.0.0,S3.x86_64.1.0.0]"
	for _, tc := range []followCase{
		{
			name:  "split packages alone while P is published",
			state: stateOf(p),
			rm:    with(spec("P.x86_64.1.0.0")),
			want:  []string{missing},
		},
		{
			name:  "split packages alone while P has an update",
			state: stateOf(p),
			rm:    with(spec("P.x86_64.1.1.0")),
			want:  []string{missing, "Upgrade P.x86_64.1.0.0 -> P.x86_64.1.1.0 from stable"},
		},
		{
			name:  "split packages alone once P is retired",
			state: stateOf(p),
			rm:    with(),
			want:  []string{missing},
		},
		{
			name:  "split packages alone once P is retired without following Replaces",
			state: stateOf(p),
			rm:    with(),
			cfg:   gateCfg,
		},
		{
			name:  "two packages that replace and provide P are ambiguous and P updates in place",
			state: stateOf(p),
			rm:    stable(spec("P.x86_64.1.1.0"), spec("R1.noarch.2.0.0", repl("P"), prov("P")), spec("R2.x86_64.2.0.0", repl("P"), prov("P"))),
			want: []string{
				"skip P.x86_64.1.0.0 reason=ambiguous_replacers candidates=[R1.noarch.2.0.0,R2.x86_64.2.0.0]",
				"Upgrade P.x86_64.1.0.0 -> P.x86_64.1.1.0 from stable",
			},
		},
		{
			name:      "one package that replaces and provides P wins over split packages",
			state:     stateOf(p),
			rm:        with(spec("P.x86_64.1.1.0"), spec("R.noarch.2.0.0", repl("P"), prov("P"))),
			want:      []string{"Replace P.x86_64.1.0.0 -> R.noarch.2.0.0 from stable replaced=[P.x86_64.1.0.0] removed=[P.x86_64.1.0.0]"},
			wantDiags: []string{"update_replace_split_ignored P.x86_64.1.0.0 replacer=R.noarch.2.0.0 others=[S1.noarch.1.0.0,S2.noarch.1.0.0,S3.x86_64.1.0.0]"},
		},
		{
			name:      "one package that replaces and provides P wins over split packages once P is retired",
			state:     stateOf(p),
			rm:        with(spec("R.x86_64.2.0.0", repl("P"), prov("P"))),
			cfg:       gateCfg,
			want:      []string{"Replace P.x86_64.1.0.0 -> R.x86_64.2.0.0 from stable replaced=[P.x86_64.1.0.0] removed=[P.x86_64.1.0.0]"},
			wantDiags: []string{"update_replace_split_ignored P.x86_64.1.0.0 replacer=R.x86_64.2.0.0 others=[S1.noarch.1.0.0,S2.noarch.1.0.0,S3.x86_64.1.0.0]"},
		},
	} {
		t.Run(tc.name, tc.run)
	}
}

// TestFollowReplacesNormalizesCandidates checks that every package name
// counts once, at its highest-ranked version, before Replaces and Provides
// are looked at, so that superseded versions in a repository index never
// cause a replacement, a skip or a diagnostic.
func TestFollowReplacesNormalizesCandidates(t *testing.T) {
	p := spec("P.x86_64.1.0.0")
	pub := spec("P.x86_64.1.0.0")
	for _, tc := range []followCase{
		{
			name:  "successor that dropped Replaces is not followed",
			state: stateOf(p),
			rm:    stable(pub, spec("X.noarch.1.0.0", repl("P"), prov("P")), spec("X.noarch.2.0.0", prov("P"))),
		},
		{
			name:  "successor that dropped Provides is a split package",
			state: stateOf(p),
			rm:    stable(pub, spec("X.noarch.1.0.0", repl("P"), prov("P")), spec("X.noarch.2.0.0", repl("P"))),
			want:  []string{"skip P.x86_64.1.0.0 reason=missing_provides candidates=[X.noarch.2.0.0]"},
		},
		{
			name:  "split package that dropped Replaces is ignored",
			state: stateOf(p),
			rm:    stable(pub, spec("X.noarch.1.0.0", repl("P")), spec("X.noarch.2.0.0")),
		},
		{
			name:  "split package that dropped Replaces is ignored once P is retired",
			state: stateOf(p),
			rm:    stable(spec("X.x86_64.1.0.0", repl("P")), spec("X.x86_64.2.0.0")),
			cfg:   gateCfg,
		},
		{
			name:  "split package that added Provides is chosen without a diagnostic",
			state: stateOf(p),
			rm:    stable(pub, spec("X.noarch.1.0.0", repl("P")), spec("X.noarch.2.0.0", repl("P"), prov("P"))),
			want:  []string{"Replace P.x86_64.1.0.0 -> X.noarch.2.0.0 from stable replaced=[P.x86_64.1.0.0] removed=[P.x86_64.1.0.0]"},
		},
	} {
		t.Run(tc.name, tc.run)
	}
}

// TestFollowReplacesSelection checks the uniqueness, arch, LockArch and
// priority rules when update follows Replaces for a package that is still
// published.
func TestFollowReplacesSelection(t *testing.T) {
	p := spec("P.x86_64.1.0.0")
	pub := spec("P.x86_64.1.0.0")
	for _, tc := range []followCase{
		{
			name:  "noarch package that replaces and provides P",
			state: stateOf(p),
			rm:    stable(pub, spec("R.noarch.2.0.0", repl("P"), prov("P"))),
			want:  []string{"Replace P.x86_64.1.0.0 -> R.noarch.2.0.0 from stable replaced=[P.x86_64.1.0.0] removed=[P.x86_64.1.0.0]"},
		},
		{
			name:  "version reset is followed",
			state: stateOf(spec("P.x86_64.99.0.0")),
			rm:    stable(spec("P.x86_64.99.0.0"), spec("R.x86_64.1.36.0", repl("P"), prov("P"))),
			want:  []string{"Replace P.x86_64.99.0.0 -> R.x86_64.1.36.0 from stable replaced=[P.x86_64.99.0.0] removed=[P.x86_64.99.0.0]"},
		},
		{
			name:  "P's own version at a higher priority pins P",
			state: stateOf(p),
			rm: client.RepoMap{
				"stable": repoOf(priority.Default, spec("R.x86_64.2.0.0", repl("P"), prov("P"))),
				"pin":    repoOf(priority.Pin, pub),
			},
			want: []string{"skip P.x86_64.1.0.0 reason=pinned_priority replacer=R.x86_64.2.0.0"},
		},
		{
			name:  "retired P with a replacer below the default priority is pinned",
			state: stateOf(p),
			rm:    client.RepoMap{"low": repoOf(400, spec("R.noarch.2.0.0", repl("P"), prov("P")))},
			want:  []string{"skip P.x86_64.1.0.0 reason=pinned_priority replacer=R.noarch.2.0.0"},
		},
		{
			name:  "best priority decides among names",
			state: stateOf(p),
			rm: client.RepoMap{
				"stable": repoOf(priority.Default, pub, spec("R2.noarch.3.0.0", repl("P"), prov("P"))),
				"canary": repoOf(priority.Canary, spec("R1.noarch.2.0.0", repl("P"), prov("P"))),
			},
			want: []string{"Replace P.x86_64.1.0.0 -> R1.noarch.2.0.0 from canary replaced=[P.x86_64.1.0.0] removed=[P.x86_64.1.0.0]"},
		},
		{
			name:  "locked P doesn't move to a locked replacer for another arch",
			state: stateOf(spec("P.x86_64.1.0.0", locked)),
			rm:    stable(pub, spec("R.noarch.2.0.0", locked, repl("P"), prov("P"))),
		},
		{
			name:  "replacer for another arch is not admissible",
			state: stateOf(spec("P.x86_32.1.0.0")),
			rm:    stable(spec("P.x86_32.1.0.0"), spec("R.x86_64.2.0.0", repl("P"), prov("P"))),
		},
		{
			name:  "replacer whose Replaces entry is above P's version is ignored",
			state: stateOf(p),
			rm:    stable(pub, spec("R.noarch.2.0.0", repl("P.x86_64.1.5.0"), prov("P"))),
		},
	} {
		t.Run(tc.name, tc.run)
	}
}

// TestFollowReplacesCycles checks that update never follows Replaces into a
// package that something else would replace again, so that repeated runs
// converge, and that an acyclic rename chain moves one step per run.
func TestFollowReplacesCycles(t *testing.T) {
	for _, tc := range []followCase{
		{
			name:  "two packages that replace each other",
			state: stateOf(spec("P.noarch.1.0.0", repl("R"), prov("R"))),
			rm:    stable(spec("P.noarch.1.0.0", repl("R"), prov("R")), spec("R.noarch.1.0.0", repl("P"), prov("P"))),
			want:  []string{"skip P.noarch.1.0.0 reason=replaces_cycle replacer=R.noarch.1.0.0"},
		},
		{
			name:  "three packages that replace each other in a loop",
			state: stateOf(spec("A.noarch.1.0.0")),
			rm: stable(
				spec("A.noarch.1.0.0", repl("C"), prov("C")),
				spec("B.noarch.1.0.0", repl("A"), prov("A")),
				spec("C.noarch.1.0.0", repl("B"), prov("B")),
			),
			want: []string{"skip A.noarch.1.0.0 reason=replaces_cycle replacer=B.noarch.1.0.0"},
		},
		{
			name:  "P for another arch replaces the replacer",
			state: stateOf(spec("P.x86_64.1.0.0")),
			rm:    stable(spec("P.x86_64.1.0.0"), spec("P.noarch.1.0.0", repl("R")), spec("R.noarch.2.0.0", repl("P"), prov("P"))),
			want:  []string{"skip P.x86_64.1.0.0 reason=replaces_cycle replacer=R.noarch.2.0.0"},
		},
		{
			name:  "installed package that is not replaced replaces the replacer",
			state: stateOf(spec("P.x86_64.1.0.0"), spec("Q.noarch.1.0.0", repl("R"))),
			rm:    stable(spec("P.x86_64.1.0.0"), spec("Q.noarch.1.0.0", repl("R")), spec("R.noarch.2.0.0", repl("P"), prov("P"))),
			want:  []string{"skip P.x86_64.1.0.0 reason=replaces_cycle replacer=R.noarch.2.0.0"},
		},
	} {
		t.Run(tc.name, tc.run)
	}

	t.Run("acyclic chain converges one step per run", func(t *testing.T) {
		rm := stable(
			spec("P.noarch.1.0.0"),
			spec("R.noarch.1.0.0", repl("P"), prov("P")),
			spec("S.noarch.1.0.0", repl("R"), prov("R")),
		)
		state := stateOf(spec("P.noarch.1.0.0"))
		var steps []string
		for run := 0; run < 4; run++ {
			plan := planUpdates(state, rm, followCfg)
			acts := plan.actions()
			if len(acts) == 0 {
				break
			}
			if len(acts) != 1 || acts[0].op != opReplace {
				t.Fatalf("run %d: actions = %q, want one replacement", run, summarize(plan))
			}
			ip, err := install.PlanState(acts[0].info(), acts[0].repo, rm, state, planArchs, install.Options{})
			if err != nil {
				t.Fatalf("run %d: PlanState: %v", run, err)
			}
			state = ip.State()
			steps = append(steps, acts[0].to.String())
		}
		if diff := cmp.Diff([]string{"R.noarch.1.0.0", "S.noarch.1.0.0"}, steps); diff != "" {
			t.Errorf("replacements per run (-want +got):\n%s", diff)
		}
	})
}

// TestFollowReplacesSequentialState checks that each replacement is planned
// over the state the earlier replacements leave, so that a replacement that
// only fails after an earlier one is skipped at planning time and its
// package is updated in place instead.
func TestFollowReplacesSequentialState(t *testing.T) {
	for _, tc := range []followCase{
		{
			name: "second replacement would orphan a dependent the first one left satisfied",
			state: stateOf(
				spec("P.x86_64.1.0.0"),
				spec("P.x86_32.1.0.0"),
				spec("D.noarch.1.0.0", dep("P", "1.0.0")),
			),
			rm: stable(
				spec("P.x86_64.1.0.0"),
				spec("P.x86_32.1.1.0"),
				spec("D.noarch.1.0.0", dep("P", "1.0.0")),
				spec("R64.x86_64.2.0.0", repl("P.x86_64"), prov("P=0.5.0")),
				spec("R32.x86_32.2.0.0", repl("P.x86_32"), prov("P=0.5.0")),
			),
			want: []string{
				"Replace P.x86_64.1.0.0 -> R64.x86_64.2.0.0 from stable replaced=[P.x86_64.1.0.0] removed=[P.x86_64.1.0.0]",
				"skip P.x86_32.1.0.0 reason=orphaned_dependents replacer=R32.x86_32.2.0.0",
				"Upgrade P.x86_32.1.0.0 -> P.x86_32.1.1.0 from stable",
			},
		},
		{
			name:  "second replacement conflicts with the first one",
			state: stateOf(spec("P1.noarch.1.0.0"), spec("P2.noarch.1.0.0")),
			rm: stable(
				spec("P1.noarch.1.0.0"),
				spec("P2.noarch.1.1.0"),
				spec("R1.noarch.2.0.0", repl("P1"), prov("P1"), conf("R2")),
				spec("R2.noarch.2.0.0", repl("P2"), prov("P2"), conf("R1")),
			),
			want: []string{
				"Replace P1.noarch.1.0.0 -> R1.noarch.2.0.0 from stable replaced=[P1.noarch.1.0.0] removed=[P1.noarch.1.0.0]",
				"skip P2.noarch.1.0.0 reason=plan_rejected replacer=R2.noarch.2.0.0",
				"Upgrade P2.noarch.1.0.0 -> P2.noarch.1.1.0 from stable",
			},
		},
	} {
		t.Run(tc.name, tc.run)
	}
}

// TestFollowReplacesMultiVictimOrder checks that a package that replaces two
// installed packages is scheduled once, ahead of every in-place update, and
// that neither replaced package is updated in place or reported as skipped,
// whichever order the installed packages come in.
func TestFollowReplacesMultiVictimOrder(t *testing.T) {
	p1, p2 := spec("P1.noarch.1.0.0"), spec("P2.noarch.1.0.0")
	for _, provides := range [][]string{{"P2"}, {"P1", "P2"}} {
		for _, extra := range []bool{false, true} {
			name := "provides " + strings.Join(provides, ",")
			if extra {
				name += " with another update"
			}
			t.Run(name, func(t *testing.T) {
				specs := []*goolib.PkgSpec{
					spec("P1.noarch.1.1.0"),
					spec("P2.noarch.1.1.0"),
					spec("R.noarch.2.0.0", repl("P1", "P2"), prov(provides...)),
				}
				installed := []*goolib.PkgSpec{p1, p2}
				if extra {
					specs = append(specs, spec("O.noarch.2.0.0"))
					installed = append(installed, spec("O.noarch.1.0.0"))
				}
				rm := stable(specs...)
				for _, order := range []string{"forward", "reversed"} {
					st := slices.Clone(installed)
					if order == "reversed" {
						slices.Reverse(st)
					}
					plan := planUpdates(stateOf(st...), rm, followCfg)
					acts := plan.actions()
					var replaces, others int
					for i, a := range acts {
						if a.op == opReplace {
							replaces++
							if i != 0 {
								t.Errorf("%s: replacement is action %d, want it first", order, i)
							}
							want := []goolib.PackageInfo{{Name: "P1", Arch: "noarch", Ver: "1.0.0"}, {Name: "P2", Arch: "noarch", Ver: "1.0.0"}}
							if diff := cmp.Diff(want, a.replaced()); diff != "" {
								t.Errorf("%s: replaced (-want +got):\n%s", order, diff)
							}
							continue
						}
						others++
						if a.from.Name != "O" {
							t.Errorf("%s: unexpected in-place update of %s", order, a.from)
						}
					}
					if replaces != 1 || (extra && others != 1) || (!extra && others != 0) {
						t.Errorf("%s: actions = %q, want one replacement and only O updated in place", order, summarize(plan))
					}
					for _, e := range plan.entries {
						if e.skip != nil {
							t.Errorf("%s: unexpected skip %+v", order, *e.skip)
						}
					}
				}
			})
		}
	}
}

// TestFollowReplacesCoexistence checks how update treats a package P that is
// installed alongside a package R that replaces and provides it.
func TestFollowReplacesCoexistence(t *testing.T) {
	r2 := func(ver string, opts ...specOpt) *goolib.PkgSpec {
		return spec("R.noarch."+ver, append([]specOpt{repl("P"), prov("P")}, opts...)...)
	}
	for _, tc := range []followCase{
		{
			name:        "replacer installed for another arch doesn't block the replacement",
			state:       stateOf(spec("P.x86_64.1.0.0"), spec("R.x86_32.2.0.0")),
			rm:          stable(spec("P.x86_64.1.0.0"), spec("R.x86_64.2.0.0", repl("P"), prov("P")), spec("R.x86_32.2.0.0")),
			want:        []string{"Replace P.x86_64.1.0.0 -> R.x86_64.2.0.0 from stable replaced=[P.x86_64.1.0.0] removed=[P.x86_64.1.0.0]"},
			wantTargets: []string{"R.x86_64.2.0.0"},
		},
		{
			name:        "update of an installed replacer that newly replaces P removes P without updating it",
			state:       stateOf(spec("P.noarch.1.0.0"), spec("R.noarch.1.0.0")),
			rm:          stable(spec("P.noarch.1.5.0"), r2("2.0.0")),
			want:        []string{"Replace P.noarch.1.0.0 -> R.noarch.2.0.0 from stable replaced=[P.noarch.1.0.0] removed=[P.noarch.1.0.0]"},
			wantTargets: []string{"R.noarch.2.0.0"},
		},
		{
			name:  "orphaning update of an installed replacer keeps P's own update",
			state: stateOf(spec("D.noarch.1.0.0", dep("P", "1.0.0")), spec("P.noarch.1.0.0"), spec("R.noarch.1.0.0")),
			rm:    stable(spec("D.noarch.1.0.0", dep("P", "1.0.0")), spec("P.noarch.1.5.0"), spec("R.noarch.2.0.0", repl("P"), prov("P=0.5.0"))),
			want: []string{
				"skip P.noarch.1.0.0 reason=orphaned_dependents replacer=R.noarch.2.0.0",
				"Upgrade P.noarch.1.0.0 -> P.noarch.1.5.0 from stable",
				"Upgrade R.noarch.1.0.0 -> R.noarch.2.0.0 from stable",
			},
			wantTargets: []string{"P.noarch.1.5.0", "R.noarch.2.0.0"},
		},
		{
			name:      "current replacer keeps P from updating into a package it replaces",
			state:     stateOf(spec("P.noarch.1.0.0"), r2("2.0.0")),
			rm:        stable(spec("P.noarch.1.5.0"), r2("2.0.0")),
			wantDiags: []string{"update_replace_coexisting_current P.noarch.1.0.0 replacer=R.noarch.2.0.0"},
		},
		{
			name:        "current replacer without following Replaces leaves P's update alone",
			state:       stateOf(spec("P.noarch.1.0.0"), r2("2.0.0")),
			rm:          stable(spec("P.noarch.1.5.0"), r2("2.0.0")),
			cfg:         gateCfg,
			want:        []string{"Upgrade P.noarch.1.0.0 -> P.noarch.1.5.0 from stable"},
			wantTargets: []string{"P.noarch.1.5.0"},
		},
		{
			name:        "replacer update removes the coexisting P",
			state:       stateOf(spec("P.noarch.1.0.0"), r2("2.0.0")),
			rm:          stable(spec("P.noarch.1.5.0"), r2("2.1.0")),
			want:        []string{"Replace P.noarch.1.0.0 -> R.noarch.2.1.0 from stable replaced=[P.noarch.1.0.0] removed=[P.noarch.1.0.0]"},
			wantTargets: []string{"R.noarch.2.1.0"},
		},
		{
			name:  "installed packages that replace each other are both kept",
			state: stateOf(spec("A.noarch.1.0.0", repl("B"), prov("B")), spec("B.noarch.1.0.0", repl("A"), prov("A"))),
			rm:    stable(spec("A.noarch.1.0.0", repl("B"), prov("B")), spec("B.noarch.1.0.0", repl("A"), prov("A"))),
			want: []string{
				"skip A.noarch.1.0.0 reason=replaces_cycle replacer=B.noarch.1.0.0",
				"skip B.noarch.1.0.0 reason=replaces_cycle replacer=A.noarch.1.0.0",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.run(t)
			reversed := slices.Clone(tc.state)
			slices.Reverse(reversed)
			got := planUpdates(reversed, tc.rm, tc.config())
			var gotActs []string
			for _, a := range got.actions() {
				gotActs = append(gotActs, a.to.String())
			}
			slices.Sort(gotActs)
			if diff := cmp.Diff(tc.wantTargets, gotActs); diff != "" {
				t.Errorf("reversed order: installed targets (-want +got):\n%s", diff)
			}
		})
	}
}

// TestFollowReplacesOffKeepsPublishedPackages checks that, without
// followReplaces, a package that is still published only follows its own
// versions, however many packages replace and provide it.
func TestFollowReplacesOffKeepsPublishedPackages(t *testing.T) {
	for _, tc := range []followCase{
		{
			name:  "published at the installed version",
			state: stateOf(spec("P.x86_64.1.0.0")),
			rm:    stable(spec("P.x86_64.1.0.0"), spec("R.x86_64.2.0.0", repl("P"), prov("P"))),
			cfg:   gateCfg,
		},
		{
			name:  "published with an update",
			state: stateOf(spec("P.x86_64.1.0.0")),
			rm:    stable(spec("P.x86_64.1.1.0"), spec("R.x86_64.2.0.0", repl("P"), prov("P")), spec("S.noarch.2.0.0", repl("P"))),
			cfg:   gateCfg,
			want:  []string{"Upgrade P.x86_64.1.0.0 -> P.x86_64.1.1.0 from stable"},
		},
	} {
		t.Run(tc.name, tc.run)
	}
}

// TestRenderDiagnostics checks that renderUpdates logs each diagnostic as
// one structured informational line and writes nothing for it to the user.
func TestRenderDiagnostics(t *testing.T) {
	var plan updatePlan
	plan.addDiag(replaceDiag{kind: diagSplitIgnored, pkg: spec("P.x86_64.1.0.0"), replacer: spec("R.noarch.2.0.0"), others: []*goolib.PkgSpec{spec("S1.noarch.1.0.0"), spec("S2.noarch.1.0.0")}})
	plan.addDiag(replaceDiag{kind: diagCoexistingCurrent, pkg: spec("P.noarch.1.0.0"), replacer: spec("R.noarch.2.0.0")})
	logs := testutil.CaptureLogs(t)
	var w bytes.Buffer
	renderUpdates(&w, plan)
	if got := w.String(); got != "Searching for available updates...\n" {
		t.Errorf("output = %q, want only the search line", got)
	}
	var lines []string
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		if !strings.HasPrefix(line, "INFO : ") {
			t.Errorf("log line %q is not informational", line)
		}
		lines = append(lines, line[strings.Index(line, "update_replace_"):])
	}
	want := []string{
		"update_replace_split_ignored package=P.x86_64.1.0.0 replacer=R.noarch.2.0.0 ignored=S1.noarch.1.0.0,S2.noarch.1.0.0",
		"update_replace_coexisting_current package=P.noarch.1.0.0 replacer=R.noarch.2.0.0",
	}
	if diff := cmp.Diff(want, lines); diff != "" {
		t.Errorf("log lines (-want +got):\n%s", diff)
	}
}

// TestPrunesSkipsOfReplacedPackages checks that no skip is reported for an
// installed package that a later replacement removes, so that the reported
// skips don't depend on the order of the installed packages.
func TestPrunesSkipsOfReplacedPackages(t *testing.T) {
	rm := stable(
		spec("R.x86_64.2.0.0", repl("P1", "P2"), prov("P1", "P2")),
		spec("S.x86_64.2.0.0", repl("P1"), prov("P1")),
	)
	p1, p2 := spec("P1.x86_64.1.0.0"), spec("P2.x86_64.1.0.0")
	want := []string{"Replace P2.x86_64.1.0.0 -> R.x86_64.2.0.0 from stable replaced=[P1.x86_64.1.0.0, P2.x86_64.1.0.0] removed=[P1.x86_64.1.0.0, P2.x86_64.1.0.0]"}
	for cfgName, cfg := range map[string]updateConfig{"default": gateCfg, "follow": followCfg} {
		for order, st := range map[string]client.GooGetState{"forward": stateOf(p1, p2), "reversed": stateOf(p2, p1)} {
			if diff := cmp.Diff(want, summarize(planUpdates(st, rm, cfg))); diff != "" {
				t.Errorf("%s/%s: planUpdates (-want +got):\n%s", cfgName, order, diff)
			}
		}
	}
}

// TestPrunesEntriesOfPackagesALaterReplacementTouches checks that a skip or a
// diagnostic recorded for an installed package is dropped once a replacement
// for another installed package upgrades that package as a dependency or
// removes it, whichever order the installed packages come in.
func TestPrunesEntriesOfPackagesALaterReplacementTouches(t *testing.T) {
	a, p := spec("A.x86_64.1.0.0"), spec("P.x86_64.1.0.0")
	for _, tc := range []struct {
		name string
		rm   client.RepoMap
		want []string
	}{
		{
			name: "ambiguous skip of a package upgraded as a dependency",
			rm: stable(
				spec("A.x86_64.2.0.0"),
				spec("RA1.x86_64.2.0.0", repl("A"), prov("A")),
				spec("RA2.x86_64.2.0.0", repl("A"), prov("A")),
				spec("RP.x86_64.2.0.0", repl("P"), prov("P"), dep("A", "2.0.0")),
			),
			want: []string{"Replace P.x86_64.1.0.0 -> RP.x86_64.2.0.0 from stable replaced=[P.x86_64.1.0.0] removed=[P.x86_64.1.0.0]"},
		},
		{
			name: "split diagnostic and ambiguous skip of a package removed by another replacement",
			rm: stable(
				spec("A.x86_64.1.0.0"),
				spec("S.x86_64.2.0.0", repl("A")),
				spec("RA1.x86_64.2.0.0", repl("A"), prov("A")),
				spec("RA2.x86_64.2.0.0", repl("A"), prov("A")),
				spec("RP.x86_64.2.0.0", repl("P", "A"), prov("P")),
			),
			want: []string{"Replace P.x86_64.1.0.0 -> RP.x86_64.2.0.0 from stable replaced=[A.x86_64.1.0.0, P.x86_64.1.0.0] removed=[A.x86_64.1.0.0, P.x86_64.1.0.0]"},
		},
	} {
		for order, st := range map[string]client.GooGetState{"forward": stateOf(a, p), "reversed": stateOf(p, a)} {
			t.Run(tc.name+"/"+order, func(t *testing.T) {
				plan := planUpdates(st, tc.rm, followCfg)
				if diff := cmp.Diff(tc.want, summarize(plan)); diff != "" {
					t.Errorf("planUpdates (-want +got):\n%s", diff)
				}
				if got := diags(plan); len(got) != 0 {
					t.Errorf("diagnostics = %q, want none", got)
				}
			})
		}
	}
}
