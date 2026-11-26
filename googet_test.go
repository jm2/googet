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
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/googet/v2/cli"
	"github.com/google/googet/v2/client"
	"github.com/google/googet/v2/googetdb"
	"github.com/google/googet/v2/goolib"
	"github.com/google/googet/v2/repo"
	"github.com/google/googet/v2/settings"
	"github.com/google/googet/v2/system"
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

// Environment variables that runGooget sets for the child process.
const (
	// runChildEnv, when set to "1", makes the test binary run googet's run()
	// with its arguments instead of the tests. run() registers flags on
	// flag.CommandLine and commands on subcommands.DefaultCommander, so each
	// run needs a fresh process.
	runChildEnv = "GOOGET_TEST_RUN_CHILD"
	// childAdminEnv, when set to "1", makes the child's admin check pass.
	// Otherwise the child behaves as an unelevated process.
	childAdminEnv = "GOOGET_TEST_CHILD_ADMIN"
	// childTraceEnv is a file path where the child appends one line per step
	// of run() that it calls.
	childTraceEnv = "GOOGET_TEST_CHILD_TRACE"
	// childPrepareModeEnv overrides prepareRoot in the child process:
	// "fail" returns an error, "repaired" creates the root and returns a
	// repair report with a warning, "repair_conf" replaces an unreadable
	// googet.conf entry with a valid file, and "created" creates the root and
	// returns Created only.
	childPrepareModeEnv = "GOOGET_TEST_CHILD_PREPARE_MODE"
	// childCheckModeEnv overrides checkEntries in the child process:
	// "lock-fail" reports a quarantined googet.conf on the pre-lock check,
	// "lock-err" returns an error on the pre-lock check, "post-fail" reports
	// an untrusted googet.db on the post-lock check, and "post-err" returns
	// an error on the post-lock check.
	childCheckModeEnv = "GOOGET_TEST_CHILD_CHECK_MODE"
	// childRecordLockAgeEnv, when set to "1", makes obtainLock print the
	// maxAge argument prefixed by lockAgePrefix.
	childRecordLockAgeEnv = "GOOGET_TEST_CHILD_RECORD_LOCK_AGE"
	lockAgePrefix         = "googet-test-lock-age: "
)

func TestMain(m *testing.M) {
	if os.Getenv(runChildEnv) == "1" {
		os.Exit(runChild())
	}
	os.Exit(m.Run())
}

