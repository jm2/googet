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

package install

import (
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/googet/v2/client"
	"github.com/google/googet/v2/goolib"
)

// TestPlanRemovalAccessorsZeroAndNonAliasing checks that RemovalDetails and
// Replaced return nil on nil, zero, or non-removing plans and return fresh
// ReplacedBy pointers that callers cannot alias across entries.
func TestPlanRemovalAccessorsZeroAndNonAliasing(t *testing.T) {
	for _, p := range []*Plan{nil, {}} {
		if got := p.RemovalDetails(); got != nil {
			t.Errorf("RemovalDetails() = %v, want nil", got)
		}
		if got := p.Replaced(); got != nil {
			t.Errorf("Replaced() = %v, want nil", got)
		}
	}

	rm := client.RepoMap{
		"repo": client.Repo{
			Packages: []goolib.RepoSpec{
				{PackageSpec: &goolib.PkgSpec{Name: "fresh", Arch: "noarch", Version: "1.0.0"}},
			},
		},
	}
	nonRemoving, err := PlanState(goolib.PackageInfo{Name: "fresh"}, "repo", rm, nil, []string{"noarch"}, Options{})
	if err != nil {
		t.Fatalf("PlanState(fresh) unexpected error: %v", err)
	}
	if got := nonRemoving.RemovalDetails(); got != nil {
		t.Errorf("nonRemoving.RemovalDetails() = %v, want nil", got)
	}
	if got := nonRemoving.Replaced(); got != nil {
		t.Errorf("nonRemoving.Replaced() = %v, want nil", got)
	}

	replacingRM := client.RepoMap{
		"repo": client.Repo{
			Packages: []goolib.RepoSpec{
				{PackageSpec: &goolib.PkgSpec{Name: "winner", Arch: "noarch", Version: "2.0.0", Replaces: []string{"oldA", "oldB"}}},
			},
		},
	}
	state := client.GooGetState{
		{PackageSpec: &goolib.PkgSpec{Name: "oldA", Arch: "noarch", Version: "1.0.0"}},
		{PackageSpec: &goolib.PkgSpec{Name: "oldB", Arch: "noarch", Version: "1.0.0"}},
	}
	replacing, err := PlanState(goolib.PackageInfo{Name: "winner"}, "repo", replacingRM, state, []string{"noarch"}, Options{})
	if err != nil {
		t.Fatalf("PlanState(winner) unexpected error: %v", err)
	}
	details := replacing.RemovalDetails()
	if len(details) != 2 || details[0].ReplacedBy == nil || details[1].ReplacedBy == nil {
		t.Fatalf("RemovalDetails() = %+v, want 2 replaced victims", details)
	}
	details[0].ReplacedBy.Name = "mutated"
	if details[1].ReplacedBy.Name != "winner" {
		t.Errorf("details[1].ReplacedBy.Name = %q, want %q", details[1].ReplacedBy.Name, "winner")
	}
	freshDetails := replacing.RemovalDetails()
	if freshDetails[0].ReplacedBy.Name != "winner" {
		t.Errorf("subsequent RemovalDetails()[0].ReplacedBy.Name = %q, want %q", freshDetails[0].ReplacedBy.Name, "winner")
	}
}

