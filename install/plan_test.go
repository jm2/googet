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
	"go/parser"
	"go/token"
	"math/rand/v2"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/googet/v2/client"
	"github.com/google/googet/v2/goolib"
	"github.com/google/googet/v2/internal/relation"
	"github.com/google/googet/v2/testutil"
)

var planSeeds = flag.Int("plan_seeds", 150, "number of random install scenarios TestBuildPlanMatchesFromRepo checks")

// TestBuildPlanMatchesFromRepo checks buildPlan against FromRepo over random
// install scenarios: the plan must fail exactly when FromRepo does, with the
// same error, and otherwise predict the installed packages, the install and
// removal order, and the resolution log lines and console messages.
func TestBuildPlanMatchesFromRepo(t *testing.T) {
	seeds := []uint64{1, 2, 3, 4, 5}
	rng := rand.New(rand.NewPCG(uint64(*planSeeds), 7))
	for range *planSeeds {
		seeds = append(seeds, rng.Uint64())
	}
	var failed, replaced, multiStep int
	for _, seed := range seeds {
		c := genCorpusCase(seed)
		planned, live := runCase(t, c)
		if live.Err != "" {
			failed++
			live.Final, live.Installs, live.Removed = nil, nil, nil
		}
		if len(planned.Removed) > 0 {
			replaced++
		}
		if len(planned.Installs) > 1 {
			multiStep++
		}
		if diff := cmp.Diff(live, planned); diff != "" {
			t.Errorf("seed %d (%+v): plan differs from FromRepo (-FromRepo +plan):\n%s", seed, c, diff)
		}
	}
	if failed == 0 || replaced == 0 || multiStep == 0 || failed == len(seeds) {
		t.Errorf("degenerate corpus: %d of %d failed, %d replaced packages, %d installed several packages", failed, len(seeds), replaced, multiStep)
	}
	t.Logf("%d scenarios: %d failed, %d replaced packages, %d installed several packages", len(seeds), failed, replaced, multiStep)
}

// TestBuildPlanSteps checks the steps buildPlan returns for hand-written
// scenarios, and that FromRepo does the same.
func TestBuildPlanSteps(t *testing.T) {
	for _, tc := range []struct {
		name      string
		c         corpusCase
		want      trace
		wantSteps []string // Formatted as name.arch.version, with "<-old" for an upgrade.
	}{
		{
			name: "dependencies install before the package that needs them",
			c: corpusCase{
				Target: "A",
				Available: []goolib.PkgSpec{
					{Name: "A", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"C": "1", "B": "1"}},
					{Name: "B", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"C": "1"}},
					{Name: "C", Arch: "noarch", Version: "1"},
				},
			},
			wantSteps: []string{"C.noarch.1", "B.noarch.1", "A.noarch.1"},
		},
		{
			name: "a satisfied dependency gets no step",
			c: corpusCase{
				Target:    "A",
				Installed: []goolib.PkgSpec{{Name: "B", Arch: "noarch", Version: "1"}},
				Available: []goolib.PkgSpec{
					{Name: "A", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"B": "1"}},
					{Name: "B", Arch: "noarch", Version: "2"},
				},
			},
			wantSteps: []string{"A.noarch.1"},
		},
		{
			name: "a dependency resolves to a provider",
			c: corpusCase{
				Target: "A",
				Available: []goolib.PkgSpec{
					{Name: "A", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"virt": "2"}},
					{Name: "B", Arch: "noarch", Version: "1", Provides: []string{"virt=3"}},
				},
			},
			wantSteps: []string{"B.noarch.1", "A.noarch.1"},
		},
		{
			name: "an upgrade records the version it overwrites",
			c: corpusCase{
				Target:    "A",
				Installed: []goolib.PkgSpec{{Name: "A", Arch: "noarch", Version: "1"}},
				Available: []goolib.PkgSpec{{Name: "A", Arch: "noarch", Version: "2"}},
			},
			wantSteps: []string{"A.noarch.2<-1"},
		},
		{
			name: "a dependency's replacements run in its own step",
			c: corpusCase{
				Target: "A",
				Installed: []goolib.PkgSpec{
					{Name: "P", Arch: "noarch", Version: "1"},
					{Name: "D", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"P": "1"}},
				},
				Available: []goolib.PkgSpec{
					{Name: "A", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"R": "1"}},
					{Name: "R", Arch: "noarch", Version: "1", Replaces: []string{"P"}},
				},
			},
			wantSteps: []string{"R.noarch.1 replacing D.noarch.1,P.noarch.1", "A.noarch.1"},
		},
		{
			name: "a dependency cycle fails before any step",
			c: corpusCase{
				Target: "A",
				Available: []goolib.PkgSpec{
					{Name: "A", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"B": "1"}},
					{Name: "B", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"A": "1"}},
				},
			},
			want: trace{Err: "cannot install A.noarch: dependency cycle A.noarch -> B.noarch -> A.noarch"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRelFixture(t, tc.c.Installed, tc.c.Available)
			state, err := f.db.FetchPkgs("")
			if err != nil {
				t.Fatal(err)
			}
			p, err := buildPlan(planTarget{pi: goolib.PackageInfo{Name: tc.c.Target}}, f.rm, state, corpusArchs, planPolicy{}, &recordingSink{})
			if tc.want.Err != "" {
				if err == nil || err.Error() != tc.want.Err {
					t.Fatalf("buildPlan = %v, want error %q", err, tc.want.Err)
				}
			} else {
				if err != nil {
					t.Fatalf("buildPlan: %v", err)
				}
				var got []string
				for _, s := range p.steps {
					desc := s.spec.PackageSpec.String()
					if s.old != nil {
						desc += "<-" + s.old.PackageSpec.Version
					}
					if len(s.removed) > 0 {
						var names []string
						for _, r := range s.removed {
							names = append(names, r.PackageSpec.String())
						}
						desc += " replacing " + strings.Join(names, ",")
					}
					got = append(got, desc)
				}
				if diff := cmp.Diff(tc.wantSteps, got); diff != "" {
					t.Errorf("plan steps (-want +got):\n%s", diff)
				}
			}
			planned := runPlan(tc.c, f.rm, state)
			live := runLive(t, tc.c, f)
			if live.Err != "" {
				live.Final, live.Installs, live.Removed = nil, nil, nil
			}
			if diff := cmp.Diff(live, planned); diff != "" {
				t.Errorf("plan differs from FromRepo (-FromRepo +plan):\n%s", diff)
			}
		})
	}
}