func appendTrace(line string) {
	p := os.Getenv(childTraceEnv)
	if p == "" {
		return
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintln(f, line)
}

// rulePhase identifies which checkEntries rule set rules belongs to.
func rulePhase(rules []system.EntryRule) string {
	for _, r := range rules {
		switch r.Pattern {
		case "googet.lock":
			return "pre-lock"
		case "googet.db":
			return "post-lock"
		}
	}
	return "unknown"
}

// runChild runs run() in a child process started by runGooget. isAdmin fails
// unless the parent allowed admin rights. obtainLock fails if the googet root
// doesn't exist yet, because the root must be created, with its own mode,
// before the lock file.
func runChild() int {
	if os.Getenv(childAdminEnv) == "1" {
		isAdmin = func() error {
			appendTrace("isAdmin")
			return nil
		}
	} else {
		isAdmin = func() error {
			appendTrace("isAdmin")
			return errors.New("test process is not elevated")
		}
	}
	prepareRoot = func(root string, subdirs []string) (system.RootReport, error) {
		appendTrace("prepareRoot:" + strings.Join(subdirs, ","))
		switch os.Getenv(childPrepareModeEnv) {
		case "fail":
			return system.RootReport{}, errors.New("injected prepareRoot failure")
		case "repaired":
			if err := os.MkdirAll(root, 0774); err != nil {
				return system.RootReport{}, err
			}
			return system.RootReport{
				DACLReplaced: true,
				Warnings:     []string{"parent directory warning"},
			}, nil
		case "repair_conf":
			if err := os.MkdirAll(root, 0774); err != nil {
				return system.RootReport{}, err
			}
			confPath := filepath.Join(root, "googet.conf")
			if err := os.Remove(confPath); err != nil && !os.IsNotExist(err) {
				return system.RootReport{}, err
			}
			if err := os.WriteFile(confPath, []byte("progress: false\n"), 0644); err != nil {
				return system.RootReport{}, err
			}
			return system.RootReport{DACLReplaced: true}, nil
		case "created":
			if err := os.MkdirAll(root, 0774); err != nil {
				return system.RootReport{}, err
			}
			return system.RootReport{Created: true}, nil
		default:
			return system.PrepareRoot(root, subdirs)
		}
	}
	origCheckEntries := checkEntries
	checkEntries = func(root string, rules []system.EntryRule, prep system.RootReport) (system.EntryReport, error) {
		phase := rulePhase(rules)
		appendTrace("checkEntries:" + phase)
		switch os.Getenv(childCheckModeEnv) {
		case "lock-fail":
			if phase == "pre-lock" {
				conf := filepath.Join(root, "googet.conf")
				return system.EntryReport{
					Quarantined: map[string]string{conf: conf + ".untrusted-1"},
					Failures:    []string{conf + " was renamed to " + conf + ".untrusted-1"},
				}, nil
			}
		case "lock-err":
			if phase == "pre-lock" {
				return system.EntryReport{}, errors.New("injected pre-lock checkEntries error")
			}
		case "post-fail":
			if phase == "post-lock" {
				db := filepath.Join(root, "googet.db")
				return system.EntryReport{
					Failures: []string{db + " has an untrusted owner"},
				}, nil
			}
		case "post-err":
			if phase == "post-lock" {
				return system.EntryReport{}, errors.New("injected post-lock checkEntries error")
			}
		}
		return origCheckEntries(root, rules, prep)
	}
	origLoadConf := loadConf
	loadConf = func() error {
		appendTrace("loadConf")
		return origLoadConf()
	}
	obtainLock = func(lockFile string, maxAge time.Duration) (func(), error) {
		appendTrace("obtainLock")
		if os.Getenv(childRecordLockAgeEnv) == "1" {
			fmt.Println(lockAgePrefix + maxAge.String())
		}
		root := filepath.Dir(lockFile)
		if !isDir(root) {
			return nil, fmt.Errorf("googet root %q doesn't exist when taking the lock", root)
		}
		return system.ObtainLock(lockFile, maxAge)
	}
	origRotateLogFile := rotateLogFile
	rotateLogFile = func(logPath string, maxSize int64, keep int) []error {
		appendTrace("rotateLog")
		return origRotateLogFile(logPath, maxSize, keep)
	}
	origCreateDB := createDB
	createDB = func(dbFile string) error {
		appendTrace("createDB")
		return origCreateDB(dbFile)
	}
	origMkdirAll := mkdirAll
	mkdirAll = func(path string, perm os.FileMode) error {
		appendTrace("mkdirAll:" + filepath.Base(path))
		return origMkdirAll(path, perm)
	}
	origExecuteCommand := executeCommand
	executeCommand = func(ctx context.Context) int {
		appendTrace("execute")
		return origExecuteCommand(ctx)
	}
	return run(context.Background())
}

// runGooget runs googet with args in a child process and returns its exit
// code and combined output. The GooGetRoot environment variable is unset, so
// only -root sets the root.
func runGooget(t *testing.T, admin bool, args ...string) (int, string) {
	t.Helper()
	return runGoogetEnv(t, admin, nil, args...)
}

// runGoogetEnv is runGooget with extra environment variables for the child.
func runGoogetEnv(t *testing.T, admin bool, extraEnv []string, args ...string) (int, string) {
	t.Helper()
	cmd := childCommand(admin, extraEnv, args...)
	out, err := cmd.CombinedOutput()
	return childExitCode(t, err, args), string(out)
}

// runGoogetSplit is runGoogetEnv with the child's stdout and stderr returned
// separately.
func runGoogetSplit(t *testing.T, admin bool, extraEnv []string, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	cmd := childCommand(admin, extraEnv, args...)
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &outBuf, &errBuf
	err := cmd.Run()
	return childExitCode(t, err, args), outBuf.String(), errBuf.String()
}

// childCommand returns the command that runs googet with args in a child
// process, as runGooget describes.
func childCommand(admin bool, extraEnv []string, args ...string) *exec.Cmd {
	cmd := exec.Command(os.Args[0], args...)
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(strings.ToUpper(kv), strings.ToUpper(envVar)+"=") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	// The race detector otherwise sleeps for a second before each exit.
	cmd.Env = append(cmd.Env, runChildEnv+"=1", "GORACE="+strings.TrimSpace(os.Getenv("GORACE")+" atexit_sleep_ms=0"))
	if admin {
		cmd.Env = append(cmd.Env, childAdminEnv+"=1")
	}
	cmd.Env = append(cmd.Env, extraEnv...)
	return cmd
}

// childExitCode returns the exit code of a child started by childCommand,
// given the error from running it.
func childExitCode(t *testing.T, err error, args []string) int {
	t.Helper()
	var exitErr *exec.ExitError
	switch {
	case err == nil:
		return 0
	case errors.As(err, &exitErr):
		return exitErr.ExitCode()
	default:
		t.Fatalf("running googet %q: %v", args, err)
		return 0
	}
}

func readTrace(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatalf("reading trace file %q: %v", path, err)
	}
	trimmed := strings.TrimSpace(string(b))
	if trimmed == "" {
		return nil
	}
	return strings.Split(trimmed, "\n")
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
			code, out := runGooget(t, false, args...)
			if code != 1 {
				t.Errorf("unelevated googet %q exit code = %d, want 1; output:\n%s", tc.args, code, out)
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
			code, out := runGooget(t, false, args...)
			if code != tc.wantCode {
				t.Errorf("unelevated googet %q with an empty root exit code = %d, want %d; output:\n%s", tc.args, code, tc.wantCode, out)
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

// skipUnlessElevatedOnWindows skips the test on Windows unless the process
// is elevated, because the root is created with an owner and ACL that need
// an elevated process. GitHub Actions runners are elevated, so there the test
// runs and fails if it isn't.
func skipUnlessElevatedOnWindows(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" && system.IsAdmin() != nil && os.Getenv("GITHUB_ACTIONS") == "" {
		t.Skip("needs an elevated process on Windows")
	}
}

func TestRunElevatedCreatesRoot(t *testing.T) {
	skipUnlessElevatedOnWindows(t)
	root := filepath.Join(t.TempDir(), "googet")
	code, out := runGooget(t, true, "-root", root, "-system_log=false", "-noconfirm", "listrepos")
	if code != 0 {
		t.Fatalf("elevated googet listrepos exit code = %d, want 0; output:\n%s", code, out)
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
				code, out := runGooget(t, false, args...)
				if code != 0 {
					t.Errorf("googet %q exit code = %d, want 0; output:\n%s", args, code, out)
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

func TestRunPrepareRootOrder(t *testing.T) {
	root := filepath.Join(t.TempDir(), "googet")
	trace := filepath.Join(t.TempDir(), "trace.txt")
	code, out := runGoogetEnv(t, true, []string{
		childTraceEnv + "=" + trace,
		childPrepareModeEnv + "=created",
	}, "-root", root, "-system_log=false", "-noconfirm", "listrepos")
	if code != 0 {
		t.Fatalf("elevated googet listrepos exit code = %d, want 0; output:\n%s", code, out)
	}
	want := []string{
		"isAdmin",
		"prepareRoot:repos,cache",
		"checkEntries:pre-lock",
		"loadConf",
		"obtainLock",
		"checkEntries:post-lock",
		"rotateLog",
		"createDB",
		"mkdirAll:cache",
		"mkdirAll:repos",
		"execute",
	}
	if diff := cmp.Diff(want, readTrace(t, trace)); diff != "" {
		t.Errorf("call order mismatch (-want +got):\n%s", diff)
	}
}

func TestRunPrepareRootFailureStopsBeforeLock(t *testing.T) {
	root := filepath.Join(t.TempDir(), "googet")
	trace := filepath.Join(t.TempDir(), "trace.txt")
	code, out := runGoogetEnv(t, true, []string{
		childTraceEnv + "=" + trace,
		childPrepareModeEnv + "=fail",
	}, "-root", root, "-system_log=false", "-noconfirm", "listrepos")
	if code != 1 {
		t.Errorf("exit code = %d, want 1; output:\n%s", code, out)
	}
	if !strings.Contains(out, "injected prepareRoot failure") {
		t.Errorf("output = %q, want it to contain the prepareRoot error", out)
	}
	want := []string{"isAdmin", "prepareRoot:repos,cache"}
	if diff := cmp.Diff(want, readTrace(t, trace)); diff != "" {
		t.Errorf("call order mismatch (-want +got):\n%s", diff)
	}
	if _, err := os.Lstat(root); !os.IsNotExist(err) {
		t.Errorf("root %q exists after prepareRoot failure (Lstat error: %v)", root, err)
	}
}

func TestRunUnelevatedSkipsPrepareRoot(t *testing.T) {
	t.Run("listrepos with existing root", func(t *testing.T) {
		root := t.TempDir()
		createTestDB(t, root)
		trace := filepath.Join(t.TempDir(), "trace.txt")
		code, out := runGoogetEnv(t, false, []string{
			childTraceEnv + "=" + trace,
		}, "-root", root, "-system_log=false", "listrepos")
		if code != 0 {
			t.Fatalf("exit code = %d, want 0; output:\n%s", code, out)
		}
		if diff := cmp.Diff([]string{"loadConf", "execute"}, readTrace(t, trace)); diff != "" {
			t.Errorf("trace mismatch (-want +got):\n%s", diff)
		}
	})

	t.Run("installed with existing root and db", func(t *testing.T) {
		root := t.TempDir()
		db, err := googetdb.NewDB(filepath.Join(root, "googet.db"))
		if err != nil {
			t.Fatalf("creating test db: %v", err)
		}
		db.Close()
		trace := filepath.Join(t.TempDir(), "trace.txt")
		code, out := runGoogetEnv(t, false, []string{
			childTraceEnv + "=" + trace,
		}, "-root", root, "-system_log=false", "installed", "-format=json")
		if code != 0 {
			t.Fatalf("exit code = %d, want 0; output:\n%s", code, out)
		}
		if diff := cmp.Diff([]string{"loadConf", "execute"}, readTrace(t, trace)); diff != "" {
			t.Errorf("trace mismatch (-want +got):\n%s", diff)
		}
	})

	t.Run("install with missing root", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "googet")
		trace := filepath.Join(t.TempDir(), "trace.txt")
		code, out := runGoogetEnv(t, false, []string{
			childTraceEnv + "=" + trace,
		}, "-root", root, "-system_log=false", "install", "foo")
		if code != 1 {
			t.Fatalf("exit code = %d, want 1; output:\n%s", code, out)
		}
		if diff := cmp.Diff([]string{"isAdmin"}, readTrace(t, trace)); diff != "" {
			t.Errorf("trace mismatch (-want +got):\n%s", diff)
		}
		if _, err := os.Lstat(root); !os.IsNotExist(err) {
			t.Errorf("missing root %q was created (Lstat error: %v)", root, err)
		}
	})
}

func TestRunLogsPrepareRootReportToLogFile(t *testing.T) {
	t.Run("repaired root and warning logged to googet.log", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "googet")
		code, out := runGoogetEnv(t, true, []string{
			childPrepareModeEnv + "=repaired",
		}, "-root", root, "-system_log=false", "-noconfirm", "listrepos")
		if code != 0 {
			t.Fatalf("exit code = %d, want 0; output:\n%s", code, out)
		}
		logBytes, err := os.ReadFile(filepath.Join(root, "googet.log"))
		if err != nil {
			t.Fatalf("reading googet.log: %v", err)
		}
		logText := string(logBytes)
		if !strings.Contains(logText, "Reset the permissions of the googet root") || !strings.Contains(logText, "replaced the access list") {
			t.Errorf("googet.log missing repair summary; got:\n%s", logText)
		}
		if !strings.Contains(logText, "parent directory warning") {
			t.Errorf("googet.log missing parent warning; got:\n%s", logText)
		}
	})

	t.Run("created root logs no repair warning", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "googet")
		code, out := runGoogetEnv(t, true, []string{
			childPrepareModeEnv + "=created",
		}, "-root", root, "-system_log=false", "-noconfirm", "listrepos")
		if code != 0 {
			t.Fatalf("exit code = %d, want 0; output:\n%s", code, out)
		}
		logBytes, err := os.ReadFile(filepath.Join(root, "googet.log"))
		if err != nil {
			t.Fatalf("reading googet.log: %v", err)
		}
		if strings.Contains(string(logBytes), "Reset the permissions of the googet root") {
			t.Errorf("googet.log unexpectedly logged a repair warning for a newly created root:\n%s", logBytes)
		}
	})
}

