/*
Copyright 2026 Google LLC

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
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/googet/v2/client"
	"github.com/google/googet/v2/goolib"
	"github.com/google/googet/v2/install"
	"github.com/google/googet/v2/priority"
	"github.com/google/googet/v2/testutil"
	"github.com/google/subcommands"
)

// TestDependencyStepReplacerAndCascadeAgreement verifies that install.PlanState,
// planUpdates, renderUpdates, update -dry_run, and live update execution all
// agree when winner W depends on D, D replaces X, and W replaces Y while
// cascading dependent Z.
func TestDependencyStepReplacerAndCascadeAgreement(t *testing.T) {
	installed := []goolib.PkgSpec{
		{Name: "X", Arch: "noarch", Version: "1.0.0"},
		{Name: "Y", Arch: "noarch", Version: "1.0.0"},
		{Name: "Z", Arch: "noarch", Version: "1.0.0", PkgDependencies: map[string]string{"Y": "1.0.0"}},
		{Name: "L", Arch: "noarch", Version: "1.0.0", PkgDependencies: map[string]string{"virtY": "1.0.0"}},
	}
	installed[1].Provides = []string{"virtY"}

	repoPkgs := []goolib.PkgSpec{
		{Name: "X", Arch: "noarch", Version: "1.0.0"},
		{Name: "Y", Arch: "noarch", Version: "1.0.0", Provides: []string{"virtY"}},
		{Name: "Z", Arch: "noarch", Version: "1.0.0", PkgDependencies: map[string]string{"Y": "1.0.0"}},
		{Name: "L", Arch: "noarch", Version: "1.0.0", PkgDependencies: map[string]string{"virtY": "1.0.0"}},
		{Name: "D", Arch: "noarch", Version: "1.0.0", Replaces: []string{"X"}},
		{
			Name:            "W",
			Arch:            "noarch",
			Version:         "2.0.0",
			PkgDependencies: map[string]string{"D": "1.0.0"},
			Replaces:        []string{"Y"},
			Provides:        []string{"Y=0.5.0"},
		},
	}

	var state client.GooGetState
	for i := range installed {
		ps := installed[i]
		state = append(state, client.PackageState{PackageSpec: &ps})
	}
	var specs []goolib.RepoSpec
	for i := range repoPkgs {
		ps := repoPkgs[i]
		specs = append(specs, goolib.RepoSpec{PackageSpec: &ps})
	}
	rm := client.RepoMap{"stable": client.Repo{Priority: priority.Default, Packages: specs}}
	opts := install.Options{OrphanedDependents: install.OrphanRemove}

	ip, err := install.PlanState(goolib.PackageInfo{Name: "W"}, "stable", rm, state, []string{"noarch"}, opts)
	if err != nil {
		t.Fatalf("install.PlanState(W) unexpected error: %v", err)
	}

	depInfo := goolib.PackageInfo{Name: "D", Arch: "noarch", Ver: "1.0.0"}
	winInfo := goolib.PackageInfo{Name: "W", Arch: "noarch", Ver: "2.0.0"}
	wantDetails := []install.Removal{
		{Package: goolib.PackageInfo{Name: "X", Arch: "noarch", Ver: "1.0.0"}, ReplacedBy: &depInfo},
		{Package: goolib.PackageInfo{Name: "Z", Arch: "noarch", Ver: "1.0.0"}, ReplacedBy: nil},
		{Package: goolib.PackageInfo{Name: "Y", Arch: "noarch", Ver: "1.0.0"}, ReplacedBy: &winInfo},
	}
	if diff := cmp.Diff(wantDetails, ip.RemovalDetails()); diff != "" {
		t.Errorf("install.PlanState RemovalDetails() mismatch (-want +got):\n%s", diff)
	}

	uPlan := planUpdates(state, rm, updateConfig{
		archs:          []string{"noarch"},
		opts:           opts,
		followReplaces: true,
	})
	actions := uPlan.actions()
	if len(actions) != 1 {
		t.Fatalf("planUpdates() action count = %d, want 1 (%+v)", len(actions), actions)
	}
	if diff := cmp.Diff(wantDetails, actions[0].removals); diff != "" {
		t.Errorf("updateAction.removals mismatch against Plan.RemovalDetails (-want +got):\n%s", diff)
	}

	var rendered bytes.Buffer
	renderUpdates(&rendered, uPlan)
	wantRendered := strings.Join([]string{
		"Searching for available updates...",
		"  Y.noarch, 1.0.0 --> W.noarch, 2.0.0 from stable (replaces Y.noarch.1.0.0; D.noarch.1.0.0 replaces X.noarch.1.0.0; removes orphaned Z.noarch.1.0.0)",
		"",
	}, "\n")
	if diff := cmp.Diff(wantRendered, rendered.String()); diff != "" {
		t.Errorf("renderUpdates() mismatch (-want +got):\n%s", diff)
	}

	_, repoURL := testutil.SetupCLIRepo(t, "", installed, repoPkgs)
	status, dryOut := runUpdate(t, "-sources="+repoURL, "-follow_replaces", "-orphaned_dependents=remove", "-dry_run")
	if status != subcommands.ExitSuccess {
		t.Fatalf("update -dry_run = %v; output:\n%s", status, dryOut)
	}
	wantCLILine := "  Y.noarch, 1.0.0 --> W.noarch, 2.0.0 from " + repoURL + " (replaces Y.noarch.1.0.0; D.noarch.1.0.0 replaces X.noarch.1.0.0; removes orphaned Z.noarch.1.0.0)\n"
	if !strings.Contains(dryOut, wantCLILine) {
		t.Errorf("dry-run output missing %q:\n%s", wantCLILine, dryOut)
	}

	status, liveOut := runUpdate(t, "-sources="+repoURL, "-follow_replaces", "-orphaned_dependents=remove")
	if status != subcommands.ExitSuccess {
		t.Fatalf("live update = %v; output:\n%s", status, liveOut)
	}
	if planPrefix := strings.TrimSuffix(dryOut, dryRunLine); !strings.HasPrefix(liveOut, planPrefix) {
		t.Errorf("live output does not start with dry-run plan.\ndry:\n%s\nlive:\n%s", planPrefix, liveOut)
	}
	wantInstalled := []string{"D.noarch.1.0.0", "L.noarch.1.0.0", "W.noarch.2.0.0"}
	if diff := cmp.Diff(wantInstalled, testutil.InstalledNames(t)); diff != "" {
		t.Errorf("installed after live update (-want +got):\n%s", diff)
	}
}

// TestReplaceDetailPartitionsDirectDependencyAndCascadedRemovals checks
// replaceDetail across empty, direct-only, multi-dependency, and cascaded
// combinations while preserving step order.
func TestReplaceDetailPartitionsDirectDependencyAndCascadedRemovals(t *testing.T) {
	winner := &goolib.PkgSpec{Name: "W", Arch: "noarch", Version: "2.0.0"}
	winInfo := specInfo(winner)
	dep1 := goolib.PackageInfo{Name: "D1", Arch: "noarch", Ver: "1.0.0"}
	dep2 := goolib.PackageInfo{Name: "D2", Arch: "x86_64", Ver: "3.0.0"}

	tests := []struct {
		name     string
		removals []install.Removal
		want     string
	}{
		{
			name:     "empty_removals",
			removals: nil,
			want:     "",
		},
		{
			name: "direct_only",
			removals: []install.Removal{
				{Package: goolib.PackageInfo{Name: "P", Arch: "noarch", Ver: "1.0.0"}, ReplacedBy: &winInfo},
			},
			want: " (replaces P.noarch.1.0.0)",
		},
		{
			name: "dependency_steps_preserve_first_seen_order_and_group_victims",
			removals: []install.Removal{
				{Package: goolib.PackageInfo{Name: "X1", Arch: "noarch", Ver: "1.0.0"}, ReplacedBy: &dep1},
				{Package: goolib.PackageInfo{Name: "X3", Arch: "x86_64", Ver: "1.0.0"}, ReplacedBy: &dep2},
				{Package: goolib.PackageInfo{Name: "X2", Arch: "noarch", Ver: "1.5.0"}, ReplacedBy: &dep1},
				{Package: goolib.PackageInfo{Name: "C1", Arch: "noarch", Ver: "1.0.0"}, ReplacedBy: nil},
				{Package: goolib.PackageInfo{Name: "P", Arch: "noarch", Ver: "1.0.0"}, ReplacedBy: &winInfo},
			},
			want: " (replaces P.noarch.1.0.0; D1.noarch.1.0.0 replaces X1.noarch.1.0.0, X2.noarch.1.5.0; D2.x86_64.3.0.0 replaces X3.x86_64.1.0.0; removes orphaned C1.noarch.1.0.0)",
		},
		{
			name: "dependency_and_cascaded_without_direct_victim",
			removals: []install.Removal{
				{Package: goolib.PackageInfo{Name: "X1", Arch: "noarch", Ver: "1.0.0"}, ReplacedBy: &dep1},
				{Package: goolib.PackageInfo{Name: "C1", Arch: "noarch", Ver: "1.0.0"}, ReplacedBy: nil},
			},
			want: " (D1.noarch.1.0.0 replaces X1.noarch.1.0.0; removes orphaned C1.noarch.1.0.0)",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := replaceDetail(updateAction{op: opReplace, to: winner, removals: tc.removals})
			if got != tc.want {
				t.Errorf("replaceDetail() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestSingleClassifierSourceInUpdatePackage parses production Go files in
// cli/update to confirm that removed-row classification is delegated to
// install.Plan.RemovalDetails rather than re-running Replaces matching.
func TestSingleClassifierSourceInUpdatePackage(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	fset := token.NewFileSet()
	var (
		replacesInstalledCallers []string
		minInstalledCallers      []string
		replacesInfoDecls        []string
	)

	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("ParseFile(%s): %v", name, err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			if fn.Name.Name == "replacesInfo" {
				replacesInfoDecls = append(replacesInfoDecls, name)
			}
			if fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				switch fun := call.Fun.(type) {
				case *ast.SelectorExpr:
					switch fun.Sel.Name {
					case "ReplacesInstalled":
						replacesInstalledCallers = append(replacesInstalledCallers, name+":"+fn.Name.Name)
					case "MinInstalled":
						minInstalledCallers = append(minInstalledCallers, name+":"+fn.Name.Name)
					}
				case *ast.Ident:
					if fun.Name == "replaces" && (fn.Name.Name == "scheduleReplacement" || name == "render.go") {
						replacesInstalledCallers = append(replacesInstalledCallers, name+":"+fn.Name.Name+"->replaces")
					}
				}
				return true
			})
		}
	}

	if len(replacesInfoDecls) != 0 {
		t.Errorf("unexpected replacesInfo helper remaining in cli/update: %v", replacesInfoDecls)
	}
	if len(minInstalledCallers) != 0 {
		t.Errorf("unexpected MinInstalled calls in cli/update: %v", minInstalledCallers)
	}
	wantReplacesInstalled := []string{
		"plan.go:selectReplacer",
		"plan.go:replaces",
	}
	if diff := cmp.Diff(wantReplacesInstalled, replacesInstalledCallers); diff != "" {
		t.Errorf("ReplacesInstalled callers in cli/update mismatch (-want +got):\n%s", diff)
	}
}
