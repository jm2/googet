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
	"slices"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/googet/v2/goolib"
	"github.com/google/googet/v2/testutil"
)

// TestPlanMatchesNonDBOnlyExecutionFilesAndCache compares Installer.Plan
// predictions against non-DBOnly FromRepo execution across multi-step
// scenarios: Plan emits zero log or console output, Installs and Removals
// match the executed steps, installed payload files match the final state, and
// cached archives match the installed packages (with replaced archives deleted
// at commit unless still used by an installed package).
func TestPlanMatchesNonDBOnlyExecutionFilesAndCache(t *testing.T) {
	scenarios := []corpusCase{
		{
			Installed: []goolib.PkgSpec{
				{Name: "P", Arch: "noarch", Version: "1"},
				{Name: "D", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"P": "1"}},
				{Name: "Keep", Arch: "noarch", Version: "1"},
			},
			Available: []goolib.PkgSpec{
				{Name: "T", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"B": "1"}, Replaces: []string{"P"}},
				{Name: "B", Arch: "noarch", Version: "1"},
			},
			Target: "T",
		},
		{
			Installed: []goolib.PkgSpec{
				{Name: "A", Arch: "noarch", Version: "1"},
				{Name: "P", Arch: "noarch", Version: "1"},
				{Name: "Q", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"P": "1"}},
			},
			Available: []goolib.PkgSpec{
				{Name: "A", Arch: "noarch", Version: "2", PkgDependencies: map[string]string{"R": "1"}},
				{Name: "R", Arch: "noarch", Version: "1", Replaces: []string{"P"}},
			},
			Target: "A",
		},
		{
			// Self-replacing package shares its cached archive with the removed copy.
			Installed: []goolib.PkgSpec{
				{Name: "X", Arch: "noarch", Version: "2", Replaces: []string{"X"}},
			},
			Available: []goolib.PkgSpec{
				{Name: "X", Arch: "noarch", Version: "2", Replaces: []string{"X"}},
			},
			Target: "X",
		},
		{
			// Earlier step replaces P.noarch.2 and later step installs P.noarch.2 again.
			Installed: []goolib.PkgSpec{
				{Name: "P", Arch: "noarch", Version: "2"},
			},
			Available: []goolib.PkgSpec{
				{Name: "T", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"B": "1", "P": "2"}},
				{Name: "B", Arch: "noarch", Version: "1", Replaces: []string{"P"}},
				{Name: "P", Arch: "noarch", Version: "2"},
			},
			Target: "T",
		},
	}

	for idx, c := range scenarios {
		f := newRelFixture(t, c.Installed, c.Available)
		ins := New(f.cache, f.db, Options{PackageConflicts: c.Mode})

		logs := testutil.CaptureLogs(t)
		var planned *Plan
		var planErr error
		planOut := captureStdout(t, func() {
			planned, planErr = ins.Plan(goolib.PackageInfo{Name: c.Target}, "", f.rm, corpusArchs)
		})
		if planErr != nil {
			t.Fatalf("case %d: Plan(%s) unexpected error: %v", idx, c.Target, planErr)
		}
		if logs.Len() != 0 || planOut != "" {
			t.Errorf("case %d: Plan emitted logs=%q stdout=%q, want both empty", idx, logs.String(), planOut)
		}

		var installedOrder, uninstalledOrder []string
		ri := &repoInstall{
			Installer:  ins,
			rm:         f.rm,
			archs:      corpusArchs,
			downloader: f.downloader,
			stageHook: func(stage applyStage, step string) error {
				switch stage {
				case stageAfterUninstall:
					uninstalledOrder = append(uninstalledOrder, step)
				case stageCommitted:
					installedOrder = append(installedOrder, step)
				}
				return nil
			},
		}
		captureStdout(t, func() {
			if err := ri.fromRepo(t.Context(), goolib.PackageInfo{Name: c.Target}, ""); err != nil {
				t.Fatalf("case %d: fromRepo(%s) unexpected error: %v", idx, c.Target, err)
			}
		})

		var wantInstalls, wantRemovals []string
		for _, pi := range planned.Installs() {
			wantInstalls = append(wantInstalls, pi.String())
		}
		for _, pi := range planned.Removals() {
			wantRemovals = append(wantRemovals, pi.String())
		}
		if diff := cmp.Diff(wantInstalls, installedOrder); diff != "" {
			t.Errorf("case %d: Installs() vs committed steps (-want +got):\n%s", idx, diff)
		}
		if diff := cmp.Diff(wantRemovals, uninstalledOrder); diff != "" {
			t.Errorf("case %d: Removals() vs uninstalled steps (-want +got):\n%s", idx, diff)
		}

		// Every final installed package has both its payload file on disk and
		// its .goo archive in the cache, and no stale removed archives remain.
		finalPkgs := f.installed(t)
		var wantCache []string
		var wantDstFiles []string
		for _, pkg := range finalPkgs {
			pi := goolib.PkgNameSplit(pkg)
			wantCache = append(wantCache, pkg+".goo")
			wantDstFiles = append(wantDstFiles, pi.Name)
		}
		slices.Sort(wantCache)
		slices.Sort(wantDstFiles)

		gotCache := f.cacheFiles(t)
		slices.Sort(gotCache)
		if diff := cmp.Diff(wantCache, gotCache); diff != "" {
			t.Errorf("case %d: cache directory archives (-want +got):\n%s", idx, diff)
		}

		dstMap := treeDigest(t, f.dst)
		var gotDstFiles []string
		for rel := range dstMap {
			gotDstFiles = append(gotDstFiles, rel)
		}
		slices.Sort(gotDstFiles)
		if diff := cmp.Diff(wantDstFiles, gotDstFiles); diff != "" {
			t.Errorf("case %d: destination payload files (-want +got):\n%s", idx, diff)
		}
	}
}