func TestRootSubdirsMatchSettings(t *testing.T) {
	orig := settings.RootDir
	t.Cleanup(func() { settings.RootDir = orig })
	settings.RootDir = filepath.Join(t.TempDir(), "googet")
	want := []string{filepath.Base(settings.RepoDir()), filepath.Base(settings.CacheDir())}
	if diff := cmp.Diff(want, rootSubdirs()); diff != "" {
		t.Errorf("rootSubdirs() mismatch (-want +got):\n%s", diff)
	}
}

func TestRunPrepareRootBeforeSettingsInit(t *testing.T) {
	t.Run("prepareRoot failure skips googet.conf", func(t *testing.T) {
		root := t.TempDir()
		// A directory at googet.conf makes ReadFile fail with a non-NotExist
		// error, which panics in settings.readConf if Initialize runs before
		// prepareRoot.
		if err := os.Mkdir(filepath.Join(root, "googet.conf"), 0755); err != nil {
			t.Fatal(err)
		}
		code, out := runGoogetEnv(t, true, []string{
			childPrepareModeEnv + "=fail",
		}, "-root", root, "-system_log=false", "-noconfirm", "installed")
		if code != 1 {
			t.Errorf("exit code = %d, want 1; output:\n%s", code, out)
		}
		if !strings.Contains(out, "injected prepareRoot failure") {
			t.Errorf("output = %q, want prepareRoot error", out)
		}
		if strings.Contains(out, "panic") || strings.Contains(out, "Error unmarshalling conf file") {
			t.Errorf("output unexpectedly read googet.conf before prepareRoot:\n%s", out)
		}
	})

	t.Run("prepareRoot repairs root before loadConf reads googet.conf", func(t *testing.T) {
		root := t.TempDir()
		if err := os.Mkdir(filepath.Join(root, "googet.conf"), 0755); err != nil {
			t.Fatal(err)
		}
		code, out := runGoogetEnv(t, true, []string{
			childPrepareModeEnv + "=repair_conf",
		}, "-root", root, "-system_log=false", "-noconfirm", "installed", "-format=json")
		if code != 0 {
			t.Fatalf("exit code = %d, want 0; output:\n%s", code, out)
		}
		if strings.Contains(out, "panic") || strings.Contains(out, "Error unmarshalling conf file") {
			t.Errorf("output unexpectedly reported conf error:\n%s", out)
		}
	})
}

