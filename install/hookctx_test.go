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

package install

import (
	"context"
	"database/sql"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/googet/v2/client"
	"github.com/google/googet/v2/googetdb"
	"github.com/google/googet/v2/goolib"
	"github.com/google/googet/v2/settings"
	"github.com/google/googet/v2/testutil"
)

func recordInstallHooks(t *testing.T) *[]goolib.InstallerContext {
	t.Helper()
	var got []goolib.InstallerContext
	prev := installHookObserver
	installHookObserver = func(ic goolib.InstallerContext) {
		got = append(got, ic)
	}
	t.Cleanup(func() { installHookObserver = prev })
	return &got
}

// TestPureContextBuilders verifies installContext, removalContext,
// recoverContext, and reinstallContext across every action and version-comparison
// branch without invoking external commands.
func TestPureContextBuilders(t *testing.T) {
	stateRow := func(name, arch, ver string) client.PackageState {
		return client.PackageState{
			PackageSpec: &goolib.PkgSpec{Name: name, Arch: arch, Version: ver},
		}
	}
	statePtr := func(name, arch, ver string) *client.PackageState {
		r := stateRow(name, arch, ver)
		return &r
	}
	repoSpec := func(name, arch, ver string) goolib.RepoSpec {
		return goolib.RepoSpec{
			PackageSpec: &goolib.PkgSpec{Name: name, Arch: arch, Version: ver},
		}
	}

	t.Run("install_context", func(t *testing.T) {
		tests := []struct {
			name string
			step planStep
			want goolib.InstallerContext
		}{
			{
				name: "fresh_install",
				step: planStep{spec: repoSpec("alpha", "noarch", "1.0.0")},
				want: goolib.InstallerContext{
					Action:  goolib.ActionInstall,
					Package: "alpha.noarch.1.0.0",
				},
			},
			{
				name: "upgrade",
				step: planStep{
					old:  statePtr("alpha", "noarch", "1.0.0"),
					spec: repoSpec("alpha", "noarch", "2.0.0"),
				},
				want: goolib.InstallerContext{
					Action:          goolib.ActionUpgrade,
					Package:         "alpha.noarch.2.0.0",
					PreviousVersion: "1.0.0",
				},
			},
			{
				name: "downgrade",
				step: planStep{
					old:  statePtr("alpha", "noarch", "2.0.0"),
					spec: repoSpec("alpha", "noarch", "1.0.0"),
				},
				want: goolib.InstallerContext{
					Action:          goolib.ActionDowngrade,
					Package:         "alpha.noarch.1.0.0",
					PreviousVersion: "2.0.0",
				},
			},
			{
				name: "reinstall_same_semver",
				step: planStep{
					old:  statePtr("alpha", "x86_64", "1.2.0"),
					spec: repoSpec("alpha", "x86_64", "1.2.0"),
				},
				want: goolib.InstallerContext{
					Action:          goolib.ActionReinstall,
					Package:         "alpha.x86_64.1.2.0",
					PreviousVersion: "1.2.0",
				},
			},
			{
				name: "uncomparable_equal_strings_reinstall",
				step: planStep{
					old:  statePtr("alpha", "noarch", "custom-ver"),
					spec: repoSpec("alpha", "noarch", "custom-ver"),
				},
				want: goolib.InstallerContext{
					Action:          goolib.ActionReinstall,
					Package:         "alpha.noarch.custom-ver",
					PreviousVersion: "custom-ver",
				},
			},
			{
				name: "uncomparable_unequal_strings_upgrade",
				step: planStep{
					old:  statePtr("alpha", "noarch", "custom-ver"),
					spec: repoSpec("alpha", "noarch", "2.0.0"),
				},
				want: goolib.InstallerContext{
					Action:          goolib.ActionUpgrade,
					Package:         "alpha.noarch.2.0.0",
					PreviousVersion: "custom-ver",
				},
			},
			{
				name: "dotted_package_name",
				step: planStep{
					old:  statePtr("google.agent.core", "x86_64", "1.0.0"),
					spec: repoSpec("google.agent.core", "x86_64", "1.1.0"),
				},
				want: goolib.InstallerContext{
					Action:          goolib.ActionUpgrade,
					Package:         "google.agent.core.x86_64.1.1.0",
					PreviousVersion: "1.0.0",
				},
			},
		}

		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				got := installContext(tc.step)
				if diff := cmp.Diff(tc.want, got); diff != "" {
					t.Errorf("installContext() mismatch (-want +got):\n%s", diff)
				}
			})
		}
	})

	t.Run("removal_context", func(t *testing.T) {
		step := planStep{spec: repoSpec("successor", "x86_64", "3.0.0")}
		victim := removalContext(step, removal{row: stateRow("legacy", "x86_64", "1.4.0"), victim: true})
		wantVictim := goolib.InstallerContext{
			Action:     goolib.ActionReplace,
			Package:    "legacy.x86_64.1.4.0",
			ReplacedBy: "successor.x86_64.3.0.0",
		}
		if diff := cmp.Diff(wantVictim, victim); diff != "" {
			t.Errorf("removalContext(victim=true) mismatch (-want +got):\n%s", diff)
		}

		cascaded := removalContext(step, removal{row: stateRow("helper", "noarch", "1.0.0"), victim: false})
		wantCascaded := goolib.InstallerContext{
			Action:  goolib.ActionRemoveDependent,
			Package: "helper.noarch.1.0.0",
		}
		if diff := cmp.Diff(wantCascaded, cascaded); diff != "" {
			t.Errorf("removalContext(victim=false) mismatch (-want +got):\n%s", diff)
		}
	})

	t.Run("recover_and_reinstall_contexts", func(t *testing.T) {
		for _, victim := range []bool{true, false} {
			gotRecover := recoverContext(recordEntry{
				row:    stateRow("restored.pkg", "noarch", "2.5.0"),
				victim: victim,
			})
			wantRecover := goolib.InstallerContext{
				Action:  goolib.ActionRecover,
				Package: "restored.pkg.noarch.2.5.0",
			}
			if diff := cmp.Diff(wantRecover, gotRecover); diff != "" {
				t.Errorf("recoverContext(victim=%t) mismatch (-want +got):\n%s", victim, diff)
			}
		}

		gotReinstall := reinstallContext(stateRow("pinned.pkg", "x86_64", "4.2.1"))
		wantReinstall := goolib.InstallerContext{
			Action:          goolib.ActionReinstall,
			Package:         "pinned.pkg.x86_64.4.2.1",
			PreviousVersion: "4.2.1",
		}
		if diff := cmp.Diff(wantReinstall, gotReinstall); diff != "" {
			t.Errorf("reinstallContext() mismatch (-want +got):\n%s", diff)
		}
	})
}

