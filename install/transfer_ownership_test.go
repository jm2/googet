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
	"github.com/google/googet/v2/googetdb"
	"github.com/google/googet/v2/goolib"
	"github.com/google/googet/v2/internal/ownership"
)

// TestTransfersFor verifies that transfersFor selects only non-empty-checksum
// entries from other rows whose checksum differs from the newly installed
// file's checksum, preserves each owner row's exact path spelling under
// WindowsRule, transfers across different architectures of the same package
// name, and never includes the incoming row's own (Name, Arch) or
// empty-checksum entries.
func TestTransfersFor(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		state client.GooGetState
		skip  ownership.RowID
		row   client.PackageState
		want  []googetdb.FileTransfer
	}{
		{
			name: "mixed_owners_exact_and_case_variants_under_windows_rule",
			state: client.GooGetState{
				{
					PackageSpec: &goolib.PkgSpec{Name: "pkgA", Arch: "noarch", Version: "1.0.0"},
					InstalledFiles: map[string]string{
						`c:/Program Files/Shared/tool.dll`: "sum-same",
					},
				},
				{
					PackageSpec: &goolib.PkgSpec{Name: "pkgB", Arch: "x86_64", Version: "1.0.0"},
					InstalledFiles: map[string]string{
						`C:\Program Files\Shared\Tool.dll`: "sum-old-self",
					},
				},
				{
					PackageSpec: &goolib.PkgSpec{Name: "pkgB", Arch: "x86_32", Version: "1.0.0"},
					InstalledFiles: map[string]string{
						`C:\Program Files\Shared\Tool.dll`: "sum-other-arch",
					},
				},
				{
					PackageSpec: &goolib.PkgSpec{Name: "pkgC", Arch: "noarch", Version: "1.0.0"},
					InstalledFiles: map[string]string{
						`C:\PROGRAM FILES\SHARED\TOOL.DLL`: "sum-diff-c",
						`C:\Program Files\Shared\OnlyC`:    "sum-only-c",
					},
				},
				{
					PackageSpec: &goolib.PkgSpec{Name: "pkgD", Arch: "noarch", Version: "1.0.0"},
					InstalledFiles: map[string]string{
						`C:\Program Files\Shared\Tool.dll`: "",
						`C:\Program Files\Shared`:          "sum-file-on-dir",
					},
				},
			},
			// Even if the caller forgot to skip pkgB.x86_64 when building ix,
			// transfersFor itself must exclude pkgB.x86_64 while keeping
			// pkgB.x86_32.
			skip: ownership.RowID{},
			row: client.PackageState{
				PackageSpec: &goolib.PkgSpec{Name: "pkgB", Arch: "x86_64", Version: "2.0.0"},
				InstalledFiles: map[string]string{
					`C:\Program Files\Shared\tool.dll`: "sum-same",
					`C:\Program Files\Shared`:          "",
				},
			},
			want: []googetdb.FileTransfer{
				{
					Name:  "pkgB",
					Arch:  "x86_32",
					Files: []string{`C:\Program Files\Shared\Tool.dll`},
				},
				{
					Name:  "pkgC",
					Arch:  "noarch",
					Files: []string{`C:\PROGRAM FILES\SHARED\TOOL.DLL`},
				},
			},
		},
		{
			name: "identical_checksums_and_empty_entries_yield_nil",
			state: client.GooGetState{
				{
					PackageSpec: &goolib.PkgSpec{Name: "pkgA", Arch: "noarch", Version: "1.0.0"},
					InstalledFiles: map[string]string{
						`C:\Shared\same.dll`:   "sum-1",
						`C:\Shared\legacy.dll`: "",
					},
				},
			},
			skip: ownership.RowID{Name: "pkgB", Arch: "noarch"},
			row: client.PackageState{
				PackageSpec: &goolib.PkgSpec{Name: "pkgB", Arch: "noarch", Version: "1.0.0"},
				InstalledFiles: map[string]string{
					`c:\shared\SAME.dll`:   "sum-1",
					`C:\Shared\legacy.dll`: "sum-real",
				},
			},
			want: nil,
		},
		{
			name: "multiple_transferred_files_sorted_and_deduplicated_per_owner",
			state: client.GooGetState{
				{
					PackageSpec: &goolib.PkgSpec{Name: "pkgZ", Arch: "noarch", Version: "1.0.0"},
					InstalledFiles: map[string]string{
						`C:\App\z.dll`: "sum-z-old",
						`C:\App\a.dll`: "sum-a-old",
					},
				},
				{
					PackageSpec: &goolib.PkgSpec{Name: "pkgA", Arch: "arm64", Version: "1.0.0"},
					InstalledFiles: map[string]string{
						`c:/app/a.dll`: "sum-arm-old",
					},
				},
			},
			skip: ownership.RowID{Name: "pkgB", Arch: "noarch"},
			row: client.PackageState{
				PackageSpec: &goolib.PkgSpec{Name: "pkgB", Arch: "noarch", Version: "1.0.0"},
				InstalledFiles: map[string]string{
					`C:\App\a.dll`: "sum-new-a",
					`C:\App\z.dll`: "sum-new-z",
				},
			},
			want: []googetdb.FileTransfer{
				{
					Name:  "pkgA",
					Arch:  "arm64",
					Files: []string{`c:/app/a.dll`},
				},
				{
					Name:  "pkgZ",
					Arch:  "noarch",
					Files: []string{`C:\App\a.dll`, `C:\App\z.dll`},
				},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ix := ownership.NewIndexWithRule(tc.state, tc.skip, ownership.WindowsRule)
			got := transfersFor(tc.row, ix)
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("transfersFor() mismatch (-want +got):\n%s", diff)
			}
		})
	}

	if got := transfersFor(client.PackageState{}, nil); got != nil {
		t.Errorf("transfersFor(empty, nil) = %v, want nil", got)
	}
}
