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
	"context"
	"database/sql"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/googet/v2/goolib"
	"github.com/google/googet/v2/settings"
	"github.com/google/googet/v2/testutil"
)

// execDB runs query against the database file directly, bypassing GooDB.
func execDB(t *testing.T, query string, args ...any) error {
	t.Helper()
	raw, err := sql.Open("sqlite", settings.DBFile())
	if err != nil {
		return err
	}
	defer raw.Close()
	_, err = raw.ExecContext(context.Background(), query, args...)
	return err
}

// TestRecordRetryReusesItsPlan checks that when recording a replacing
// package fails once and the second attempt succeeds, the second attempt
// writes the row that was planned for the first, rather than reading the
// installed packages again: a row that stops decoding between the attempts
// doesn't affect it.
func TestRecordRetryReusesItsPlan(t *testing.T) {
	f := replaceFixture(t, goolib.PkgSpec{})
	before := f.rows(t)
	logs := testutil.CaptureLogs(t)
	attempts := 0
	err := f.runWithHook(t, t.Context(), Options{OrphanedDependents: OrphanRemove}, "R", func(stage applyStage, step string) error {
		if stage != stageAddPkg || step != "R.noarch.1" {
			return nil
		}
		attempts++
		if attempts > 1 {
			return nil
		}
		// Any later read of every installed row fails to decode this one.
		if err := execDB(t, `INSERT INTO InstalledPackages (pkg_name, pkg_arch, pkg_ver, pkg_json) VALUES (?, ?, ?, ?)`, "corrupt", "noarch", "1", []byte("{bad json")); err != nil {
			return err
		}
		return errors.New("busy")
	})
	if dbErr := execDB(t, `DELETE FROM InstalledPackages WHERE pkg_name = 'corrupt'`); dbErr != nil {
		t.Fatal(dbErr)
	}
	if err != nil {
		t.Fatalf("install R = %v, want the second attempt to record it", err)
	}
	if attempts != 2 {
		t.Errorf("recording R was attempted %d times, want 2", attempts)
	}
	if diff := cmp.Diff([]string{"R.noarch.1"}, f.installed(t)); diff != "" {
		t.Errorf("installed packages (-want +got):\n%s", diff)
	}
	if n := strings.Count(logs.String(), "Recording R.noarch.1 failed, trying again: busy"); n != 1 {
		t.Errorf("logged the retry %d times, want once; got:\n%s", n, logs)
	}
	if !f.fileExists("R") {
		t.Errorf("R's file is missing")
	}
	// Once R is recorded, the archives kept for recovery are deleted.
	for _, name := range []string{"P", "D"} {
		if _, err := os.Stat(before[name].LocalPath); !os.IsNotExist(err) {
			t.Errorf("%s's kept archive: Stat error = %v, want it deleted", name, err)
		}
	}
}
