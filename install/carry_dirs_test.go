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
	"runtime"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/googet/v2/internal/ownership"
)

// TestCarryDirs verifies that carryDirs adds only candidate entries with an
// empty checksum that still exist on disk as directories and are not already
// present in insFiles under the ownership index's key rule, skipping regular
// files with legacy empty checksums, non-empty-checksum entries, missing
// directories, and case-variant duplicates.
func TestCarryDirs(t *testing.T) {
	t.Parallel()
	root := t.TempDir()

	existingParent := filepath.Join(root, "created_parent")
	caseVariantExisting := filepath.Join(root, "MixedParent")
	dedupUpper := filepath.Join(root, "DupChain")
	dedupLower := filepath.Join(root, "dupchain")
	missingFirstUpper := filepath.Join(root, "MissingFirst")
	existingSecondLower := filepath.Join(root, "missingfirst")
	nonEmptySumDir := filepath.Join(root, "weird_sum_dir")
	for _, d := range []string{existingParent, caseVariantExisting, dedupUpper, dedupLower, existingSecondLower, nonEmptySumDir} {
		if err := os.MkdirAll(d, 0755); err != nil {
			t.Fatalf("MkdirAll(%q): %v", d, err)
		}
	}

	legacyEmptySumFile := filepath.Join(root, "legacy_db_only.dll")
	if err := os.WriteFile(legacyEmptySumFile, []byte("payload"), 0644); err != nil {
		t.Fatalf("WriteFile(%q): %v", legacyEmptySumFile, err)
	}
	missingDir := filepath.Join(root, "already_removed_dir")
	newCaseVariant := filepath.Join(root, "mixedparent")
	newTool := filepath.Join(newCaseVariant, "tool.exe")

	candidates := map[string]string{
		existingParent:      "",
		caseVariantExisting: "",
		dedupUpper:          "",
		dedupLower:          "",
		missingFirstUpper:   "",
		existingSecondLower: "",
		legacyEmptySumFile:  "",
		missingDir:          "",
		nonEmptySumDir:      "not-a-dir-checksum",
	}
	insFiles := map[string]string{
		newCaseVariant: "",
		newTool:        "sum-tool",
	}

	ix := ownership.NewIndexWithRule(nil, ownership.RowID{}, ownership.WindowsRule)
	got := carryDirs(candidates, insFiles, ix)

	want := map[string]string{
		newCaseVariant:      "",
		newTool:             "sum-tool",
		existingParent:      "",
		dedupUpper:          "",
		existingSecondLower: "",
	}
	if runtime.GOOS == "windows" {
		// The file system ignores case, so MissingFirst names the existing
		// missingfirst directory and, sorting first, is the spelling carried.
		delete(want, existingSecondLower)
		want[missingFirstUpper] = ""
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("carryDirs() mismatch (-want +got):\n%s", diff)
	}

	// Verify nil Index (used by replacement recovery when captured != nil)
	// normalizes keys with ownership.Key and carries only existing directories.
	uncleanedExisting := existingParent + string(filepath.Separator) + "."
	gotNilIx := carryDirs(
		map[string]string{
			existingParent:     "",
			dedupUpper:         "",
			legacyEmptySumFile: "",
			missingDir:         "",
		},
		map[string]string{
			uncleanedExisting: "",
		},
		nil,
	)
	wantNilIx := map[string]string{
		uncleanedExisting: "",
		dedupUpper:        "",
	}
	if diff := cmp.Diff(wantNilIx, gotNilIx); diff != "" {
		t.Errorf("carryDirs(nil Index) mismatch (-want +got):\n%s", diff)
	}

	// Verify nil insFiles stays empty when no candidate qualifies and carries
	// valid directories when present.
	if gotEmpty := carryDirs(nil, nil, ix); gotEmpty != nil {
		t.Errorf("carryDirs(nil, nil) = %v, want nil", gotEmpty)
	}
	gotFromNil := carryDirs(map[string]string{legacyEmptySumFile: "", missingDir: ""}, nil, ix)
	if len(gotFromNil) != 0 {
		t.Errorf("carryDirs(no valid dirs, nil) = %v, want empty", gotFromNil)
	}
	gotCarriedIntoNil := carryDirs(map[string]string{existingParent: ""}, nil, ix)
	if diff := cmp.Diff(map[string]string{existingParent: ""}, gotCarriedIntoNil); diff != "" {
		t.Errorf("carryDirs(valid dir, nil) mismatch (-want +got):\n%s", diff)
	}
}