func TestRunUnreadableConf(t *testing.T) {
	for _, tc := range []struct {
		desc  string
		admin bool
		args  []string
	}{
		{
			desc:  "privileged path",
			admin: true,
			args:  []string{"installed"},
		},
		{
			desc: "listrepos without the admin check",
			args: []string{"listrepos"},
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			if tc.admin && runtime.GOOS == "windows" {
				t.Skip("on Windows, the entry checks handle a googet.conf that isn't a regular file before it is read")
			}
			root := t.TempDir()
			if !tc.admin {
				// Unelevated runs need an existing database.
				createTestDB(t, root)
			}
			// A directory at googet.conf exists but can't be read as a file.
			if err := os.Mkdir(filepath.Join(root, "googet.conf"), 0755); err != nil {
				t.Fatal(err)
			}
			args := append([]string{"-root", root, "-system_log=false", "-noconfirm"}, tc.args...)
			code, out := runGooget(t, tc.admin, args...)
			if code != 1 {
				t.Errorf("exit code = %d, want 1; output:\n%s", code, out)
			}
			if !strings.Contains(out, "Unable to initialize settings") {
				t.Errorf("output = %q, want the googet.conf read error", out)
			}
			if strings.Contains(out, "panic") {
				t.Errorf("output unexpectedly panicked:\n%s", out)
			}
		})
	}
}