// TestBuildPlanReplacesRootBySelectingItAgain checks that the plan removes a
// Replaces entry's package the way FromRepo does: once the entry's dependents
// are gone, the package is selected again, which can pick a row other than
// the one that met the entry's minimum version.
func TestBuildPlanReplacesRootBySelectingItAgain(t *testing.T) {
	c := corpusCase{
		Target: "R",
		Installed: []goolib.PkgSpec{
			{Name: "P", Arch: "noarch", Version: "1"},
			{Name: "P", Arch: "x86_64", Version: "3", PkgDependencies: map[string]string{"P": "1"}},
		},
		Available: []goolib.PkgSpec{{Name: "R", Arch: "noarch", Version: "1", Replaces: []string{"P..2"}}},
		DBOnly:    true,
	}
	f := newRelFixture(t, c.Installed, c.Available)
	state, err := f.db.FetchPkgs("")
	if err != nil {
		t.Fatal(err)
	}
	planned := runPlan(c, f.rm, state)
	if want := []string{`"P"`, `"P"`}; !slices.Equal(planned.Removed, want) {
		t.Errorf("plan removed %v, want %v", planned.Removed, want)
	}
	if want := []string{"R.noarch.1"}; !slices.Equal(planned.Final, want) {
		t.Errorf("plan final state %v, want %v (P.noarch.1 is removed although it is below the minimum version)", planned.Final, want)
	}
	if diff := cmp.Diff(runLive(t, c, f), planned); diff != "" {
		t.Errorf("plan differs from FromRepo (-FromRepo +plan):\n%s", diff)
	}
}

// TestBuildPlanLockStateSeesEarlierSteps checks that a dependency's lock
// state is looked up in the installed state as earlier steps leave it, with
// the database's LIKE matching: installing aAb.x86_64, which locks its arch,
// makes a later dependency on a_b resolve to its x86_64 build too.
func TestBuildPlanLockStateSeesEarlierSteps(t *testing.T) {
	c := corpusCase{
		Target: "T",
		Available: []goolib.PkgSpec{
			{Name: "T", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"aAb": "1", "a_b": "1"}},
			{Name: "aAb", Arch: "x86_64", Version: "1", LockArch: true},
			{Name: "a_b", Arch: "noarch", Version: "1", LockArch: true},
			{Name: "a_b", Arch: "x86_64", Version: "1", LockArch: true},
		},
		DBOnly: true,
	}
	f := newRelFixture(t, nil, c.Available)
	planned := runPlan(c, f.rm, nil)
	if want := []string{"aAb.x86_64.1", "a_b.x86_64.1", "T.noarch.1"}; !slices.Equal(planned.Installs, want) {
		t.Errorf("plan installs %v, want %v", planned.Installs, want)
	}
	if diff := cmp.Diff(runLive(t, c, f), planned); diff != "" {
		t.Errorf("plan differs from FromRepo (-FromRepo +plan):\n%s", diff)
	}
}

