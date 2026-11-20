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
	"os"
	"path/filepath"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/googet/v2/client"
	"github.com/google/googet/v2/googetdb"
	"github.com/google/googet/v2/goolib"
)

// TestRecordPolicies pins the commit-path reconciliation policy for each
// installer caller.
func TestRecordPolicies(t *testing.T) {
	t.Parallel()
	captured := &client.PackageState{}
	tests := []struct {
		name         string
		got          recordPolicy
		captured     *client.PackageState
		want         recordPolicy
		wantSnapshot bool
	}{
		{
			name:         "apply_step_real_install",
			got:          applyStepPolicy(false),
			want:         recordPolicy{cleanup: true, removeArchive: true, carryDirs: true, transfers: true},
			wantSnapshot: true,
		},
		{
			name:         "apply_step_db_only",
			got:          applyStepPolicy(true),
			want:         recordPolicy{cleanup: false, removeArchive: true, carryDirs: false, transfers: false},
			wantSnapshot: true,
		},
		{
			name:         "from_disk_real_install",
			got:          fromDiskPolicy(false),
			want:         recordPolicy{cleanup: false, removeArchive: false, carryDirs: true, transfers: true},
			wantSnapshot: true,
		},
		{
			name:         "from_disk_db_only",
			got:          fromDiskPolicy(true),
			want:         recordPolicy{cleanup: false, removeArchive: false, carryDirs: false, transfers: false},
			wantSnapshot: false,
		},
		{
			name:         "recover_reinstall",
			got:          recoverPolicy(false),
			captured:     captured,
			want:         recordPolicy{cleanup: false, removeArchive: false, carryDirs: true, transfers: false},
			wantSnapshot: false,
		},
		{
			name:         "recover_policy_without_captured_row_requires_snapshot",
			got:          recoverPolicy(false),
			captured:     nil,
			want:         recordPolicy{cleanup: false, removeArchive: false, carryDirs: true, transfers: false},
			wantSnapshot: true,
		},
		{
			name:         "recover_db_only",
			got:          recoverPolicy(true),
			captured:     captured,
			want:         recordPolicy{cleanup: false, removeArchive: false, carryDirs: false, transfers: false},
			wantSnapshot: false,
		},
		{
			name:         "reinstall",
			got:          reinstallPolicy(),
			want:         recordPolicy{cleanup: true, removeArchive: false, carryDirs: true, transfers: true},
			wantSnapshot: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if tc.got != tc.want {
				t.Errorf("policy = %+v, want %+v", tc.got, tc.want)
			}
			if gotSnap := tc.got.needsSnapshot(tc.captured); gotSnap != tc.wantSnapshot {
				t.Errorf("policy.needsSnapshot(%v) = %t, want %t", tc.captured != nil, gotSnap, tc.wantSnapshot)
			}
		})
	}
}

type countingStateReader struct {
	state client.GooGetState
	err   error
	calls int
	args  []string
}

func (r *countingStateReader) FetchPkgs(name string) (client.GooGetState, error) {
	r.calls++
	r.args = append(r.args, name)
	if r.err != nil {
		return nil, r.err
	}
	return r.state, nil
}

