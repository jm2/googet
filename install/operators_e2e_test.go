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
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/googet/v2/client"
	"github.com/google/googet/v2/googetdb"
	"github.com/google/googet/v2/goolib"
	"github.com/google/googet/v2/priority"
	"github.com/google/googet/v2/remove"
	"github.com/google/googet/v2/settings"
	"github.com/google/googet/v2/testutil"
	"github.com/google/googet/v2/verify"
)

// e2eFixture serves goopack-built .goo archives over httptest against a real
// SQLite state database and on-disk destination directory.
type e2eFixture struct {
	goopackBin string
	db         *googetdb.GooDB
	rm         client.RepoMap
	repoURL    string
	gooDir     string
	cache      string
	dst        string
	downloader *client.Downloader
}

// newE2EFixture initializes an httptest repository backed by goopack-built
// archives and seeds installed packages onto disk and into SQLite.
func newE2EFixture(t *testing.T, goopackBin string, installed, available []goolib.PkgSpec) *e2eFixture {
	t.Helper()
	t.Cleanup(settings.Snapshot())
	rootDir := t.TempDir()
	settings.Initialize(rootDir, false)

	db, err := googetdb.NewDB(settings.DBFile())
	if err != nil {
		t.Fatalf("googetdb.NewDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	downloader, err := client.NewDownloader("")
	if err != nil {
		t.Fatalf("client.NewDownloader: %v", err)
	}

	gooDir := t.TempDir()
	cacheDir := t.TempDir()
	dstDir := t.TempDir()
	srv := testutil.ServeGoo(t, gooDir)
	t.Cleanup(srv.Close)

	var state client.GooGetState
	for _, ps := range installed {
		rs := testutil.GenGooWithGoopack(t, goopackBin, gooDir, dstDir, ps)
		local := filepath.Join(cacheDir, rs.Source)
		copyFile(t, filepath.Join(gooDir, rs.Source), local)
		dstFile := filepath.Join(dstDir, ps.Name)
		if err := os.WriteFile(dstFile, []byte(rs.PackageSpec.String()), 0644); err != nil {
			t.Fatal(err)
		}
		state = append(state, client.PackageState{
			SourceRepo:     srv.URL,
			DownloadURL:    srv.URL + "/" + rs.Source,
			Checksum:       rs.Checksum,
			LocalPath:      local,
			PackageSpec:    rs.PackageSpec,
			InstalledFiles: map[string]string{dstFile: checksumOf(t, dstFile)},
		})
	}
	if err := db.WriteStateToDB(state); err != nil {
		t.Fatalf("WriteStateToDB: %v", err)
	}

	var repoSpecs []goolib.RepoSpec
	for _, ps := range available {
		repoSpecs = append(repoSpecs, testutil.GenGooWithGoopack(t, goopackBin, gooDir, dstDir, ps))
	}
	return &e2eFixture{
		goopackBin: goopackBin,
		db:         db,
		rm:         client.RepoMap{srv.URL: client.Repo{Priority: priority.Default, Packages: repoSpecs}},
		repoURL:    srv.URL,
		gooDir:     gooDir,
		cache:      cacheDir,
		dst:        dstDir,
		downloader: downloader,
	}
}

func (f *e2eFixture) installed(t *testing.T) []string {
	t.Helper()
	state, err := f.db.FetchPkgs("")
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, ps := range state {
		out = append(out, ps.PackageSpec.String())
	}
	slices.Sort(out)
	return out
}

func (f *e2eFixture) fileExists(name string) bool {
	_, err := os.Stat(filepath.Join(f.dst, name))
	return err == nil
}

func (f *e2eFixture) installWith(t *testing.T, opts Options, name string) error {
	t.Helper()
	return New(f.cache, f.db, opts).FromRepo(t.Context(), goolib.PackageInfo{Name: name}, "", f.rm, []string{"noarch", "x86_64"}, f.downloader)
}

// TestEndToEndObsoletesIdiomWithGoopack verifies installing a goopack-built
// obsoleting package (Conflicts/Replaces P<2.0.0, Provides P=2.0.0) against
// in-range and out-of-range installed versions, including victim exemption,
// replacement recording, and orphan protection.
func TestEndToEndObsoletesIdiomWithGoopack(t *testing.T) {
	goopackBin := testutil.BuildGoopack(t)
	replacer := goolib.PkgSpec{
		Name:      "R",
		Arch:      "noarch",
		Version:   "2.0.0",
		Conflicts: []string{"P<2.0.0"},
		Replaces:  []string{"P<2.0.0"},
		Provides:  []string{"P=2.0.0"},
	}

	t.Run("in_range_replaced_with_exemption_record_and_satisfied_dependent", func(t *testing.T) {
		logs := testutil.CaptureLogs(t)
		installed := []goolib.PkgSpec{
			{Name: "P", Arch: "noarch", Version: "1.0.0"},
			{Name: "D", Arch: "noarch", Version: "1.0.0", PkgDependencies: map[string]string{"P": "1.0.0"}},
		}
		f := newE2EFixture(t, goopackBin, installed, []goolib.PkgSpec{replacer})

		stBefore, err := f.db.FetchPkgs("")
		if err != nil {
			t.Fatal(err)
		}
		predictedVictims, err := victimsIn(&replacer, stBefore)
		if err != nil {
			t.Fatal(err)
		}
		if len(predictedVictims) != 1 || predictedVictims[0].PackageSpec.String() != "P.noarch.1.0.0" {
			t.Fatalf("victimsIn predicted %v, want [P.noarch.1.0.0]", predictedVictims)
		}

		if err := f.installWith(t, Options{}, "R"); err != nil {
			t.Fatalf("install R failed: %v", err)
		}
		if diff := cmp.Diff([]string{"D.noarch.1.0.0", "R.noarch.2.0.0"}, f.installed(t)); diff != "" {
			t.Errorf("installed (-want +got):\n%s", diff)
		}
		if f.fileExists("P") || !f.fileExists("D") || !f.fileExists("R") {
			t.Errorf("files on disk: P=%t D=%t R=%t, want P=false D=true R=true", f.fileExists("P"), f.fileExists("D"), f.fileExists("R"))
		}
		if !strings.Contains(logs.String(), `conflict "P<2.0.0" with P.noarch.1.0.0 skipped: installing R.noarch.2.0.0 removes it`) {
			t.Errorf("logs missing victim exemption line:\n%s", logs.String())
		}
		if !strings.Contains(logs.String(), "replace_record replacer=R.noarch.2.0.0 rows=P.noarch.1.0.0") {
			t.Errorf("logs missing replace_record line:\n%s", logs.String())
		}
	})

	t.Run("orphan_dependent_blocks_before_removing_victim", func(t *testing.T) {
		lowProviderReplacer := goolib.PkgSpec{
			Name:      "R",
			Arch:      "noarch",
			Version:   "2.0.0",
			Conflicts: []string{"P<2.0.0"},
			Replaces:  []string{"P<2.0.0"},
			Provides:  []string{"P=1.0.0"},
		}
		installed := []goolib.PkgSpec{
			{Name: "P", Arch: "noarch", Version: "1.5.0"},
			{Name: "D", Arch: "noarch", Version: "1.0.0", PkgDependencies: map[string]string{"P": "1.5.0"}},
		}
		f := newE2EFixture(t, goopackBin, installed, []goolib.PkgSpec{lowProviderReplacer})

		err := f.installWith(t, Options{OrphanedDependents: OrphanFail}, "R")
		if !errors.Is(err, ErrOrphanedDependents) {
			t.Fatalf("install R error = %v, want errors.Is(ErrOrphanedDependents)", err)
		}
		if diff := cmp.Diff([]string{"D.noarch.1.0.0", "P.noarch.1.5.0"}, f.installed(t)); diff != "" {
			t.Errorf("installed (-want +got):\n%s", diff)
		}
		if !f.fileExists("P") || !f.fileExists("D") || f.fileExists("R") {
			t.Errorf("files on disk changed after orphan block: P=%t D=%t R=%t", f.fileExists("P"), f.fileExists("D"), f.fileExists("R"))
		}
	})

	t.Run("out_of_range_coexists_with_nothing_removed", func(t *testing.T) {
		installed := []goolib.PkgSpec{{Name: "P", Arch: "noarch", Version: "2.0.0"}}
		f := newE2EFixture(t, goopackBin, installed, []goolib.PkgSpec{replacer})

		stBefore, err := f.db.FetchPkgs("")
		if err != nil {
			t.Fatal(err)
		}
		predictedVictims, err := victimsIn(&replacer, stBefore)
		if err != nil {
			t.Fatal(err)
		}
		if len(predictedVictims) != 0 {
			t.Fatalf("victimsIn predicted %v, want empty", predictedVictims)
		}

		if err := f.installWith(t, Options{}, "R"); err != nil {
			t.Fatalf("install R failed: %v", err)
		}
		if diff := cmp.Diff([]string{"P.noarch.2.0.0", "R.noarch.2.0.0"}, f.installed(t)); diff != "" {
			t.Errorf("installed (-want +got):\n%s", diff)
		}
		if !f.fileExists("P") || !f.fileExists("R") {
			t.Errorf("files on disk: P=%t R=%t, want both true", f.fileExists("P"), f.fileExists("R"))
		}
	})
}

// TestEndToEndExactOperatorVersionMatching checks exact operator Replaces
// ("P=1.0.0") against 1.0.0, 1.0.0@1, 1.0.1, and 1.0.0+build5 using
// goopack-built archives.
func TestEndToEndExactOperatorVersionMatching(t *testing.T) {
	goopackBin := testutil.BuildGoopack(t)
	replacer := goolib.PkgSpec{
		Name:     "R",
		Arch:     "noarch",
		Version:  "2.0.0",
		Replaces: []string{"P=1.0.0"},
		Provides: []string{"P=2.0.0"},
	}

	for _, tc := range []struct {
		name         string
		installedVer string
		wantReplaced bool
	}{
		{name: "exact_1.0.0_replaced", installedVer: "1.0.0", wantReplaced: true},
		{name: "release_suffix_1.0.0@1_not_replaced", installedVer: "1.0.0@1", wantReplaced: false},
		{name: "patch_1.0.1_not_replaced", installedVer: "1.0.1", wantReplaced: false},
		{name: "build_metadata_1.0.0+build5_replaced", installedVer: "1.0.0+build5", wantReplaced: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			installed := []goolib.PkgSpec{{Name: "P", Arch: "noarch", Version: tc.installedVer}}
			f := newE2EFixture(t, goopackBin, installed, []goolib.PkgSpec{replacer})

			if err := f.installWith(t, Options{}, "R"); err != nil {
				t.Fatalf("install R failed: %v", err)
			}
			var want []string
			if tc.wantReplaced {
				want = []string{"R.noarch.2.0.0"}
			} else {
				want = []string{"P.noarch." + tc.installedVer, "R.noarch.2.0.0"}
			}
			if diff := cmp.Diff(want, f.installed(t)); diff != "" {
				t.Errorf("installed (-want +got):\n%s", diff)
			}
			if f.fileExists("P") == tc.wantReplaced {
				t.Errorf("fileExists(P) = %t, want %t", f.fileExists("P"), !tc.wantReplaced)
			}
			if !f.fileExists("R") {
				t.Error("fileExists(R) = false, want true")
			}
		})
	}
}

// TestEndToEndFromDiskDryRunAndPromptWithOperators checks FromDisk remove-first
// refusal only when an installed version is in range and verifies that Plan
// rendering (used by dry-run and interactive prompts) shows "replaced by"
// exclusively for operator victims.
func TestEndToEndFromDiskDryRunAndPromptWithOperators(t *testing.T) {
	goopackBin := testutil.BuildGoopack(t)
	replacer := goolib.PkgSpec{
		Name:     "R",
		Arch:     "noarch",
		Version:  "2.0.0",
		Replaces: []string{"P<2.0.0"},
		Provides: []string{"P=2.0.0"},
	}

	t.Run("fromdisk_refuses_in_range_and_allows_out_of_range", func(t *testing.T) {
		fIn := newE2EFixture(t, goopackBin, []goolib.PkgSpec{{Name: "P", Arch: "noarch", Version: "1.0.0"}}, nil)
		archiveDir := t.TempDir()
		rsIn := testutil.GenGooWithGoopack(t, goopackBin, archiveDir, fIn.dst, replacer)
		err := New(fIn.cache, fIn.db, Options{}).FromDisk(filepath.Join(archiveDir, rsIn.Source), false)
		wantErr := "cannot install, replaces installed package, remove first then try installation again: P<2.0.0 (installed: P.noarch.1.0.0)"
		if err == nil || err.Error() != wantErr {
			t.Fatalf("FromDisk(in-range) = %v, want %q", err, wantErr)
		}
		if diff := cmp.Diff([]string{"P.noarch.1.0.0"}, fIn.installed(t)); diff != "" {
			t.Errorf("installed after refused FromDisk (-want +got):\n%s", diff)
		}

		fOut := newE2EFixture(t, goopackBin, []goolib.PkgSpec{{Name: "P", Arch: "noarch", Version: "2.0.0"}}, nil)
		archiveDirOut := t.TempDir()
		rsOut := testutil.GenGooWithGoopack(t, goopackBin, archiveDirOut, fOut.dst, replacer)
		if err := New(fOut.cache, fOut.db, Options{}).FromDisk(filepath.Join(archiveDirOut, rsOut.Source), false); err != nil {
			t.Fatalf("FromDisk(out-of-range) unexpected error: %v", err)
		}
		if diff := cmp.Diff([]string{"P.noarch.2.0.0", "R.noarch.2.0.0"}, fOut.installed(t)); diff != "" {
			t.Errorf("installed after allowed FromDisk (-want +got):\n%s", diff)
		}
	})

	t.Run("plan_string_for_dry_run_and_prompt_shows_replaced_by_for_operator_victims", func(t *testing.T) {
		installed := []goolib.PkgSpec{
			{Name: "P", Arch: "noarch", Version: "1.0.0"},
			{Name: "P", Arch: "x86_64", Version: "2.5.0"},
		}
		f := newE2EFixture(t, goopackBin, installed, []goolib.PkgSpec{replacer})
		plan, err := New(f.cache, f.db, Options{}).Plan(goolib.PackageInfo{Name: "R"}, "", f.rm, []string{"noarch", "x86_64"})
		if err != nil {
			t.Fatalf("Plan(R) failed: %v", err)
		}
		want := strings.Join([]string{
			"The following packages will be installed:",
			"  R.noarch.2.0.0",
			"The following packages will be removed:",
			"  P.noarch.1.0.0 (replaced by R.noarch.2.0.0)",
			"",
		}, "\n")
		if diff := cmp.Diff(want, plan.String()); diff != "" {
			t.Errorf("Plan.String() mismatch (-want +got):\n%s", diff)
		}
		// Planning alone leaves the database and files untouched, matching dry-run
		// and declined confirmation prompts.
		if diff := cmp.Diff([]string{"P.noarch.1.0.0", "P.x86_64.2.5.0"}, f.installed(t)); diff != "" {
			t.Errorf("installed after Plan (-want +got):\n%s", diff)
		}
	})
}

// TestEndToEndOldClientCoexistenceState simulates a database where a 3.4.x
// client installed R (Conflicts/Replaces P<2.0.0) alongside P 1.0.0 and checks
// that unrelated installs, unrelated removals, and verify keep P intact,
// installing P 1.5.0 warns or errors per mode, and an explicit install of R
// removes P.
func TestEndToEndOldClientCoexistenceState(t *testing.T) {
	goopackBin := testutil.BuildGoopack(t)
	coexistingR := goolib.PkgSpec{
		Name:      "R",
		Arch:      "noarch",
		Version:   "2.0.0",
		Conflicts: []string{"P<2.0.0"},
		Replaces:  []string{"P<2.0.0"},
		Provides:  []string{"P=2.0.0"},
	}
	coexistingP := goolib.PkgSpec{Name: "P", Arch: "noarch", Version: "1.0.0"}
	unrelatedU := goolib.PkgSpec{Name: "U", Arch: "noarch", Version: "1.0.0"}
	otherPkg := goolib.PkgSpec{Name: "Other", Arch: "noarch", Version: "1.0.0"}
	upgradeP := goolib.PkgSpec{Name: "P", Arch: "noarch", Version: "1.5.0"}
	upgradeR := goolib.PkgSpec{
		Name:      "R",
		Arch:      "noarch",
		Version:   "2.1.0",
		Conflicts: []string{"P<2.0.0"},
		Replaces:  []string{"P<2.0.0"},
		Provides:  []string{"P=2.1.0"},
	}

	f := newE2EFixture(t, goopackBin,
		[]goolib.PkgSpec{coexistingP, coexistingR, unrelatedU},
		[]goolib.PkgSpec{otherPkg, upgradeP, upgradeR},
	)

	// 1. Installing an unrelated package keeps P and R intact.
	if err := f.installWith(t, Options{}, "Other"); err != nil {
		t.Fatalf("install Other failed: %v", err)
	}
	if diff := cmp.Diff([]string{"Other.noarch.1.0.0", "P.noarch.1.0.0", "R.noarch.2.0.0", "U.noarch.1.0.0"}, f.installed(t)); diff != "" {
		t.Fatalf("after unrelated install (-want +got):\n%s", diff)
	}

	// 2. Removing an unrelated package keeps P and R intact.
	if err := remove.All(t.Context(), goolib.PackageInfo{Name: "U", Arch: "noarch", Ver: "1.0.0"}, remove.DepMap{}, false, f.downloader, f.db); err != nil {
		t.Fatalf("remove U failed: %v", err)
	}
	if diff := cmp.Diff([]string{"Other.noarch.1.0.0", "P.noarch.1.0.0", "R.noarch.2.0.0"}, f.installed(t)); diff != "" {
		t.Fatalf("after unrelated remove (-want +got):\n%s", diff)
	}

	// 3. Verifying all installed packages succeeds and removes nothing.
	rows, err := f.db.FetchPkgs("")
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		okFiles, err := verify.Files(row)
		if err != nil || !okFiles {
			t.Fatalf("verify.Files(%s) = (%t, %v), want (true, nil)", row.PackageSpec, okFiles, err)
		}
		okCmd, err := verify.Command(t.Context(), row, f.downloader)
		if err != nil || !okCmd {
			t.Fatalf("verify.Command(%s) = (%t, %v), want (true, nil)", row.PackageSpec, okCmd, err)
		}
	}
	if diff := cmp.Diff([]string{"Other.noarch.1.0.0", "P.noarch.1.0.0", "R.noarch.2.0.0"}, f.installed(t)); diff != "" {
		t.Fatalf("after verify (-want +got):\n%s", diff)
	}

	// 4. Installing P 1.5.0 fails in ConflictError mode and warns in ConflictWarn mode.
	err = f.installWith(t, Options{PackageConflicts: ConflictError}, "P")
	if !errors.Is(err, ErrPackageConflict) {
		t.Fatalf("install P 1.5.0 (ConflictError) = %v, want errors.Is(ErrPackageConflict)", err)
	}
	if diff := cmp.Diff([]string{"Other.noarch.1.0.0", "P.noarch.1.0.0", "R.noarch.2.0.0"}, f.installed(t)); diff != "" {
		t.Fatalf("after blocked install P (-want +got):\n%s", diff)
	}

	logs := testutil.CaptureLogs(t)
	if err := f.installWith(t, Options{PackageConflicts: ConflictWarn}, "P"); err != nil {
		t.Fatalf("install P 1.5.0 (ConflictWarn) failed: %v", err)
	}
	if !strings.Contains(logs.String(), `direction=reverse_replaces mode=warn result=warned installed=R.noarch.2.0.0 entry="P<2.0.0" incoming=P.noarch.1.5.0`) {
		t.Errorf("logs missing reverse_replaces warning:\n%s", logs.String())
	}
	if diff := cmp.Diff([]string{"Other.noarch.1.0.0", "P.noarch.1.5.0", "R.noarch.2.0.0"}, f.installed(t)); diff != "" {
		t.Fatalf("after warned install P (-want +got):\n%s", diff)
	}

	// 5. Explicit install of R removes P.
	if err := f.installWith(t, Options{}, "R"); err != nil {
		t.Fatalf("explicit install R failed: %v", err)
	}
	if diff := cmp.Diff([]string{"Other.noarch.1.0.0", "R.noarch.2.1.0"}, f.installed(t)); diff != "" {
		t.Errorf("after explicit install R (-want +got):\n%s", diff)
	}
	if f.fileExists("P") || !f.fileExists("R") {
		t.Errorf("files on disk: P=%t R=%t, want P=false R=true", f.fileExists("P"), f.fileExists("R"))
	}
}