// TestPlannerStepsToHookContexts checks that planner-generated steps for
// self-replace, multi-architecture coexistence, and dependency-step replacers
// map to the exact expected removalContext and installContext values.
func TestPlannerStepsToHookContexts(t *testing.T) {
	t.Run("self_replace_and_multiarch", func(t *testing.T) {
		state := client.GooGetState{
			{PackageSpec: &goolib.PkgSpec{Name: "selfpkg", Arch: "noarch", Version: "1.0.0"}},
			{PackageSpec: &goolib.PkgSpec{Name: "multi", Arch: "x86_64", Version: "1.0.0"}},
		}
		rm := client.RepoMap{
			"repo": client.Repo{
				Packages: []goolib.RepoSpec{
					{PackageSpec: &goolib.PkgSpec{Name: "selfpkg", Arch: "noarch", Version: "2.0.0", Replaces: []string{"selfpkg<2.0.0"}}},
					{PackageSpec: &goolib.PkgSpec{Name: "multi", Arch: "noarch", Version: "2.0.0"}},
				},
			},
		}

		selfPlan, err := PlanState(goolib.PackageInfo{Name: "selfpkg"}, "", rm, state, []string{"noarch", "x86_64"}, Options{})
		if err != nil {
			t.Fatalf("PlanState(selfpkg): %v", err)
		}
		step := selfPlan.p.steps[0]
		if len(step.removed) != 1 {
			t.Fatalf("selfpkg removed count = %d, want 1", len(step.removed))
		}
		if diff := cmp.Diff(goolib.InstallerContext{
			Action:     goolib.ActionReplace,
			Package:    "selfpkg.noarch.1.0.0",
			ReplacedBy: "selfpkg.noarch.2.0.0",
		}, removalContext(step, step.removed[0])); diff != "" {
			t.Errorf("self-replace removalContext mismatch (-want +got):\n%s", diff)
		}
		if diff := cmp.Diff(goolib.InstallerContext{
			Action:  goolib.ActionInstall,
			Package: "selfpkg.noarch.2.0.0",
		}, installContext(step)); diff != "" {
			t.Errorf("self-replace installContext mismatch (-want +got):\n%s", diff)
		}

		multiPlan, err := PlanState(goolib.PackageInfo{Name: "multi", Arch: "noarch", Ver: "2.0.0"}, "repo", rm, state, []string{"noarch", "x86_64"}, Options{})
		if err != nil {
			t.Fatalf("PlanState(multi.noarch): %v", err)
		}
		if diff := cmp.Diff(goolib.InstallerContext{
			Action:  goolib.ActionInstall,
			Package: "multi.noarch.2.0.0",
		}, installContext(multiPlan.p.steps[0])); diff != "" {
			t.Errorf("multi-arch installContext mismatch (-want +got):\n%s", diff)
		}
	})

	t.Run("dependency_step_replacer_and_winner_cascade_agreement", func(t *testing.T) {
		state := client.GooGetState{
			{PackageSpec: &goolib.PkgSpec{Name: "X", Arch: "noarch", Version: "1.0.0"}},
			{PackageSpec: &goolib.PkgSpec{Name: "Y", Arch: "noarch", Version: "1.0.0"}},
			{PackageSpec: &goolib.PkgSpec{Name: "Z", Arch: "noarch", Version: "1.0.0", PkgDependencies: map[string]string{"Y": "1.0.0"}}},
		}
		rm := client.RepoMap{
			"repo": client.Repo{
				Packages: []goolib.RepoSpec{
					{PackageSpec: &goolib.PkgSpec{Name: "D", Arch: "noarch", Version: "1.0.0", Replaces: []string{"X"}}},
					{PackageSpec: &goolib.PkgSpec{
						Name:            "W",
						Arch:            "noarch",
						Version:         "2.0.0",
						PkgDependencies: map[string]string{"D": "1.0.0"},
						Replaces:        []string{"Y"},
						Provides:        []string{"Y=0.5.0"},
					}},
				},
			},
		}

		p, err := PlanState(goolib.PackageInfo{Name: "W"}, "", rm, state, []string{"noarch"}, Options{OrphanedDependents: OrphanRemove})
		if err != nil {
			t.Fatalf("PlanState(W): %v", err)
		}

		var gotRemovals, gotInstalls []goolib.InstallerContext
		for _, s := range p.p.steps {
			for _, r := range s.removed {
				gotRemovals = append(gotRemovals, removalContext(s, r))
			}
			gotInstalls = append(gotInstalls, installContext(s))
		}
		wantRemovals := []goolib.InstallerContext{
			{Action: goolib.ActionReplace, Package: "X.noarch.1.0.0", ReplacedBy: "D.noarch.1.0.0"},
			{Action: goolib.ActionRemoveDependent, Package: "Z.noarch.1.0.0"},
			{Action: goolib.ActionReplace, Package: "Y.noarch.1.0.0", ReplacedBy: "W.noarch.2.0.0"},
		}
		wantInstalls := []goolib.InstallerContext{
			{Action: goolib.ActionInstall, Package: "D.noarch.1.0.0"},
			{Action: goolib.ActionInstall, Package: "W.noarch.2.0.0"},
		}
		if diff := cmp.Diff(wantRemovals, gotRemovals); diff != "" {
			t.Errorf("step removalContexts mismatch (-want +got):\n%s", diff)
		}
		if diff := cmp.Diff(wantInstalls, gotInstalls); diff != "" {
			t.Errorf("step installContexts mismatch (-want +got):\n%s", diff)
		}
	})
}

