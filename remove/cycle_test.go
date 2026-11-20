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
	"fmt"
	"math/rand"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/googet/v2/client"
	"github.com/google/googet/v2/googetdb"
	"github.com/google/googet/v2/goolib"
	"github.com/google/googet/v2/settings"
)

// noarch returns an installed noarch package with the given dependencies,
// each on any version.
func noarch(name string, deps ...string) client.PackageState {
	pd := map[string]string{}
	for _, d := range deps {
		pd[d] = "0.0.0"
	}
	return client.PackageState{PackageSpec: &goolib.PkgSpec{Name: name, Arch: "noarch", Version: "1", PkgDependencies: pd}}
}

// sortedDeps returns deps with each list of dependents sorted, so that maps
// built in a different order compare equal.
func sortedDeps(deps DepMap) DepMap {
	out := DepMap{}
	for k, v := range deps {
		out[k] = slices.Sorted(slices.Values(v))
	}
	return out
}

func TestBuildCycle(t *testing.T) {
	state := client.GooGetState{noarch("A", "B"), noarch("B", "A")}
	deps := DepMap{}
	deps.build("A", "noarch", state)
	want := DepMap{"A.noarch": {"B.noarch"}, "B.noarch": {"A.noarch"}}
	if diff := cmp.Diff(want, deps); diff != "" {
		t.Errorf("build(A) (-want +got):\n%s", diff)
	}
}

func TestBuildDuplicateKeys(t *testing.T) {
	// D names A both without and with its arch.
	state := client.GooGetState{noarch("A"), noarch("D", "A", "A.noarch")}
	deps := DepMap{}
	deps.build("A", "noarch", state)
	want := DepMap{"A.noarch": {"D.noarch"}, "D.noarch": nil}
	if diff := cmp.Diff(want, deps); diff != "" {
		t.Errorf("build(A) (-want +got):\n%s", diff)
	}
}

// oldBuild is build as it was before it tracked visited packages, kept to
// check that the two agree on graphs without cycles.
func (deps DepMap) oldBuild(name, arch string, state client.GooGetState) {
	deps[name+"."+arch] = nil
	for _, p := range state {
		if p.PackageSpec.Name == name && p.PackageSpec.Arch == arch {
			continue
		}
		for d := range p.PackageSpec.PkgDependencies {
			di := goolib.PkgNameSplit(d)
			if di.Name == name && (di.Arch == arch || di.Arch == "") {
				n, a := p.PackageSpec.Name, p.PackageSpec.Arch
				deps[name+"."+arch] = append(deps[name+"."+arch], n+"."+a)
				deps.oldBuild(n, a, state)
			}
		}
	}
}

// TestBuildMatchesOldBuildWithoutCycles checks build against oldBuild on
// random dependency graphs without cycles, where each dependent names each of
// its dependencies once. Pairs of packages share a name across two arches,
// and dependency keys are arch-less, name the dependency's arch, or name the
// other arch.
func TestBuildMatchesOldBuildWithoutCycles(t *testing.T) {
	archs := []string{"noarch", "x86_64"}
	rng := rand.New(rand.NewSource(1))
	for i := range 1000 {
		n := 1 + rng.Intn(10)
		var state client.GooGetState
		for j := range n {
			name, arch := fmt.Sprintf("p%d", j/2), archs[j%2]
			// Packages only depend on names with a lower index, so there
			// are no cycles.
			pd := map[string]string{}
			for k := range j / 2 {
				if rng.Intn(3) != 0 {
					continue
				}
				switch dep := fmt.Sprintf("p%d", k); rng.Intn(3) {
				case 0:
					pd[dep] = "0.0.0"
				default:
					pd[dep+"."+archs[rng.Intn(2)]] = "0.0.0"
				}
			}
			state = append(state, client.PackageState{PackageSpec: &goolib.PkgSpec{Name: name, Arch: arch, Version: "1", PkgDependencies: pd}})
		}
		root := state[rng.Intn(n)].PackageSpec
		rootArch := root.Arch
		if rng.Intn(4) == 0 {
			rootArch = "" // as for a Replaces entry without an arch
		}
		got, want := DepMap{}, DepMap{}
		got.build(root.Name, rootArch, state)
		want.oldBuild(root.Name, rootArch, state)
		if diff := cmp.Diff(sortedDeps(want), sortedDeps(got)); diff != "" {
			t.Fatalf("graph %d, root %s.%s: build differs from oldBuild (-old +new):\n%s", i, root.Name, rootArch, diff)
		}
	}
}

