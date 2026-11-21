/*
Copyright 2016 Google Inc. All Rights Reserved.
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

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/googet/v2/cli"
	"github.com/google/googet/v2/client"
	"github.com/google/googet/v2/googetdb"
	"github.com/google/googet/v2/goolib"
	"github.com/google/googet/v2/settings"
	"github.com/google/googet/v2/testutil"
	"github.com/google/logger"
	"github.com/google/subcommands"
)

// writeLogs creates each named file under dir with its name as content, so
// tests can tell after rotation which file ended up where. The live log gets
// live bytes of padding after its name.
func writeLogs(t *testing.T, dir string, live int64, names ...string) {
	t.Helper()
	for _, n := range names {
		content := []byte(n)
		if n == "googet.log" {
			content = append(content, make([]byte, live)...)
		}
		if err := os.WriteFile(filepath.Join(dir, n), content, 0644); err != nil {
			t.Fatalf("writing %s: %v", n, err)
		}
	}
}

// logState maps each file in dir to the name it was created with, as written
// by writeLogs.
func logState(t *testing.T, dir string) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}
	got := map[string]string{}
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("reading %s: %v", e.Name(), err)
		}
		got[e.Name()] = strings.TrimRight(string(b), "\x00")
	}
	return got
}

// logNames lists the names in dir.
func logNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// spyRotate wraps rotateRename and rotateRemove for the rest of the test. It
// records every call and fails the ones fail selects. fail gets the base
// names of the source and destination; dst is empty for removes.
func spyRotate(t *testing.T, fail func(op, src, dst string) bool) *[]string {
	t.Helper()
	oldRename, oldRemove := rotateRename, rotateRemove
	t.Cleanup(func() { rotateRename, rotateRemove = oldRename, oldRemove })
	var calls []string
	rotateRename = func(src, dst string) error {
		calls = append(calls, "rename "+filepath.Base(src)+" "+filepath.Base(dst))
		if fail != nil && fail("rename", filepath.Base(src), filepath.Base(dst)) {
			return &os.LinkError{Op: "rename", Old: src, New: dst, Err: errors.New("file is locked")}
		}
		return oldRename(src, dst)
	}
	rotateRemove = func(name string) error {
		calls = append(calls, "remove "+filepath.Base(name))
		if fail != nil && fail("remove", filepath.Base(name), "") {
			return &os.PathError{Op: "remove", Path: name, Err: errors.New("file is locked")}
		}
		return oldRemove(name)
	}
	return &calls
}

// lockedOnWindows returns a fail function for spyRotate that models a file
// locked on Windows: it can't be deleted, moved, or replaced by a rename.
func lockedOnWindows(locked string) func(op, src, dst string) bool {
	return func(op, src, dst string) bool { return src == locked || dst == locked }
}

func TestLogGeneration(t *testing.T) {
	for n, want := range map[int]string{0: "g.log.old", 1: "g.log.old", 2: "g.log.old.2", 5: "g.log.old.5"} {
		if got := logGeneration("g.log", n); got != want {
			t.Errorf("logGeneration(g.log, %d) = %q, want %q", n, got, want)
		}
	}
}

func TestRotateLog(t *testing.T) {
	const maxSize = 1024
	for _, tc := range []struct {
		desc  string
		live  int64
		files []string
		want  map[string]string // File name to original name.
	}{
		{
			desc:  "below the cap",
			live:  maxSize - 20,
			files: []string{"googet.log", "googet.log.old"},
			want:  map[string]string{"googet.log": "googet.log", "googet.log.old": "googet.log.old"},
		},
		{
			desc:  "no old logs",
			live:  maxSize,
			files: []string{"googet.log"},
			want:  map[string]string{"googet.log.old": "googet.log"},
		},
		{
			// A legacy .old written by an older googet becomes generation 2.
			desc:  "legacy old log only",
			live:  maxSize,
			files: []string{"googet.log", "googet.log.old"},
			want:  map[string]string{"googet.log.old": "googet.log", "googet.log.old.2": "googet.log.old"},
		},
		{
			desc:  "all generations present",
			live:  maxSize,
			files: []string{"googet.log", "googet.log.old", "googet.log.old.2", "googet.log.old.3"},
			want: map[string]string{
				"googet.log.old":   "googet.log",
				"googet.log.old.2": "googet.log.old",
				"googet.log.old.3": "googet.log.old.2",
			},
		},
		{
			// A missing generation is skipped without a warning.
			desc:  "missing middle generation",
			live:  maxSize,
			files: []string{"googet.log", "googet.log.old", "googet.log.old.3"},
			want:  map[string]string{"googet.log.old": "googet.log", "googet.log.old.2": "googet.log.old"},
		},
		{
			// Generations beyond keep, such as after keep is lowered, are
			// left alone rather than counted.
			desc:  "generation beyond keep",
			live:  maxSize,
			files: []string{"googet.log", "googet.log.old", "googet.log.old.4"},
			want: map[string]string{
				"googet.log.old":   "googet.log",
				"googet.log.old.2": "googet.log.old",
				"googet.log.old.4": "googet.log.old.4",
			},
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			calls := spyRotate(t, nil)
			dir := t.TempDir()
			writeLogs(t, dir, tc.live, tc.files...)
			if errs := rotateLog(filepath.Join(dir, "googet.log"), maxSize, 3); len(errs) != 0 {
				t.Errorf("rotateLog() = %v, want no warnings", errs)
			}
			if got := logState(t, dir); !maps.Equal(got, tc.want) {
				t.Errorf("after rotateLog() files = %v, want %v", got, tc.want)
			}
			if tc.live < maxSize && len(*calls) != 0 {
				t.Errorf("rotateLog() below the cap made calls %q, want none", *calls)
			}
		})
	}
}

// TestRotateLogBestEffort checks that a file that can't be moved or deleted
// produces a warning, never an error that stops googet.
func TestRotateLogBestEffort(t *testing.T) {
	const maxSize = 1024
	all := []string{"googet.log", "googet.log.old", "googet.log.old.2", "googet.log.old.3"}
	for _, tc := range []struct {
		desc      string
		fail      func(op, src, dst string) bool
		want      map[string]string
		wantWarns int
	}{
		{
			// Only renames from .old.2 fail, as on POSIX, where a locked file
			// can still be renamed over. .old moves onto .old.2 and replaces
			// it, and the live log still rotates.
			desc: "older generation can't be moved",
			fail: func(op, src, dst string) bool { return op == "rename" && src == "googet.log.old.2" },
			want: map[string]string{
				"googet.log.old":   "googet.log",
				"googet.log.old.2": "googet.log.old",
			},
			wantWarns: 1,
		},
		{
			// On Windows a locked .old.2 can't be moved or renamed over, so
			// .old stays in place and the live log isn't rotated. The oldest
			// generation has already been deleted by then.
			desc: "older generation locked on Windows",
			fail: lockedOnWindows("googet.log.old.2"),
			want: map[string]string{
				"googet.log":       "googet.log",
				"googet.log.old":   "googet.log.old",
				"googet.log.old.2": "googet.log.old.2",
			},
			wantWarns: 3,
		},
		{
			// On Windows a locked oldest generation can't be deleted or
			// renamed over, so .old.2 stays where it is and .old replaces it.
			// The locked file survives and the live log still rotates.
			desc: "oldest generation locked on Windows",
			fail: lockedOnWindows("googet.log.old.3"),
			want: map[string]string{
				"googet.log.old":   "googet.log",
				"googet.log.old.2": "googet.log.old",
				"googet.log.old.3": "googet.log.old.3",
			},
			wantWarns: 2,
		},
		{
			desc: "oldest generation can't be deleted",
			fail: func(op, src, dst string) bool { return op == "remove" && src == "googet.log.old.3" },
			want: map[string]string{
				"googet.log.old":   "googet.log",
				"googet.log.old.2": "googet.log.old",
				"googet.log.old.3": "googet.log.old.2",
			},
			wantWarns: 1,
		},
		{
			// .old is never overwritten, so the live log stays in place and
			// googet keeps appending to it.
			desc: "newest generation locked",
			fail: func(op, src, dst string) bool { return op == "rename" && src == "googet.log.old" },
			want: map[string]string{
				"googet.log":       "googet.log",
				"googet.log.old":   "googet.log.old",
				"googet.log.old.3": "googet.log.old.2",
			},
			wantWarns: 2,
		},
		{
			desc: "live log locked",
			fail: func(op, src, dst string) bool { return op == "rename" && src == "googet.log" },
			want: map[string]string{
				"googet.log":       "googet.log",
				"googet.log.old.2": "googet.log.old",
				"googet.log.old.3": "googet.log.old.2",
			},
			wantWarns: 1,
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			spyRotate(t, tc.fail)
			dir := t.TempDir()
			writeLogs(t, dir, maxSize, all...)
			errs := rotateLog(filepath.Join(dir, "googet.log"), maxSize, 3)
			if len(errs) != tc.wantWarns {
				t.Errorf("rotateLog() = %d warnings %v, want %d", len(errs), errs, tc.wantWarns)
			}
			if got := logState(t, dir); !maps.Equal(got, tc.want) {
				t.Errorf("after rotateLog() files = %v, want %v", got, tc.want)
			}
		})
	}

	t.Run("generation is a non-empty directory", func(t *testing.T) {
		dir := t.TempDir()
		writeLogs(t, dir, maxSize, "googet.log", "googet.log.old")
		blocker := filepath.Join(dir, "googet.log.old.3", "x")
		if err := os.MkdirAll(blocker, 0755); err != nil {
			t.Fatal(err)
		}
		errs := rotateLog(filepath.Join(dir, "googet.log"), maxSize, 3)
		if len(errs) == 0 {
			t.Error("rotateLog() = no warnings, want one for the undeletable generation")
		}
		got := logNames(t, dir)
		if !slices.Contains(got, "googet.log.old") || slices.Contains(got, "googet.log") {
			t.Errorf("after rotateLog() files = %v, want the live log rotated to googet.log.old", got)
		}
	})
}

func TestWantProgress(t *testing.T) {
	for _, tc := range []struct {
		desc                            string
		conf, flagSet, flagVal, verbose bool
		want                            bool
	}{
		{desc: "defaults", conf: true, want: true},
		{desc: "config off", conf: false, want: false},
		{desc: "flag off overrides config on", conf: true, flagSet: true, flagVal: false, want: false},
		{desc: "flag on overrides config off", conf: false, flagSet: true, flagVal: true, want: true},
		{desc: "unset flag value is ignored", conf: false, flagSet: false, flagVal: true, want: false},
		{desc: "verbose disables", conf: true, verbose: true, want: false},
		{desc: "verbose beats explicit flag on", conf: true, flagSet: true, flagVal: true, verbose: true, want: false},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			if got := wantProgress(tc.conf, tc.flagSet, tc.flagVal, tc.verbose); got != tc.want {
				t.Errorf("wantProgress(conf=%v, flagSet=%v, flagVal=%v, verbose=%v) = %v, want %v",
					tc.conf, tc.flagSet, tc.flagVal, tc.verbose, got, tc.want)
			}
		})
	}
}

const (
	// runArgsEnv, when set, makes the test binary run googet's run() with
	// the JSON-encoded arguments it holds instead of running tests. run()
	// registers flags on the global FlagSet, so each call needs a fresh
	// process.
	runArgsEnv = "GOOGET_TEST_RUN_ARGS"
	// spyDirEnv names the directory where the isAdmin and obtainLock spies
	// record that they were called.
	spyDirEnv = "GOOGET_TEST_SPY_DIR"
	// allowEnv, when set, makes the spies succeed instead of failing.
	allowEnv = "GOOGET_TEST_ALLOW_PRIVILEGE"
)

// TestMain serves all tests in package main. When runArgsEnv is set, the test
// binary acts as googet for runGooget instead of running the tests.
func TestMain(m *testing.M) {
	if args := os.Getenv(runArgsEnv); args != "" {
		os.Exit(runWithSpies(args))
	}
	os.Exit(m.Run())
}

// runWithSpies runs googet with isAdmin and obtainLock replaced by spies
// that record the call and fail, or succeed if allowEnv is set. obtainLock
// also fails if the googet root doesn't exist yet, because the root must be
// created, with its own mode, before the lock file.
func runWithSpies(encodedArgs string) int {
	var args []string
	if err := json.Unmarshal([]byte(encodedArgs), &args); err != nil {
		panic(err)
	}
	spyDir := os.Getenv(spyDirEnv)
	record := func(name string) {
		if err := os.WriteFile(filepath.Join(spyDir, name), nil, 0644); err != nil {
			panic(err)
		}
	}
	allow := os.Getenv(allowEnv) != ""
	isAdmin = func() error {
		record("isAdmin")
		if allow {
			return nil
		}
		return errors.New("spy: not an administrator")
	}
	obtainLock = func(lockFile string, _ time.Duration) (func(), error) {
		record("obtainLock")
		if root := filepath.Dir(lockFile); !isDir(root) {
			return nil, fmt.Errorf("googet root %q doesn't exist when taking the lock", root)
		}
		if allow {
			return func() {}, nil
		}
		return nil, errors.New("spy: lock refused")
	}
	os.Args = append([]string{"googet"}, args...)
	return run(context.Background())
}

type runResult struct {
	code           int
	stdout, stderr string
	spies          map[string]bool // Spies that were called.
}

// output returns everything the child wrote, stdout first.
func (r runResult) output() string {
	return r.stdout + r.stderr
}

// runGooget runs googet with args in a subprocess, with isAdmin and
// obtainLock replaced by spies that succeed if allow is set and fail
// otherwise.
func runGooget(t *testing.T, allow bool, args ...string) runResult {
	t.Helper()
	enc, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	spyDir := t.TempDir()
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), runArgsEnv+"="+string(enc), spyDirEnv+"="+spyDir, envVar+"=")
	// The race detector otherwise sleeps for a second before each exit.
	cmd.Env = append(cmd.Env, "GORACE="+strings.TrimSpace(os.Getenv("GORACE")+" atexit_sleep_ms=0"))
	if allow {
		cmd.Env = append(cmd.Env, allowEnv+"=1")
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err = cmd.Run()
	res := runResult{stdout: stdout.String(), stderr: stderr.String(), spies: map[string]bool{}}
	var exitErr *exec.ExitError
	switch {
	case errors.As(err, &exitErr):
		res.code = exitErr.ExitCode()
	case err != nil:
		t.Fatalf("running googet %q: %v", args, err)
	}
	for _, s := range []string{"isAdmin", "obtainLock"} {
		if _, err := os.Stat(filepath.Join(spyDir, s)); err == nil {
			res.spies[s] = true
		}
	}
	return res
}

// fixtureRoot returns a googet root whose database has package A and package
// B, which depends on A.
func fixtureRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	db, err := googetdb.NewDB(filepath.Join(root, "googet.db"))
	if err != nil {
		t.Fatalf("googetdb.NewDB: %v", err)
	}
	state := client.GooGetState{
		{PackageSpec: &goolib.PkgSpec{Name: "A", Arch: "noarch", Version: "1.0.0@1"}},
		{PackageSpec: &goolib.PkgSpec{Name: "B", Arch: "noarch", Version: "1.0.0@1", PkgDependencies: map[string]string{"A": "1.0.0@1"}}},
	}
	if err := db.WriteStateToDB(state); err != nil {
		db.Close()
		t.Fatalf("db.WriteStateToDB: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("db.Close: %v", err)
	}
	// Backdate the database so that any later write moves its mtime forward,
	// whatever the filesystem's timestamp granularity.
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(filepath.Join(root, "googet.db"), old, old); err != nil {
		t.Fatalf("os.Chtimes: %v", err)
	}
	return root
}

// rootSnapshot records the googet root's file names and the database's
// bytes and modification time.
type rootSnapshot struct {
	names   []string
	db      []byte
	dbMtime time.Time
}

func snapshotRoot(t *testing.T, root string) rootSnapshot {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	var s rootSnapshot
	for _, e := range entries {
		s.names = append(s.names, e.Name())
	}
	dbFile := filepath.Join(root, "googet.db")
	if s.db, err = os.ReadFile(dbFile); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(dbFile)
	if err != nil {
		t.Fatal(err)
	}
	s.dbMtime = fi.ModTime()
	return s
}

// installed reports whether package name is in the database under root.
func installed(t *testing.T, root, name string) bool {
	t.Helper()
	db, err := googetdb.NewDB(filepath.Join(root, "googet.db"))
	if err != nil {
		t.Fatalf("googetdb.NewDB: %v", err)
	}
	defer db.Close()
	ps, err := db.FetchPkg(goolib.PackageInfo{Name: name, Arch: "noarch"})
	if err != nil {
		t.Fatalf("db.FetchPkg: %v", err)
	}
	return ps.PackageSpec != nil
}

// TestRunRemoveDryRunUnprivileged checks that remove -dry_run runs without
// the lock when the admin check fails, and leaves the googet root untouched.
func TestRunRemoveDryRunUnprivileged(t *testing.T) {
	root := fixtureRoot(t)
	before := snapshotRoot(t, root)

	res := runGooget(t, false, "-root", root, "remove", "-dry_run", "A")

	if res.code != 0 {
		t.Errorf("googet remove -dry_run A exited %d, want 0; stderr:\n%s", res.code, res.stderr)
	}
	if !strings.Contains(res.stdout, "B.noarch") || !strings.Contains(res.stdout, "Dry run") {
		t.Errorf("googet remove -dry_run A printed %q, want the dry-run list naming B", res.stdout)
	}
	if !res.spies["isAdmin"] || res.spies["obtainLock"] {
		t.Errorf("spies called: %v, want only isAdmin", res.spies)
	}
	after := snapshotRoot(t, root)
	if !slices.Equal(after.names, before.names) {
		t.Errorf("root files changed from %q to %q; want no log, lock, journal, cache or repo files", before.names, after.names)
	}
	if !bytes.Equal(after.db, before.db) || !after.dbMtime.Equal(before.dbMtime) {
		t.Error("googet.db changed, want its bytes and mtime unchanged")
	}
}

// TestRunRemoveUnprivileged checks that a remove that isn't a dry run still
// goes through the admin check and stops when it fails.
func TestRunRemoveUnprivileged(t *testing.T) {
	root := fixtureRoot(t)

	res := runGooget(t, false, "-root", root, "remove", "A")

	if res.code != 1 {
		t.Errorf("googet remove A exited %d, want 1; stderr:\n%s", res.code, res.stderr)
	}
	if !res.spies["isAdmin"] {
		t.Error("isAdmin was not called, want the admin check")
	}
	if res.spies["obtainLock"] {
		t.Error("obtainLock was called after a failed admin check")
	}
	if !installed(t, root, "A") {
		t.Error("package A was removed, want it still installed")
	}
}

// TestRunRemovePrivileged checks that run() marks the context as privileged
// once the admin check and the lock succeed, so remove still removes.
func TestRunRemovePrivileged(t *testing.T) {
	root := fixtureRoot(t)

	res := runGooget(t, true, "-root", root, "-noconfirm", "-system_log=false", "remove", "-db_only", "B")

	if res.code != 0 {
		t.Errorf("googet remove -db_only B with privileges exited %d, want 0; stderr:\n%s", res.code, res.stderr)
	}
	if !res.spies["isAdmin"] || !res.spies["obtainLock"] {
		t.Errorf("spies called: %v, want isAdmin and obtainLock", res.spies)
	}
	if installed(t, root, "B") {
		t.Error("package B is still installed, want it removed")
	}
}

func TestRunUnelevatedCreatesNoRoot(t *testing.T) {
	for _, tc := range []struct {
		desc string
		args []string
	}{
		{desc: "listrepos", args: []string{"listrepos"}},
		{desc: "installed", args: []string{"installed"}},
		{desc: "remove dry run", args: []string{"remove", "-dry_run", "foo"}},
		{desc: "install", args: []string{"install", "foo"}},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "googet")
			args := append([]string{"-root", root, "-system_log=false", "-noconfirm"}, tc.args...)
			res := runGooget(t, false, args...)
			out := res.stdout + res.stderr
			if res.code != 1 {
				t.Errorf("unelevated googet %q exit code = %d, want 1; output:\n%s", tc.args, res.code, out)
			}
			if _, err := os.Lstat(root); !os.IsNotExist(err) {
				t.Errorf("unelevated googet %q left the root %q behind (Lstat error: %v); output:\n%s", tc.args, root, err, out)
			}
		})
	}
}

func TestRunUnelevatedWithEmptyRoot(t *testing.T) {
	for _, tc := range []struct {
		desc     string
		args     []string
		wantCode int
	}{
		// Unelevated runs need googet.db, which an unelevated run must not
		// create, so both commands take the privileged path and fail the admin
		// check.
		{desc: "installed", args: []string{"installed"}, wantCode: 1},
		{desc: "listrepos", args: []string{"listrepos"}, wantCode: 1},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			root := t.TempDir()
			args := append([]string{"-root", root, "-system_log=false"}, tc.args...)
			res := runGooget(t, false, args...)
			if res.code != tc.wantCode {
				t.Errorf("unelevated googet %q with an empty root exit code = %d, want %d; output:\n%s", tc.args, res.code, tc.wantCode, res.output())
			}
			entries, err := os.ReadDir(root)
			if err != nil {
				t.Fatalf("reading root: %v", err)
			}
			if len(entries) != 0 {
				t.Errorf("unelevated googet %q created %v in the root, want nothing", tc.args, entries)
			}
		})
	}
}

func TestRunElevatedCreatesRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "googet")
	res := runGooget(t, true, "-root", root, "-system_log=false", "-noconfirm", "listrepos")
	if res.code != 0 {
		t.Fatalf("elevated googet listrepos exit code = %d, want 0; output:\n%s", res.code, res.stdout+res.stderr)
	}
	for _, p := range []struct {
		path string
		dir  bool
	}{
		{root, true},
		{filepath.Join(root, "cache"), true},
		{filepath.Join(root, "repos"), true},
		{filepath.Join(root, "googet.db"), false},
	} {
		fi, err := os.Stat(p.path)
		if err != nil {
			t.Errorf("elevated googet listrepos didn't create %q: %v", p.path, err)
			continue
		}
		if fi.IsDir() != p.dir {
			t.Errorf("%q IsDir() = %v, want %v", p.path, fi.IsDir(), p.dir)
		}
	}
}

func TestRunWithoutRoot(t *testing.T) {
	for _, tc := range []struct {
		desc    string
		args    []string
		wantOut string
	}{
		{desc: "version", args: []string{"-version"}, wantOut: "GooGet version:"},
		{desc: "help", args: []string{"help"}, wantOut: "Subcommands"},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "googet")
			for _, args := range [][]string{tc.args, append([]string{"-root", root}, tc.args...)} {
				res := runGooget(t, false, args...)
				out := res.stdout + res.stderr
				if res.code != 0 {
					t.Errorf("googet %q exit code = %d, want 0; output:\n%s", args, res.code, out)
				}
				if !strings.Contains(out, tc.wantOut) {
					t.Errorf("googet %q output = %q, want it to contain %q", args, out, tc.wantOut)
				}
			}
			if _, err := os.Lstat(root); !os.IsNotExist(err) {
				t.Errorf("googet %q created the root %q (Lstat error: %v)", tc.args, root, err)
			}
		})
	}
}

// checkArgsHelperEnv, when set, makes TestRunRejectsMisplacedFlagsHelper run
// googet's run() with the JSON-encoded arguments it holds.
const checkArgsHelperEnv = "GOOGET_TEST_CHECKARGS_RUN"

// TestRunRejectsMisplacedFlagsHelper isn't a test: it runs run() in a child
// process started by TestRunRejectsMisplacedFlags.
func TestRunRejectsMisplacedFlagsHelper(t *testing.T) {
	enc := os.Getenv(checkArgsHelperEnv)
	if enc == "" {
		return
	}
	var args []string
	if err := json.Unmarshal([]byte(enc), &args); err != nil {
		panic(err)
	}
	os.Args = append([]string{"googet"}, args...)
	os.Exit(run(context.Background()))
}

// TestRunRejectsMisplacedFlags checks that a flag after a package name, or a
// positional argument for a command that takes none, is a usage error that
// run() reports before it creates the root or runs the command.
func TestRunRejectsMisplacedFlags(t *testing.T) {
	for _, tc := range []struct {
		args    []string
		wantOut string
	}{
		{args: []string{"install", "pkg", "-dry_run"}, wantOut: `"-dry_run" after "pkg"`},
		{args: []string{"install", "-dry_run", "pkg", "-reinstall"}, wantOut: `"-reinstall" after "pkg"`},
		{args: []string{"remove", "pkg", "-dry_run"}, wantOut: `"-dry_run" after "pkg"`},
		{args: []string{"update", "foo", "-dry_run"}, wantOut: `update takes no arguments, got "foo"`},
		{args: []string{"clean", "foo", "-all"}, wantOut: `clean takes no arguments, got "foo"`},
		{args: []string{"check", "foo"}, wantOut: `check takes no arguments, got "foo"`},
		{args: []string{"listrepos", "x"}, wantOut: `listrepos takes no arguments, got "x"`},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "googet")
			args := append([]string{"-root", root, "-noconfirm", "-system_log=false"}, tc.args...)
			enc, err := json.Marshal(args)
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(os.Args[0], "-test.run=^TestRunRejectsMisplacedFlagsHelper$")
			// Drop the root variable so the child can never fall back to a real
			// root; Windows matches variable names case-insensitively.
			for _, kv := range os.Environ() {
				if !strings.HasPrefix(strings.ToUpper(kv), strings.ToUpper(envVar)+"=") {
					cmd.Env = append(cmd.Env, kv)
				}
			}
			// atexit_sleep_ms=0 stops a -race child from sleeping 1s on exit.
			cmd.Env = append(cmd.Env, checkArgsHelperEnv+"="+string(enc), "GORACE="+strings.TrimSpace(os.Getenv("GORACE")+" atexit_sleep_ms=0"))
			out, err := cmd.CombinedOutput()
			code := 0
			var exitErr *exec.ExitError
			switch {
			case errors.As(err, &exitErr):
				code = exitErr.ExitCode()
			case err != nil:
				t.Fatalf("running googet %q: %v", tc.args, err)
			}
			if code != 2 {
				t.Errorf("googet %q exit code = %d, want 2; output:\n%s", tc.args, code, out)
			}
			if !strings.Contains(string(out), tc.wantOut) {
				t.Errorf("googet %q output = %q, want it to contain %q", tc.args, out, tc.wantOut)
			}
			if _, err := os.Lstat(root); !os.IsNotExist(err) {
				t.Errorf("googet %q created the root %q (Lstat error: %v), want it untouched", tc.args, root, err)
			}
		})
	}
}

// TestClassifyCommands pins each registered command's unelevated policy:
// which of its flags make cli.Classify return UnelevatedRun. How Classify
// parses flags is tested in package cli.
func TestClassifyCommands(t *testing.T) {
	for _, tc := range []struct {
		args string
		want cli.Class
	}{
		{"installed", cli.UnelevatedRun},
		{"installed -info A", cli.UnelevatedRun},
		{"listrepos", cli.UnelevatedRun},
		{"remove -dry_run pkg", cli.UnelevatedRun},
		{"remove pkg", cli.Privileged},
		{"available", cli.UnelevatedRun},
		{"available -info pkg", cli.UnelevatedRun},
		{"latest pkg", cli.UnelevatedRun},
		{"latest -compare -json pkg", cli.UnelevatedRun},
		{"update -dry_run", cli.UnelevatedRun},
		{"update", cli.Privileged},
		{"install -dry_run pkg", cli.UnelevatedRun},
		{"install -dry_run -db_only pkg", cli.UnelevatedRun},
		{"install pkg", cli.Privileged},
		{"install -dry_run -reinstall pkg", cli.Privileged},
		{"install -reinstall -dry_run pkg", cli.Privileged},
		{"install -dry_run -redownload pkg", cli.Privileged},
		{"install -dry_run -reinstall=false pkg", cli.UnelevatedRun},
		{"download pkg", cli.Privileged},
		{"download -download_dir x pkg", cli.Privileged},
		{"check", cli.Privileged},
		{"check -dry_run", cli.Privileged},
		{"verify pkg", cli.Privileged},
		{"verify -skip_files pkg", cli.Privileged},
		{"clean", cli.Privileged},
		{"clean -all", cli.Privileged},
		{"addrepo -dry_run name https://example.com/repo", cli.UnelevatedRun},
		{"addrepo -dry_run -priority 1500 -file x.repo name https://example.com/repo", cli.UnelevatedRun},
		{"addrepo name https://example.com/repo", cli.Privileged},
		{"rmrepo -dry_run name", cli.UnelevatedRun},
		{"rmrepo name", cli.Privileged},
	} {
		t.Run(tc.args, func(t *testing.T) {
			if got, _, _ := cli.Classify(subcommands.DefaultCommander, strings.Fields(tc.args)); got != tc.want {
				t.Errorf("Classify(%q) = %v, want %v", tc.args, got, tc.want)
			}
		})
	}
}

// TestAllowUnelevatedAllowlist pins the sets of registered commands that
// implement cli.AllowUnelevated and cli.LockFree, so adding one is a visible,
// reviewed change.
func TestAllowUnelevatedAllowlist(t *testing.T) {
	wantAllow := []string{"addrepo", "available", "install", "installed", "latest", "listrepos", "remove", "rmrepo", "update"}
	wantLockFree := []string{"installed", "listrepos"}
	var gotAllow, gotLockFree []string
	subcommands.DefaultCommander.VisitCommands(func(_ *subcommands.CommandGroup, c subcommands.Command) {
		if _, ok := c.(cli.AllowUnelevated); ok {
			gotAllow = append(gotAllow, c.Name())
		}
		if _, ok := c.(cli.LockFree); ok {
			gotLockFree = append(gotLockFree, c.Name())
		}
	})
	slices.Sort(gotAllow)
	slices.Sort(gotLockFree)
	if !slices.Equal(gotAllow, wantAllow) {
		t.Errorf("commands implementing cli.AllowUnelevated = %q, want %q", gotAllow, wantAllow)
	}
	if !slices.Equal(gotLockFree, wantLockFree) {
		t.Errorf("commands implementing cli.LockFree = %q, want %q", gotLockFree, wantLockFree)
	}
}

// TestRunUsageOnly checks that runs that can only print usage or a parse
// error exit with a usage error before the root check, the admin check and
// the lock, with or without a googet root.
func TestRunUsageOnly(t *testing.T) {
	for _, tc := range [][]string{
		{"install", "-h"},
		{"install", "-help"},
		{"install", "-bogus"},
		{"remove", "-dry_run=maybe", "A"},
		{"nosuchcmd"},
		{"REMOVE", "-dry_run", "A"},
	} {
		t.Run(strings.Join(tc, " "), func(t *testing.T) {
			check := func(t *testing.T, res runResult) {
				t.Helper()
				if res.code != int(subcommands.ExitUsageError) {
					t.Errorf("googet %q exit code = %d, want %d; output:\n%s", tc, res.code, subcommands.ExitUsageError, res.output())
				}
				if len(res.spies) != 0 {
					t.Errorf("googet %q called spies %v, want none", tc, res.spies)
				}
				if res.stderr == "" {
					t.Errorf("googet %q printed nothing on stderr, want usage or a parse error", tc)
				}
			}
			t.Run("no root", func(t *testing.T) {
				check(t, runGooget(t, false, tc...))
			})
			t.Run("missing root", func(t *testing.T) {
				root := filepath.Join(t.TempDir(), "googet")
				check(t, runGooget(t, false, append([]string{"-root", root}, tc...)...))
				if _, err := os.Lstat(root); !os.IsNotExist(err) {
					t.Errorf("googet %q created the root %q (Lstat error: %v)", tc, root, err)
				}
			})
			t.Run("existing root", func(t *testing.T) {
				root := fixtureRoot(t)
				before := snapshotRoot(t, root)
				check(t, runGooget(t, false, append([]string{"-root", root}, tc...)...))
				after := snapshotRoot(t, root)
				if !slices.Equal(after.names, before.names) {
					t.Errorf("root files changed from %q to %q", before.names, after.names)
				}
				if !bytes.Equal(after.db, before.db) || !after.dbMtime.Equal(before.dbMtime) {
					t.Error("googet.db changed, want its bytes and mtime unchanged")
				}
			})
		})
	}
}

// TestRunElevatedAllowUnelevated checks elevated runs of commands that may
// run unelevated, against a root with a stale repo index cache. A
// cli.LockFree command still runs without the admin check, the lock and
// googet.log. Any other command takes the privileged path: the admin check
// passes, it takes the lock, writes googet.log and, if it reads the index,
// refreshes the stale cache.
func TestRunElevatedAllowUnelevated(t *testing.T) {
	for _, tc := range []struct {
		args        []string
		wantOut     string
		privileged  bool
		wantRefresh bool
	}{
		{args: []string{"remove", "-dry_run", "A"}, wantOut: "Dry run", privileged: true},
		{args: []string{"latest", "A"}, wantOut: "2.0.0@1", privileged: true, wantRefresh: true},
		{args: []string{"update", "-dry_run"}, wantOut: "Dry run", privileged: true, wantRefresh: true},
		{args: []string{"installed"}, wantOut: "A.noarch"},
		{args: []string{"listrepos"}, wantOut: "test"},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			root := readOnlyRoot(t)
			res := runGooget(t, true, append([]string{"-root", root, "-noconfirm", "-system_log=false"}, tc.args...)...)
			if res.code != 0 {
				t.Errorf("elevated googet %q exit code = %d, want 0; output:\n%s", tc.args, res.code, res.output())
			}
			if !strings.Contains(res.stdout, tc.wantOut) {
				t.Errorf("elevated googet %q printed %q, want it to contain %q", tc.args, res.stdout, tc.wantOut)
			}
			wantSpies := map[string]bool{}
			if tc.privileged {
				wantSpies = map[string]bool{"isAdmin": true, "obtainLock": true}
			}
			if !maps.Equal(res.spies, wantSpies) {
				t.Errorf("elevated googet %q called spies %v, want %v", tc.args, res.spies, wantSpies)
			}
			_, err := os.Stat(filepath.Join(root, "googet.log"))
			if gotLog := err == nil; gotLog != tc.privileged {
				t.Errorf("elevated googet %q wrote googet.log: %v (Stat error: %v), want %v", tc.args, gotLog, err, tc.privileged)
			}
			caches, err := filepath.Glob(filepath.Join(root, "cache", "*.rs"))
			if err != nil || len(caches) != 1 {
				t.Fatalf("cache files = %q (error %v), want one", caches, err)
			}
			b, err := os.ReadFile(caches[0])
			if err != nil {
				t.Fatal(err)
			}
			if refreshed := string(b) != "[]"; refreshed != tc.wantRefresh {
				t.Errorf("elevated googet %q left the cache holding %q; refreshed: %v, want %v", tc.args, b, refreshed, tc.wantRefresh)
			}
		})
	}
}

// logCapture collects what the logger writes, for in-process tests.
type logCapture struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (c *logCapture) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.Write(p)
}

// take returns what was logged since the last call and clears it.
func (c *logCapture) take() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.buf.String()
	c.buf.Reset()
	return s
}

var (
	capturedLogsOnce sync.Once
	capturedLogs     = &logCapture{}
)

// captureLogs makes the default logger write to capturedLogs. The logger
// keeps the first Init, so this works only if nothing in this test process
// initialized it before.
func captureLogs() *logCapture {
	capturedLogsOnce.Do(func() { logger.Init("GooGet", false, false, capturedLogs) })
	capturedLogs.take()
	return capturedLogs
}

// registeredCommand returns the command registered under name, as
// subcommands.Commander.Execute finds it.
func registeredCommand(t *testing.T, name string) subcommands.Command {
	t.Helper()
	var cmd subcommands.Command
	subcommands.DefaultCommander.VisitCommands(func(_ *subcommands.CommandGroup, c subcommands.Command) {
		if cmd == nil && c.Name() == name {
			cmd = c
		}
	})
	if cmd == nil {
		t.Fatalf("command %q isn't registered", name)
	}
	return cmd
}

// captureOutput runs f with os.Stdout and os.Stderr redirected to a
// temporary file, and returns what f wrote to them.
func captureOutput(t *testing.T, f func()) string {
	t.Helper()
	tmp, err := os.CreateTemp(t.TempDir(), "output")
	if err != nil {
		t.Fatal(err)
	}
	defer tmp.Close()
	oldOut, oldErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = tmp, tmp
	f()
	os.Stdout, os.Stderr = oldOut, oldErr
	b, err := os.ReadFile(tmp.Name())
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestExecuteRefusesUnelevatedFirst runs registered commands' Execute
// without the privileged marker, with flags that their AllowUnelevated
// doesn't allow or, for a command that doesn't implement it, any flags,
// against a root that has no database, cache or repos. Each must print
// exactly one line, its refusal, before it opens the database, builds
// sources or fetches anything, and create nothing.
func TestExecuteRefusesUnelevatedFirst(t *testing.T) {
	logs := captureLogs()
	for _, args := range [][]string{
		{"install", "-dry_run", "-reinstall", "pkg"},
		{"install", "-dry_run", "-redownload", "-reinstall", "pkg"},
		{"install", "pkg"},
		{"update"},
		{"update", "-dry_run=false"},
		{"download", "pkg"},
		{"remove", "pkg"},
		{"check"},
		{"clean", "-all"},
		{"verify", "pkg"},
		{"addrepo", "name", "https://example.com/repo"},
		{"rmrepo", "name"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			root := t.TempDir()
			if err := settings.Initialize(root, false); err != nil {
				t.Fatalf("settings.Initialize: %v", err)
			}
			cmd := registeredCommand(t, args[0])
			fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
			cmd.SetFlags(fs)
			if err := fs.Parse(args[1:]); err != nil {
				t.Fatalf("parsing %q: %v", args, err)
			}
			var got subcommands.ExitStatus
			printed := captureOutput(t, func() { got = cmd.Execute(context.Background(), fs) })
			if got != subcommands.ExitFailure {
				t.Errorf("unprivileged Execute(%q) = %v, want %v", args, got, subcommands.ExitFailure)
			}
			// The logger writes to logs and to the stderr it saw at Init,
			// which captureOutput doesn't capture, so nothing is counted
			// twice.
			out := strings.TrimSpace(logs.take() + printed)
			if lines := strings.Split(out, "\n"); len(lines) != 1 || !strings.Contains(out, "requires administrator privileges") {
				t.Errorf("unprivileged Execute(%q) printed %q, want exactly one line, the refusal", args, out)
			}
			if entries, err := os.ReadDir(root); err != nil || len(entries) != 0 {
				t.Errorf("unprivileged Execute(%q) left %v in the root (error %v), want nothing", args, entries, err)
			}
		})
	}
}

// readOnlyRoot returns fixtureRoot's root with a googet.conf that allows HTTP
// repos, a repos folder naming a repo served by a test server, and a stale
// cache entry for that repo. The repo
// has A 2.0.0@1, an update for the installed A, and C 1.0.0@1. Everything
// under the root is backdated, so any write moves an mtime.
func readOnlyRoot(t *testing.T) string {
	t.Helper()
	specs := []goolib.RepoSpec{
		{Source: "A.noarch.2.0.0@1.goo", PackageSpec: &goolib.PkgSpec{Name: "A", Arch: "noarch", Version: "2.0.0@1"}},
		{Source: "C.noarch.1.0.0@1.goo", PackageSpec: &goolib.PkgSpec{Name: "C", Arch: "noarch", Version: "1.0.0@1"}},
	}
	j, err := json.Marshal(specs)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/index" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(j)
	}))
	t.Cleanup(srv.Close)

	root := fixtureRoot(t)
	for _, d := range []string{"repos", "cache"} {
		if err := os.Mkdir(filepath.Join(root, d), 0774); err != nil {
			t.Fatal(err)
		}
	}
	cacheBase := filepath.Join("cache", fmt.Sprintf("%x", sha256.Sum256([]byte(srv.URL))))
	files := map[string]string{
		"googet.conf":                       "allowunsafeurl: true\n",
		filepath.Join("repos", "test.repo"): fmt.Sprintf("- name: test\n  url: %s\n", srv.URL),
		cacheBase + ".rs":                   "[]",
		cacheBase + ".url":                  srv.URL,
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0664); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-48 * time.Hour)
	for _, p := range append(slices.Collect(maps.Keys(files)), "repos", "cache", ".") {
		if err := os.Chtimes(filepath.Join(root, p), old, old); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// TestRunUnelevatedReadsLeaveRootUnchanged runs the read-only commands
// unelevated, with failing admin and lock spies, against a root with a stale
// repo index cache. Each must succeed without taking the lock, write no
// googet.log and leave every name, byte and mtime under the root unchanged.
// Only the cli.LockFree commands skip the admin check too.
func TestRunUnelevatedReadsLeaveRootUnchanged(t *testing.T) {
	for _, tc := range []struct {
		args     []string
		wantOut  string
		lockFree bool
	}{
		{args: []string{"available"}, wantOut: "C.noarch"},
		{args: []string{"latest", "A"}, wantOut: "2.0.0@1"},
		{args: []string{"latest", "-compare", "A"}, wantOut: "2.0.0@1"},
		{args: []string{"update", "-dry_run"}, wantOut: "Dry run"},
		{args: []string{"install", "-dry_run", "C"}, wantOut: "C.noarch"},
		{args: []string{"installed"}, wantOut: "A.noarch", lockFree: true},
		{args: []string{"listrepos"}, wantOut: "test", lockFree: true},
		{args: []string{"addrepo", "-dry_run", "new", "https://example.com/new"}, wantOut: "Dry run: Would add repo"},
		{args: []string{"rmrepo", "-dry_run", "test"}, wantOut: "Dry run: Would remove repo"},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			root := readOnlyRoot(t)
			before := testutil.TreeSnapshot(t, root)
			res := runGooget(t, false, append([]string{"-root", root, "-noconfirm"}, tc.args...)...)
			if res.code != 0 {
				t.Errorf("unelevated googet %q exit code = %d, want 0; output:\n%s", tc.args, res.code, res.output())
			}
			if !strings.Contains(res.stdout, tc.wantOut) {
				t.Errorf("unelevated googet %q printed %q, want it to contain %q", tc.args, res.stdout, tc.wantOut)
			}
			wantAdminCheck := !tc.lockFree
			if res.spies["isAdmin"] != wantAdminCheck || res.spies["obtainLock"] {
				t.Errorf("unelevated googet %q called spies %v, want isAdmin: %v and no lock", tc.args, res.spies, wantAdminCheck)
			}
			if diff := cmp.Diff(before, testutil.TreeSnapshot(t, root)); diff != "" {
				t.Errorf("unelevated googet %q changed the root (-before +after):\n%s", tc.args, diff)
			}
		})
	}
}

// TestRunUnelevatedWritesStayPrivileged checks that commands and flags that
// write or run package code still go through the admin check unelevated, and
// that the failed check leaves the root unchanged.
func TestRunUnelevatedWritesStayPrivileged(t *testing.T) {
	for _, args := range [][]string{
		{"update"},
		{"install", "C"},
		{"install", "-dry_run", "-reinstall", "A"},
		{"install", "-dry_run", "-redownload", "-reinstall", "A"},
		{"download", "C"},
		{"check"},
		{"check", "-dry_run"},
		{"verify", "A"},
		{"clean"},
		{"addrepo", "new", "https://example.com/new"},
		{"rmrepo", "test"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			root := readOnlyRoot(t)
			before := testutil.TreeSnapshot(t, root)
			res := runGooget(t, false, append([]string{"-root", root, "-noconfirm"}, args...)...)
			if res.code != 1 {
				t.Errorf("unelevated googet %q exit code = %d, want 1; output:\n%s", args, res.code, res.output())
			}
			if !res.spies["isAdmin"] || res.spies["obtainLock"] {
				t.Errorf("unelevated googet %q called spies %v, want only isAdmin", args, res.spies)
			}
			if diff := cmp.Diff(before, testutil.TreeSnapshot(t, root)); diff != "" {
				t.Errorf("unelevated googet %q changed the root (-before +after):\n%s", args, diff)
			}
		})
	}
}
