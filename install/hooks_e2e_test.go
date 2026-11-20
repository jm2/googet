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

package install

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/googet/v2/client"
	"github.com/google/googet/v2/googetdb"
	"github.com/google/googet/v2/goolib"
	"github.com/google/googet/v2/priority"
	"github.com/google/googet/v2/remove"
	"github.com/google/googet/v2/settings"
	"github.com/google/googet/v2/system"
	"github.com/google/googet/v2/testutil"
)

// buildGoogetCLI compiles the root googet binary into a test-scoped temporary
// directory cleaned up automatically when t completes.
func buildGoogetCLI(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "googet")
	cmd := exec.Command("go", "build", "-o", bin, "github.com/google/googet/v2")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build googet: %v\n%s", err, out)
	}
	return bin
}

// hookStubScript returns a POSIX /bin/sh script that appends one pipe-delimited
// record of kind and all four GOOGET_* context variables to logFile, distinguishing
// empty values ("") from unset variables ("<unset>"), plus optional extraBody.
func hookStubScript(logFile, kind, extraBody string) string {
	var b strings.Builder
	b.WriteString("#!/bin/sh\nset -eu\n")
	fmt.Fprintf(&b, "printf '%%s|%%s|%%s|%%s|%%s\\n' %q \"${GOOGET_ACTION-<unset>}\" \"${GOOGET_PACKAGE-<unset>}\" \"${GOOGET_PREVIOUS_VERSION-<unset>}\" \"${GOOGET_REPLACED_BY-<unset>}\" >> %q\n", kind, logFile)
	if extraBody != "" {
		b.WriteString(extraBody)
		if !strings.HasSuffix(extraBody, "\n") {
			b.WriteByte('\n')
		}
	}
	return b.String()
}

type hookArchiveSpec struct {
	spec            goolib.PkgSpec
	installScript   string
	uninstallScript string
	verifyScript    string
}

