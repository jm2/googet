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
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/googet/v2/goolib"
)

// simulatedDeath is the panic value tests throw from stageHook to abort an
// install mid-step without running recovery, simulating a hard process kill.
type simulatedDeath struct {
	at string
}

// runUntilSimulatedDeath executes R with hook panicking at a target stage and
// catches simulatedDeath, returning captured stdout.
func runUntilSimulatedDeath(t *testing.T, f *relFixture, hook func(applyStage, string) error) string {
	t.Helper()
	var panicked bool
	out := captureStdout(t, func() {
		defer func() {
			if r := recover(); r != nil {
				if _, ok := r.(simulatedDeath); !ok {
					panic(r)
				}
				panicked = true
			}
		}()
		_ = f.runWithHook(t, t.Context(), Options{}, "R", hook)
	})
	if !panicked {
		t.Fatal("expected simulatedDeath panic, but runWithHook returned normally")
	}
	return out
}

// TestSimulatedDeathAtEachStagePreservesArchivesAndAllowsRecovery checks that
// an abrupt process termination (skipping in-memory recovery) at each apply
// stage leaves the replace_record printed, keeps every victim's cached
// archive on disk, and allows a subsequent FromRepo install of R (or manual
// FromDisk reinstall from the kept archives) to converge cleanly.
func TestSimulatedDeathAtEachStagePreservesArchivesAndAllowsRecovery(t *testing.T) {
	stages := []struct {
		name              string
		stage             applyStage
		step              string
		expectRecordLine  bool
		stillInstalledNow []string
	}{
		{
			name:              "before_first_uninstall",
			stage:             stageBeforeUninstall,
			step:              "D.noarch.1",
			expectRecordLine:  true,
			stillInstalledNow: []string{"D.noarch.1", "P.noarch.1"},
		},
		{
			name:              "after_first_uninstall",
			stage:             stageAfterUninstall,
			step:              "D.noarch.1",
			expectRecordLine:  true,
			stillInstalledNow: []string{"P.noarch.1"},
		},
		{
			name:              "before_second_uninstall",
			stage:             stageBeforeUninstall,
			step:              "P.noarch.1",
			expectRecordLine:  true,
			stillInstalledNow: []string{"P.noarch.1"},
		},
		{
			name:              "after_second_uninstall",
			stage:             stageAfterUninstall,
			step:              "P.noarch.1",
			expectRecordLine:  true,
			stillInstalledNow: nil,
		},
		{
			name:              "after_replace",
			stage:             stageAfterReplace,
			step:              "R.noarch.1",
			expectRecordLine:  true,
			stillInstalledNow: nil,
		},
		{
			name:              "after_install_pkg",
			stage:             stageAfterInstall,
			step:              "R.noarch.1",
			expectRecordLine:  true,
			stillInstalledNow: nil,
		},
		{
			name:              "during_add_pkg",
			stage:             stageAddPkg,
			step:              "R.noarch.1",
			expectRecordLine:  true,
			stillInstalledNow: nil,
		},
	}

	for _, tc := range stages {
		t.Run(tc.name+"_rerun_replacer", func(t *testing.T) {
			f := replaceFixture(t, goolib.PkgSpec{})
			before := f.rows(t)
			out := runUntilSimulatedDeath(t, f, func(stage applyStage, step string) error {
				if stage == tc.stage && step == tc.step {
					panic(simulatedDeath{at: tc.name})
				}
				return nil
			})

			wantRecord := "replace_record replacer=R.noarch.1 rows=D.noarch.1,P.noarch.1 archives=" + before["D"].LocalPath + "," + before["P"].LocalPath
			if tc.expectRecordLine && !strings.Contains(out, wantRecord) {
				t.Errorf("stdout = %q, want %q", out, wantRecord)
			}
			if diff := cmp.Diff(tc.stillInstalledNow, f.installed(t)); diff != "" {
				t.Errorf("installed right after death (-want +got):\n%s", diff)
			}
			// Both victim archives remain intact on disk at the paths named by replace_record.
			for _, name := range []string{"P", "D"} {
				if _, err := os.Stat(before[name].LocalPath); err != nil {
					t.Errorf("%s archive %s missing after simulated death: %v", name, before[name].LocalPath, err)
				}
			}

			// Rerunning FromRepo("R") completes cleanly without trying to
			// uninstall rows that were already removed before the crash.
			if err := f.install(t, "R"); err != nil {
				t.Fatalf("rerun install(R) after simulated death at %s failed: %v", tc.name, err)
			}
			if diff := cmp.Diff([]string{"R.noarch.1"}, f.installed(t)); diff != "" {
				t.Errorf("installed after rerun (-want +got):\n%s", diff)
			}
			if !f.fileExists("R") {
				t.Errorf("R's payload file missing after rerun")
			}
		})

		if tc.stage != stageAfterInstall && tc.stage != stageAddPkg {
			t.Run(tc.name+"_manual_archive_reinstall", func(t *testing.T) {
				f := replaceFixture(t, goolib.PkgSpec{})
				before := f.rows(t)
				_ = runUntilSimulatedDeath(t, f, func(stage applyStage, step string) error {
					if stage == tc.stage && step == tc.step {
						panic(simulatedDeath{at: tc.name})
					}
					return nil
				})

				// Operator manually reinstalls removed victims in dependency order
				// (P first, then D) using the kept archives from replace_record.
				ins := New(f.cache, f.db, Options{})
				for _, name := range []string{"P", "D"} {
					if err := ins.FromDisk(before[name].LocalPath, false); err != nil {
						t.Fatalf("manual FromDisk(%s) failed: %v", name, err)
					}
				}
				if diff := cmp.Diff([]string{"D.noarch.1", "P.noarch.1"}, f.installed(t)); diff != "" {
					t.Errorf("installed after manual recovery (-want +got):\n%s", diff)
				}
				for _, name := range []string{"P", "D"} {
					if !f.fileExists(name) {
						t.Errorf("%s payload missing after manual recovery", name)
					}
				}
			})
		}
	}

	t.Run("reinstall_directly_from_cached_archive_preserves_archive", func(t *testing.T) {
		f := newRelFixture(t, []goolib.PkgSpec{{Name: "P", Arch: "noarch", Version: "1"}}, nil)
		cached := f.rows(t)["P"].LocalPath
		wantSum := checksumOf(t, cached)
		if err := New(f.cache, f.db, Options{}).FromDisk(cached, true); err != nil {
			t.Fatalf("FromDisk(cached, reinstall=true): %v", err)
		}
		if gotSum := checksumOf(t, cached); gotSum != wantSum {
			t.Errorf("cached archive checksum = %s, want %s", gotSum, wantSum)
		}
		if !f.fileExists("P") {
			t.Errorf("P's payload file missing after FromDisk reinstall")
		}
	})
}