func TestRemovalOrder(t *testing.T) {
	for _, tc := range []struct {
		name          string
		deps          DepMap
		root          string
		want          []string
		wantRootCycle []string
		wantErr       []string // wantErr lists substrings of the expected error.
	}{
		{
			name: "no dependents",
			deps: DepMap{"A": nil},
			root: "A",
		},
		{
			name: "chain",
			deps: DepMap{"A": {"B"}, "B": {"C"}, "C": nil},
			root: "A",
			want: []string{"C", "B"},
		},
		{
			name: "diamond",
			deps: DepMap{"A": {"B", "C"}, "B": {"D"}, "C": {"D"}, "D": nil},
			root: "A",
			want: []string{"D", "B", "C"},
		},
		{
			name:          "cycle through root",
			deps:          DepMap{"A": {"B"}, "B": {"A"}},
			root:          "A",
			want:          []string{"B"},
			wantRootCycle: []string{"B"},
		},
		{
			name:          "longer cycle through root",
			deps:          DepMap{"A": {"B"}, "B": {"C"}, "C": {"A"}},
			root:          "A",
			want:          []string{"C", "B"},
			wantRootCycle: []string{"C"},
		},
		{
			// Replaces entries without an arch name the root without one.
			name: "root that isn't a key",
			deps: DepMap{"A.": {"B"}, "B": {"C"}, "C": nil},
			root: "A.noarch",
			want: []string{"C", "B", "A."},
		},
		{
			name:    "cycle not through root",
			deps:    DepMap{"A": {"B", "L"}, "B": {"C"}, "C": {"B"}, "L": nil},
			root:    "A",
			wantErr: []string{"cannot remove A", "B, C"},
		},
		{
			name: "dependent that isn't a key",
			deps: DepMap{"A": {"B"}},
			root: "A",
			want: []string{"B"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := fmt.Sprint(tc.deps)
			got, gotRootCycle, err := removalOrder(tc.deps, tc.root)
			if tc.wantErr != nil {
				if err == nil {
					t.Fatalf("removalOrder() = %v, want an error", got)
				}
				for _, s := range tc.wantErr {
					if !strings.Contains(err.Error(), s) {
						t.Errorf("removalOrder() error = %q, want it to contain %q", err, s)
					}
				}
			} else if err != nil {
				t.Fatalf("removalOrder(): %v", err)
			}
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("removalOrder() order (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(tc.wantRootCycle, gotRootCycle); diff != "" {
				t.Errorf("removalOrder() rootCycle (-want +got):\n%s", diff)
			}
			if after := fmt.Sprint(tc.deps); after != before {
				t.Errorf("removalOrder() modified deps: before %s, after %s", before, after)
			}
		})
	}
}

// allWithTimeout runs All for the noarch package name with -db_only, and
// fails the test instead of hanging if All doesn't return.
func allWithTimeout(t *testing.T, db *googetdb.GooDB, name string) error {
	t.Helper()
	pi := goolib.PackageInfo{Name: name, Arch: "noarch"}
	deps, _, err := EnumerateDeps(pi, db)
	if err != nil {
		t.Fatalf("EnumerateDeps(%s): %v", name, err)
	}
	done := make(chan error, 1)
	go func() { done <- All(t.Context(), pi, deps, true, nil, db) }()
	select {
	case err := <-done:
		return err
	case <-time.After(30 * time.Second):
		t.Fatalf("All(%s) didn't return", name)
		return nil
	}
}

func newDB(t *testing.T, state client.GooGetState) *googetdb.GooDB {
	t.Helper()
	settings.Initialize(t.TempDir(), false)
	db, err := googetdb.NewDB(settings.DBFile())
	if err != nil {
		t.Fatalf("googetdb.NewDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.WriteStateToDB(state); err != nil {
		t.Fatalf("WriteStateToDB: %v", err)
	}
	return db
}

func installedNames(t *testing.T, db *googetdb.GooDB) []string {
	t.Helper()
	state, err := db.FetchPkgs("")
	if err != nil {
		t.Fatalf("FetchPkgs: %v", err)
	}
	var names []string
	for _, ps := range state {
		names = append(names, ps.PackageSpec.Name)
	}
	slices.Sort(names)
	return names
}

func TestAll(t *testing.T) {
	for _, tc := range []struct {
		name    string
		state   client.GooGetState
		remove  string
		want    []string // want lists the packages still installed afterwards.
		wantErr string
	}{
		{
			// D lists A twice in A's dependents. Before dependents were
			// deduplicated, removing D dropped only one of them, so A never
			// became removable and All looped forever.
			name:   "dependent names a dependency twice",
			state:  client.GooGetState{noarch("R"), noarch("A", "R"), noarch("D", "A", "A.noarch")},
			remove: "R",
		},
		{
			name:   "cycle through the removed package",
			state:  client.GooGetState{noarch("A", "B"), noarch("B", "A")},
			remove: "A",
		},
		{
			name:    "cycle among dependents removes nothing",
			state:   client.GooGetState{noarch("A"), noarch("L", "A"), noarch("B", "A", "C"), noarch("C", "B")},
			remove:  "A",
			want:    []string{"A", "B", "C", "L"},
			wantErr: "B.noarch, C.noarch",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := newDB(t, tc.state)
			err := allWithTimeout(t, db, tc.remove)
			if tc.wantErr == "" && err != nil {
				t.Fatalf("All(%s): %v", tc.remove, err)
			}
			if tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("All(%s) = %v, want an error containing %q", tc.remove, err, tc.wantErr)
			}
			if diff := cmp.Diff(tc.want, installedNames(t, db)); diff != "" {
				t.Errorf("installed packages (-want +got):\n%s", diff)
			}
		})
	}
}
