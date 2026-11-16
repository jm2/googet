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
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/googet/v2/client"
	"github.com/google/googet/v2/goolib"
	"github.com/google/googet/v2/internal/ownership"
	"github.com/google/googet/v2/testutil"
)

// TestConflictMapWindowsRule verifies that conflictMap backed by an Index with
// WindowsRule detects case and separator variants while preserving same-name
// exclusion, directory exclusion, and ascending (Name, Arch) last-owner order.
func TestConflictMapWindowsRule(t *testing.T) {
	state := client.GooGetState{
		{
			PackageSpec: &goolib.PkgSpec{Name: "pkgZ", Arch: "noarch", Version: "1.0.0"},
			InstalledFiles: map[string]string{
				`C:\Program Files\Shared\coowned.dll`: "sum-z",
			},
		},
		{
			PackageSpec: &goolib.PkgSpec{Name: "pkgA", Arch: "noarch", Version: "1.0.0"},
			InstalledFiles: map[string]string{
				`C:\Program Files\App\Tool.exe`:       "sum-a",
				`C:\Program Files\Shared\COOWNED.DLL`: "sum-a-shared",
				`C:\Program Files\App\Dir`:            "",
			},
		},
		{
			PackageSpec: &goolib.PkgSpec{Name: "incoming", Arch: "x86_64", Version: "1.0.0"},
			InstalledFiles: map[string]string{
				`C:\Program Files\Incoming\self.dll`: "sum-self-x64",
			},
		},
		{
			PackageSpec: &goolib.PkgSpec{Name: "incoming", Arch: "noarch", Version: "1.0.0"},
			InstalledFiles: map[string]string{
				`c:/program files/incoming/SELF.dll`: "sum-self-noarch",
			},
		},
	}

	ix := ownership.NewIndexWithRule(state, ownership.RowID{}, ownership.WindowsRule)
	cm := newConflictMap(ix, "incoming")

	for _, lookup := range []string{
		`C:\Program Files\App\Tool.exe`,
		`c:\program files\app\tool.exe`,
		`c:/Program Files/App/TOOL.EXE`,
	} {
		owner, ok := cm.owner(lookup)
		if !ok || owner != "pkgA" {
			t.Errorf("cm.owner(%q) = (%q, %t), want (\"pkgA\", true)", lookup, owner, ok)
		}
	}

	// Multiple owners report the last owner in ascending (Name, Arch) order.
	if owner, ok := cm.owner(`c:/program files/shared/coowned.dll`); !ok || owner != "pkgZ" {
		t.Errorf("cm.owner(coowned) = (%q, %t), want (\"pkgZ\", true)", owner, ok)
	}

	// Directory entries with empty checksums are never reported as file conflicts.
	if owner, ok := cm.owner(`c:/program files/app/dir`); ok {
		t.Errorf("cm.owner(directory) = (%q, true), want (\"\", false)", owner)
	}

	// Every architecture of currentPkg is excluded from conflict reporting.
	if owner, ok := cm.owner(`C:\Program Files\Incoming\SELF.DLL`); ok {
		t.Errorf("cm.owner(currentPkg other arch) = (%q, true), want (\"\", false)", owner)
	}

	// Zero conflictMap safely reports no owner.
	var empty conflictMap
	if owner, ok := empty.owner(`C:\anything`); ok {
		t.Errorf("empty.owner() = (%q, true), want (\"\", false)", owner)
	}

	// Exercise makeInstallFunction against a case-variant owner through WindowsRule.
	srcDir := filepath.Join(t.TempDir(), "src")
	dstDir := t.TempDir()
	if err := os.MkdirAll(srcDir, 0755); err != nil {
		t.Fatal(err)
	}
	srcFile := filepath.Join(srcDir, "tool.exe")
	if err := os.WriteFile(srcFile, []byte("new binary"), 0644); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(srcFile)
	if err != nil {
		t.Fatal(err)
	}

	installedUpper := filepath.Join(dstDir, "TOOL.EXE")
	walkCM := newConflictMap(ownership.NewIndexWithRule(client.GooGetState{
		{
			PackageSpec:    &goolib.PkgSpec{Name: "pkgOwner", Arch: "noarch", Version: "1.0.0"},
			InstalledFiles: map[string]string{installedUpper: "sum-owner"},
		},
	}, ownership.RowID{}, ownership.WindowsRule), "pkgIncoming")

	logs := testutil.CaptureLogs(t)
	fnWarn := makeInstallFunction(srcDir, dstDir, make(map[string]string), Options{}, walkCM, &undo{})
	if err := fnWarn(srcFile, fi, nil); err != nil {
		t.Fatalf("makeInstallFunction(default) on case-variant overlap returned error: %v", err)
	}
	wantWarn := "Warning: file conflict: " + filepath.Join(dstDir, "tool.exe") + " is already owned by package pkgOwner"
	if !strings.Contains(logs.String(), wantWarn) {
		t.Errorf("makeInstallFunction(default) logs = %q, want substring %q", logs.String(), wantWarn)
	}

	fnStrict := makeInstallFunction(srcDir, dstDir, make(map[string]string), Options{StrictConflicts: true}, walkCM, &undo{})
	err = fnStrict(srcFile, fi, nil)
	if err == nil || !strings.Contains(err.Error(), "already owned by package pkgOwner") {
		t.Errorf("makeInstallFunction(StrictConflicts) = %v, want file conflict error naming pkgOwner", err)
	}
}
