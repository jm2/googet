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
	"math/rand/v2"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/googet/v2/client"
	"github.com/google/googet/v2/goolib"
	"github.com/google/googet/v2/internal/relation"
)

func TestOrderSet(t *testing.T) {
	stateOf := func(specs ...goolib.PkgSpec) client.GooGetState {
		var out client.GooGetState
		for i := range specs {
			ps := specs[i]
			out = append(out, client.PackageState{PackageSpec: &ps})
		}
		return out
	}

	t.Run("victim depending on another victim is removed first", func(t *testing.T) {
		st := stateOf(
			goolib.PkgSpec{Name: "A", Arch: "noarch", Version: "1"},
			goolib.PkgSpec{Name: "B", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"A": "1"}},
		)
		got, cycle, err := OrderSet([]string{"A.noarch", "B.noarch"}, []string{"A.noarch", "B.noarch"}, st)
		if err != nil {
			t.Fatalf("OrderSet: %v", err)
		}
		if diff := cmp.Diff([]string{"B.noarch", "A.noarch"}, got); diff != "" {
			t.Errorf("order (-want +got):\n%s", diff)
		}
		if len(cycle) != 0 {
			t.Errorf("rootCycle = %v, want empty", cycle)
		}
	})

	t.Run("victims in mutual cycle remove external dependent first", func(t *testing.T) {
		st := stateOf(
			goolib.PkgSpec{Name: "A", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"B": "1"}},
			goolib.PkgSpec{Name: "B", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"A": "1"}},
			goolib.PkgSpec{Name: "D", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"A": "1"}},
		)
		got, _, err := OrderSet([]string{"A.noarch", "B.noarch", "D.noarch"}, []string{"A.noarch", "B.noarch"}, st)
		if err != nil {
			t.Fatalf("OrderSet: %v", err)
		}
		if diff := cmp.Diff([]string{"D.noarch", "A.noarch", "B.noarch"}, got); diff != "" {
			t.Errorf("order (-want +got):\n%s", diff)
		}
	})

	t.Run("cycle among non-victim dependents returns error naming them", func(t *testing.T) {
		st := stateOf(
			goolib.PkgSpec{Name: "A", Arch: "noarch", Version: "1"},
			goolib.PkgSpec{Name: "C", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"A": "1", "D": "1"}},
			goolib.PkgSpec{Name: "D", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"C": "1"}},
		)
		_, _, err := OrderSet([]string{"A.noarch", "C.noarch", "D.noarch"}, []string{"A.noarch"}, st)
		if err == nil || !strings.Contains(err.Error(), "C.noarch, D.noarch") {
			t.Fatalf("OrderSet = %v, want cycle error naming C.noarch, D.noarch", err)
		}
	})

	t.Run("missing package returns error", func(t *testing.T) {
		st := stateOf(goolib.PkgSpec{Name: "A", Arch: "noarch", Version: "1"})
		_, _, err := OrderSet([]string{"A.noarch", "Missing.noarch"}, []string{"A.noarch"}, st)
		if err == nil || !strings.Contains(err.Error(), "Missing") {
			t.Fatalf("OrderSet = %v, want not-found error naming Missing", err)
		}
	})
}

