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
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/googet/v2/client"
	"github.com/google/googet/v2/goolib"
	"github.com/google/googet/v2/internal/ownership"
)

// TestPlanCleanupClassification verifies that planCleanup classifies dropped
// entries using old[file], ignores case-only renames under WindowsRule, keeps
// dropped paths still listed by other rows regardless of case or checksum, and
// sends legacy empty-checksum file entries through the directory pass.
func TestPlanCleanupClassification(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		state client.GooGetState
		old   map[string]string
		ins   map[string]string
		want  ownership.DeletePlan
	}{
		{
			name: "classifies_by_old_checksum_and_legacy_empty_checksum_goes_to_dirs",
			old: map[string]string{
				`C:\App\dropped.dll`:               "sum-dropped",
				`C:\App\OldDir\legacy_db_only.dll`: "",
				`C:\App\OldDir`:                    "",
				`C:\App\OldDir\Sub`:                "",
				`C:\App\kept.dll`:                  "sum-v1",
			},
			ins: map[string]string{
				`C:\App\kept.dll`: "sum-v2",
			},
			want: ownership.DeletePlan{
				Files: []string{`C:\App\dropped.dll`},
				Dirs: []string{
					`C:\App\OldDir\Sub`,
					`C:\App\OldDir\legacy_db_only.dll`,
					`C:\App\OldDir`,
				},
			},
		},
		{
			name: "case_only_rename_is_not_dropped",
			old: map[string]string{
				`C:\App\Foo.dll`:     "sum-v1",
				`C:\App\Sub\Bar.dll`: "sum-bar-v1",
			},
			ins: map[string]string{
				`c:\app\foo.dll`:     "sum-v2",
				`C:/App/sub/bar.dll`: "sum-bar-v2",
			},
			want: ownership.DeletePlan{},
		},
		{
			name: "dropped_entries_owned_in_any_case_or_checksum_go_to_kept",
			state: client.GooGetState{
				{
					PackageSpec: &goolib.PkgSpec{Name: "pkgB", Arch: "noarch", Version: "1"},
					InstalledFiles: map[string]string{
						`c:/shared/file.dll`:       "sum-b",
						`c:\shared\legacy_dir_own`: "",
					},
				},
				{
					PackageSpec: &goolib.PkgSpec{Name: "pkgC", Arch: "x86_64", Version: "1"},
					InstalledFiles: map[string]string{
						`C:\SHARED\EMPTY_FILE.DLL`: "",
					},
				},
			},
			old: map[string]string{
				`C:\Shared\File.dll`:       "sum-a",
				`C:\Shared\Legacy_Dir_Own`: "",
				`C:\Shared\empty_file.dll`: "sum-a-file",
				`C:\OnlyA\gone.dll`:        "sum-gone",
			},
			ins: map[string]string{},
			want: ownership.DeletePlan{
				Files: []string{`C:\OnlyA\gone.dll`},
				Kept: []ownership.Kept{
					{
						Path:   `C:\Shared\empty_file.dll`,
						Owners: []ownership.RowID{{Name: "pkgC", Arch: "x86_64"}},
					},
					{
						Path:   `C:\Shared\File.dll`,
						Owners: []ownership.RowID{{Name: "pkgB", Arch: "noarch"}},
					},
					{
						Path:   `C:\Shared\Legacy_Dir_Own`,
						Owners: []ownership.RowID{{Name: "pkgB", Arch: "noarch"}},
					},
				},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ix := ownership.NewIndexWithRule(tc.state, ownership.RowID{Name: "pkgA", Arch: "noarch"}, ownership.WindowsRule)
			got := planCleanup(tc.old, tc.ins, ix)
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("planCleanup() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestPlanCleanupCaseOnlyRename verifies that upgrading a package whose file
// path changes only in casing produces an empty deletion plan under WindowsRule
// and leaves legacy empty-checksum files deletable when actually dropped.
func TestPlanCleanupCaseOnlyRename(t *testing.T) {
	t.Parallel()
	ix := ownership.NewIndexWithRule(nil, ownership.RowID{Name: "app", Arch: "noarch"}, ownership.WindowsRule)
	oldFiles := map[string]string{
		`C:\Program Files\App\Foo.dll`: "sum-v1",
	}
	newFiles := map[string]string{
		`C:\Program Files\App\foo.dll`: "sum-v2",
	}
	got := planCleanup(oldFiles, newFiles, ix)
	if diff := cmp.Diff(ownership.DeletePlan{}, got); diff != "" {
		t.Errorf("planCleanup(Foo.dll -> foo.dll) = %+v, want empty DeletePlan (-want +got):\n%s", got, diff)
	}

	// Verify that cleanOldFiles passes a dropped legacy empty-checksum file
	// entry to the remover via the directory pass.
	var removed []string
	st := client.PackageState{
		PackageSpec:    &goolib.PkgSpec{Name: "app", Arch: "noarch", Version: "1"},
		InstalledFiles: map[string]string{`C:\Program Files\App\legacy.dll`: ""},
	}
	cleanOldFiles(st, map[string]string{}, ix, func(p string) (string, error) {
		removed = append(removed, p)
		return "", nil
	}, ownership.Logger{})
	if diff := cmp.Diff([]string{`C:\Program Files\App\legacy.dll`}, removed); diff != "" {
		t.Errorf("cleanOldFiles(legacy empty-checksum file) removed = %v (-want +got):\n%s", removed, diff)
	}
}