func TestRunCallOrder(t *testing.T) {
	for _, tc := range []struct {
		desc     string
		admin    bool
		root     func(t *testing.T) string
		args     []string
		elevated bool
		wantCode int
		want     []string
	}{
		{
			desc:     "privileged path loads googet.conf after the checks",
			admin:    true,
			root:     func(t *testing.T) string { return filepath.Join(t.TempDir(), "googet") },
			args:     []string{"listrepos"},
			elevated: true,
			wantCode: 0,
			want: []string{
				"isAdmin",
				"prepareRoot:repos,cache",
				"checkEntries:pre-lock",
				"loadConf",
				"obtainLock",
				"checkEntries:post-lock",
				"rotateLog",
				"createDB",
				"mkdirAll:cache",
				"mkdirAll:repos",
				"execute",
			},
		},
		{
			desc: "listrepos without the admin check loads googet.conf first",
			root: func(t *testing.T) string {
				root := t.TempDir()
				createTestDB(t, root)
				return root
			},
			args:     []string{"listrepos"},
			wantCode: 0,
			want:     []string{"loadConf", "execute"},
		},
		{
			desc: "installed without the admin check loads googet.conf first",
			root: func(t *testing.T) string {
				root := t.TempDir()
				db, err := googetdb.NewDB(filepath.Join(root, "googet.db"))
				if err != nil {
					t.Fatalf("creating test db: %v", err)
				}
				db.Close()
				return root
			},
			args:     []string{"installed", "-format=json"},
			wantCode: 0,
			want:     []string{"loadConf", "execute"},
		},
		{
			desc:     "failed admin check doesn't load googet.conf",
			root:     func(t *testing.T) string { return t.TempDir() },
			args:     []string{"install", "foo"},
			wantCode: 1,
			want:     []string{"isAdmin"},
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			if tc.elevated {
				skipUnlessElevatedOnWindows(t)
			}
			trace := filepath.Join(t.TempDir(), "trace.txt")
			args := append([]string{"-root", tc.root(t), "-system_log=false", "-noconfirm"}, tc.args...)
			code, out := runGoogetEnv(t, tc.admin, []string{childTraceEnv + "=" + trace}, args...)
			if code != tc.wantCode {
				t.Errorf("googet %q exit code = %d, want %d; output:\n%s", tc.args, code, tc.wantCode, out)
			}
			if diff := cmp.Diff(tc.want, readTrace(t, trace)); diff != "" {
				t.Errorf("googet %q calls mismatch (-want +got):\n%s\noutput:\n%s", tc.args, diff, out)
			}
		})
	}
}

func TestRunQuarantinesUntrustedConfAndRepo(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("googet checks the files in its root only on Windows")
	}
	if system.IsAdmin() != nil {
		if os.Getenv("GITHUB_ACTIONS") != "" {
			t.Fatal("this test needs an elevated process")
		}
		t.Skip("this test needs an elevated process")
	}
	root := filepath.Join(t.TempDir(), "googet")
	args := []string{"-root", root, "-system_log=false", "-noconfirm", "clean"}
	if code, out := runGooget(t, true, args...); code != 0 {
		t.Fatalf("googet clean on a new root exit code = %d, want 0; output:\n%s", code, out)
	}
	conf := filepath.Join(root, "googet.conf")
	repos := filepath.Join(root, "repos")
	repoFile := filepath.Join(repos, "x.repo")
	for p, data := range map[string]string{conf: "lockfilemaxage: 1h\n", repoFile: "- name: x\n  url: https://example.com/x\n"} {
		if err := os.WriteFile(p, []byte(data), 0666); err != nil {
			t.Fatal(err)
		}
		// icacls enables the restore privilege to set another owner.
		if out, err := exec.Command("icacls", p, "/setowner", "*S-1-5-32-545").CombinedOutput(); err != nil {
			t.Fatalf("icacls %q /setowner: %v\n%s", p, err, out)
		}
	}
	if rfs, err := repo.ConfigFiles(repos); err != nil || len(rfs) != 1 {
		t.Fatalf("precondition: repo.ConfigFiles(%q) = %v, %v; want x.repo", repos, rfs, err)
	}

	trace := filepath.Join(t.TempDir(), "trace.txt")
	code, out := runGoogetEnv(t, true, []string{childTraceEnv + "=" + trace}, args...)
	if code != 1 {
		t.Errorf("googet clean with untrusted files exit code = %d, want 1; output:\n%s", code, out)
	}
	calls := readTrace(t, trace)
	for _, forbidden := range []string{"loadConf", "createDB", "mkdirAll:cache", "mkdirAll:repos", "execute"} {
		for _, got := range calls {
			if got == forbidden {
				t.Errorf("googet clean with untrusted conf called %q, want it skipped; calls: %q", forbidden, calls)
			}
		}
	}
	for _, p := range []string{conf, repoFile} {
		if !strings.Contains(out, p) {
			t.Errorf("googet clean output doesn't mention %q:\n%s", p, out)
		}
		if _, err := os.Lstat(p); !os.IsNotExist(err) {
			t.Errorf("%q still exists (Lstat error: %v)", p, err)
		}
		if m, _ := filepath.Glob(p + ".untrusted-*"); len(m) != 1 {
			t.Errorf("renamed copies of %q = %q, want one", p, m)
		}
	}
	if rfs, err := repo.ConfigFiles(repos); err != nil || len(rfs) != 0 {
		t.Errorf("repo.ConfigFiles(%q) = %v, %v; want no repo files", repos, rfs, err)
	}
	if log, err := os.ReadFile(filepath.Join(root, "googet.log")); err != nil || !strings.Contains(string(log), conf) {
		t.Errorf("googet.log = %q, %v; want it to mention %q", log, err, conf)
	}

	if code, out := runGooget(t, true, args...); code != 0 {
		t.Errorf("googet clean after the files were renamed exit code = %d, want 0; output:\n%s", code, out)
	}
}

func recordedLockAge(out string) string {
	for _, line := range strings.Split(out, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), lockAgePrefix); ok {
			return v
		}
	}
	return ""
}

