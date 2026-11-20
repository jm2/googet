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
	"maps"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"

	"github.com/google/googet/v2/goolib"
	"github.com/google/googet/v2/internal/relation"
)

// reachable reports whether to can be reached from from by following edges,
// from each package to the packages listed for it, at least once.
func reachable(edges map[string][]string, from, to string) bool {
	seen := map[string]bool{}
	stack := slices.Clone(edges[from])
	for len(stack) > 0 {
		n := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if n == to {
			return true
		}
		if !seen[n] {
			seen[n] = true
			stack = append(stack, edges[n]...)
		}
	}
	return false
}

// hasCycle reports whether the graph that edges describes, from each package
// to the packages listed for it, has a cycle.
func hasCycle(edges map[string][]string) bool {
	const (
		visiting = 1
		done     = 2
	)
	state := map[string]int{}
	var visit func(string) bool
	visit = func(n string) bool {
		switch state[n] {
		case visiting:
			return true
		case done:
			return false
		}
		state[n] = visiting
		for _, m := range edges[n] {
			if visit(m) {
				return true
			}
		}
		state[n] = done
		return false
	}
	for _, n := range slices.Sorted(maps.Keys(edges)) {
		if visit(n) {
			return true
		}
	}
	return false
}

// TestRemovalOrderSetProperties checks removalOrderSet on random dependency
// maps and root sets against its contract: it fails exactly when packages
// other than roots depend on each other in a cycle; otherwise every package
// appears once, after every package that depends on it, except that a
// non-root may come before a root that depends on it, and roots in one cycle
// of roots may come in either order relative to each other. rootCycle lists, sorted,
// the non-roots that a root depends on.
func TestRemovalOrderSetProperties(t *testing.T) {
	rng := rand.New(rand.NewPCG(21, 22))
	names := []string{"A.noarch", "B.noarch", "C.noarch", "D.noarch", "E.noarch", "F.noarch", "G.noarch"}
	for i := range 5000 {
		deps := DepMap{}
		for _, n := range names {
			if rng.IntN(4) == 0 {
				continue
			}
			var dependents []string
			for _, d := range names {
				if d != n && rng.IntN(5) == 0 {
					dependents = append(dependents, d)
				}
			}
			deps[n] = dependents
		}
		var roots []string
		for _, n := range names {
			if rng.IntN(3) == 0 {
				roots = append(roots, n)
			}
		}
		isRoot := func(n string) bool { return slices.Contains(roots, n) }

		all := map[string]bool{}
		nonRootEdges := map[string][]string{}
		rootEdges := map[string][]string{}
		var wantCycle []string
		for pkg, dependents := range deps {
			all[pkg] = true
			for _, d := range dependents {
				all[d] = true
				switch {
				case !isRoot(pkg) && !isRoot(d):
					nonRootEdges[pkg] = append(nonRootEdges[pkg], d)
				case isRoot(pkg) && isRoot(d):
					rootEdges[pkg] = append(rootEdges[pkg], d)
				}
			}
			if !isRoot(pkg) && slices.ContainsFunc(dependents, isRoot) {
				wantCycle = append(wantCycle, pkg)
			}
		}
		slices.Sort(wantCycle)

		order, rootCycle, err := removalOrderSet(deps, roots)
		if wantErr := hasCycle(nonRootEdges); (err != nil) != wantErr {
			t.Fatalf("case %d: removalOrderSet(%v, %v) error = %v, want error: %t", i, deps, roots, err, wantErr)
		}
		if err != nil {
			if !strings.HasPrefix(err.Error(), "cannot remove "+strings.Join(roots, ", ")+": these packages depend on each other in a cycle: ") {
				t.Fatalf("case %d: error = %q", i, err)
			}
			continue
		}
		if got := slices.Sorted(slices.Values(order)); !slices.Equal(got, slices.Sorted(maps.Keys(all))) {
			t.Fatalf("case %d: removalOrderSet(%v, %v) = %v, want each of %v once", i, deps, roots, order, slices.Sorted(maps.Keys(all)))
		}
		if !slices.Equal(rootCycle, wantCycle) {
			t.Fatalf("case %d: removalOrderSet(%v, %v) rootCycle = %v, want %v", i, deps, roots, rootCycle, wantCycle)
		}
		for pkg, dependents := range deps {
			for _, d := range dependents {
				if isRoot(d) && (!isRoot(pkg) || reachable(rootEdges, d, pkg)) {
					continue
				}
				if slices.Index(order, d) > slices.Index(order, pkg) {
					t.Fatalf("case %d: removalOrderSet(%v, %v) = %v removes %s before its dependent %s", i, deps, roots, order, pkg, d)
				}
			}
		}
	}
}

// TestSplitKeyMatchesPkgNameSplitWithoutDots checks that keys whose name has
// no dot split exactly as goolib.PkgNameSplit split them before dotted names
// were supported.
func TestSplitKeyMatchesPkgNameSplitWithoutDots(t *testing.T) {
	for _, name := range []string{"P", "foo-bar", "foo_bar", "a"} {
		for _, arch := range []string{"", "noarch", "x86_64", "x86_32", "arm64"} {
			key := name + "." + arch
			if got, want := relation.SplitKey(key), goolib.PkgNameSplit(key); got != want {
				t.Errorf("SplitKey(%q) = %+v, want %+v as PkgNameSplit returns", key, got, want)
			}
		}
	}
}
