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
	"slices"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/googet/v2/client"
	"github.com/google/googet/v2/goolib"
	"github.com/google/googet/v2/testutil"
)

// planStateRepo returns a repository offering A 2.0.0, which depends on B
// and replaces P, B 1.0.0, and C 1.0.0, which conflicts with nothing and
// depends on A.
func planStateRepo() client.RepoMap {
	return client.RepoMap{"repo": client.Repo{Packages: []goolib.RepoSpec{
		{PackageSpec: &goolib.PkgSpec{Name: "A", Arch: "noarch", Version: "2.0.0", PkgDependencies: map[string]string{"B": "1.0.0"}, Replaces: []string{"P"}}},
		{PackageSpec: &goolib.PkgSpec{Name: "B", Arch: "noarch", Version: "1.0.0"}},
		{PackageSpec: &goolib.PkgSpec{Name: "C", Arch: "noarch", Version: "1.0.0", PkgDependencies: map[string]string{"A": "2.0.0"}}},
	}}}
}

// planStateInstalled returns a fresh installed state with A 1.0.0, P 1.0.0,
// D 1.0.0, which depends on P, and X 1.0.0, which conflicts with B.
func planStateInstalled() client.GooGetState {
	return client.GooGetState{
		{PackageSpec: &goolib.PkgSpec{Name: "A", Arch: "noarch", Version: "1.0.0"}, InstalledFiles: map[string]string{"/a": "sum"}},
		{PackageSpec: &goolib.PkgSpec{Name: "P", Arch: "noarch", Version: "1.0.0", Replaces: []string{"old"}}},
		{PackageSpec: &goolib.PkgSpec{Name: "D", Arch: "noarch", Version: "1.0.0", PkgDependencies: map[string]string{"P": "1.0.0"}}},
		{PackageSpec: &goolib.PkgSpec{Name: "X", Arch: "noarch", Version: "1.0.0", Conflicts: []string{"B"}}},
	}
}

func infoStrings(pis []goolib.PackageInfo) []string {
	var out []string
	for _, pi := range pis {
		out = append(out, pi.String())
	}
	return out
}

func stateStrings(st client.GooGetState) []string {
	var out []string
	for _, row := range st {
		out = append(out, row.PackageSpec.String())
	}
	slices.Sort(out)
	return out
}