// TestOrderSetEdgeCases covers empty inputs, nil specs, duplicate keys, dotted
// package names, rootCycle reporting, upstream vs downstream root cycles, and
// multi-arch identity edges.
func TestOrderSetEdgeCases(t *testing.T) {
	stateOf := func(specs ...*goolib.PkgSpec) client.GooGetState {
		t.Helper()
		var out client.GooGetState
		for _, ps := range specs {
			out = append(out, client.PackageState{PackageSpec: ps})
		}
		return out
	}

	t.Run("empty_pkgs_and_nil_specs_do_not_mutate_state", func(t *testing.T) {
		a := &goolib.PkgSpec{Name: "A", Arch: "noarch", Version: "1"}
		st := stateOf(nil, a)
		orig := slices.Clone(st)
		got, cycle, err := OrderSet(nil, []string{"A.noarch"}, st)
		if err != nil || got != nil || cycle != nil {
			t.Fatalf("OrderSet(nil) = (%v, %v, %v), want (nil, nil, nil)", got, cycle, err)
		}
		if diff := cmp.Diff(orig, st); diff != "" {
			t.Errorf("state mutated (-before +after):\n%s", diff)
		}
	})

	t.Run("duplicate_pkgs_and_dotted_names_preserve_unrelated_prefix", func(t *testing.T) {
		st := stateOf(
			nil,
			&goolib.PkgSpec{Name: "foo", Arch: "noarch", Version: "1"},
			&goolib.PkgSpec{Name: "P", Arch: "noarch", Version: "1"},
			&goolib.PkgSpec{Name: "foo.bar", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"P": "1"}},
		)
		got, cycle, err := OrderSet(
			[]string{"P.noarch", "foo.bar.noarch", "P.noarch", "foo.bar.noarch"},
			[]string{"P.noarch"},
			st,
		)
		if err != nil {
			t.Fatalf("OrderSet: %v", err)
		}
		if diff := cmp.Diff([]string{"foo.bar.noarch", "P.noarch"}, got); diff != "" {
			t.Errorf("order (-want +got):\n%s", diff)
		}
		if len(cycle) != 0 {
			t.Errorf("rootCycle = %v, want empty", cycle)
		}

		_, _, err = OrderSet([]string{"missing.dotted.noarch"}, []string{"missing.dotted.noarch"}, st)
		wantSub := `package "missing.dotted" not found in state database`
		if err == nil || !strings.Contains(err.Error(), wantSub) {
			t.Errorf("OrderSet(missing.dotted.noarch) = %v, want substring %q", err, wantSub)
		}
	})

	t.Run("root_cycle_with_non_root_reports_sorted_root_cycle", func(t *testing.T) {
		st := stateOf(
			&goolib.PkgSpec{Name: "A", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"D1": "1", "D2": "1"}},
			&goolib.PkgSpec{Name: "D2", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"A": "1"}},
			&goolib.PkgSpec{Name: "D1", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"A": "1"}},
		)
		got, rootCycle, err := OrderSet([]string{"A.noarch", "D1.noarch", "D2.noarch"}, []string{"A.noarch"}, st)
		if err != nil {
			t.Fatalf("OrderSet: %v", err)
		}
		if diff := cmp.Diff([]string{"D1.noarch", "D2.noarch", "A.noarch"}, got); diff != "" {
			t.Errorf("order (-want +got):\n%s", diff)
		}
		if diff := cmp.Diff([]string{"D1.noarch", "D2.noarch"}, rootCycle); diff != "" {
			t.Errorf("rootCycle (-want +got):\n%s", diff)
		}
	})

	t.Run("upstream_root_cycle_removed_before_earlier_named_downstream_root_cycle", func(t *testing.T) {
		// X <-> Y is a root cycle where X also depends on A, and A <-> B is a
		// downstream root cycle. Even though A sorts before X, X and Y depend on
		// A and must be removed before A and B.
		st := stateOf(
			&goolib.PkgSpec{Name: "A", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"B": "1"}},
			&goolib.PkgSpec{Name: "B", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"A": "1"}},
			&goolib.PkgSpec{Name: "X", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"Y": "1", "A": "1"}},
			&goolib.PkgSpec{Name: "Y", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"X": "1"}},
		)
		keys := []string{"A.noarch", "B.noarch", "X.noarch", "Y.noarch"}
		got, _, err := OrderSet(keys, keys, st)
		if err != nil {
			t.Fatalf("OrderSet: %v", err)
		}
		if diff := cmp.Diff([]string{"X.noarch", "Y.noarch", "A.noarch", "B.noarch"}, got); diff != "" {
			t.Errorf("order (-want +got):\n%s", diff)
		}
	})

	t.Run("arch_qualified_and_arch_less_dependencies_across_multiarch_victims", func(t *testing.T) {
		st := stateOf(
			&goolib.PkgSpec{Name: "P", Arch: "noarch", Version: "1"},
			&goolib.PkgSpec{Name: "P", Arch: "x86_64", Version: "1", PkgDependencies: map[string]string{"P.noarch": "1"}},
			&goolib.PkgSpec{Name: "Q", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"P": "1"}},
		)
		got, _, err := OrderSet(
			[]string{"P.noarch", "P.x86_64", "Q.noarch"},
			[]string{"P.noarch", "P.x86_64"},
			st,
		)
		if err != nil {
			t.Fatalf("OrderSet: %v", err)
		}
		if diff := cmp.Diff([]string{"Q.noarch", "P.x86_64", "P.noarch"}, got); diff != "" {
			t.Errorf("order (-want +got):\n%s", diff)
		}
	})
}