// TestRecordInstallSingleSnapshotRead verifies that recordInstall reads
// FetchPkgs("") at most once per commit, reconciles only the exact name.arch
// row, skips reading when the policy needs no snapshot, and aborts before write
// when FetchPkgs fails.
func TestRecordInstallSingleSnapshotRead(t *testing.T) {
	t.Parallel()

	t.Run("apply_step_reads_once_and_cleans_exact_arch", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		droppedFile := filepath.Join(dir, "old-only.txt")
		keptFile := filepath.Join(dir, "kept.txt")
		otherArchFile := filepath.Join(dir, "other-arch.txt")
		oldArchive := filepath.Join(dir, "pkg.noarch.1.goo")
		otherArchive := filepath.Join(dir, "pkg.x86_64.1.goo")
		for _, p := range []string{droppedFile, keptFile, otherArchFile, oldArchive, otherArchive} {
			if err := os.WriteFile(p, []byte("data"), 0644); err != nil {
				t.Fatal(err)
			}
		}

		reader := &countingStateReader{
			state: client.GooGetState{
				{
					PackageSpec: &goolib.PkgSpec{Name: "pkg", Arch: "noarch", Version: "1"},
					LocalPath:   oldArchive,
					InstalledFiles: map[string]string{
						droppedFile: "sum-old",
						keptFile:    "sum-kept",
					},
				},
				{
					PackageSpec: &goolib.PkgSpec{Name: "pkg", Arch: "x86_64", Version: "1"},
					LocalPath:   otherArchive,
					InstalledFiles: map[string]string{
						otherArchFile: "sum-x64",
					},
				},
			},
		}
		newRow := client.PackageState{
			PackageSpec:    &goolib.PkgSpec{Name: "pkg", Arch: "noarch", Version: "2"},
			InstalledFiles: map[string]string{keptFile: "sum-kept"},
		}

		var written []client.PackageState
		err := recordInstall(reader, newRow, nil, applyStepPolicy(false), func(got client.PackageState, _ []googetdb.FileTransfer) error {
			written = append(written, got)
			return nil
		})
		if err != nil {
			t.Fatalf("recordInstall() unexpected error: %v", err)
		}
		if reader.calls != 1 {
			t.Errorf("FetchPkgs call count = %d, want 1", reader.calls)
		}
		if diff := cmp.Diff([]string{""}, reader.args); diff != "" {
			t.Errorf("FetchPkgs args (-want +got):\n%s", diff)
		}
		if len(written) != 1 {
			t.Fatalf("write call count = %d, want 1", len(written))
		}
		for _, gone := range []string{droppedFile, oldArchive} {
			if _, statErr := os.Stat(gone); !os.IsNotExist(statErr) {
				t.Errorf("Stat(%s) = %v, want removed", gone, statErr)
			}
		}
		for _, kept := range []string{keptFile, otherArchFile, otherArchive} {
			if _, statErr := os.Stat(kept); statErr != nil {
				t.Errorf("Stat(%s) = %v, want preserved", kept, statErr)
			}
		}
	})

	t.Run("apply_step_db_only_reads_once_and_skips_file_cleanup", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		droppedFile := filepath.Join(dir, "old-only.txt")
		oldArchive := filepath.Join(dir, "pkg.noarch.1.goo")
		for _, p := range []string{droppedFile, oldArchive} {
			if err := os.WriteFile(p, []byte("data"), 0644); err != nil {
				t.Fatal(err)
			}
		}

		reader := &countingStateReader{
			state: client.GooGetState{
				{
					PackageSpec:    &goolib.PkgSpec{Name: "pkg", Arch: "noarch", Version: "1"},
					LocalPath:      oldArchive,
					InstalledFiles: map[string]string{droppedFile: "sum-old"},
				},
			},
		}
		newRow := client.PackageState{
			PackageSpec:    &goolib.PkgSpec{Name: "pkg", Arch: "noarch", Version: "2"},
			InstalledFiles: map[string]string{},
		}

		writeCalls := 0
		if err := recordInstall(reader, newRow, nil, applyStepPolicy(true), func(client.PackageState, []googetdb.FileTransfer) error {
			writeCalls++
			return nil
		}); err != nil {
			t.Fatalf("recordInstall(dbOnly) unexpected error: %v", err)
		}
		if reader.calls != 1 {
			t.Errorf("FetchPkgs call count = %d, want 1", reader.calls)
		}
		if writeCalls != 1 {
			t.Errorf("write call count = %d, want 1", writeCalls)
		}
		if _, statErr := os.Stat(droppedFile); statErr != nil {
			t.Errorf("Stat(%s) = %v, want preserved under dbOnly", droppedFile, statErr)
		}
		if _, statErr := os.Stat(oldArchive); !os.IsNotExist(statErr) {
			t.Errorf("Stat(%s) = %v, want old archive removed", oldArchive, statErr)
		}
	})

	t.Run("no_snapshot_policies_do_not_call_fetch_pkgs", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		survivingCapturedDir := filepath.Join(dir, "captured_dir")
		missingCapturedDir := filepath.Join(dir, "gone_dir")
		capturedFile := filepath.Join(dir, "captured_file.txt")
		if err := os.MkdirAll(survivingCapturedDir, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(capturedFile, []byte("file"), 0644); err != nil {
			t.Fatal(err)
		}

		capturedState := &client.PackageState{
			PackageSpec: &goolib.PkgSpec{Name: "pkg", Arch: "noarch", Version: "1"},
			InstalledFiles: map[string]string{
				survivingCapturedDir: "",
				missingCapturedDir:   "",
				capturedFile:         "sum-file",
			},
		}
		for _, tc := range []struct {
			name     string
			policy   recordPolicy
			captured *client.PackageState
			wantDirs map[string]string
		}{
			{
				name:     "from_disk_db_only",
				policy:   fromDiskPolicy(true),
				captured: nil,
				wantDirs: nil,
			},
			{
				name:     "recover_real_install_carries_captured_dirs_without_snapshot",
				policy:   recoverPolicy(false),
				captured: capturedState,
				wantDirs: map[string]string{survivingCapturedDir: ""},
			},
			{
				name:     "recover_db_only_skips_carry_and_snapshot",
				policy:   recoverPolicy(true),
				captured: capturedState,
				wantDirs: nil,
			},
		} {
			reader := &countingStateReader{err: errors.New("unexpected snapshot read")}
			var written []client.PackageState
			row := client.PackageState{PackageSpec: &goolib.PkgSpec{Name: "pkg", Arch: "noarch", Version: "1"}}
			if err := recordInstall(reader, row, tc.captured, tc.policy, func(got client.PackageState, _ []googetdb.FileTransfer) error {
				written = append(written, got)
				return nil
			}); err != nil {
				t.Fatalf("%s: recordInstall(%+v) unexpected error: %v", tc.name, tc.policy, err)
			}
			if reader.calls != 0 {
				t.Errorf("%s: FetchPkgs call count = %d, want 0", tc.name, reader.calls)
			}
			if len(written) != 1 {
				t.Fatalf("%s: write call count = %d, want 1", tc.name, len(written))
			}
			if diff := cmp.Diff(tc.wantDirs, written[0].InstalledFiles); diff != "" {
				t.Errorf("%s: InstalledFiles mismatch (-want +got):\n%s", tc.name, diff)
			}
		}
	})

	t.Run("carry_dirs_with_nil_captured_reads_snapshot_once_and_carries_old_dirs", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		survivingOldDir := filepath.Join(dir, "old_surviving_dir")
		missingOldDir := filepath.Join(dir, "old_missing_dir")
		if err := os.MkdirAll(survivingOldDir, 0755); err != nil {
			t.Fatal(err)
		}

		reader := &countingStateReader{
			state: client.GooGetState{
				{
					PackageSpec: &goolib.PkgSpec{Name: "pkg", Arch: "noarch", Version: "1"},
					InstalledFiles: map[string]string{
						survivingOldDir: "",
						missingOldDir:   "",
					},
				},
			},
		}
		newFile := filepath.Join(dir, "new.txt")
		row := client.PackageState{
			PackageSpec:    &goolib.PkgSpec{Name: "pkg", Arch: "noarch", Version: "2"},
			InstalledFiles: map[string]string{newFile: "sum-new"},
		}
		var written []client.PackageState
		if err := recordInstall(reader, row, nil, recordPolicy{carryDirs: true}, func(got client.PackageState, _ []googetdb.FileTransfer) error {
			written = append(written, got)
			return nil
		}); err != nil {
			t.Fatalf("recordInstall(carryDirs only, captured=nil) unexpected error: %v", err)
		}
		if reader.calls != 1 {
			t.Errorf("FetchPkgs call count = %d, want 1", reader.calls)
		}
		if len(written) != 1 {
			t.Fatalf("write call count = %d, want 1", len(written))
		}
		wantFiles := map[string]string{
			newFile:         "sum-new",
			survivingOldDir: "",
		}
		if diff := cmp.Diff(wantFiles, written[0].InstalledFiles); diff != "" {
			t.Errorf("written InstalledFiles mismatch (-want +got):\n%s", diff)
		}
	})

	t.Run("fetch_pkgs_error_aborts_before_write", func(t *testing.T) {
		t.Parallel()
		wantErr := errors.New("corrupt state row")
		reader := &countingStateReader{err: wantErr}
		writeCalls := 0
		row := client.PackageState{PackageSpec: &goolib.PkgSpec{Name: "pkg", Arch: "noarch", Version: "2"}}
		err := recordInstall(reader, row, nil, applyStepPolicy(false), func(client.PackageState, []googetdb.FileTransfer) error {
			writeCalls++
			return nil
		})
		if !errors.Is(err, wantErr) {
			t.Errorf("recordInstall() error = %v, want %v", err, wantErr)
		}
		if reader.calls != 1 {
			t.Errorf("FetchPkgs call count = %d, want 1", reader.calls)
		}
		if writeCalls != 0 {
			t.Errorf("write call count = %d, want 0", writeCalls)
		}
	})
}