// TestPlanStateOutcomes checks what PlanState reports over an in-memory
// state: the installs, removals, warnings and final state of a replacing
// upgrade, and the errors of the conflict and orphan policies, with nothing
// written to the log or the console and the input state left alone.
func TestPlanStateOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name         string
		opts         Options
		wantErr      error
		wantInstalls []string
		wantRemovals []string
		wantFinal    []string
		wantString   string
	}{
		{
			name:    "orphan fail",
			wantErr: ErrOrphanedDependents,
		},
		{
			name:    "conflict error",
			opts:    Options{PackageConflicts: ConflictError, OrphanedDependents: OrphanRemove},
			wantErr: ErrPackageConflict,
		},
		{
			name:         "orphan remove",
			opts:         Options{OrphanedDependents: OrphanRemove},
			wantInstalls: []string{"B.noarch.1.0.0", "A.noarch.2.0.0"},
			wantRemovals: []string{"D.noarch.1.0.0", "P.noarch.1.0.0"},
			wantFinal:    []string{"A.noarch.2.0.0", "B.noarch.1.0.0", "X.noarch.1.0.0"},
			wantString: "The following packages will be installed:\n" +
				"  B.noarch.1.0.0\n" +
				"  A.noarch.2.0.0 (upgrade from 1.0.0)\n" +
				"The following packages will be removed:\n" +
				"  D.noarch.1.0.0 (depends on a package A.noarch.2.0.0 replaces)\n" +
				"  P.noarch.1.0.0 (replaced by A.noarch.2.0.0)\n" +
				"Warning: installed package X.noarch.1.0.0 conflicts with B.noarch.1.0.0 (entry \"B\"); installing anyway (package_conflicts=warn)\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs := testutil.CaptureLogs(t)
			state := planStateInstalled()
			var p *Plan
			var err error
			out := captureConsole(t, func() {
				p, err = PlanState(goolib.PackageInfo{Name: "A"}, "", planStateRepo(), state, []string{"noarch"}, tc.opts)
			})
			if logs.Len() != 0 || out != "" {
				t.Errorf("PlanState wrote log lines %q and console output %q, want none", logs.String(), out)
			}
			if diff := cmp.Diff(planStateInstalled(), state); diff != "" {
				t.Errorf("PlanState modified its input state (-before +after):\n%s", diff)
			}
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) || p != nil {
					t.Fatalf("PlanState = %v, %v; want nil, %v", p, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("PlanState: %v", err)
			}
			if diff := cmp.Diff(tc.wantInstalls, infoStrings(p.Installs())); diff != "" {
				t.Errorf("Installs() (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(tc.wantRemovals, infoStrings(p.Removals())); diff != "" {
				t.Errorf("Removals() (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(tc.wantFinal, stateStrings(p.State())); diff != "" {
				t.Errorf("State() (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(tc.wantString, p.String()); diff != "" {
				t.Errorf("String() (-want +got):\n%s", diff)
			}
		})
	}
}

// TestPlanStateChains checks that passing one plan's State to the next
// PlanState call plans against the packages the first plan installs and
// removes.
func TestPlanStateChains(t *testing.T) {
	opts := Options{OrphanedDependents: OrphanRemove}
	first, err := PlanState(goolib.PackageInfo{Name: "A"}, "", planStateRepo(), planStateInstalled(), []string{"noarch"}, opts)
	if err != nil {
		t.Fatalf("PlanState(A): %v", err)
	}
	second, err := PlanState(goolib.PackageInfo{Name: "C"}, "", planStateRepo(), first.State(), []string{"noarch"}, opts)
	if err != nil {
		t.Fatalf("PlanState(C): %v", err)
	}
	// A 2.0.0 is already in the first plan's state, so C installs alone.
	if diff := cmp.Diff([]string{"C.noarch.1.0.0"}, infoStrings(second.Installs())); diff != "" {
		t.Errorf("second Installs() (-want +got):\n%s", diff)
	}
	if got := second.Removals(); len(got) != 0 {
		t.Errorf("second Removals() = %v, want none", got)
	}
	if diff := cmp.Diff([]string{"A.noarch.2.0.0", "B.noarch.1.0.0", "C.noarch.1.0.0", "X.noarch.1.0.0"}, stateStrings(second.State())); diff != "" {
		t.Errorf("second State() (-want +got):\n%s", diff)
	}

	// Over the original state, C pulls in A and its replacement again.
	fresh, err := PlanState(goolib.PackageInfo{Name: "C"}, "", planStateRepo(), planStateInstalled(), []string{"noarch"}, opts)
	if err != nil {
		t.Fatalf("PlanState(C) over the original state: %v", err)
	}
	if diff := cmp.Diff([]string{"B.noarch.1.0.0", "A.noarch.2.0.0", "C.noarch.1.0.0"}, infoStrings(fresh.Installs())); diff != "" {
		t.Errorf("fresh Installs() (-want +got):\n%s", diff)
	}
}

// TestPlanStateCopiesState checks that State returns rows, specs, relation
// lists, dependency maps and installed file maps that share no memory with
// the plan or with the state the plan started from.
func TestPlanStateCopiesState(t *testing.T) {
	input := planStateInstalled()
	p, err := PlanState(goolib.PackageInfo{Name: "B"}, "", planStateRepo(), input, []string{"noarch"}, Options{})
	if err != nil {
		t.Fatalf("PlanState(B): %v", err)
	}
	want := p.State()
	got := p.State()
	for i := range got {
		ps := got[i].PackageSpec
		ps.Version = "9.9.9"
		if len(ps.Replaces) > 0 {
			ps.Replaces[0] = "mutated"
		}
		if len(ps.Conflicts) > 0 {
			ps.Conflicts[0] = "mutated"
		}
		for k := range ps.PkgDependencies {
			ps.PkgDependencies[k] = "9.9.9"
		}
		for k := range got[i].InstalledFiles {
			got[i].InstalledFiles[k] = "mutated"
		}
	}
	got[0] = client.PackageState{}
	if diff := cmp.Diff(want, p.State()); diff != "" {
		t.Errorf("State() changed after modifying an earlier result (-before +after):\n%s", diff)
	}
	if diff := cmp.Diff(planStateInstalled(), input); diff != "" {
		t.Errorf("input state changed after modifying State() (-before +after):\n%s", diff)
	}
}

// TestPlanStateZeroValue checks that a nil or zero Plan has a nil State.
func TestPlanStateZeroValue(t *testing.T) {
	for _, p := range []*Plan{nil, {}} {
		if got := p.State(); got != nil {
			t.Errorf("State() = %v, want nil", got)
		}
	}
}

// TestPlanReplaced checks that Replaced lists, in removal order, only the
// installed packages that the Replaces entries of the package removing them
// name, whether that package is the one asked for or a dependency of it,
// while Removals also lists the dependents removed with them.
func TestPlanReplaced(t *testing.T) {
	var nilPlan *Plan
	if got := nilPlan.Replaced(); got != nil {
		t.Errorf("nil Plan Replaced() = %v, want nil", got)
	}
	chainRepo := client.RepoMap{"repo": client.Repo{Packages: []goolib.RepoSpec{
		{PackageSpec: &goolib.PkgSpec{Name: "A", Arch: "noarch", Version: "2.0.0", Replaces: []string{"P"}}},
		{PackageSpec: &goolib.PkgSpec{Name: "E", Arch: "noarch", Version: "1.0.0", PkgDependencies: map[string]string{"A": "2.0.0"}, Replaces: []string{"Q"}}},
	}}}
	chainInstalled := client.GooGetState{
		{PackageSpec: &goolib.PkgSpec{Name: "P", Arch: "noarch", Version: "1.0.0"}},
		{PackageSpec: &goolib.PkgSpec{Name: "Q", Arch: "noarch", Version: "1.0.0"}},
	}
	for _, tc := range []struct {
		name         string
		pkg          string
		rm           client.RepoMap
		state        client.GooGetState
		wantReplaced []string
		wantRemovals []string
	}{
		{
			name:  "nothing removed",
			pkg:   "B",
			rm:    planStateRepo(),
			state: planStateInstalled(),
		},
		{
			name:         "dependent removed with the replaced package",
			pkg:          "A",
			rm:           planStateRepo(),
			state:        planStateInstalled(),
			wantReplaced: []string{"P.noarch.1.0.0"},
			wantRemovals: []string{"D.noarch.1.0.0", "P.noarch.1.0.0"},
		},
		{
			name:         "package replaced by a dependency",
			pkg:          "C",
			rm:           planStateRepo(),
			state:        planStateInstalled(),
			wantReplaced: []string{"P.noarch.1.0.0"},
			wantRemovals: []string{"D.noarch.1.0.0", "P.noarch.1.0.0"},
		},
		{
			name:         "packages replaced by a dependency and by the package asked for",
			pkg:          "E",
			rm:           chainRepo,
			state:        chainInstalled,
			wantReplaced: []string{"P.noarch.1.0.0", "Q.noarch.1.0.0"},
			wantRemovals: []string{"P.noarch.1.0.0", "Q.noarch.1.0.0"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, err := PlanState(goolib.PackageInfo{Name: tc.pkg}, "", tc.rm, tc.state, []string{"noarch"}, Options{OrphanedDependents: OrphanRemove})
			if err != nil {
				t.Fatalf("PlanState(%s): %v", tc.pkg, err)
			}
			if diff := cmp.Diff(tc.wantReplaced, infoStrings(p.Replaced())); diff != "" {
				t.Errorf("Replaced() (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(tc.wantRemovals, infoStrings(p.Removals())); diff != "" {
				t.Errorf("Removals() (-want +got):\n%s", diff)
			}
		})
	}
}