func TestRunConfQuarantineSkipsLoadConfAndLogsFailure(t *testing.T) {
	root := filepath.Join(t.TempDir(), "googet")
	if err := os.MkdirAll(root, 0774); err != nil {
		t.Fatal(err)
	}
	// Leave a googet.conf with a custom lockfilemaxage in place; because
	// checkEntries reports a pre-lock failure, LoadConf must not be called.
	if err := os.WriteFile(filepath.Join(root, "googet.conf"), []byte("lockfilemaxage: 1m\n"), 0666); err != nil {
		t.Fatal(err)
	}
	trace := filepath.Join(t.TempDir(), "trace.txt")
	code, out := runGoogetEnv(t, true, []string{
		childPrepareModeEnv + "=created",
		childCheckModeEnv + "=lock-fail",
		childTraceEnv + "=" + trace,
		childRecordLockAgeEnv + "=1",
	}, "-root", root, "-system_log=false", "-noconfirm", "clean")
	if code != 1 {
		t.Errorf("exit code = %d, want 1; output:\n%s", code, out)
	}
	wantCalls := []string{
		"isAdmin",
		"prepareRoot:repos,cache",
		"checkEntries:pre-lock",
		"obtainLock",
		"checkEntries:post-lock",
		"rotateLog",
	}
	if diff := cmp.Diff(wantCalls, readTrace(t, trace)); diff != "" {
		t.Errorf("call order mismatch (-want +got):\n%s\noutput:\n%s", diff, out)
	}
	if got := recordedLockAge(out); got != (24 * time.Hour).String() {
		t.Errorf("obtainLock maxAge = %q, want default %q when conf failed check", got, (24 * time.Hour).String())
	}
	logBytes, err := os.ReadFile(filepath.Join(root, "googet.log"))
	if err != nil {
		t.Fatalf("reading googet.log: %v", err)
	}
	logText := string(logBytes)
	if !strings.Contains(logText, "Removed or renamed entries in the googet root") {
		t.Errorf("googet.log missing change warning; got:\n%s", logText)
	}
	if !strings.Contains(logText, "googet.conf.untrusted-1") {
		t.Errorf("googet.log missing quarantine failure; got:\n%s", logText)
	}
	if _, err := os.Lstat(filepath.Join(root, "googet.db")); !os.IsNotExist(err) {
		t.Errorf("googet.db was created after pre-lock entry check failure (Lstat error: %v)", err)
	}
}

func TestRunHonorsLockFileMaxAgeFromTrustedConf(t *testing.T) {
	root := filepath.Join(t.TempDir(), "googet")
	if err := os.MkdirAll(root, 0774); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "googet.conf"), []byte("lockfilemaxage: 42m\n"), 0666); err != nil {
		t.Fatal(err)
	}
	code, out := runGoogetEnv(t, true, []string{
		childPrepareModeEnv + "=created",
		childRecordLockAgeEnv + "=1",
	}, "-root", root, "-system_log=false", "-noconfirm", "clean")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; output:\n%s", code, out)
	}
	if got := recordedLockAge(out); got != (42 * time.Minute).String() {
		t.Errorf("obtainLock maxAge = %q, want %q from googet.conf", got, (42 * time.Minute).String())
	}
}

func TestRunPostLockFailureAndCheckErrorsStopEarly(t *testing.T) {
	for _, tc := range []struct {
		desc        string
		prepareMode string
		mode        string
		wantCalls   []string
		wantLog     string
		wantOut     string
	}{
		{
			desc:        "post-lock failure logs error and exits before createDB",
			prepareMode: "created",
			mode:        "post-fail",
			wantCalls: []string{
				"isAdmin",
				"prepareRoot:repos,cache",
				"checkEntries:pre-lock",
				"loadConf",
				"obtainLock",
				"checkEntries:post-lock",
				"rotateLog",
			},
			wantLog: "has an untrusted owner",
		},
		{
			desc:        "pre-lock check error stops before loadConf and obtainLock and still logs root repair",
			prepareMode: "repaired",
			mode:        "lock-err",
			wantCalls: []string{
				"isAdmin",
				"prepareRoot:repos,cache",
				"checkEntries:pre-lock",
			},
			wantOut: "Reset the permissions of the googet root",
		},
		{
			desc:        "post-lock check error stops before rotateLog and createDB and still logs root repair",
			prepareMode: "repaired",
			mode:        "post-err",
			wantCalls: []string{
				"isAdmin",
				"prepareRoot:repos,cache",
				"checkEntries:pre-lock",
				"loadConf",
				"obtainLock",
				"checkEntries:post-lock",
			},
			wantOut: "Reset the permissions of the googet root",
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "googet")
			trace := filepath.Join(t.TempDir(), "trace.txt")
			code, out := runGoogetEnv(t, true, []string{
				childPrepareModeEnv + "=" + tc.prepareMode,
				childCheckModeEnv + "=" + tc.mode,
				childTraceEnv + "=" + trace,
			}, "-root", root, "-system_log=false", "-noconfirm", "listrepos")
			if code != 1 {
				t.Errorf("exit code = %d, want 1; output:\n%s", code, out)
			}
			if diff := cmp.Diff(tc.wantCalls, readTrace(t, trace)); diff != "" {
				t.Errorf("call order mismatch (-want +got):\n%s\noutput:\n%s", diff, out)
			}
			if tc.wantLog != "" {
				logBytes, err := os.ReadFile(filepath.Join(root, "googet.log"))
				if err != nil {
					t.Fatalf("reading googet.log: %v", err)
				}
				if !strings.Contains(string(logBytes), tc.wantLog) {
					t.Errorf("googet.log = %q, want it to contain %q", logBytes, tc.wantLog)
				}
			}
			if tc.wantOut != "" && !strings.Contains(out, tc.wantOut) {
				t.Errorf("output = %q, want it to contain %q", out, tc.wantOut)
			}
			if _, err := os.Lstat(filepath.Join(root, "googet.db")); !os.IsNotExist(err) {
				t.Errorf("googet.db was created despite %s (Lstat error: %v)", tc.mode, err)
			}
		})
	}
}