// buildHookGoo writes a .goo archive in gooDir with optional executable
// install.sh, uninstall.sh, and verify.sh scripts and a single payload file.
func buildHookGoo(t *testing.T, gooDir, dstDir string, hs hookArchiveSpec) goolib.RepoSpec {
	t.Helper()
	ps := hs.spec
	payloadPath := filepath.Join(dstDir, ps.Name+"."+ps.Arch)
	ps.Files = map[string]string{"payload": payloadPath}
	if hs.installScript != "" {
		ps.Install = goolib.ExecFile{Path: "install.sh"}
	}
	if hs.uninstallScript != "" {
		ps.Uninstall = goolib.ExecFile{Path: "uninstall.sh"}
	}
	if hs.verifyScript != "" {
		ps.Verify = goolib.ExecFile{Path: "verify.sh"}
	}

	specBytes, err := json.Marshal(ps)
	if err != nil {
		t.Fatalf("json.Marshal(PkgSpec): %v", err)
	}
	archiveName := ps.String() + ".goo"
	archivePath := filepath.Join(gooDir, archiveName)
	f, err := os.Create(archivePath)
	if err != nil {
		t.Fatalf("Create(%s): %v", archivePath, err)
	}
	defer f.Close()

	h := sha256.New()
	gw := gzip.NewWriter(io.MultiWriter(h, f))
	tw := tar.NewWriter(gw)
	modTime := time.Now()

	type tarFile struct {
		name string
		mode int64
		body []byte
	}
	entries := []tarFile{
		{name: "payload", mode: 0644, body: []byte(ps.String())},
		{name: ps.Name + ".pkgspec", mode: 0644, body: specBytes},
	}
	if hs.installScript != "" {
		entries = append(entries, tarFile{name: "install.sh", mode: 0755, body: []byte(hs.installScript)})
	}
	if hs.uninstallScript != "" {
		entries = append(entries, tarFile{name: "uninstall.sh", mode: 0755, body: []byte(hs.uninstallScript)})
	}
	if hs.verifyScript != "" {
		entries = append(entries, tarFile{name: "verify.sh", mode: 0755, body: []byte(hs.verifyScript)})
	}
	for _, e := range entries {
		if err := tw.WriteHeader(&tar.Header{Name: e.name, Mode: e.mode, Size: int64(len(e.body)), ModTime: modTime}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(e.body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	return goolib.RepoSpec{
		Checksum:    fmt.Sprintf("%x", h.Sum(nil)),
		Source:      archiveName,
		PackageSpec: &ps,
	}
}

type hookEnvFixture struct {
	rootDir    string
	cacheDir   string
	dstDir     string
	gooDir     string
	repoURL    string
	logFile    string
	db         *googetdb.GooDB
	rm         client.RepoMap
	downloader *client.Downloader
}

// newHookEnvFixture creates a hermetic repository, cache, destination directory,
// and SQLite database seeded with installed packages and serving available
// packages over HTTP.
func newHookEnvFixture(t *testing.T, logFile string, installed, available []hookArchiveSpec) *hookEnvFixture {
	t.Helper()
	t.Cleanup(settings.Snapshot())
	rootDir := t.TempDir()
	cacheDir := filepath.Join(rootDir, "cache")
	reposDir := filepath.Join(rootDir, "repos")
	for _, d := range []string{cacheDir, reposDir} {
		if err := os.MkdirAll(d, 0755); err != nil {
			t.Fatal(err)
		}
	}
	conf := "allowunsafeurl: true\narchs: [noarch, x86_64]\n"
	if err := os.WriteFile(filepath.Join(rootDir, "googet.conf"), []byte(conf), 0644); err != nil {
		t.Fatal(err)
	}
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
	dstDir := t.TempDir()
	srv := testutil.ServeGoo(t, gooDir)
	t.Cleanup(srv.Close)
	repoURL := srv.URL + "/repo"

	var state client.GooGetState
	for _, hs := range installed {
		if hs.installScript == "" {
			hs.installScript = hookStubScript(logFile, "install", "")
		}
		if hs.uninstallScript == "" {
			hs.uninstallScript = hookStubScript(logFile, "uninstall", "")
		}
		rs := buildHookGoo(t, gooDir, dstDir, hs)
		localPath := filepath.Join(cacheDir, rs.Source)
		copyFile(t, filepath.Join(gooDir, rs.Source), localPath)
		payloadFile := filepath.Join(dstDir, hs.spec.Name+"."+hs.spec.Arch)
		if err := os.WriteFile(payloadFile, []byte(rs.PackageSpec.String()), 0644); err != nil {
			t.Fatal(err)
		}
		state = append(state, client.PackageState{
			SourceRepo:     repoURL,
			DownloadURL:    srv.URL + "/" + rs.Source,
			Checksum:       rs.Checksum,
			LocalPath:      localPath,
			PackageSpec:    rs.PackageSpec,
			InstalledFiles: map[string]string{payloadFile: checksumOf(t, payloadFile)},
		})
	}
	if err := db.WriteStateToDB(state); err != nil {
		t.Fatalf("WriteStateToDB: %v", err)
	}

	var specs []goolib.RepoSpec
	for _, hs := range available {
		if hs.installScript == "" {
			hs.installScript = hookStubScript(logFile, "install", "")
		}
		if hs.uninstallScript == "" {
			hs.uninstallScript = hookStubScript(logFile, "uninstall", "")
		}
		specs = append(specs, buildHookGoo(t, gooDir, dstDir, hs))
	}
	if err := os.MkdirAll(filepath.Join(gooDir, "repo"), 0755); err != nil {
		t.Fatal(err)
	}
	idx, err := json.Marshal(specs)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gooDir, "repo", "index"), idx, 0644); err != nil {
		t.Fatal(err)
	}

	return &hookEnvFixture{
		rootDir:    rootDir,
		cacheDir:   cacheDir,
		dstDir:     dstDir,
		gooDir:     gooDir,
		repoURL:    repoURL,
		logFile:    logFile,
		db:         db,
		rm:         client.RepoMap{repoURL: client.Repo{Priority: priority.Default, Packages: specs}},
		downloader: downloader,
	}
}

func readHookLines(t *testing.T, logFile string) []string {
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

// TestEndToEndRepoInstallUpgradeDowngradeReinstallAndDeps verifies the ordered
// child environment variables across fresh install, upgrade, explicit downgrade,
// same-version FromRepo reinstall, and a multi-step dependency+upgrade plan.
func TestEndToEndRepoInstallUpgradeDowngradeReinstallAndDeps(t *testing.T) {
	logFile := filepath.Join(t.TempDir(), "hooks.log")
	available := []hookArchiveSpec{
		{spec: goolib.PkgSpec{Name: "alpha", Arch: "noarch", Version: "1.0.0"}},
		{spec: goolib.PkgSpec{Name: "alpha", Arch: "noarch", Version: "2.0.0"}},
		{spec: goolib.PkgSpec{Name: "deplib", Arch: "noarch", Version: "1.0.0"}},
		{spec: goolib.PkgSpec{Name: "alpha", Arch: "noarch", Version: "3.0.0", PkgDependencies: map[string]string{"deplib": "1.0.0"}}},
	}
	f := newHookEnvFixture(t, logFile, nil, available)
	logs := testutil.CaptureLogs(t)
	ins := New(f.cacheDir, f.db, Options{})

	// Fresh install of 1.0.0.
	if err := ins.FromRepo(t.Context(), goolib.PackageInfo{Name: "alpha", Arch: "noarch", Ver: "1.0.0"}, f.repoURL, f.rm, []string{"noarch"}, f.downloader); err != nil {
		t.Fatalf("FromRepo(alpha 1.0.0): %v", err)
	}
	// Upgrade to 2.0.0.
	if err := ins.FromRepo(t.Context(), goolib.PackageInfo{Name: "alpha", Arch: "noarch", Ver: "2.0.0"}, f.repoURL, f.rm, []string{"noarch"}, f.downloader); err != nil {
		t.Fatalf("FromRepo(alpha 2.0.0): %v", err)
	}
	// Explicit downgrade back to 1.0.0.
	if err := ins.FromRepo(t.Context(), goolib.PackageInfo{Name: "alpha", Arch: "noarch", Ver: "1.0.0"}, f.repoURL, f.rm, []string{"noarch"}, f.downloader); err != nil {
		t.Fatalf("FromRepo(alpha downgrade 1.0.0): %v", err)
	}
	// Same-version reinstall of 1.0.0 via FromRepo.
	if err := ins.FromRepo(t.Context(), goolib.PackageInfo{Name: "alpha", Arch: "noarch", Ver: "1.0.0"}, f.repoURL, f.rm, []string{"noarch"}, f.downloader); err != nil {
		t.Fatalf("FromRepo(alpha reinstall 1.0.0): %v", err)
	}
	// Multi-step plan: fresh dependency deplib 1.0.0 then upgrade alpha 1.0.0 -> 3.0.0.
	if err := ins.FromRepo(t.Context(), goolib.PackageInfo{Name: "alpha"}, "", f.rm, []string{"noarch"}, f.downloader); err != nil {
		t.Fatalf("FromRepo(alpha latest 3.0.0): %v", err)
	}

	want := []string{
		"install|install|alpha.noarch.1.0.0||<unset>",
		"install|upgrade|alpha.noarch.2.0.0|1.0.0|<unset>",
		"install|downgrade|alpha.noarch.1.0.0|2.0.0|<unset>",
		"install|reinstall|alpha.noarch.1.0.0|1.0.0|<unset>",
		"install|install|deplib.noarch.1.0.0||<unset>",
		"install|upgrade|alpha.noarch.3.0.0|1.0.0|<unset>",
	}
	if diff := cmp.Diff(want, readHookLines(t, logFile)); diff != "" {
		t.Errorf("hook log mismatch (-want +got):\n%s", diff)
	}
	if wantLog := "installer_context hook=install action=upgrade package=alpha.noarch.3.0.0 previous=1.0.0 replaced_by="; !strings.Contains(logs.String(), wantLog) {
		t.Errorf("captured logs missing %q:\n%s", wantLog, logs.String())
	}
}

// TestEndToEndReplaceCascadeListedSelfReplaceAndMultiArch verifies child hook
// variables across multi-victim replacement, cascaded name-based dependents,
// kept Provides-only dependents, self-replacement, and multi-arch installs.
func TestEndToEndReplaceCascadeListedSelfReplaceAndMultiArch(t *testing.T) {
	t.Run("two_victims_cascade_and_kept_provides_dependent", func(t *testing.T) {
		logFile := filepath.Join(t.TempDir(), "hooks.log")
		installed := []hookArchiveSpec{
			{spec: goolib.PkgSpec{Name: "victimA", Arch: "noarch", Version: "1.0.0", Provides: []string{"virtCap"}}},
			{spec: goolib.PkgSpec{Name: "victimB", Arch: "noarch", Version: "1.2.0"}},
			{spec: goolib.PkgSpec{Name: "cascDep", Arch: "noarch", Version: "1.0.0", PkgDependencies: map[string]string{"victimA": "1.0.0"}}},
			{spec: goolib.PkgSpec{Name: "keptProv", Arch: "noarch", Version: "1.0.0", PkgDependencies: map[string]string{"virtCap": "1.0.0"}}},
		}
		available := []hookArchiveSpec{
			{spec: goolib.PkgSpec{Name: "replacer", Arch: "noarch", Version: "2.0.0", Replaces: []string{"victimA", "victimB"}}},
		}
		f := newHookEnvFixture(t, logFile, installed, available)
		logs := testutil.CaptureLogs(t)

		if err := New(f.cacheDir, f.db, Options{OrphanedDependents: OrphanRemove}).FromRepo(
			t.Context(), goolib.PackageInfo{Name: "replacer"}, "", f.rm, []string{"noarch"}, f.downloader,
		); err != nil {
			t.Fatalf("FromRepo(replacer): %v", err)
		}

		// cascDep and victimB both have zero dependents in the initial removal wave
		// (sorted alphabetically), after which victimA becomes ready once cascDep
		// is removed.
		want := []string{
			"uninstall|remove-dependent|cascDep.noarch.1.0.0||<unset>",
			"uninstall|replace|victimB.noarch.1.2.0||replacer.noarch.2.0.0",
			"uninstall|replace|victimA.noarch.1.0.0||replacer.noarch.2.0.0",
			"install|install|replacer.noarch.2.0.0||<unset>",
		}
		if diff := cmp.Diff(want, readHookLines(t, logFile)); diff != "" {
			t.Errorf("hook log mismatch (-want +got):\n%s", diff)
		}
		for _, wantLog := range []string{
			"installer_context hook=uninstall action=remove-dependent package=cascDep.noarch.1.0.0 previous= replaced_by=",
			"installer_context hook=uninstall action=replace package=victimA.noarch.1.0.0 previous= replaced_by=replacer.noarch.2.0.0",
		} {
			if !strings.Contains(logs.String(), wantLog) {
				t.Errorf("captured logs missing %q:\n%s", wantLog, logs.String())
			}
		}
	})

	t.Run("self_replace_and_multiarch", func(t *testing.T) {
		logFile := filepath.Join(t.TempDir(), "hooks.log")
		installed := []hookArchiveSpec{
			{spec: goolib.PkgSpec{Name: "selfpkg", Arch: "noarch", Version: "1.0.0"}},
			{spec: goolib.PkgSpec{Name: "dualarch", Arch: "x86_64", Version: "1.0.0"}},
		}
		available := []hookArchiveSpec{
			{spec: goolib.PkgSpec{Name: "selfpkg", Arch: "noarch", Version: "2.0.0", Replaces: []string{"selfpkg<2.0.0"}}},
			{spec: goolib.PkgSpec{Name: "dualarch", Arch: "noarch", Version: "2.0.0"}},
		}
		f := newHookEnvFixture(t, logFile, installed, available)
		ins := New(f.cacheDir, f.db, Options{})

		if err := ins.FromRepo(t.Context(), goolib.PackageInfo{Name: "selfpkg"}, "", f.rm, []string{"noarch", "x86_64"}, f.downloader); err != nil {
			t.Fatalf("FromRepo(selfpkg): %v", err)
		}
		if err := ins.FromRepo(t.Context(), goolib.PackageInfo{Name: "dualarch", Arch: "noarch", Ver: "2.0.0"}, f.repoURL, f.rm, []string{"noarch", "x86_64"}, f.downloader); err != nil {
			t.Fatalf("FromRepo(dualarch.noarch): %v", err)
		}

		want := []string{
			"uninstall|replace|selfpkg.noarch.1.0.0||selfpkg.noarch.2.0.0",
			"install|install|selfpkg.noarch.2.0.0||<unset>",
			"install|install|dualarch.noarch.2.0.0||<unset>",
		}
		if diff := cmp.Diff(want, readHookLines(t, logFile)); diff != "" {
			t.Errorf("hook log mismatch (-want +got):\n%s", diff)
		}
	})
}

// TestEndToEndRecoveryDBOnlyAndCorruptIndex verifies recovery hook ordering
// on stageAfterUninstall, failing replacer scripts, failing second victim
// uninstalls, and a replacer whose row can't be written, plus the absence of
// hooks under DBOnly and when the ownership index can't be built.
func TestEndToEndRecoveryDBOnlyAndCorruptIndex(t *testing.T) {
	t.Run("stage_after_uninstall_recovers_removed_victim", func(t *testing.T) {
		logFile := filepath.Join(t.TempDir(), "hooks.log")
		installed := []hookArchiveSpec{
			{spec: goolib.PkgSpec{Name: "victimA", Arch: "noarch", Version: "1.0.0"}},
			{spec: goolib.PkgSpec{Name: "victimB", Arch: "noarch", Version: "1.0.0"}},
		}
		available := []hookArchiveSpec{
			{spec: goolib.PkgSpec{Name: "replacer", Arch: "noarch", Version: "2.0.0", Replaces: []string{"victimA", "victimB"}}},
		}
		f := newHookEnvFixture(t, logFile, installed, available)

		ri := &repoInstall{
			Installer:  New(f.cacheDir, f.db, Options{}),
			rm:         f.rm,
			archs:      []string{"noarch"},
			downloader: f.downloader,
			stageHook: func(stage applyStage, step string) error {
				if stage == stageAfterUninstall && step == "victimA.noarch.1.0.0" {
					return errors.New("abort after first victim")
				}
				return nil
			},
		}
		if err := ri.fromRepo(t.Context(), goolib.PackageInfo{Name: "replacer"}, ""); err == nil {
			t.Fatal("fromRepo() = nil, want stageAfterUninstall error")
		}

		want := []string{
			"uninstall|replace|victimA.noarch.1.0.0||replacer.noarch.2.0.0",
			"install|recover|victimA.noarch.1.0.0||<unset>",
		}
		if diff := cmp.Diff(want, readHookLines(t, logFile)); diff != "" {
			t.Errorf("hook log mismatch (-want +got):\n%s", diff)
		}
	})

	t.Run("failing_replacer_install_script_recovers_newest_first", func(t *testing.T) {
		logFile := filepath.Join(t.TempDir(), "hooks.log")
		installed := []hookArchiveSpec{
			{spec: goolib.PkgSpec{Name: "victimA", Arch: "noarch", Version: "1.0.0"}},
			{spec: goolib.PkgSpec{Name: "cascDep", Arch: "noarch", Version: "1.0.0", PkgDependencies: map[string]string{"victimA": "1.0.0"}}},
			{spec: goolib.PkgSpec{Name: "victimB", Arch: "noarch", Version: "1.0.0"}},
		}
		available := []hookArchiveSpec{
			{
				spec:          goolib.PkgSpec{Name: "failingRep", Arch: "noarch", Version: "2.0.0", Replaces: []string{"victimA", "victimB"}},
				installScript: hookStubScript(logFile, "install", "exit 1"),
			},
		}
		f := newHookEnvFixture(t, logFile, installed, available)

		if err := New(f.cacheDir, f.db, Options{OrphanedDependents: OrphanRemove}).FromRepo(
			t.Context(), goolib.PackageInfo{Name: "failingRep"}, "", f.rm, []string{"noarch"}, f.downloader,
		); err == nil {
			t.Fatal("FromRepo(failingRep) = nil, want script failure")
		}

		// Removal wave 1 removes cascDep and victimB (zero dependents), wave 2
		// removes victimA, and recovery runs in exact reverse order (victimA,
		// victimB, cascDep).
		want := []string{
			"uninstall|remove-dependent|cascDep.noarch.1.0.0||<unset>",
			"uninstall|replace|victimB.noarch.1.0.0||failingRep.noarch.2.0.0",
			"uninstall|replace|victimA.noarch.1.0.0||failingRep.noarch.2.0.0",
			"install|install|failingRep.noarch.2.0.0||<unset>",
			"install|recover|victimA.noarch.1.0.0||<unset>",
			"install|recover|victimB.noarch.1.0.0||<unset>",
			"install|recover|cascDep.noarch.1.0.0||<unset>",
		}
		if diff := cmp.Diff(want, readHookLines(t, logFile)); diff != "" {
			t.Errorf("hook log mismatch (-want +got):\n%s", diff)
		}
	})

	t.Run("failing_second_victim_uninstall_recovers_first_victim", func(t *testing.T) {
		logFile := filepath.Join(t.TempDir(), "hooks.log")
		installed := []hookArchiveSpec{
			{spec: goolib.PkgSpec{Name: "victimA", Arch: "noarch", Version: "1.0.0"}},
			{
				spec:            goolib.PkgSpec{Name: "victimB", Arch: "noarch", Version: "1.0.0"},
				uninstallScript: hookStubScript(logFile, "uninstall", "exit 1"),
			},
		}
		available := []hookArchiveSpec{
			{spec: goolib.PkgSpec{Name: "replacer", Arch: "noarch", Version: "2.0.0", Replaces: []string{"victimA", "victimB"}}},
		}
		f := newHookEnvFixture(t, logFile, installed, available)

		if err := New(f.cacheDir, f.db, Options{}).FromRepo(
			t.Context(), goolib.PackageInfo{Name: "replacer"}, "", f.rm, []string{"noarch"}, f.downloader,
		); err == nil {
			t.Fatal("FromRepo(replacer) = nil, want victimB uninstall failure")
		}

		want := []string{
			"uninstall|replace|victimA.noarch.1.0.0||replacer.noarch.2.0.0",
			"uninstall|replace|victimB.noarch.1.0.0||replacer.noarch.2.0.0",
			"install|recover|victimA.noarch.1.0.0||<unset>",
		}
		if diff := cmp.Diff(want, readHookLines(t, logFile)); diff != "" {
			t.Errorf("hook log mismatch (-want +got):\n%s", diff)
		}
	})

	t.Run("add_pkg_fails_twice_rolls_back_and_recovers_victim", func(t *testing.T) {
		logFile := filepath.Join(t.TempDir(), "hooks.log")
		installed := []hookArchiveSpec{
			{spec: goolib.PkgSpec{Name: "victimA", Arch: "noarch", Version: "1.0.0"}},
		}
		available := []hookArchiveSpec{
			{spec: goolib.PkgSpec{Name: "replacer", Arch: "noarch", Version: "2.0.0", Replaces: []string{"victimA"}}},
		}
		f := newHookEnvFixture(t, logFile, installed, available)

		addAttempts := 0
		ri := &repoInstall{
			Installer:  New(f.cacheDir, f.db, Options{}),
			rm:         f.rm,
			archs:      []string{"noarch"},
			downloader: f.downloader,
			stageHook: func(stage applyStage, _ string) error {
				if stage == stageAddPkg {
					addAttempts++
					return errors.New("simulated database write failure")
				}
				return nil
			},
		}
		err := ri.fromRepo(t.Context(), goolib.PackageInfo{Name: "replacer"}, "")
		if err == nil || !strings.Contains(err.Error(), "reinstalled the packages that installing replacer.noarch.2.0.0 had removed: victimA.noarch.1.0.0") {
			t.Fatalf("fromRepo() = %v, want the write error and victimA reinstalled", err)
		}
		if addAttempts != 2 {
			t.Errorf("stageAddPkg attempts = %d, want 2", addAttempts)
		}

		// The replacer's install hook ran before its row failed to write, so
		// recovery runs the victim's install hook again.
		want := []string{
			"uninstall|replace|victimA.noarch.1.0.0||replacer.noarch.2.0.0",
			"install|install|replacer.noarch.2.0.0||<unset>",
			"install|recover|victimA.noarch.1.0.0||<unset>",
		}
		if diff := cmp.Diff(want, readHookLines(t, logFile)); diff != "" {
			t.Errorf("hook log mismatch (-want +got):\n%s", diff)
		}
	})

	t.Run("db_only_across_every_entry_point_leaves_log_empty", func(t *testing.T) {
		logFile := filepath.Join(t.TempDir(), "hooks.log")
		installed := []hookArchiveSpec{
			{spec: goolib.PkgSpec{Name: "victimA", Arch: "noarch", Version: "1.0.0"}},
			{spec: goolib.PkgSpec{Name: "keepRemove", Arch: "noarch", Version: "1.0.0"}},
		}
		available := []hookArchiveSpec{
			{spec: goolib.PkgSpec{Name: "replacer", Arch: "noarch", Version: "2.0.0", Replaces: []string{"victimA"}}},
		}
		f := newHookEnvFixture(t, logFile, installed, available)
		localDir := t.TempDir()
		localRS := buildHookGoo(t, localDir, f.dstDir, hookArchiveSpec{
			spec:          goolib.PkgSpec{Name: "localOnly", Arch: "noarch", Version: "1.0.0"},
			installScript: hookStubScript(logFile, "install", ""),
		})
		logs := testutil.CaptureLogs(t)

		dbOnlyIns := New(f.cacheDir, f.db, Options{DBOnly: true})
		if err := dbOnlyIns.FromRepo(t.Context(), goolib.PackageInfo{Name: "replacer"}, "", f.rm, []string{"noarch"}, f.downloader); err != nil {
			t.Fatalf("FromRepo(DBOnly): %v", err)
		}
		if err := dbOnlyIns.FromDisk(filepath.Join(localDir, localRS.Source), false); err != nil {
			t.Fatalf("FromDisk(DBOnly): %v", err)
		}
		rowRemove, err := f.db.FetchPkg(goolib.PackageInfo{Name: "keepRemove", Arch: "noarch"})
		if err != nil {
			t.Fatalf("FetchPkg(keepRemove): %v", err)
		}
		if err := remove.UninstallRow(t.Context(), rowRemove, remove.Options{DBOnly: true, Downloader: f.downloader}, f.db); err != nil {
			t.Fatalf("UninstallRow(DBOnly): %v", err)
		}
		if err := remove.All(t.Context(), goolib.PackageInfo{Name: "localOnly", Arch: "noarch"}, remove.DepMap{"localOnly.noarch": nil}, true, f.downloader, f.db); err != nil {
			t.Fatalf("remove.All(DBOnly): %v", err)
		}

		if got := readHookLines(t, logFile); len(got) != 0 {
			t.Errorf("DBOnly hook lines = %v, want empty", got)
		}
		if strings.Contains(logs.String(), "installer_context") {
			t.Errorf("DBOnly emitted installer_context log line:\n%s", logs.String())
		}
	})

	t.Run("undecodable_unrelated_row_fails_before_uninstall_hook_runs", func(t *testing.T) {
		logFile := filepath.Join(t.TempDir(), "hooks.log")
		installed := []hookArchiveSpec{
			{spec: goolib.PkgSpec{Name: "victimA", Arch: "noarch", Version: "1.0.0"}},
		}
		available := []hookArchiveSpec{
			{spec: goolib.PkgSpec{Name: "replacer", Arch: "noarch", Version: "2.0.0", Replaces: []string{"victimA"}}},
		}
		f := newHookEnvFixture(t, logFile, installed, available)
		logs := testutil.CaptureLogs(t)

		// Inject an undecodable unrelated row at stageBeforeReplace (after
		// checkRemovals, right before removeReplaced calls remove.UninstallRow).
		ri := &repoInstall{
			Installer:  New(f.cacheDir, f.db, Options{}),
			rm:         f.rm,
			archs:      []string{"noarch"},
			downloader: f.downloader,
			stageHook: func(stage applyStage, _ string) error {
				if stage == stageBeforeReplace {
					rawDB, err := sql.Open("sqlite", settings.DBFile())
					if err != nil {
						return err
					}
					defer rawDB.Close()
					_, err = rawDB.ExecContext(context.Background(),
						`INSERT INTO InstalledPackages (pkg_name, pkg_arch, pkg_ver, pkg_json) VALUES (?, ?, ?, ?)`,
						"broken_row", "noarch", "1.0.0", []byte("{invalid json"),
					)
					return err
				}
				return nil
			},
		}
		if err := ri.fromRepo(t.Context(), goolib.PackageInfo{Name: "replacer"}, ""); err == nil {
			t.Fatal("fromRepo() = nil, want corrupt-row failure during uninstall index build")
		}
		if got := readHookLines(t, logFile); len(got) != 0 {
			t.Errorf("hook lines on corrupt index = %v, want empty", got)
		}
		if strings.Contains(logs.String(), "installer_context") {
			t.Errorf("corrupt index emitted installer_context log line:\n%s", logs.String())
		}
	})
}

// TestEndToEndFromDiskReinstallAndCLISubcommands verifies FromDisk (fresh,
// upgrade, same-version -reinstall, older-archive -reinstall), Reinstall,
// cli/install -reinstall, cli/verify -reinstall, remove.All with cascade, and
// cli/update -follow_replaces dry-run versus live hook parity.
func TestEndToEndFromDiskReinstallAndCLISubcommands(t *testing.T) {
	googetBin := buildGoogetCLI(t)

	t.Run("from_disk_reinstall_cli_and_remove_all_cascade", func(t *testing.T) {
		logFile := filepath.Join(t.TempDir(), "hooks.log")
		verifyLog := filepath.Join(t.TempDir(), "verify.log")
		f := newHookEnvFixture(t, logFile, nil, nil)
		localDir := t.TempDir()

		verifyStub := fmt.Sprintf("#!/bin/sh\nset -eu\nprintf '%%s|%%s\\n' \"${GOOGET_ACTION-<unset>}\" \"${GOOGET_REPLACED_BY-<unset>}\" >> %q\n", verifyLog)
		v1 := buildHookGoo(t, localDir, f.dstDir, hookArchiveSpec{
			spec:            goolib.PkgSpec{Name: "diskpkg", Arch: "noarch", Version: "1.0.0"},
			installScript:   hookStubScript(logFile, "install", ""),
			uninstallScript: hookStubScript(logFile, "uninstall", ""),
			verifyScript:    verifyStub,
		})
		v2 := buildHookGoo(t, localDir, f.dstDir, hookArchiveSpec{
			spec:            goolib.PkgSpec{Name: "diskpkg", Arch: "noarch", Version: "2.0.0"},
			installScript:   hookStubScript(logFile, "install", ""),
			uninstallScript: hookStubScript(logFile, "uninstall", ""),
			verifyScript:    verifyStub,
		})
		childPkg := buildHookGoo(t, localDir, f.dstDir, hookArchiveSpec{
			spec:            goolib.PkgSpec{Name: "childpkg", Arch: "noarch", Version: "1.5.0", PkgDependencies: map[string]string{"diskpkg": "1.0.0"}},
			installScript:   hookStubScript(logFile, "install", ""),
			uninstallScript: hookStubScript(logFile, "uninstall", ""),
		})

		ins := New(f.cacheDir, f.db, Options{})
		// Fresh FromDisk install (1.0.0).
		if err := ins.FromDisk(filepath.Join(localDir, v1.Source), false); err != nil {
			t.Fatalf("FromDisk(v1 fresh): %v", err)
		}
		// Upgrade FromDisk install (2.0.0).
		if err := ins.FromDisk(filepath.Join(localDir, v2.Source), false); err != nil {
			t.Fatalf("FromDisk(v2 upgrade): %v", err)
		}
		// Same-version FromDisk -reinstall (2.0.0).
		if err := ins.FromDisk(filepath.Join(localDir, v2.Source), true); err != nil {
			t.Fatalf("FromDisk(v2 same-version reinstall): %v", err)
		}
		// Older-file FromDisk -reinstall (1.0.0 over 2.0.0 -> downgrade).
		if err := ins.FromDisk(filepath.Join(localDir, v1.Source), true); err != nil {
			t.Fatalf("FromDisk(v1 older-file downgrade): %v", err)
		}

		// Direct install.Reinstall call.
		rowV1, err := f.db.FetchPkg(goolib.PackageInfo{Name: "diskpkg", Arch: "noarch"})
		if err != nil {
			t.Fatalf("FetchPkg(diskpkg): %v", err)
		}
		if err := Reinstall(t.Context(), rowV1, false, false, f.downloader, f.db); err != nil {
			t.Fatalf("Reinstall(diskpkg): %v", err)
		}

		// Install dependent childpkg via FromDisk before running CLI subcommands.
		if err := ins.FromDisk(filepath.Join(localDir, childPkg.Source), false); err != nil {
			t.Fatalf("FromDisk(childpkg): %v", err)
		}
		if err := f.db.Close(); err != nil {
			t.Fatalf("db.Close before CLI subprocesses: %v", err)
		}

		// Run `googet install -reinstall diskpkg`.
		cmdInstall := exec.Command(googetBin, "-root="+f.rootDir, "-noconfirm", "-system_log=false", "install", "-reinstall", "diskpkg")
		if out, err := cmdInstall.CombinedOutput(); err != nil {
			t.Fatalf("googet install -reinstall failed: %v\n%s", err, out)
		}

		// Tamper with diskpkg's installed payload so `googet verify -reinstall`
		// runs verify.sh (inheriting stale GOOGET_ACTION unchanged) and then
		// triggers install.Reinstall (stripping stale GOOGET_* and setting reinstall).
		if err := os.WriteFile(filepath.Join(f.dstDir, "diskpkg.noarch"), []byte("tampered"), 0644); err != nil {
			t.Fatal(err)
		}
		cmdVerify := exec.Command(googetBin, "-root="+f.rootDir, "-noconfirm", "-system_log=false", "verify", "-reinstall", "diskpkg")
		cmdVerify.Env = append(os.Environ(), "GOOGET_ACTION=stale_from_parent", "GOOGET_REPLACED_BY=stale_pkg")
		if out, err := cmdVerify.CombinedOutput(); err != nil {
			t.Fatalf("googet verify -reinstall failed: %v\n%s", err, out)
		}
		if diff := cmp.Diff([]string{"stale_from_parent|stale_pkg"}, readHookLines(t, verifyLog)); diff != "" {
			t.Errorf("verify.sh inherited env mismatch (-want +got):\n%s", diff)
		}

		// Run `googet remove diskpkg` (passing bare package name on CLI, cascading childpkg).
		cmdRemove := exec.Command(googetBin, "-root="+f.rootDir, "-noconfirm", "-system_log=false", "remove", "diskpkg")
		cmdRemove.Env = append(os.Environ(), "GOOGET_ACTION=replace", "GOOGET_REPLACED_BY=stale_pkg")
		if out, err := cmdRemove.CombinedOutput(); err != nil {
			t.Fatalf("googet remove diskpkg failed: %v\n%s", err, out)
		}

		want := []string{
			"install|install|diskpkg.noarch.1.0.0||<unset>",
			"install|upgrade|diskpkg.noarch.2.0.0|1.0.0|<unset>",
			"install|reinstall|diskpkg.noarch.2.0.0|2.0.0|<unset>",
			"install|downgrade|diskpkg.noarch.1.0.0|2.0.0|<unset>",
			"install|reinstall|diskpkg.noarch.1.0.0|1.0.0|<unset>",
			"install|install|childpkg.noarch.1.5.0||<unset>",
			"install|reinstall|diskpkg.noarch.1.0.0|1.0.0|<unset>",
			"install|reinstall|diskpkg.noarch.1.0.0|1.0.0|<unset>",
			"uninstall|remove|childpkg.noarch.1.5.0||<unset>",
			"uninstall|remove|diskpkg.noarch.1.0.0||<unset>",
		}
		if diff := cmp.Diff(want, readHookLines(t, logFile)); diff != "" {
			t.Errorf("hook log mismatch (-want +got):\n%s", diff)
		}
	})

	t.Run("cli_update_follow_replaces_dry_run_matches_live_hook_actions", func(t *testing.T) {
		logFile := filepath.Join(t.TempDir(), "hooks.log")
		installed := []hookArchiveSpec{
			{spec: goolib.PkgSpec{Name: "X", Arch: "noarch", Version: "1.0.0"}},
			{spec: goolib.PkgSpec{Name: "Y", Arch: "noarch", Version: "1.0.0"}},
			{spec: goolib.PkgSpec{Name: "Z", Arch: "noarch", Version: "1.0.0", PkgDependencies: map[string]string{"Y": "1.0.0"}}},
		}
		available := []hookArchiveSpec{
			{spec: goolib.PkgSpec{Name: "X", Arch: "noarch", Version: "1.0.0"}},
			{spec: goolib.PkgSpec{Name: "Y", Arch: "noarch", Version: "1.0.0"}},
			{spec: goolib.PkgSpec{Name: "Z", Arch: "noarch", Version: "1.0.0", PkgDependencies: map[string]string{"Y": "1.0.0"}}},
			{spec: goolib.PkgSpec{Name: "D", Arch: "noarch", Version: "1.0.0", Replaces: []string{"X"}}},
			{spec: goolib.PkgSpec{
				Name:            "W",
				Arch:            "noarch",
				Version:         "2.0.0",
				PkgDependencies: map[string]string{"D": "1.0.0"},
				Replaces:        []string{"Y"},
				Provides:        []string{"Y=0.5.0"},
			}},
		}
		f := newHookEnvFixture(t, logFile, installed, available)
		if err := f.db.Close(); err != nil {
			t.Fatalf("db.Close before CLI update: %v", err)
		}

		dryCmd := exec.Command(googetBin,
			"-root="+f.rootDir, "-noconfirm", "-system_log=false",
			"update", "-sources="+f.repoURL, "-follow_replaces", "-orphaned_dependents=remove", "-dry_run",
		)
		dryOutBytes, err := dryCmd.CombinedOutput()
		if err != nil {
			t.Fatalf("googet update -dry_run failed: %v\n%s", err, dryOutBytes)
		}
		dryOut := string(dryOutBytes)
		if got := readHookLines(t, logFile); len(got) != 0 {
			t.Fatalf("dry-run executed hooks %v, want none", got)
		}

		dryReplaced, dryOrphaned := parseUpdateDryRunRemovals(t, dryOut)

		liveCmd := exec.Command(googetBin,
			"-root="+f.rootDir, "-noconfirm", "-system_log=false",
			"update", "-sources="+f.repoURL, "-follow_replaces", "-orphaned_dependents=remove",
		)
		if liveOut, err := liveCmd.CombinedOutput(); err != nil {
			t.Fatalf("googet update live failed: %v\n%s", err, liveOut)
		}

		gotLines := readHookLines(t, logFile)
		wantLines := []string{
			"uninstall|replace|X.noarch.1.0.0||D.noarch.1.0.0",
			"install|install|D.noarch.1.0.0||<unset>",
			"uninstall|remove-dependent|Z.noarch.1.0.0||<unset>",
			"uninstall|replace|Y.noarch.1.0.0||W.noarch.2.0.0",
			"install|install|W.noarch.2.0.0||<unset>",
		}
		if diff := cmp.Diff(wantLines, gotLines); diff != "" {
			t.Errorf("live update hook log mismatch (-want +got):\n%s", diff)
		}

		liveReplaced := make(map[string]string)
		var liveOrphaned []string
		for _, line := range gotLines {
			parts := strings.Split(line, "|")
			if parts[0] != "uninstall" {
				continue
			}
			switch parts[1] {
			case "replace":
				liveReplaced[parts[2]] = parts[4]
			case "remove-dependent":
				liveOrphaned = append(liveOrphaned, parts[2])
			}
		}
		slices.Sort(liveOrphaned)

		if diff := cmp.Diff(dryReplaced, liveReplaced); diff != "" {
			t.Errorf("dry-run 'replaces' map mismatch against live replace hooks (-dry +live):\n%s", diff)
		}
		if diff := cmp.Diff(dryOrphaned, liveOrphaned); diff != "" {
			t.Errorf("dry-run 'removes orphaned' set mismatch against live remove-dependent hooks (-dry +live):\n%s", diff)
		}
	})
}

// parseUpdateDryRunRemovals extracts the victim->replacer map and sorted
// orphaned-dependent slice from `googet update -dry_run` stdout.
func parseUpdateDryRunRemovals(t *testing.T, stdout string) (map[string]string, []string) {
	t.Helper()
	lineRe := regexp.MustCompile(`--> ([^,\s]+), (\S+) from \S+ \(([^)]+)\)`)
	matches := lineRe.FindAllStringSubmatch(stdout, -1)
	if len(matches) == 0 {
		t.Fatalf("no replacement line found in dry-run output:\n%s", stdout)
	}
	replaced := make(map[string]string)
	var orphaned []string
	for _, m := range matches {
		winner := m[1] + "." + m[2]
		for _, clause := range strings.Split(m[3], "; ") {
			switch {
			case strings.HasPrefix(clause, "replaces "):
				for _, pkg := range strings.Split(strings.TrimPrefix(clause, "replaces "), ", ") {
					replaced[pkg] = winner
				}
			case strings.HasPrefix(clause, "removes orphaned "):
				for _, pkg := range strings.Split(strings.TrimPrefix(clause, "removes orphaned "), ", ") {
					orphaned = append(orphaned, pkg)
				}
			case strings.Contains(clause, " replaces "):
				dep, rest, _ := strings.Cut(clause, " replaces ")
				for _, pkg := range strings.Split(rest, ", ") {
					replaced[pkg] = dep
				}
			default:
				t.Fatalf("unexpected dry-run clause %q in %q", clause, stdout)
			}
		}
	}
	slices.Sort(orphaned)
	return replaced, orphaned
}

// TestEndToEndEnvironmentIsolationAndZeroContextInheritance sets mixed-case
// GOOGET_* variables and GooGetRoot in the test process and verifies that
// FromRepo and remove.All strip every inherited GOOGET_* variable while
// preserving GooGetRoot and all other environment entries, whereas system.Verify
// and zero-context system.Install inherit the parent environment unchanged.
func TestEndToEndEnvironmentIsolationAndZeroContextInheritance(t *testing.T) {
	t.Setenv("GOOGET_ACTION", "replace")
	t.Setenv("GOOGET_REPLACED_BY", "stale.noarch.9.9.9")
	t.Setenv("googet_foo", "leaked")
	t.Setenv("GOOGET_SOURCES", "https://internal.example/repo")
	t.Setenv("GOOGET_SHA256", "0123456789abcdef")
	t.Setenv("GooGetRoot", "/preserved/googet/root")

	dumpDir := t.TempDir()
	envDumpScript := func(outPath string) string {
		return fmt.Sprintf("#!/bin/sh\nset -eu\n/usr/bin/env > %q\n", outPath)
	}
	parseEnvDump := func(path string) map[string]string {
		t.Helper()
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("ReadFile(%s): %v", path, err)
		}
		out := make(map[string]string)
		for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
			if line == "" {
				continue
			}
			k, v, ok := strings.Cut(line, "=")
			if ok {
				out[k] = v
			}
		}
		return out
	}

	// 1. Direct zero-context system.Install and system.Verify inherit GOOGET_* unchanged.
	legacyDir := t.TempDir()
	legacyInstallDump := filepath.Join(dumpDir, "legacy_install.env")
	verifyDump := filepath.Join(dumpDir, "verify.env")
	if err := os.WriteFile(filepath.Join(legacyDir, "install.sh"), []byte(envDumpScript(legacyInstallDump)), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacyDir, "verify.sh"), []byte(envDumpScript(verifyDump)), 0755); err != nil {
		t.Fatal(err)
	}
	directSpec := &goolib.PkgSpec{
		Name:    "direct",
		Arch:    "noarch",
		Version: "1.0.0",
		Install: goolib.ExecFile{Path: "install.sh"},
		Verify:  goolib.ExecFile{Path: "verify.sh"},
	}
	if err := system.Install(legacyDir, directSpec); err != nil {
		t.Fatalf("system.Install(zero context): %v", err)
	}
	if err := system.Verify(legacyDir, directSpec); err != nil {
		t.Fatalf("system.Verify(): %v", err)
	}

	legacyEnv := parseEnvDump(legacyInstallDump)
	verifyEnv := parseEnvDump(verifyDump)
	for label, envMap := range map[string]map[string]string{
		"system.Install": legacyEnv,
		"system.Verify":  verifyEnv,
	} {
		if envMap["GOOGET_ACTION"] != "replace" || envMap["GOOGET_REPLACED_BY"] != "stale.noarch.9.9.9" ||
			envMap["googet_foo"] != "leaked" || envMap["GOOGET_SOURCES"] != "https://internal.example/repo" ||
			envMap["GOOGET_SHA256"] != "0123456789abcdef" || envMap["GooGetRoot"] != "/preserved/googet/root" {
			t.Errorf("%s did not preserve inherited environment: %+v", label, envMap)
		}
	}

	// 2. Real FromRepo install and remove.All strip all inherited GOOGET_* keys
	// case-insensitively, preserve GooGetRoot and non-GOOGET_* keys, and set only
	// fresh context variables.
	installDump := filepath.Join(dumpDir, "hook_install.env")
	removeDump := filepath.Join(dumpDir, "hook_remove.env")
	logFile := filepath.Join(dumpDir, "unused.log")
	f := newHookEnvFixture(t, logFile, nil, []hookArchiveSpec{
		{
			spec:            goolib.PkgSpec{Name: "isolated", Arch: "noarch", Version: "1.0.0"},
			installScript:   envDumpScript(installDump),
			uninstallScript: envDumpScript(removeDump),
		},
	})

	if err := New(f.cacheDir, f.db, Options{}).FromRepo(
		t.Context(), goolib.PackageInfo{Name: "isolated"}, "", f.rm, []string{"noarch"}, f.downloader,
	); err != nil {
		t.Fatalf("FromRepo(isolated): %v", err)
	}
	if err := remove.All(
		t.Context(), goolib.PackageInfo{Name: "isolated", Arch: "noarch"}, remove.DepMap{"isolated.noarch": nil}, false, f.downloader, f.db,
	); err != nil {
		t.Fatalf("remove.All(isolated): %v", err)
	}

	gotInstallEnv := parseEnvDump(installDump)
	gotRemoveEnv := parseEnvDump(removeDump)

	assertIsolatedEnv := func(label string, got map[string]string, wantGooget map[string]string) {
		t.Helper()
		gotGooget := make(map[string]string)
		for k, v := range got {
			if strings.HasPrefix(strings.ToUpper(k), "GOOGET_") {
				gotGooget[k] = v
			}
		}
		if diff := cmp.Diff(wantGooget, gotGooget); diff != "" {
			t.Errorf("%s GOOGET_* variables mismatch (-want +got):\n%s", label, diff)
		}
		if got["GooGetRoot"] != "/preserved/googet/root" {
			t.Errorf("%s GooGetRoot = %q, want %q", label, got["GooGetRoot"], "/preserved/googet/root")
		}
		// Every non-GOOGET_* variable other than shell-managed PWD/SHLVL/_ must
		// match the legacy child environment.
		for k, wantVal := range legacyEnv {
			if strings.HasPrefix(strings.ToUpper(k), "GOOGET_") || k == "PWD" || k == "SHLVL" || k == "_" {
				continue
			}
			if gotVal, ok := got[k]; !ok || gotVal != wantVal {
				t.Errorf("%s non-GOOGET_ variable %q = (%q, %t), want (%q, true)", label, k, gotVal, ok, wantVal)
			}
		}
	}

	assertIsolatedEnv("FromRepo install hook", gotInstallEnv, map[string]string{
		"GOOGET_ACTION":           "install",
		"GOOGET_PACKAGE":          "isolated.noarch.1.0.0",
		"GOOGET_PREVIOUS_VERSION": "",
	})
	assertIsolatedEnv("remove.All uninstall hook", gotRemoveEnv, map[string]string{
		"GOOGET_ACTION":           "remove",
		"GOOGET_PACKAGE":          "isolated.noarch.1.0.0",
		"GOOGET_PREVIOUS_VERSION": "",
	})
}
