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
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/google/googet/v2/goolib"
	"github.com/google/googet/v2/settings"
	"github.com/google/googet/v2/testutil"
)

// failDatabaseInserts makes every later attempt to write a row to the
// database fail, as a database that has stopped accepting writes does.
func failDatabaseInserts(t *testing.T) error {
	t.Helper()
	raw, err := sql.Open("sqlite", settings.DBFile())
	if err != nil {
		return err
	}
	defer raw.Close()
	_, err = raw.ExecContext(context.Background(), `CREATE TRIGGER fail_inserts BEFORE INSERT ON InstalledPackages BEGIN SELECT RAISE(ABORT, 'simulated database failure'); END;`)
	return err
}

// TestPersistentRecordFailureNamesKeptArchives checks what is left when the
// database stops accepting writes after a step removed the packages its
// package replaces: the replacing package is rolled back, each removed
// package is installed again and then rolled back because it can't be
// recorded either, and the error and the not_restored replace_record line
// name every package that wasn't restored with the archive kept for it.
func TestPersistentRecordFailureNamesKeptArchives(t *testing.T) {
	for _, dbOnly := range []bool{false, true} {
		t.Run(map[bool]string{false: "full", true: "database only"}[dbOnly], func(t *testing.T) {
			f := newRelFixture(t,
				[]goolib.PkgSpec{{Name: "P", Arch: "noarch", Version: "1"}, {Name: "Q", Arch: "noarch", Version: "1"}},
				[]goolib.PkgSpec{{Name: "R", Arch: "noarch", Version: "1", Replaces: []string{"P", "Q"}}})
			before := f.rows(t)
			logs := testutil.CaptureLogs(t)
			attempts := 0
			var err error
			out := captureStdout(t, func() {
				err = f.runWithHook(t, t.Context(), Options{DBOnly: dbOnly}, "R", func(stage applyStage, step string) error {
					switch {
					case stage == stageAfterReplace:
						return failDatabaseInserts(t)
					case stage == stageAddPkg && step == "R.noarch.1":
						attempts++
					}
					return nil
				})
			})
			if err == nil || !strings.Contains(err.Error(), "simulated database failure") {
				t.Fatalf("install R = %v, want the database error", err)
			}
			if attempts != 2 {
				t.Errorf("recording R was attempted %d times, want 2", attempts)
			}
			// The not_restored line lists the packages newest removal first;
			// which of P and Q is removed first isn't what this test checks.
			const linePrefix = "replace_record replacer=R.noarch.1 result=not_restored removed="
			var line string
			for _, l := range strings.Split(out, "\n") {
				if strings.HasPrefix(l, linePrefix) {
					line = l
				}
			}
			if line == "" || !strings.Contains(logs.String(), line) {
				t.Errorf("console output %q and logs %q, want both to contain a line starting %q", out, logs, linePrefix)
			}
			for _, name := range []string{"P", "Q"} {
				wantErr := fmt.Sprintf("couldn't reinstall %s.noarch.1, which installing R.noarch.1 removed (archive %q)", name, before[name].LocalPath)
				wantEntry := name + ".noarch.1 (archive " + before[name].LocalPath + ")"
				if dbOnly {
					wantErr = "couldn't reinstall " + name + ".noarch.1, which installing R.noarch.1 removed: "
					wantEntry = name + ".noarch.1"
				}
				if !strings.Contains(err.Error(), wantErr) {
					t.Errorf("install R = %v, want an error containing %q", err, wantErr)
				}
				if entries := strings.Split(strings.TrimPrefix(line, linePrefix), ","); !slices.Contains(entries, wantEntry) {
					t.Errorf("not_restored line %q, want it to name %q", line, wantEntry)
				}
			}
			if strings.Contains(err.Error(), "reinstalled the packages") {
				t.Errorf("install R = %v, want no claim that a package was reinstalled", err)
			}
			if !strings.Contains(logs.String(), "replace_recovery replacer=R.noarch.1 reinstalled= failed=2") {
				t.Errorf("logs missing the recovery line; got:\n%s", logs)
			}
			if strings.Contains(out+logs.String(), "result=unrecorded") {
				t.Errorf("printed or logged an unrecorded replace_record line; output:\n%s\nlogs:\n%s", out, logs)
			}
			if got := f.installed(t); len(got) != 0 {
				t.Errorf("installed packages = %v, want none: no row could be written", got)
			}
			for _, name := range []string{"P", "Q"} {
				if _, statErr := os.Stat(before[name].LocalPath); statErr != nil {
					t.Errorf("%s's archive: %v, want it kept", name, statErr)
				}
			}
			if dbOnly {
				return
			}
			for _, name := range []string{"R", "P", "Q"} {
				if f.fileExists(name) {
					t.Errorf("%s's file is on disk, want it rolled back because its row couldn't be written", name)
				}
			}
		})
	}
}