// TestBuildPlanIgnoresEntriesWithoutSpec checks that state entries with no
// package spec, which the database can't store, change nothing.
func TestBuildPlanIgnoresEntriesWithoutSpec(t *testing.T) {
	c := corpusCase{
		Target:    "A",
		Installed: []goolib.PkgSpec{{Name: "B", Arch: "noarch", Version: "1"}},
		Available: []goolib.PkgSpec{
			{Name: "A", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"B": "1", "a%": "1"}, Replaces: []string{"B"}},
			{Name: "aZ", Arch: "noarch", Version: "1"},
		},
	}
	f := newRelFixture(t, c.Installed, c.Available)
	state, err := f.db.FetchPkgs("")
	if err != nil {
		t.Fatal(err)
	}
	want := runPlan(c, f.rm, state)
	got := runPlan(c, f.rm, append(client.GooGetState{{}}, append(state, client.PackageState{})...))
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("plan with spec-less entries (-without +with):\n%s", diff)
	}
}

// TestSatisfiedInMatchesIsSatisfied checks satisfiedIn against isSatisfied,
// which reads the database, over random installed states and requirements.
func TestSatisfiedInMatchesIsSatisfied(t *testing.T) {
	rng := rand.New(rand.NewPCG(5, 6))
	names := []string{"P", "Q", "virt"}
	archs := []string{"noarch", "x86_64"}
	vers := []string{"1", "2.0.0", "3", "notaver", ""}
	provs := []string{"virt", "virt=2", "virt=notaver", "P=3", "Q"}
	for i := range testutil.ScaledIterations(300) {
		db := openTestDB(t)
		var st client.GooGetState
		for range rng.IntN(5) {
			ps := &goolib.PkgSpec{Name: names[rng.IntN(2)], Arch: archs[rng.IntN(2)], Version: vers[rng.IntN(4)]}
			if rng.IntN(2) == 0 {
				ps.Provides = []string{provs[rng.IntN(len(provs))]}
			}
			st = append(st, client.PackageState{PackageSpec: ps})
		}
		if err := db.WriteStateToDB(st); err != nil {
			t.Fatal(err)
		}
		state, err := db.FetchPkgs("")
		if err != nil {
			t.Fatal(err)
		}
		for range 10 {
			key := names[rng.IntN(len(names))]
			if rng.IntN(3) == 0 {
				key += "." + archs[rng.IntN(2)]
			}
			r := relation.ParseDependency(key, vers[rng.IntN(len(vers))])
			want, wantErr := isSatisfied(r, db)
			got, gotErr := satisfiedIn(r, state)
			if got != want || (gotErr == nil) != (wantErr == nil) {
				t.Errorf("state %d %+v: satisfiedIn = %t, %v; isSatisfied = %t, %v", i, r, got, gotErr, want, wantErr)
			}
		}
	}
}

// TestPlannerIsPure checks that the planner's source imports nothing that
// reads or writes the database, the file system, the logger or settings, and
// prints nothing, so that planning has no side effects.
func TestPlannerIsPure(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "plan.go", nil, parser.ImportsOnly)
	if err != nil {
		t.Fatal(err)
	}
	for _, imp := range f.Imports {
		path := strings.Trim(imp.Path.Value, `"`)
		for _, banned := range []string{"os", "github.com/google/googet/v2/googetdb", "github.com/google/logger", "github.com/google/googet/v2/download", "github.com/google/googet/v2/system", "github.com/google/googet/v2/settings", "github.com/google/googet/v2/oswrap"} {
			if path == banned {
				t.Errorf("plan.go imports %s", path)
			}
		}
	}
	src, err := os.ReadFile("plan.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, call := range []string{"fmt.Print", "fmt.Fprint"} {
		if strings.Contains(string(src), call) {
			t.Errorf("plan.go calls %s", call)
		}
	}
}
