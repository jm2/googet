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

package ownership

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/googet/v2/client"
	"github.com/google/googet/v2/goolib"
)

// TestPlanDeleteMixedCaseDirectoryOrder verifies that PlanDelete orders
// unowned directories deepest-first by folded key depth and folded key, so
// descendants always precede ancestors even when byte casing varies across a
// chain.
func TestPlanDeleteMixedCaseDirectoryOrder(t *testing.T) {
	t.Parallel()
	ix := NewIndexWithRule(nil, RowID{}, WindowsRule)
	entries := map[string]string{
		`c:\`:                   "",
		`C:\vendor`:             "",
		`C:\Vendor\app`:         "",
		`C:\VENDOR\app\bin`:     "",
		`C:\vendor\Zeta`:        "",
		`C:\Vendor\alpha`:       "",
		`C:\vendor\alpha\leaf`:  "",
		`C:\VENDOR\app\bin\run`: "chksum-run",
	}

	plan := PlanDelete(entries, ix)

	wantFiles := []string{`C:\VENDOR\app\bin\run`}
	if diff := cmp.Diff(wantFiles, plan.Files); diff != "" {
		t.Errorf("PlanDelete Files mismatch (-want +got):\n%s", diff)
	}

	wantDirs := []string{
		`C:\VENDOR\app\bin`,
		`C:\vendor\alpha\leaf`,
		`C:\vendor\Zeta`,
		`C:\Vendor\app`,
		`C:\Vendor\alpha`,
		`C:\vendor`,
		`c:\`,
	}
	if diff := cmp.Diff(wantDirs, plan.Dirs); diff != "" {
		t.Errorf("PlanDelete Dirs mismatch (-want +got):\n%s", diff)
	}

	// Verify the general invariant: if dir A is a strict ancestor of dir B under
	// FoldWindows, B must appear at an earlier index than A.
	for i, first := range plan.Dirs {
		kFirst := strings.TrimRight(FoldWindows(first), `\`)
		for j := i + 1; j < len(plan.Dirs); j++ {
			second := plan.Dirs[j]
			kSecond := strings.TrimRight(FoldWindows(second), `\`)
			if kFirst != kSecond && strings.HasPrefix(kSecond, kFirst+`\`) {
				t.Errorf("ancestor %q (index %d) ordered before descendant %q (index %d)", first, i, second, j)
			}
		}
	}
}

// TestPlanDeleteCaseVariantOwnershipAndExecute verifies that PlanDelete keeps
// files and directories owned by another row in a different case, deduplicates
// owner RowIDs, and executes removals and structured log lines deterministically.
func TestPlanDeleteCaseVariantOwnershipAndExecute(t *testing.T) {
	t.Parallel()
	state := client.GooGetState{
		{
			PackageSpec: &goolib.PkgSpec{Name: "pkgA", Arch: "noarch", Version: "1"},
			InstalledFiles: map[string]string{
				`C:\App\Shared.dll`: "sum-a",
				`C:\App\OnlyA.dll`:  "sum-only-a",
				`C:\App\SharedDir`:  "",
				`C:\App\OnlyADir`:   "",
			},
		},
		{
			PackageSpec: &goolib.PkgSpec{Name: "pkgB", Arch: "x86_64", Version: "1"},
			InstalledFiles: map[string]string{
				`c:/app/shared.dll`: "sum-b-x64",
			},
		},
		{
			PackageSpec: &goolib.PkgSpec{Name: "pkgB", Arch: "noarch", Version: "1"},
			InstalledFiles: map[string]string{
				`C:\APP\SHARED.DLL`: "sum-b1",
				`c:\app\shared.dll`: "sum-b2",
				`c:/app/shareddir`:  "",
			},
		},
	}

	ix := NewIndexWithRule(state, RowID{Name: "pkgA", Arch: "noarch"}, WindowsRule)
	plan := PlanDelete(state[0].InstalledFiles, ix)

	wantPlan := DeletePlan{
		Files: []string{`C:\App\OnlyA.dll`},
		Dirs:  []string{`C:\App\OnlyADir`},
		Kept: []Kept{
			{
				Path: `C:\App\Shared.dll`,
				Owners: []RowID{
					{Name: "pkgB", Arch: "noarch"},
					{Name: "pkgB", Arch: "x86_64"},
				},
			},
			{
				Path: `C:\App\SharedDir`,
				Owners: []RowID{
					{Name: "pkgB", Arch: "noarch"},
				},
			},
		},
	}
	if diff := cmp.Diff(wantPlan, plan); diff != "" {
		t.Errorf("PlanDelete mismatch (-want +got):\n%s", diff)
	}

	gotLine := keptLine("remove", "pkgA.noarch.1", plan.Kept[0])
	wantLine := `file_kept op=remove pkg=pkgA.noarch.1 path="C:\\App\\Shared.dll" owners=pkgB.noarch,pkgB.x86_64`
	if gotLine != wantLine {
		t.Errorf("keptLine() = %q, want %q", gotLine, wantLine)
	}

	// Verify Execute order and log levels purely via an injected remover and
	// captured Logger without touching the host filesystem.
	okFile := "/virtual/ok.dll"
	failFile := "/virtual/locked.dll"
	removedDir := "/virtual/empty"
	nonEmptyDir := "/virtual/nonempty"

	execPlan := DeletePlan{
		Files: []string{okFile, failFile},
		Dirs:  []string{removedDir, nonEmptyDir},
		Kept:  plan.Kept[:1],
	}

	var removedOrder []string
	var infoLines, errLines []string
	rm := func(p string) (string, error) {
		removedOrder = append(removedOrder, p)
		switch p {
		case failFile:
			return "", errors.New("file busy")
		case nonEmptyDir:
			return "", errors.New("directory not empty")
		default:
			return "", nil
		}
	}
	lg := Logger{
		Infof: func(format string, args ...any) {
			infoLines = append(infoLines, fmt.Sprintf(format, args...))
		},
		Errorf: func(format string, args ...any) {
			errLines = append(errLines, fmt.Sprintf(format, args...))
		},
	}

	Execute(execPlan, "remove", "pkgA.noarch.1", rm, lg)

	if diff := cmp.Diff([]string{okFile, failFile, removedDir, nonEmptyDir}, removedOrder); diff != "" {
		t.Errorf("Execute remover call order (-want +got):\n%s", diff)
	}
	wantInfo := []string{
		wantLine,
		fmt.Sprintf("Removing %q", okFile),
		fmt.Sprintf("Removing %q", failFile),
		fmt.Sprintf("Removing %q", removedDir),
		fmt.Sprintf("Removing %q", nonEmptyDir),
		"directory not empty",
	}
	if diff := cmp.Diff(wantInfo, infoLines); diff != "" {
		t.Errorf("Execute info logs (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff([]string{"file busy"}, errLines); diff != "" {
		t.Errorf("Execute error logs (-want +got):\n%s", diff)
	}
}
