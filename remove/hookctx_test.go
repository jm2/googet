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
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/googet/v2/client"
	"github.com/google/googet/v2/goolib"
)

func recordUninstallHooks(t *testing.T) *[]goolib.InstallerContext {
	t.Helper()
	var got []goolib.InstallerContext
	prev := uninstallHookObserver
	uninstallHookObserver = func(ic goolib.InstallerContext) {
		got = append(got, ic)
	}
	t.Cleanup(func() { uninstallHookObserver = prev })
	return &got
}

// TestRemoveContextPureBuilder verifies removeContext across standard,
// multi-arch, and dotted package names.
func TestRemoveContextPureBuilder(t *testing.T) {
	tests := []struct {
		name string
		ps   client.PackageState
		want goolib.InstallerContext
	}{
		{
			name: "standard_noarch",
			ps: client.PackageState{
				PackageSpec: &goolib.PkgSpec{Name: "alpha", Arch: "noarch", Version: "1.0.0"},
			},
			want: goolib.InstallerContext{
				Action:  goolib.ActionRemove,
				Package: "alpha.noarch.1.0.0",
			},
		},
		{
			name: "dotted_name_and_x86_64",
			ps: client.PackageState{
				PackageSpec: &goolib.PkgSpec{Name: "corp.agent.service", Arch: "x86_64", Version: "2.4.1@3"},
			},
			want: goolib.InstallerContext{
				Action:  goolib.ActionRemove,
				Package: "corp.agent.service.x86_64.2.4.1@3",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := removeContext(tc.ps)
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("removeContext() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestRemoveAllAndUninstallRowHookPlumbing verifies that All resolves the
// installed row's full name.arch.version even when called with a bare name,
// UninstallRow forwards Options.Hook (and preserves zero legacy contexts),
// DBOnly computes no context, and an undecodable unrelated row aborts before
// any hook observer fires.
func TestRemoveAllAndUninstallRowHookPlumbing(t *testing.T) {
	t.Run("all_with_bare_name_and_cascade_uses_installed_rows", func(t *testing.T) {
		db, downloader, _ := newRemoveTestDB(t)
		cacheDir := t.TempDir()

		rootSpec := goolib.PkgSpec{Name: "base_pkg", Arch: "x86_64", Version: "1.5.0"}
		depSpec := goolib.PkgSpec{
			Name:            "leaf.pkg",
			Arch:            "noarch",
			Version:         "2.0.0",
			PkgDependencies: map[string]string{"base_pkg": "1.0.0"},
		}
		rootGoo := filepath.Join(cacheDir, "base_pkg.x86_64.1.5.0.goo")
		depGoo := filepath.Join(cacheDir, "leaf.pkg.noarch.2.0.0.goo")
		writeOwnershipTestGoo(t, rootGoo, rootSpec)
		writeOwnershipTestGoo(t, depGoo, depSpec)

		if err := db.WriteStateToDB(client.GooGetState{
			{PackageSpec: &rootSpec, LocalPath: rootGoo},
			{PackageSpec: &depSpec, LocalPath: depGoo},
		}); err != nil {
			t.Fatalf("WriteStateToDB: %v", err)
		}

		// Build DepMap keyed as EnumerateDeps builds it, then pass a bare name
		// without version (and test uninstallPkg directly with bare Name only).
		deps, _, err := EnumerateDeps(goolib.PackageInfo{Name: "base_pkg", Arch: "x86_64"}, db)
		if err != nil {
			t.Fatalf("EnumerateDeps: %v", err)
		}
		got := recordUninstallHooks(t)
		if err := All(t.Context(), goolib.PackageInfo{Name: "base_pkg", Arch: "x86_64"}, deps, false, downloader, db); err != nil {
			t.Fatalf("All(cascade): %v", err)
		}

		// Also seed one package and call All with a bare Name-only PackageInfo
		// (empty Arch and Ver) to verify FetchPkg resolution populates Package.
		soloSpec := goolib.PkgSpec{Name: "solo.pkg", Arch: "x86_64", Version: "3.2.1"}
		soloGoo := filepath.Join(cacheDir, "solo.pkg.x86_64.3.2.1.goo")
		writeOwnershipTestGoo(t, soloGoo, soloSpec)
		if err := db.WriteStateToDB(client.GooGetState{{PackageSpec: &soloSpec, LocalPath: soloGoo}}); err != nil {
			t.Fatalf("WriteStateToDB(solo): %v", err)
		}
		if err := All(t.Context(), goolib.PackageInfo{Name: "solo.pkg"}, DepMap{"solo.pkg.": nil}, false, downloader, db); err != nil {
			t.Fatalf("All(bare name): %v", err)
		}

		want := []goolib.InstallerContext{
			{Action: goolib.ActionRemove, Package: "leaf.pkg.noarch.2.0.0"},
			{Action: goolib.ActionRemove, Package: "base_pkg.x86_64.1.5.0"},
			{Action: goolib.ActionRemove, Package: "solo.pkg.x86_64.3.2.1"},
		}
		if diff := cmp.Diff(want, *got); diff != "" {
			t.Errorf("observed All() hook contexts mismatch (-want +got):\n%s", diff)
		}
	})

	t.Run("uninstall_row_explicit_hook_and_legacy_zero_hook", func(t *testing.T) {
		db, downloader, _ := newRemoveTestDB(t)
		cacheDir := t.TempDir()

		specA := goolib.PkgSpec{Name: "victim", Arch: "noarch", Version: "1.0.0"}
		specB := goolib.PkgSpec{Name: "external", Arch: "noarch", Version: "1.0.0"}
		gooA := filepath.Join(cacheDir, "victim.noarch.1.0.0.goo")
		gooB := filepath.Join(cacheDir, "external.noarch.1.0.0.goo")
		writeOwnershipTestGoo(t, gooA, specA)
		writeOwnershipTestGoo(t, gooB, specB)

		rowA := client.PackageState{PackageSpec: &specA, LocalPath: gooA}
		rowB := client.PackageState{PackageSpec: &specB, LocalPath: gooB}
		if err := db.WriteStateToDB(client.GooGetState{rowA, rowB}); err != nil {
			t.Fatalf("WriteStateToDB: %v", err)
		}

		got := recordUninstallHooks(t)
		wantHook := goolib.InstallerContext{
			Action:     goolib.ActionReplace,
			Package:    "victim.noarch.1.0.0",
			ReplacedBy: "successor.noarch.2.0.0",
		}
		if err := UninstallRow(t.Context(), rowA, Options{Downloader: downloader, Hook: wantHook}, db); err != nil {
			t.Fatalf("UninstallRow(explicit Hook): %v", err)
		}
		if err := UninstallRow(t.Context(), rowB, Options{Downloader: downloader}, db); err != nil {
			t.Fatalf("UninstallRow(zero Hook): %v", err)
		}

		want := []goolib.InstallerContext{
			wantHook,
			{},
		}
		if diff := cmp.Diff(want, *got); diff != "" {
			t.Errorf("observed UninstallRow hook contexts mismatch (-want +got):\n%s", diff)
		}
	})

	t.Run("db_only_and_corrupt_index_never_invoke_hook_observer", func(t *testing.T) {
		db, downloader, dbFile := newRemoveTestDB(t)
		cacheDir := t.TempDir()

		specA := goolib.PkgSpec{Name: "keepA", Arch: "noarch", Version: "1.0.0"}
		specB := goolib.PkgSpec{Name: "keepB", Arch: "noarch", Version: "1.0.0"}
		gooA := filepath.Join(cacheDir, "keepA.noarch.1.0.0.goo")
		gooB := filepath.Join(cacheDir, "keepB.noarch.1.0.0.goo")
		writeOwnershipTestGoo(t, gooA, specA)
		writeOwnershipTestGoo(t, gooB, specB)

		rowA := client.PackageState{PackageSpec: &specA, LocalPath: gooA}
		rowB := client.PackageState{PackageSpec: &specB, LocalPath: gooB}
		if err := db.WriteStateToDB(client.GooGetState{rowA, rowB}); err != nil {
			t.Fatalf("WriteStateToDB: %v", err)
		}

		rawDB, err := sql.Open("sqlite", dbFile)
		if err != nil {
			t.Fatalf("sql.Open: %v", err)
		}
		t.Cleanup(func() { rawDB.Close() })
		if _, err := rawDB.ExecContext(context.Background(),
			`INSERT INTO InstalledPackages (pkg_name, pkg_arch, pkg_ver, pkg_json) VALUES (?, ?, ?, ?)`,
			"unrelated_corrupt", "noarch", "1.0.0", []byte("{invalid json"),
		); err != nil {
			t.Fatalf("inserting corrupt row: %v", err)
		}

		got := recordUninstallHooks(t)

		// Non-db_only UninstallRow and All both fail during ownership index
		// construction before uninstallHookObserver fires.
		nonZero := goolib.InstallerContext{Action: goolib.ActionReplace, Package: "keepA.noarch.1.0.0", ReplacedBy: "next.noarch.2.0.0"}
		var syntaxErr *json.SyntaxError
		if err := UninstallRow(t.Context(), rowA, Options{Downloader: downloader, Hook: nonZero}, db); !errors.As(err, &syntaxErr) {
			t.Fatalf("UninstallRow(non-db_only with corrupt row) = %v, want *json.SyntaxError", err)
		}
		if err := All(t.Context(), goolib.PackageInfo{Name: "keepB", Arch: "noarch"}, DepMap{"keepB.noarch": nil}, false, downloader, db); !errors.As(err, &syntaxErr) {
			t.Fatalf("All(non-db_only with corrupt row) = %v, want *json.SyntaxError", err)
		}

		// DBOnly UninstallRow and All succeed without computing or observing any hook context.
		if err := UninstallRow(t.Context(), rowA, Options{DBOnly: true, Downloader: downloader, Hook: nonZero}, db); err != nil {
			t.Fatalf("UninstallRow(DBOnly=true): %v", err)
		}
		if err := All(t.Context(), goolib.PackageInfo{Name: "keepB", Arch: "noarch"}, DepMap{"keepB.noarch": nil}, true, downloader, db); err != nil {
			t.Fatalf("All(DBOnly=true): %v", err)
		}

		if len(*got) != 0 {
			t.Errorf("observed %d hook calls (%+v), want 0 across corrupt-index failure and DBOnly", len(*got), *got)
		}
	})
}