func TestRunUntrustedStateFailsBeforeCreateDB(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("googet checks the files in its root only on Windows")
	}
	if system.IsAdmin() != nil {
		if os.Getenv("GITHUB_ACTIONS") != "" {
			t.Fatal("this test needs an elevated process")
		}
		t.Skip("this test needs an elevated process")
	}
	for _, name := range []string{"googet.state", "googet.state.bak"} {
		t.Run(name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "googet")
			if _, err := system.PrepareRoot(root, []string{"repos", "cache"}); err != nil {
				t.Fatalf("PrepareRoot(%q): %v", root, err)
			}
			stateFile := filepath.Join(root, name)
			if err := os.WriteFile(stateFile, []byte("[]"), 0666); err != nil {
				t.Fatal(err)
			}
			if out, err := exec.Command("icacls", stateFile, "/setowner", "*S-1-5-32-545").CombinedOutput(); err != nil {
				t.Fatalf("icacls %q /setowner: %v\n%s", stateFile, err, out)
			}
			trace := filepath.Join(t.TempDir(), "trace.txt")
			code, out := runGoogetEnv(t, true, []string{childTraceEnv + "=" + trace}, "-root", root, "-system_log=false", "-noconfirm", "clean")
			if code != 1 {
				t.Errorf("exit code = %d, want 1; output:\n%s", code, out)
			}
			calls := readTrace(t, trace)
			for _, forbidden := range []string{"createDB", "mkdirAll:cache", "mkdirAll:repos", "execute"} {
				for _, got := range calls {
					if got == forbidden {
						t.Errorf("run with untrusted %s called %q, want it skipped; calls: %q", name, forbidden, calls)
					}
				}
			}
			if _, err := os.Lstat(filepath.Join(root, "googet.db")); !os.IsNotExist(err) {
				t.Errorf("googet.db was created from untrusted %s (Lstat error: %v)", name, err)
			}
			if got, err := os.ReadFile(stateFile); err != nil || string(got) != "[]" {
				t.Errorf("%s = %q, %v; want it left untouched", name, got, err)
			}
		})
	}
}