// TestObserverCallSiteWiringAndDBOnlyGate exercises every internal install call
// site via installHookObserver, confirming non-zero actions on real runs,
// distinct contexts between FromDisk and Reinstall, reverse-order recovery
// contexts, zero observer invocations under DBOnly, and single-snapshot read
// behavior.
func TestObserverCallSiteWiringAndDBOnlyGate(t *testing.T) {
	t.Run("from_repo_steps_and_recovery_newest_first", func(t *testing.T) {
		installed := []goolib.PkgSpec{
			{Name: "victimA", Arch: "noarch", Version: "1.0.0"},
			{Name: "cascadeB", Arch: "noarch", Version: "1.0.0", PkgDependencies: map[string]string{"victimA": "1.0.0"}},
			{Name: "parent", Arch: "noarch", Version: "1.0.0"},
		}
		available := []goolib.PkgSpec{
			{Name: "depNew", Arch: "noarch", Version: "1.0.0"},
			{Name: "parent", Arch: "noarch", Version: "1.0.0"},
			{Name: "parent", Arch: "noarch", Version: "2.0.0", PkgDependencies: map[string]string{"depNew": "1.0.0"}},
			{Name: "replacer", Arch: "noarch", Version: "3.0.0", Replaces: []string{"victimA"}},
		}
		f := newRelFixture(t, installed, available)
		var repoURL string
		for u := range f.rm {
			repoURL = u
		}
		got := recordInstallHooks(t)

		// Multi-step install: new dependency followed by parent upgrade.
		if err := New(f.cache, f.db, Options{}).FromRepo(t.Context(), goolib.PackageInfo{Name: "parent"}, "", f.rm, []string{"noarch"}, f.downloader); err != nil {
			t.Fatalf("FromRepo(parent upgrade): %v", err)
		}
		// Explicit downgrade of parent back to 1.0.0.
		if err := New(f.cache, f.db, Options{}).FromRepo(t.Context(), goolib.PackageInfo{Name: "parent", Arch: "noarch", Ver: "1.0.0"}, repoURL, f.rm, []string{"noarch"}, f.downloader); err != nil {
			t.Fatalf("FromRepo(parent downgrade): %v", err)
		}
		// Explicit same-version reinstall of parent 1.0.0 via FromRepo.
		if err := New(f.cache, f.db, Options{}).FromRepo(t.Context(), goolib.PackageInfo{Name: "parent", Arch: "noarch", Ver: "1.0.0"}, repoURL, f.rm, []string{"noarch"}, f.downloader); err != nil {
			t.Fatalf("FromRepo(parent same-version): %v", err)
		}

		// Replacement removes cascadeB then victimA, then fails at stageAfterReplace,
		// triggering recoverRemoved in reverse order (victimA, then cascadeB).
		ri := &repoInstall{
			Installer:  New(f.cache, f.db, Options{OrphanedDependents: OrphanRemove}),
			rm:         f.rm,
			archs:      []string{"noarch"},
			downloader: f.downloader,
			stageHook: func(stage applyStage, _ string) error {
				if stage == stageAfterReplace {
					return errors.New("injected failure after removals")
				}
				return nil
			},
		}
		if err := ri.fromRepo(t.Context(), goolib.PackageInfo{Name: "replacer"}, ""); err == nil {
			t.Fatal("fromRepo(replacer) succeeded despite stageAfterReplace error")
		}

		want := []goolib.InstallerContext{
			{Action: goolib.ActionInstall, Package: "depNew.noarch.1.0.0"},
			{Action: goolib.ActionUpgrade, Package: "parent.noarch.2.0.0", PreviousVersion: "1.0.0"},
			{Action: goolib.ActionDowngrade, Package: "parent.noarch.1.0.0", PreviousVersion: "2.0.0"},
			{Action: goolib.ActionReinstall, Package: "parent.noarch.1.0.0", PreviousVersion: "1.0.0"},
			{Action: goolib.ActionRecover, Package: "victimA.noarch.1.0.0"},
			{Action: goolib.ActionRecover, Package: "cascadeB.noarch.1.0.0"},
		}
		if diff := cmp.Diff(want, *got); diff != "" {
			t.Errorf("observed FromRepo/recover contexts mismatch (-want +got):\n%s", diff)
		}
		for i, ic := range *got {
			if ic.Action == goolib.ActionUnspecified {
				t.Errorf("observed context[%d] has ActionUnspecified: %+v", i, ic)
			}
		}
	})

	t.Run("from_disk_versus_reinstall_distinct_contexts", func(t *testing.T) {
		f := newRelFixture(t, nil, nil)
		archiveDir := t.TempDir()
		v1 := testutil.GenGoo(t, archiveDir, f.dst, goolib.PkgSpec{Name: "diskpkg", Arch: "noarch", Version: "1.0.0"})
		v2 := testutil.GenGoo(t, archiveDir, f.dst, goolib.PkgSpec{Name: "diskpkg", Arch: "noarch", Version: "2.0.0"})

		got := recordInstallHooks(t)
		ins := New(f.cache, f.db, Options{})

		// Fresh install via FromDisk (even when reinstall=true is passed on an uninstalled package).
		if err := ins.FromDisk(filepath.Join(archiveDir, v1.Source), true); err != nil {
			t.Fatalf("FromDisk(v1 fresh): %v", err)
		}
		// Upgrade to 2.0.0 via FromDisk.
		if err := ins.FromDisk(filepath.Join(archiveDir, v2.Source), false); err != nil {
			t.Fatalf("FromDisk(v2 upgrade): %v", err)
		}
		// Reinstall installed 2.0.0 row via Reinstall.
		rowV2, err := f.db.FetchPkg(goolib.PackageInfo{Name: "diskpkg", Arch: "noarch"})
		if err != nil {
			t.Fatalf("FetchPkg(diskpkg): %v", err)
		}
		if err := Reinstall(t.Context(), rowV2, false, false, f.downloader, f.db); err != nil {
			t.Fatalf("Reinstall(v2): %v", err)
		}
		// Install the older 1.0.0 archive over 2.0.0 via FromDisk with reinstall enabled to classify as a downgrade.
		if err := ins.FromDisk(filepath.Join(archiveDir, v1.Source), true); err != nil {
			t.Fatalf("FromDisk(v1 over v2 with reinstall=true): %v", err)
		}
		// Install the same 1.0.0 archive over 1.0.0 via FromDisk with reinstall enabled to classify as a reinstall.
		if err := ins.FromDisk(filepath.Join(archiveDir, v1.Source), true); err != nil {
			t.Fatalf("FromDisk(v1 over v1 with reinstall=true): %v", err)
		}

		want := []goolib.InstallerContext{
			{Action: goolib.ActionInstall, Package: "diskpkg.noarch.1.0.0"},
			{Action: goolib.ActionUpgrade, Package: "diskpkg.noarch.2.0.0", PreviousVersion: "1.0.0"},
			{Action: goolib.ActionReinstall, Package: "diskpkg.noarch.2.0.0", PreviousVersion: "2.0.0"},
			{Action: goolib.ActionDowngrade, Package: "diskpkg.noarch.1.0.0", PreviousVersion: "2.0.0"},
			{Action: goolib.ActionReinstall, Package: "diskpkg.noarch.1.0.0", PreviousVersion: "1.0.0"},
		}
		if diff := cmp.Diff(want, *got); diff != "" {
			t.Errorf("FromDisk/Reinstall contexts mismatch (-want +got):\n%s", diff)
		}
	})

	t.Run("db_only_computes_and_observes_no_context", func(t *testing.T) {
		installed := []goolib.PkgSpec{
			{Name: "old", Arch: "noarch", Version: "1.0.0"},
		}
		available := []goolib.PkgSpec{
			{Name: "rep", Arch: "noarch", Version: "2.0.0", Replaces: []string{"old"}},
		}
		f := newRelFixture(t, installed, available)
		archiveDir := t.TempDir()
		localPkg := testutil.GenGoo(t, archiveDir, f.dst, goolib.PkgSpec{Name: "local", Arch: "noarch", Version: "1.0.0"})

		got := recordInstallHooks(t)
		dbOnlyIns := New(f.cache, f.db, Options{DBOnly: true})
		if err := dbOnlyIns.FromRepo(t.Context(), goolib.PackageInfo{Name: "rep"}, "", f.rm, []string{"noarch"}, f.downloader); err != nil {
			t.Fatalf("FromRepo(DBOnly): %v", err)
		}
		if err := dbOnlyIns.FromDisk(filepath.Join(archiveDir, localPkg.Source), false); err != nil {
			t.Fatalf("FromDisk(DBOnly): %v", err)
		}

		// Also exercise DBOnly replacement recovery.
		ri := &repoInstall{
			Installer:  New(f.cache, f.db, Options{DBOnly: true}),
			rm:         client.RepoMap{"r": client.Repo{Packages: []goolib.RepoSpec{{PackageSpec: &goolib.PkgSpec{Name: "rep2", Arch: "noarch", Version: "3.0.0", Replaces: []string{"rep"}}}}}},
			archs:      []string{"noarch"},
			downloader: f.downloader,
			stageHook: func(stage applyStage, _ string) error {
				if stage == stageAfterReplace {
					return errors.New("abort db_only replace")
				}
				return nil
			},
		}
		// Copy rep2 archive into cache or serve it from f.rm: build a fresh fixture for clean repo URL.
		f2 := newRelFixture(t,
			[]goolib.PkgSpec{{Name: "victim", Arch: "noarch", Version: "1.0.0"}},
			[]goolib.PkgSpec{{Name: "rep2", Arch: "noarch", Version: "2.0.0", Replaces: []string{"victim"}}},
		)
		ri.Installer = New(f2.cache, f2.db, Options{DBOnly: true})
		ri.rm = f2.rm
		ri.downloader = f2.downloader
		if err := ri.fromRepo(t.Context(), goolib.PackageInfo{Name: "rep2"}, ""); err == nil {
			t.Fatal("fromRepo(DBOnly recovery) succeeded despite stageAfterReplace error")
		}

		if len(*got) != 0 {
			t.Errorf("DBOnly runs invoked installHookObserver %d times (%+v), want 0", len(*got), *got)
		}
	})

	t.Run("previous_version_uses_plan_snapshot_and_record_install_reads_once", func(t *testing.T) {
		f := newRelFixture(t,
			[]goolib.PkgSpec{{Name: "target", Arch: "noarch", Version: "1.0.0"}},
			[]goolib.PkgSpec{{Name: "target", Arch: "noarch", Version: "2.0.0"}},
		)
		got := recordInstallHooks(t)

		// Rewrite target's live DB version to 9.9.9 at stageFetched (after
		// planning's snapshot read, before applyStep runs) and inject a corrupt
		// row at stageAfterInstall (right before recordInstall). Because
		// applyStep computes installContext from s.old in the plan rather than
		// re-reading the live DB or post-install state, installHookObserver
		// receives PreviousVersion="1.0.0" rather than "9.9.9".
		ri := &repoInstall{
			Installer:  New(f.cache, f.db, Options{}),
			rm:         f.rm,
			archs:      []string{"noarch"},
			downloader: f.downloader,
			stageHook: func(stage applyStage, _ string) error {
				switch stage {
				case stageFetched:
					return f.db.WriteStateToDB(client.GooGetState{
						{PackageSpec: &goolib.PkgSpec{Name: "target", Arch: "noarch", Version: "9.9.9"}},
					})
				case stageAfterInstall:
					rawDB, err := sql.Open("sqlite", settings.DBFile())
					if err != nil {
						return err
					}
					defer rawDB.Close()
					_, err = rawDB.ExecContext(context.Background(),
						`INSERT INTO InstalledPackages (pkg_name, pkg_arch, pkg_ver, pkg_json) VALUES (?, ?, ?, ?)`,
						"corrupt", "noarch", "1.0.0", []byte("{bad json"),
					)
					return err
				}
				return nil
			},
		}
		if err := ri.fromRepo(t.Context(), goolib.PackageInfo{Name: "target"}, ""); err == nil {
			t.Fatal("fromRepo() = nil, want post-install recordInstall error from corrupt row")
		}
		want := []goolib.InstallerContext{
			{Action: goolib.ActionUpgrade, Package: "target.noarch.2.0.0", PreviousVersion: "1.0.0"},
		}
		if diff := cmp.Diff(want, *got); diff != "" {
			t.Errorf("observed hook before post-install recordInstall failure mismatch (-want +got):\n%s", diff)
		}

		// Also verify that every recordInstall caller policy still reads the
		// installed rows at most once after context threading.
		captured := &client.PackageState{PackageSpec: &goolib.PkgSpec{Name: "target", Arch: "noarch", Version: "1.0.0"}}
		for _, tc := range []struct {
			name      string
			pol       recordPolicy
			captured  *client.PackageState
			wantReads int
		}{
			{name: "apply_step", pol: applyStepPolicy(false), wantReads: 1},
			{name: "apply_step_db_only", pol: applyStepPolicy(true), wantReads: 1},
			{name: "from_disk", pol: fromDiskPolicy(false), wantReads: 1},
			{name: "from_disk_db_only", pol: fromDiskPolicy(true), wantReads: 0},
			{name: "recover", pol: recoverPolicy(false), captured: captured, wantReads: 0},
			{name: "recover_db_only", pol: recoverPolicy(true), captured: captured, wantReads: 0},
			{name: "reinstall", pol: reinstallPolicy(), wantReads: 1},
		} {
			reader := &countingStateReader{}
			row := client.PackageState{PackageSpec: &goolib.PkgSpec{Name: "target", Arch: "noarch", Version: "2.0.0"}}
			if err := recordInstall(reader, row, tc.captured, tc.pol, func(client.PackageState, []googetdb.FileTransfer) error {
				return nil
			}); err != nil {
				t.Fatalf("%s: recordInstall() unexpected error: %v", tc.name, err)
			}
			if reader.calls != tc.wantReads {
				t.Errorf("%s: FetchPkgs call count = %d, want %d", tc.name, reader.calls, tc.wantReads)
			}
		}
	})
}