// TestClassifierConsistencyAcrossPlannerFixtures verifies that planReplacement
// is consistent with removalReason, Plan.RemovalDetails, Plan.Removals,
// Plan.Replaced, Plan.String, classifyVersion, and versionChange across
// multi-victim, cascaded, Provides-only, self-replace, multi-arch, and
// dependency-step replacer fixtures.
func TestClassifierConsistencyAcrossPlannerFixtures(t *testing.T) {
	info := func(name, arch, ver string) goolib.PackageInfo {
		return goolib.PackageInfo{Name: name, Arch: arch, Ver: ver}
	}
	infoPtr := func(name, arch, ver string) *goolib.PackageInfo {
		pi := info(name, arch, ver)
		return &pi
	}

	tests := []struct {
		name        string
		installed   []goolib.PkgSpec
		available   []goolib.PkgSpec
		target      goolib.PackageInfo
		opts        Options
		wantDetails []Removal
		wantString  string
		wantActions []goolib.Action
	}{
		{
			name: "multi_victim_consolidation",
			installed: []goolib.PkgSpec{
				{Name: "victimA", Arch: "noarch", Version: "1.0.0"},
				{Name: "victimB", Arch: "noarch", Version: "1.2.0"},
			},
			available: []goolib.PkgSpec{
				{Name: "replacer", Arch: "noarch", Version: "2.0.0", Replaces: []string{"victimA", "victimB"}},
			},
			target: info("replacer", "", ""),
			wantDetails: []Removal{
				{Package: info("victimA", "noarch", "1.0.0"), ReplacedBy: infoPtr("replacer", "noarch", "2.0.0")},
				{Package: info("victimB", "noarch", "1.2.0"), ReplacedBy: infoPtr("replacer", "noarch", "2.0.0")},
			},
			wantString: strings.Join([]string{
				"The following packages will be installed:",
				"  replacer.noarch.2.0.0",
				"The following packages will be removed:",
				"  victimA.noarch.1.0.0 (replaced by replacer.noarch.2.0.0)",
				"  victimB.noarch.1.2.0 (replaced by replacer.noarch.2.0.0)",
				"",
			}, "\n"),
			wantActions: []goolib.Action{goolib.ActionInstall},
		},
		{
			name: "cascaded_name_dependents_and_kept_provides_dependent",
			installed: []goolib.PkgSpec{
				{Name: "victim", Arch: "noarch", Version: "1.0.0", Provides: []string{"virt"}},
				{Name: "midDep", Arch: "noarch", Version: "1.0.0", PkgDependencies: map[string]string{"victim": "1.0.0"}},
				{Name: "leafDep", Arch: "noarch", Version: "1.0.0", PkgDependencies: map[string]string{"midDep": "1.0.0"}},
				{Name: "provOnly", Arch: "noarch", Version: "1.0.0", PkgDependencies: map[string]string{"virt": "1.0.0"}},
			},
			available: []goolib.PkgSpec{
				{Name: "replacer", Arch: "noarch", Version: "2.0.0", Replaces: []string{"victim"}},
			},
			target: info("replacer", "", ""),
			opts:   Options{OrphanedDependents: OrphanRemove},
			wantDetails: []Removal{
				{Package: info("leafDep", "noarch", "1.0.0"), ReplacedBy: nil},
				{Package: info("midDep", "noarch", "1.0.0"), ReplacedBy: nil},
				{Package: info("victim", "noarch", "1.0.0"), ReplacedBy: infoPtr("replacer", "noarch", "2.0.0")},
			},
			wantString: strings.Join([]string{
				"The following packages will be installed:",
				"  replacer.noarch.2.0.0",
				"The following packages will be removed:",
				"  leafDep.noarch.1.0.0 (depends on a package replacer.noarch.2.0.0 replaces)",
				"  midDep.noarch.1.0.0 (depends on a package replacer.noarch.2.0.0 replaces)",
				"  victim.noarch.1.0.0 (replaced by replacer.noarch.2.0.0)",
				"The following packages will have an unsatisfied dependency (not removed):",
				"  provOnly.noarch.1.0.0 (requires virt >= 1.0.0, provided by victim.noarch.1.0.0; replaced by replacer.noarch.2.0.0)",
				"",
			}, "\n"),
			wantActions: []goolib.Action{goolib.ActionInstall},
		},
		{
			name: "self_replace_yields_victim_removal_and_fresh_install_step",
			installed: []goolib.PkgSpec{
				{Name: "selfpkg", Arch: "noarch", Version: "1.0.0"},
			},
			available: []goolib.PkgSpec{
				{Name: "selfpkg", Arch: "noarch", Version: "2.0.0", Replaces: []string{"selfpkg<2.0.0"}},
			},
			target: info("selfpkg", "", ""),
			wantDetails: []Removal{
				{Package: info("selfpkg", "noarch", "1.0.0"), ReplacedBy: infoPtr("selfpkg", "noarch", "2.0.0")},
			},
			wantString: strings.Join([]string{
				"The following packages will be installed:",
				"  selfpkg.noarch.2.0.0",
				"The following packages will be removed:",
				"  selfpkg.noarch.1.0.0 (replaced by selfpkg.noarch.2.0.0)",
				"",
			}, "\n"),
			wantActions: []goolib.Action{goolib.ActionInstall},
		},
		{
			name: "noarch_install_alongside_installed_x86_64_is_fresh_install",
			installed: []goolib.PkgSpec{
				{Name: "dual", Arch: "x86_64", Version: "1.0.0"},
			},
			available: []goolib.PkgSpec{
				{Name: "dual", Arch: "noarch", Version: "2.0.0"},
			},
			target:      info("dual", "noarch", "2.0.0"),
			wantDetails: nil,
			wantString: strings.Join([]string{
				"The following packages will be installed:",
				"  dual.noarch.2.0.0",
				"",
			}, "\n"),
			wantActions: []goolib.Action{goolib.ActionInstall},
		},
		{
			name: "dependency_step_replacer_and_winner_replacer_with_cascade",
			installed: []goolib.PkgSpec{
				{Name: "victimX", Arch: "noarch", Version: "1.0.0"},
				{Name: "victimY", Arch: "noarch", Version: "1.0.0"},
				{Name: "cascadeZ", Arch: "noarch", Version: "1.0.0", PkgDependencies: map[string]string{"victimY": "1.0.0"}},
			},
			available: []goolib.PkgSpec{
				{Name: "depD", Arch: "noarch", Version: "1.0.0", Replaces: []string{"victimX"}},
				{
					Name:            "winnerW",
					Arch:            "noarch",
					Version:         "2.0.0",
					PkgDependencies: map[string]string{"depD": "1.0.0"},
					Replaces:        []string{"victimY"},
					Provides:        []string{"victimY=0.5.0"},
				},
			},
			target: info("winnerW", "", ""),
			opts:   Options{OrphanedDependents: OrphanRemove},
			wantDetails: []Removal{
				{Package: info("victimX", "noarch", "1.0.0"), ReplacedBy: infoPtr("depD", "noarch", "1.0.0")},
				{Package: info("cascadeZ", "noarch", "1.0.0"), ReplacedBy: nil},
				{Package: info("victimY", "noarch", "1.0.0"), ReplacedBy: infoPtr("winnerW", "noarch", "2.0.0")},
			},
			wantString: strings.Join([]string{
				"The following packages will be installed:",
				"  depD.noarch.1.0.0",
				"  winnerW.noarch.2.0.0",
				"The following packages will be removed:",
				"  victimX.noarch.1.0.0 (replaced by depD.noarch.1.0.0)",
				"  cascadeZ.noarch.1.0.0 (depends on a package winnerW.noarch.2.0.0 replaces)",
				"  victimY.noarch.1.0.0 (replaced by winnerW.noarch.2.0.0)",
				"",
			}, "\n"),
			wantActions: []goolib.Action{goolib.ActionInstall, goolib.ActionInstall},
		},
		{
			name: "multi_step_dependency_install_then_parent_upgrade",
			installed: []goolib.PkgSpec{
				{Name: "app", Arch: "noarch", Version: "1.0.0"},
			},
			available: []goolib.PkgSpec{
				{Name: "lib", Arch: "noarch", Version: "1.0.0"},
				{Name: "app", Arch: "noarch", Version: "2.0.0", PkgDependencies: map[string]string{"lib": "1.0.0"}},
			},
			target:      info("app", "", ""),
			wantDetails: nil,
			wantString: strings.Join([]string{
				"The following packages will be installed:",
				"  lib.noarch.1.0.0",
				"  app.noarch.2.0.0 (upgrade from 1.0.0)",
				"",
			}, "\n"),
			wantActions: []goolib.Action{goolib.ActionInstall, goolib.ActionUpgrade},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var state client.GooGetState
			for i := range tc.installed {
				ps := tc.installed[i]
				state = append(state, client.PackageState{PackageSpec: &ps})
			}
			var repoSpecs []goolib.RepoSpec
			for i := range tc.available {
				ps := tc.available[i]
				repoSpecs = append(repoSpecs, goolib.RepoSpec{PackageSpec: &ps})
			}
			rm := client.RepoMap{"repo": client.Repo{Packages: repoSpecs}}
			repoName := ""
			if tc.target.Ver != "" {
				repoName = "repo"
			}

			gotPlan, err := PlanState(tc.target, repoName, rm, state, []string{"noarch", "x86_64"}, tc.opts)
			if err != nil {
				t.Fatalf("PlanState(%v) unexpected error: %v", tc.target, err)
			}

			if diff := cmp.Diff(tc.wantDetails, gotPlan.RemovalDetails()); diff != "" {
				t.Errorf("RemovalDetails() mismatch (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(tc.wantString, gotPlan.String()); diff != "" {
				t.Errorf("Plan.String() mismatch (-want +got):\n%s", diff)
			}

			var wantRemovals, wantReplaced []goolib.PackageInfo
			for _, d := range tc.wantDetails {
				wantRemovals = append(wantRemovals, d.Package)
				if d.ReplacedBy != nil {
					wantReplaced = append(wantReplaced, d.Package)
				}
			}
			if diff := cmp.Diff(wantRemovals, gotPlan.Removals()); diff != "" {
				t.Errorf("Removals() mismatch (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(wantReplaced, gotPlan.Replaced()); diff != "" {
				t.Errorf("Replaced() mismatch (-want +got):\n%s", diff)
			}

			var stepActions []goolib.Action
			detailIdx := 0
			for _, s := range gotPlan.p.steps {
				sv := classifyVersion(s.old, s.spec.PackageSpec)
				stepActions = append(stepActions, sv.action)
				gotText := versionChange(s)
				wantText := expectedVersionChangeText(sv)
				if gotText != wantText {
					t.Errorf("step %s versionChange() = %q, want %q from classifyVersion(%+v)", s.spec.PackageSpec, gotText, wantText, sv)
				}

				for _, r := range s.removed {
					gotReason := removalReason(s, r)
					wantReason := fmt.Sprintf("depends on a package %s replaces", s.spec.PackageSpec)
					if r.victim {
						wantReason = fmt.Sprintf("replaced by %s", s.spec.PackageSpec)
					}
					if gotReason != wantReason {
						t.Errorf("step %s removal %s removalReason() = %q, want %q", s.spec.PackageSpec, r.row.PackageSpec, gotReason, wantReason)
					}
					d := gotPlan.RemovalDetails()[detailIdx]
					detailIdx++
					if (d.ReplacedBy != nil) != r.victim {
						t.Errorf("RemovalDetails entry %+v ReplacedBy!=nil is %t, want victim=%t", d, d.ReplacedBy != nil, r.victim)
					}
				}
			}
			if diff := cmp.Diff(tc.wantActions, stepActions); diff != "" {
				t.Errorf("step actions mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func expectedVersionChangeText(sv stepVersion) string {
	if sv.uncomparable {
		return fmt.Sprintf(" (replacing version %s)", sv.from)
	}
	switch sv.action {
	case goolib.ActionUpgrade:
		return fmt.Sprintf(" (upgrade from %s)", sv.from)
	case goolib.ActionDowngrade:
		return fmt.Sprintf(" (downgrade from %s)", sv.from)
	case goolib.ActionReinstall:
		return " (reinstall)"
	default:
		return ""
	}
}

// TestSharedVersionClassificationTable applies one shared (old, new) version
// table across both classifyVersion and versionChange, including Compare-error
// rows with equal and unequal strings.
func TestSharedVersionClassificationTable(t *testing.T) {
	rowWithVersion := func(ver string) *client.PackageState {
		return &client.PackageState{
			PackageSpec: &goolib.PkgSpec{Name: "pkg", Arch: "noarch", Version: ver},
		}
	}

	tests := []struct {
		name           string
		old            *client.PackageState
		newVer         string
		wantAction     goolib.Action
		wantFrom       string
		wantUncomp     bool
		wantRenderText string
	}{
		{
			name:           "nil_installed_is_fresh_install",
			old:            nil,
			newVer:         "1.0.0",
			wantAction:     goolib.ActionInstall,
			wantFrom:       "",
			wantUncomp:     false,
			wantRenderText: "",
		},
		{
			name:           "higher_new_version_is_upgrade",
			old:            rowWithVersion("1.0.0"),
			newVer:         "2.0.0",
			wantAction:     goolib.ActionUpgrade,
			wantFrom:       "1.0.0",
			wantUncomp:     false,
			wantRenderText: " (upgrade from 1.0.0)",
		},
		{
			name:           "lower_new_version_is_downgrade",
			old:            rowWithVersion("2.0.0"),
			newVer:         "1.0.0",
			wantAction:     goolib.ActionDowngrade,
			wantFrom:       "2.0.0",
			wantUncomp:     false,
			wantRenderText: " (downgrade from 2.0.0)",
		},
		{
			name:           "equal_semver_is_reinstall",
			old:            rowWithVersion("1.2.3@4"),
			newVer:         "1.2.3@4",
			wantAction:     goolib.ActionReinstall,
			wantFrom:       "1.2.3@4",
			wantUncomp:     false,
			wantRenderText: " (reinstall)",
		},
		{
			name:           "compare_error_with_equal_strings_is_reinstall",
			old:            rowWithVersion("not-a-semver"),
			newVer:         "not-a-semver",
			wantAction:     goolib.ActionReinstall,
			wantFrom:       "not-a-semver",
			wantUncomp:     true,
			wantRenderText: " (replacing version not-a-semver)",
		},
		{
			name:           "compare_error_with_unequal_installed_version_is_upgrade",
			old:            rowWithVersion("not-a-semver"),
			newVer:         "1.0.0",
			wantAction:     goolib.ActionUpgrade,
			wantFrom:       "not-a-semver",
			wantUncomp:     true,
			wantRenderText: " (replacing version not-a-semver)",
		},
		{
			name:           "compare_error_with_unequal_incoming_version_is_upgrade",
			old:            rowWithVersion("1.0.0"),
			newVer:         "not-a-semver",
			wantAction:     goolib.ActionUpgrade,
			wantFrom:       "1.0.0",
			wantUncomp:     true,
			wantRenderText: " (replacing version 1.0.0)",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ps := &goolib.PkgSpec{Name: "pkg", Arch: "noarch", Version: tc.newVer}
			got := classifyVersion(tc.old, ps)
			want := stepVersion{
				action:       tc.wantAction,
				from:         tc.wantFrom,
				uncomparable: tc.wantUncomp,
			}
			if got != want {
				t.Errorf("classifyVersion() = %+v, want %+v", got, want)
			}

			step := planStep{
				old:  tc.old,
				spec: goolib.RepoSpec{PackageSpec: ps},
			}
			if gotText := versionChange(step); gotText != tc.wantRenderText {
				t.Errorf("versionChange() = %q, want %q", gotText, tc.wantRenderText)
			}
		})
	}
}

// TestCheckRemovalsDetectsVictimKindDrift verifies that checkRemovals rejects
// execution when the set of removed package keys stays identical but a row's
// direct-victim versus cascaded-dependent classification changes.
func TestCheckRemovalsDetectsVictimKindDrift(t *testing.T) {
	installed := []goolib.PkgSpec{
		{Name: "victim", Arch: "noarch", Version: "1.0.0"},
		{Name: "cascade", Arch: "noarch", Version: "1.0.0", PkgDependencies: map[string]string{"victim": "1.0.0"}},
	}
	available := []goolib.PkgSpec{
		{Name: "replacer", Arch: "noarch", Version: "2.0.0", Replaces: []string{"victim"}},
	}
	f := newRelFixture(t, installed, available)
	state, err := f.db.FetchPkgs("")
	if err != nil {
		t.Fatalf("FetchPkgs: %v", err)
	}
	p, err := buildPlan(
		planTarget{pi: goolib.PackageInfo{Name: "replacer"}},
		f.rm,
		state,
		[]string{"noarch"},
		planPolicy{orphans: OrphanRemove},
		&recordingSink{},
	)
	if err != nil {
		t.Fatalf("buildPlan: %v", err)
	}

	ri := &repoInstall{
		Installer: New(f.cache, f.db, Options{OrphanedDependents: OrphanRemove}),
		rm:        f.rm,
		archs:     []string{"noarch"},
	}
	if err := ri.checkRemovals(p.steps[0]); err != nil {
		t.Fatalf("checkRemovals(unmodified) unexpected error: %v", err)
	}

	drifted := p.steps[0]
	drifted.removed = []removal{
		{row: p.steps[0].removed[0].row, victim: !p.steps[0].removed[0].victim},
		p.steps[0].removed[1],
	}
	if err := ri.checkRemovals(drifted); !errors.Is(err, errPlanDivergence) {
		t.Errorf("checkRemovals(victim flipped) = %v, want %v", err, errPlanDivergence)
	}
}

// TestSingleClassifierSourceInInstallPackage parses all production Go files in
// package install to ensure removal-reason and version-change classification
// each have a single implementation.
func TestSingleClassifierSourceInInstallPackage(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	fset := token.NewFileSet()
	var (
		replacesInstalledCalls []string
		minInstalledFiles      []string
		compareCallers         []string
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
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				switch sel.Sel.Name {
				case "ReplacesInstalled":
					replacesInstalledCalls = append(replacesInstalledCalls, name+":"+fn.Name.Name)
				case "MinInstalled":
					minInstalledFiles = append(minInstalledFiles, name+":"+fn.Name.Name)
				case "Compare":
					if pkgIdent, ok := sel.X.(*ast.Ident); ok && pkgIdent.Name == "goolib" {
						compareCallers = append(compareCallers, name+":"+fn.Name.Name)
					}
				}
				return true
			})
		}
	}

	if len(replacesInstalledCalls) != 0 {
		t.Errorf("unexpected ReplacesInstalled calls in install package: %v", replacesInstalledCalls)
	}
	for _, caller := range minInstalledFiles {
		if !strings.HasPrefix(caller, "conflicts.go:") {
			t.Errorf("unexpected MinInstalled call outside conflicts.go: %s", caller)
		}
	}
	wantCompareCallers := []string{
		"install.go:NeedsInstallation",
		"render.go:classifyVersion",
	}
	if diff := cmp.Diff(wantCompareCallers, compareCallers); diff != "" {
		t.Errorf("goolib.Compare callers in install package mismatch (-want +got):\n%s", diff)
	}
}