// TestEndToEndReverseReplacesModesWithOperators verifies reverse Replaces with
// an operator ("P<2.0.0") when installing an in-range (1.5.0) versus
// out-of-range (2.0.0) package across warn, error, and ignore modes.
func TestEndToEndReverseReplacesModesWithOperators(t *testing.T) {
	goopackBin := testutil.BuildGoopack(t)
	installedR := goolib.PkgSpec{
		Name:     "R",
		Arch:     "noarch",
		Version:  "2.0.0",
		Replaces: []string{"P<2.0.0"},
		Provides: []string{"P=2.0.0"},
	}

	t.Run("in_range_triggers_configured_mode", func(t *testing.T) {
		incoming := goolib.PkgSpec{Name: "P", Arch: "noarch", Version: "1.5.0"}
		for _, tc := range []struct {
			name       string
			mode       ConflictMode
			wantErr    bool
			wantTag    string
			wantResult string
			wantState  []string
		}{
			{
				name:       "warn",
				mode:       ConflictWarn,
				wantTag:    "WARN : ",
				wantResult: "warned",
				wantState:  []string{"P.noarch.1.5.0", "R.noarch.2.0.0"},
			},
			{
				name:       "error",
				mode:       ConflictError,
				wantErr:    true,
				wantTag:    "ERROR: ",
				wantResult: "failed",
				wantState:  []string{"R.noarch.2.0.0"},
			},
			{
				name:       "ignore",
				mode:       ConflictIgnore,
				wantTag:    "INFO : ",
				wantResult: "ignored",
				wantState:  []string{"P.noarch.1.5.0", "R.noarch.2.0.0"},
			},
		} {
			t.Run(tc.name, func(t *testing.T) {
				logs := testutil.CaptureLogs(t)
				f := newE2EFixture(t, goopackBin, []goolib.PkgSpec{installedR}, []goolib.PkgSpec{incoming})
				err := f.installWith(t, Options{PackageConflicts: tc.mode}, "P")
				if tc.wantErr {
					if !errors.Is(err, ErrPackageConflict) {
						t.Fatalf("install P (mode=%s) = %v, want errors.Is(ErrPackageConflict)", tc.name, err)
					}
				} else if err != nil {
					t.Fatalf("install P (mode=%s) unexpected error: %v", tc.name, err)
				}
				if diff := cmp.Diff(tc.wantState, f.installed(t)); diff != "" {
					t.Errorf("installed (-want +got):\n%s", diff)
				}
				wantLog := `package_conflict direction=reverse_replaces mode=` + tc.name + ` result=` + tc.wantResult + ` installed=R.noarch.2.0.0 entry="P<2.0.0" incoming=P.noarch.1.5.0`
				var found bool
				for _, line := range strings.Split(logs.String(), "\n") {
					if strings.HasPrefix(line, tc.wantTag) && strings.Contains(line, wantLog) {
						found = true
					}
				}
				if !found {
					t.Errorf("logs missing %q with prefix %q:\n%s", wantLog, tc.wantTag, logs.String())
				}
			})
		}
	})

	t.Run("out_of_range_triggers_nothing_in_any_mode", func(t *testing.T) {
		incoming := goolib.PkgSpec{Name: "P", Arch: "noarch", Version: "2.0.0"}
		for _, mode := range []ConflictMode{ConflictWarn, ConflictError, ConflictIgnore} {
			logs := testutil.CaptureLogs(t)
			f := newE2EFixture(t, goopackBin, []goolib.PkgSpec{installedR}, []goolib.PkgSpec{incoming})
			if err := f.installWith(t, Options{PackageConflicts: mode}, "P"); err != nil {
				t.Fatalf("install P 2.0.0 (mode=%v) unexpected error: %v", mode, err)
			}
			if diff := cmp.Diff([]string{"P.noarch.2.0.0", "R.noarch.2.0.0"}, f.installed(t)); diff != "" {
				t.Errorf("installed (-want +got):\n%s", diff)
			}
			if strings.Contains(logs.String(), "package_conflict") {
				t.Errorf("unexpected package_conflict log line for out-of-range P 2.0.0:\n%s", logs.String())
			}
		}
	})
}
