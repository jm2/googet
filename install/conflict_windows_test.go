//go:build windows

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
	"github.com/google/googet/v2/testutil"
)

// TestInstallCaseVariantConflict verifies that on Windows, installing a package
// whose file path differs only in casing from an already-installed package's
// file emits the unchanged conflict warning by default and fails under
// StrictConflicts without Force.
func TestInstallCaseVariantConflict(t *testing.T) {
	f := newRelFixture(t, nil, nil)
	upperPath := filepath.Join(f.dst, "SubDir", "Shared.dll")
	lowerPath := filepath.Join(f.dst, "subdir", "shared.dll")

	gooA := filepath.Join(f.cache, "A.noarch.1.goo")
	writeGoo(t, gooA, map[string]string{"Shared.dll": "from A"})
	specA := &goolib.PkgSpec{
		Name:    "A",
		Arch:    "noarch",
		Version: "1",
		Files:   map[string]string{"Shared.dll": upperPath},
	}
	if _, err := installPkg(gooA, specA, Options{}, f.db, func(insFiles map[string]string) error {
		return f.db.AddPkg(client.PackageState{
			LocalPath:      gooA,
			PackageSpec:    specA,
			InstalledFiles: insFiles,
		})
	}); err != nil {
		t.Fatalf("installPkg(A): %v", err)
	}

	gooB := filepath.Join(f.cache, "B.noarch.1.goo")
	writeGoo(t, gooB, map[string]string{"shared.dll": "from B"})
	specB := &goolib.PkgSpec{
		Name:    "B",
		Arch:    "noarch",
		Version: "1",
		Files:   map[string]string{"shared.dll": lowerPath},
	}

	// StrictConflicts without Force rejects the case-variant overlap.
	_, err := installPkg(gooB, specB, Options{StrictConflicts: true}, f.db, nil)
	wantConflict := "file conflict: " + lowerPath + " is already owned by package A"
	if err == nil || !strings.Contains(err.Error(), wantConflict) {
		t.Fatalf("installPkg(B, StrictConflicts) = %v, want error containing %q", err, wantConflict)
	}
	if got, readErr := os.ReadFile(upperPath); readErr != nil || string(got) != "from A" {
		t.Errorf("Shared.dll after rejected StrictConflicts install = %q, %v; want %q", got, readErr, "from A")
	}

	// Default options allow the overwrite and emit the unchanged warning text.
	logs := testutil.CaptureLogs(t)
	var warnErr error
	stdout := captureStdout(t, func() {
		_, warnErr = installPkg(gooB, specB, Options{}, f.db, nil)
	})
	if warnErr != nil {
		t.Fatalf("installPkg(B, default) unexpected error: %v", warnErr)
	}
	wantConsole := "Warning: " + wantConflict + ", overwriting...\n"
	if !strings.Contains(stdout, wantConsole) {
		t.Errorf("console output = %q, want substring %q", stdout, wantConsole)
	}
	if !strings.Contains(logs.String(), "Warning: "+wantConflict) {
		t.Errorf("logs = %q, want substring %q", logs.String(), "Warning: "+wantConflict)
	}
	if got, readErr := os.ReadFile(lowerPath); readErr != nil || string(got) != "from B" {
		t.Errorf("shared.dll after default install = %q, %v; want %q", got, readErr, "from B")
	}
}
