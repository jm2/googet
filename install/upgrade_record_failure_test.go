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
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/google/googet/v2/client"
	"github.com/google/googet/v2/goolib"
	"github.com/google/googet/v2/testutil"
)

// TestFailedUpgradeRecordKeepsOldVersion checks that when recording an
// upgrade fails, the new version's files are rolled back and the old
// version's files, cached archive and row are left as they were, because
// the old version is cleaned up only after the new one is recorded. With no
// replaced packages to recover, recording isn't tried again.
func TestFailedUpgradeRecordKeepsOldVersion(t *testing.T) {
	f := newRelFixture(t,
		[]goolib.PkgSpec{{Name: "foo", Arch: "noarch", Version: "1"}},
		[]goolib.PkgSpec{{Name: "foo", Arch: "noarch", Version: "2"}})
	file, archive := addInstalledFile(t, f, "foo", "noarch")
	before := f.rows(t)
	logs := testutil.CaptureLogs(t)
	full := errors.New("disk full")
	attempts := 0
	err := f.runWithHook(t, t.Context(), Options{}, "foo", func(stage applyStage, step string) error {
		if stage == stageAddPkg && step == "foo.noarch.2" {
			attempts++
			return full
		}
		return nil
	})
	if !errors.Is(err, full) {
		t.Fatalf("install foo = %v, want %v", err, full)
	}
	if attempts != 1 {
		t.Errorf("recording foo.noarch.2 was attempted %d times, want 1", attempts)
	}
	if diff := cmp.Diff(before, f.rows(t), cmpopts.IgnoreFields(client.PackageState{}, "InstalledApp")); diff != "" {
		t.Errorf("rows after the failed upgrade (-before +after):\n%s", diff)
	}
	for _, p := range []string{file, archive} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s: %v, want the old version's file kept", p, err)
		}
	}
	got, err := os.ReadFile(filepath.Join(f.dst, "foo"))
	if err != nil || string(got) != "foo.noarch.1" {
		t.Errorf("foo's file = %q, %v; want the old version's content restored", got, err)
	}
	if strings.Contains(logs.String(), "Cleaning up old file") {
		t.Errorf("cleaned up the old version although the new one wasn't recorded; logs:\n%s", logs)
	}
}