// TestInternalUninstallRowCallSitesSetHook parses production Go files in
// package install to ensure every internal call to remove.UninstallRow sets
// remove.Options.Hook explicitly rather than relying on the zero legacy value.
func TestInternalUninstallRowCallSitesSetHook(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	fset := token.NewFileSet()
	callCount := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("ParseFile(%s): %v", name, err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "UninstallRow" {
				return true
			}
			pkgIdent, ok := sel.X.(*ast.Ident)
			if !ok || pkgIdent.Name != "remove" {
				return true
			}
			callCount++
			if len(call.Args) < 3 {
				t.Fatalf("%s: remove.UninstallRow has %d args, want >= 3", name, len(call.Args))
			}
			lit, ok := call.Args[2].(*ast.CompositeLit)
			if !ok {
				t.Errorf("%s: remove.UninstallRow options arg is %T, want composite literal setting Hook", name, call.Args[2])
				return true
			}
			hasHook := false
			for _, elt := range lit.Elts {
				if kv, ok := elt.(*ast.KeyValueExpr); ok {
					if keyIdent, ok := kv.Key.(*ast.Ident); ok && keyIdent.Name == "Hook" {
						hasHook = true
					}
				}
			}
			if !hasHook {
				t.Errorf("%s: remove.UninstallRow options literal omits Hook field", name)
			}
			return true
		})
	}
	if callCount == 0 {
		t.Fatal("found 0 production remove.UninstallRow calls in install package, want >= 1")
	}
}
