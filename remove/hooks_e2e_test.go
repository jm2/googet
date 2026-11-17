//go:build linux

/*
Copyright 2026 Google LLC

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

package remove

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/googet/v2/client"
	"github.com/google/googet/v2/goolib"
	"github.com/google/googet/v2/testutil"
)

func uninstallEnvStub(logFile string) string {
	return fmt.Sprintf(
		"#!/bin/sh\nset -eu\nprintf '%%s|%%s|%%s|%%s|%%s|%%s\\n' \"${GOOGET_ACTION-<unset>}\" \"${GOOGET_PACKAGE-<unset>}\" \"${GOOGET_PREVIOUS_VERSION-<unset>}\" \"${GOOGET_REPLACED_BY-<unset>}\" \"${googet_custom-<unset>}\" \"${GooGetRoot-<unset>}\" >> %q\n",
		logFile,
	)
}

func readRemoveStubLog(t *testing.T, logFile string) []string {
	t.Helper()
	b, err := os.ReadFile(logFile)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", logFile, err)
	}
	trimmed := strings.TrimSpace(string(b))
	if trimmed == "" {
		return nil
	}
	return strings.Split(trimmed, "\n")
}

// TestEndToEndRemoveAllCascadeAndUninstallRowHooks executes /bin/sh uninstall
// stub scripts through All and UninstallRow, verifying GOOGET_* stripping and
// population on non-zero hooks, unchanged inheritance on zero-Hook external
// calls, and zero executions under DBOnly.
func TestEndToEndRemoveAllCascadeAndUninstallRowHooks(t *testing.T) {
	t.Setenv("GOOGET_ACTION", "stale_action")
	t.Setenv("GOOGET_REPLACED_BY", "stale_replacer")
	t.Setenv("googet_custom", "leaked")
	t.Setenv("GooGetRoot", "/keep/root")

	t.Run("all_with_cascade_and_bare_name_resolution", func(t *testing.T) {
		db, downloader, _ := newRemoveTestDB(t)
		cacheDir := t.TempDir()
		logFile := filepath.Join(t.TempDir(), "remove_hooks.log")
		logs := testutil.CaptureLogs(t)

		rootSpec := goolib.PkgSpec{
			Name:      "root_pkg",
			Arch:      "x86_64",
			Version:   "1.2.0",
			Uninstall: goolib.ExecFile{Path: "uninstall.sh"},
		}
		depSpec := goolib.PkgSpec{
			Name:            "child.pkg",
			Arch:            "noarch",
			Version:         "3.0.0",
			PkgDependencies: map[string]string{"root_pkg": "1.0.0"},
			Uninstall:       goolib.ExecFile{Path: "uninstall.sh"},
		}
		soloSpec := goolib.PkgSpec{
			Name:      "solo.pkg",
			Arch:      "x86_64",
			Version:   "2.0.0",
			Uninstall: goolib.ExecFile{Path: "uninstall.sh"},
		}

		rootGoo := filepath.Join(cacheDir, "root_pkg.x86_64.1.2.0.goo")
		depGoo := filepath.Join(cacheDir, "child.pkg.noarch.3.0.0.goo")
		soloGoo := filepath.Join(cacheDir, "solo.pkg.x86_64.2.0.0.goo")
		entry := archiveEntry{name: "uninstall.sh", body: uninstallEnvStub(logFile), mode: 0755}
		writeOwnershipTestGoo(t, rootGoo, rootSpec, entry)
		writeOwnershipTestGoo(t, depGoo, depSpec, entry)
		writeOwnershipTestGoo(t, soloGoo, soloSpec, entry)

		if err := db.WriteStateToDB(client.GooGetState{
			{PackageSpec: &rootSpec, LocalPath: rootGoo},
			{PackageSpec: &depSpec, LocalPath: depGoo},
			{PackageSpec: &soloSpec, LocalPath: soloGoo},
		}); err != nil {
			t.Fatalf("WriteStateToDB: %v", err)
		}

		deps, _, err := EnumerateDeps(goolib.PackageInfo{Name: "root_pkg", Arch: "x86_64"}, db)
		if err != nil {
			t.Fatalf("EnumerateDeps: %v", err)
		}
		if err := All(t.Context(), goolib.PackageInfo{Name: "root_pkg", Arch: "x86_64"}, deps, false, downloader, db); err != nil {
			t.Fatalf("All(root_pkg): %v", err)
		}
		if err := All(t.Context(), goolib.PackageInfo{Name: "solo.pkg"}, DepMap{"solo.pkg.": nil}, false, downloader, db); err != nil {
			t.Fatalf("All(bare solo.pkg): %v", err)
		}

		want := []string{
			"remove|child.pkg.noarch.3.0.0||<unset>|<unset>|/keep/root",
			"remove|root_pkg.x86_64.1.2.0||<unset>|<unset>|/keep/root",
			"remove|solo.pkg.x86_64.2.0.0||<unset>|<unset>|/keep/root",
		}
		if diff := cmp.Diff(want, readRemoveStubLog(t, logFile)); diff != "" {
			t.Errorf("remove.All stub log mismatch (-want +got):\n%s", diff)
		}
		if wantLog := "installer_context hook=uninstall action=remove package=root_pkg.x86_64.1.2.0 previous= replaced_by="; !strings.Contains(logs.String(), wantLog) {
			t.Errorf("captured logs missing %q:\n%s", wantLog, logs.String())
		}
	})

	t.Run("uninstall_row_replace_cascade_zero_legacy_and_db_only", func(t *testing.T) {
		db, downloader, _ := newRemoveTestDB(t)
		cacheDir := t.TempDir()
		logFile := filepath.Join(t.TempDir(), "uninstall_row.log")
		entry := archiveEntry{name: "uninstall.sh", body: uninstallEnvStub(logFile), mode: 0755}

		mkRow := func(name, ver string) client.PackageState {
			spec := goolib.PkgSpec{
				Name:      name,
				Arch:      "noarch",
				Version:   ver,
				Uninstall: goolib.ExecFile{Path: "uninstall.sh"},
			}
			goo := filepath.Join(cacheDir, spec.String()+".goo")
			writeOwnershipTestGoo(t, goo, spec, entry)
			return client.PackageState{PackageSpec: &spec, LocalPath: goo}
		}

		victimRow := mkRow("victim", "1.0.0")
		cascRow := mkRow("cascaded", "1.0.0")
		legacyRow := mkRow("legacy_ext", "1.0.0")
		dbOnlyRow := mkRow("db_only_pkg", "1.0.0")

		if err := db.WriteStateToDB(client.GooGetState{victimRow, cascRow, legacyRow, dbOnlyRow}); err != nil {
			t.Fatalf("WriteStateToDB: %v", err)
		}

		if err := UninstallRow(t.Context(), victimRow, Options{
			Downloader: downloader,
			Hook: goolib.InstallerContext{
				Action:     goolib.ActionReplace,
				Package:    "victim.noarch.1.0.0",
				ReplacedBy: "successor.noarch.2.0.0",
			},
		}, db); err != nil {
			t.Fatalf("UninstallRow(replace): %v", err)
		}

		if err := UninstallRow(t.Context(), cascRow, Options{
			Downloader: downloader,
			Hook: goolib.InstallerContext{
				Action:  goolib.ActionRemoveDependent,
				Package: "cascaded.noarch.1.0.0",
			},
		}, db); err != nil {
			t.Fatalf("UninstallRow(remove-dependent): %v", err)
		}

		logs := testutil.CaptureLogs(t)
		// External caller passing zero Options{} keeps inherited environment unchanged
		// and logs no installer_context line.
		if err := UninstallRow(t.Context(), legacyRow, Options{Downloader: downloader}, db); err != nil {
			t.Fatalf("UninstallRow(legacy zero Hook): %v", err)
		}
		if strings.Contains(logs.String(), "installer_context") {
			t.Errorf("zero-Hook UninstallRow logged installer_context:\n%s", logs.String())
		}

		// DBOnly runs no script even when Options.Hook is populated.
		if err := UninstallRow(t.Context(), dbOnlyRow, Options{
			DBOnly:     true,
			Downloader: downloader,
			Hook: goolib.InstallerContext{
				Action:  goolib.ActionRemove,
				Package: "db_only_pkg.noarch.1.0.0",
			},
		}, db); err != nil {
			t.Fatalf("UninstallRow(DBOnly): %v", err)
		}

		want := []string{
			"replace|victim.noarch.1.0.0||successor.noarch.2.0.0|<unset>|/keep/root",
			"remove-dependent|cascaded.noarch.1.0.0||<unset>|<unset>|/keep/root",
			"stale_action|<unset>|<unset>|stale_replacer|leaked|/keep/root",
		}
		if diff := cmp.Diff(want, readRemoveStubLog(t, logFile)); diff != "" {
			t.Errorf("UninstallRow stub log mismatch (-want +got):\n%s", diff)
		}
	})
}