// TestOrderSetSingleRootMatchesPlanProperty verifies across randomized graphs
// that OrderSet over a concrete root's transitive identity closure produces
// the exact removal order of Plan followed by the root itself, and errors on
// the exact same non-root cycles.
func TestOrderSetSingleRootMatchesPlanProperty(t *testing.T) {
	names := []string{"A", "B", "C", "D", "foo.bar", "x.y.z"}
	archs := []string{"noarch", "x86_64"}
	rng := rand.New(rand.NewPCG(315, 2026))

	var okCases, cycleErrs int
	for iter := range 150 {
		seed := rng.Uint64()
		sub := rand.New(rand.NewPCG(seed, seed^0x5a5a5a5a))

		var candidates []*goolib.PkgSpec
		for _, n := range names {
			for _, a := range archs {
				if sub.IntN(3) == 0 {
					continue
				}
				candidates = append(candidates, &goolib.PkgSpec{
					Name:            n,
					Arch:            a,
					Version:         "1",
					PkgDependencies: map[string]string{},
				})
			}
		}
		if len(candidates) < 2 {
			continue
		}
		for _, ps := range candidates {
			for _, target := range candidates {
				if ps.Name == target.Name && ps.Arch == target.Arch {
					continue
				}
				if sub.IntN(4) != 0 {
					continue
				}
				depKey := target.Name + "." + target.Arch
				if sub.IntN(2) == 0 {
					depKey = target.Name
				}
				ps.PkgDependencies[depKey] = "1"
			}
		}

		var st client.GooGetState
		for _, ps := range candidates {
			st = append(st, client.PackageState{PackageSpec: ps})
		}

		rootSpec := candidates[sub.IntN(len(candidates))]
		rootPI := relation.SplitKey(rootSpec.Name + "." + rootSpec.Arch)
		rootKey := rootSpec.Name + "." + rootSpec.Arch

		dm := make(DepMap)
		dm.buildWithLog(rootPI.Name, rootPI.Arch, st, nil)
		var closure []string
		for k := range dm {
			closure = append(closure, k)
		}

		planOrder, planErr := Plan(rootPI, st)
		setOrder, _, setErr := OrderSet(closure, []string{rootKey}, st)

		if (planErr != nil) != (setErr != nil) {
			t.Fatalf("iter %d seed %d: Plan err = %v, OrderSet err = %v", iter, seed, planErr, setErr)
		}
		if planErr != nil {
			cycleErrs++
			continue
		}
		okCases++
		want := append(slices.Clone(planOrder), rootKey)
		if diff := cmp.Diff(want, setOrder); diff != "" {
			t.Fatalf("iter %d seed %d: OrderSet vs Plan+root (-want +got):\n%s", iter, seed, diff)
		}
	}
	if okCases == 0 || cycleErrs == 0 {
		t.Fatalf("degenerate random graph distribution: okCases=%d cycleErrs=%d", okCases, cycleErrs)
	}
}