func TestLockEntryRulesCoverRotatedLogs(t *testing.T) {
	orig := settings.RootDir
	t.Cleanup(func() { settings.RootDir = orig })
	settings.RootDir = t.TempDir()
	logFile := settings.LogFile()
	names := []string{filepath.Base(logFile)}
	for n := 1; n <= logKeep; n++ {
		names = append(names, filepath.Base(logGeneration(logFile, n)))
	}
	for _, name := range names {
		var got *system.EntryRule
		for _, r := range lockEntryRules() {
			if ok, err := filepath.Match(strings.ToLower(r.Pattern), strings.ToLower(name)); err == nil && ok {
				got = &r
				break
			}
		}
		if got == nil {
			t.Errorf("lockEntryRules() has no rule for %q", name)
			continue
		}
		if got.Action != system.EntryRemove {
			t.Errorf("lockEntryRules() rule for %q has action %v, want EntryRemove", name, got.Action)
		}
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

// createTestDB creates an empty googet database in root, which cli.Classify's
// unelevated runs need.
func createTestDB(t *testing.T, root string) {
	t.Helper()
	db, err := googetdb.NewDB(filepath.Join(root, "googet.db"))
	if err != nil {
		t.Fatalf("creating test db: %v", err)
	}
	db.Close()
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
		{"install -dry_run pkg", cli.Privileged},
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
	wantAllow := []string{"installed", "listrepos", "remove"}
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
			check := func(t *testing.T, args ...string) {
				t.Helper()
				trace := filepath.Join(t.TempDir(), "trace")
				code, stdout, stderr := runGoogetSplit(t, false, []string{childTraceEnv + "=" + trace}, args...)
				if code != int(subcommands.ExitUsageError) {
					t.Errorf("googet %q exit code = %d, want %d; output:\n%s%s", tc, code, subcommands.ExitUsageError, stdout, stderr)
				}
				if steps := readTrace(t, trace); len(steps) != 0 {
					t.Errorf("googet %q called run() steps %q, want none", tc, steps)
				}
				if stderr == "" {
					t.Errorf("googet %q printed nothing on stderr, want usage or a parse error", tc)
				}
			}
			t.Run("no root", func(t *testing.T) {
				check(t, tc...)
			})
			t.Run("missing root", func(t *testing.T) {
				root := filepath.Join(t.TempDir(), "googet")
				check(t, append([]string{"-root", root}, tc...)...)
				if _, err := os.Lstat(root); !os.IsNotExist(err) {
					t.Errorf("googet %q created the root %q (Lstat error: %v)", tc, root, err)
				}
			})
			t.Run("existing root", func(t *testing.T) {
				root := fixtureRoot(t)
				before := snapshotRoot(t, root)
				check(t, append([]string{"-root", root}, tc...)...)
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

// TestRunRemoveDryRunUnprivileged checks that remove -dry_run runs without
// the lock when the admin check fails, and leaves the googet root untouched.
func TestRunRemoveDryRunUnprivileged(t *testing.T) {
	root := fixtureRoot(t)
	before := snapshotRoot(t, root)

	trace := filepath.Join(t.TempDir(), "trace")
	code, stdout, stderr := runGoogetSplit(t, false, []string{childTraceEnv + "=" + trace}, "-root", root, "remove", "-dry_run", "A")

	if code != 0 {
		t.Errorf("googet remove -dry_run A exited %d, want 0; stderr:\n%s", code, stderr)
	}
	if !strings.Contains(stdout, "B.noarch") || !strings.Contains(stdout, "Dry run") {
		t.Errorf("googet remove -dry_run A printed %q, want the dry-run list naming B", stdout)
	}
	steps := readTrace(t, trace)
	if !slices.Contains(steps, "isAdmin") {
		t.Errorf("run() steps = %q, want the admin check", steps)
	}
	for _, step := range steps {
		if step == "obtainLock" || strings.HasPrefix(step, "prepareRoot") || strings.HasPrefix(step, "checkEntries") {
			t.Errorf("run() called %s, want no lock, root preparation or entry checks", step)
		}
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

	trace := filepath.Join(t.TempDir(), "trace")
	code, out := runGoogetEnv(t, false, []string{childTraceEnv + "=" + trace}, "-root", root, "remove", "A")

	if code != 1 {
		t.Errorf("googet remove A exited %d, want 1; output:\n%s", code, out)
	}
	steps := readTrace(t, trace)
	if !slices.Contains(steps, "isAdmin") {
		t.Error("isAdmin was not called, want the admin check")
	}
	if slices.Contains(steps, "obtainLock") {
		t.Error("obtainLock was called after a failed admin check")
	}
	if !installed(t, root, "A") {
		t.Error("package A was removed, want it still installed")
	}
}

// TestRunRemovePrivileged checks that run() marks the context as privileged
// once the admin check and the lock succeed, so remove still removes.
func TestRunRemovePrivileged(t *testing.T) {
	skipUnlessElevatedOnWindows(t)
	root := fixtureRoot(t)

	trace := filepath.Join(t.TempDir(), "trace")
	code, out := runGoogetEnv(t, true, []string{childTraceEnv + "=" + trace}, "-root", root, "-noconfirm", "-system_log=false", "remove", "-db_only", "B")

	if code != 0 {
		t.Errorf("googet remove -db_only B with privileges exited %d, want 0; output:\n%s", code, out)
	}
	steps := readTrace(t, trace)
	if !slices.Contains(steps, "isAdmin") || !slices.Contains(steps, "obtainLock") {
		t.Errorf("run() steps = %q, want isAdmin and obtainLock", steps)
	}
	if installed(t, root, "B") {
		t.Error("package B is still installed, want it removed")
	}
}

// TestRunElevatedAllowUnelevated checks elevated runs of commands that may
// run unelevated. A cli.LockFree command still runs without the admin check,
// the lock and googet.log. Any other command takes the privileged path, so
// the admin check passes, it takes the lock and it writes googet.log.
func TestRunElevatedAllowUnelevated(t *testing.T) {
	for _, tc := range []struct {
		args       []string
		wantOut    string
		privileged bool
	}{
		{args: []string{"remove", "-dry_run", "A"}, wantOut: "Dry run", privileged: true},
		{args: []string{"installed"}, wantOut: "A.noarch"},
		{args: []string{"listrepos"}, wantOut: "test"},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			if tc.privileged {
				skipUnlessElevatedOnWindows(t)
			}
			root := fixtureRoot(t)
			if err := os.Mkdir(filepath.Join(root, "repos"), 0774); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, "repos", "test.repo"), []byte("- name: test\n  url: https://example.com/repo\n"), 0664); err != nil {
				t.Fatal(err)
			}
			trace := filepath.Join(t.TempDir(), "trace")
			code, stdout, stderr := runGoogetSplit(t, true, []string{childTraceEnv + "=" + trace}, append([]string{"-root", root, "-noconfirm", "-system_log=false"}, tc.args...)...)
			if code != 0 {
				t.Errorf("elevated googet %q exit code = %d, want 0; output:\n%s%s", tc.args, code, stdout, stderr)
			}
			if !strings.Contains(stdout, tc.wantOut) {
				t.Errorf("elevated googet %q printed %q, want it to contain %q", tc.args, stdout, tc.wantOut)
			}
			steps := readTrace(t, trace)
			gotPrivileged := slices.Contains(steps, "isAdmin") && slices.Contains(steps, "obtainLock")
			gotAny := slices.Contains(steps, "isAdmin") || slices.Contains(steps, "obtainLock")
			if tc.privileged && !gotPrivileged || !tc.privileged && gotAny {
				t.Errorf("elevated googet %q run() steps = %q, want isAdmin and obtainLock: %v", tc.args, steps, tc.privileged)
			}
			_, err := os.Stat(filepath.Join(root, "googet.log"))
			if gotLog := err == nil; gotLog != tc.privileged {
				t.Errorf("elevated googet %q wrote googet.log: %v (Stat error: %v), want %v", tc.args, gotLog, err, tc.privileged)
			}
		})
	}
}
