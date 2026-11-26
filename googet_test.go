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
	"io/fs"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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
	"github.com/google/googet/v2/repo"
	"github.com/google/googet/v2/settings"
	"github.com/google/googet/v2/system"
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

// Environment variables that runGooget sets for the child process.
// childEnvPrefix starts the name of every environment variable below.
const childEnvPrefix = "GOOGET_TEST_"

// Environment variables that configure a child process of the tests. Every
// name starts with childEnvPrefix, so that childEnv can drop inherited ones.
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
	// confineEnv, when set, makes the child deny itself every filesystem
	// write outside the directories listed below before it runs googet,
	// where the OS supports that. reexecGuardEnv marks the child after it has
	// confined itself and re-executed.
	confineEnv     = childEnvPrefix + "CONFINE_WRITES"
	reexecGuardEnv = childEnvPrefix + "CONFINE_REEXECED"
	// scratchEnv names the scratch tree that holds an unelevated child's
	// temporary, home and working directories, and rootEnv names the googet
	// root it was given with -root. Writes under both, and in the directory
	// of the childTraceEnv file, stay allowed (under the root's nearest
	// existing ancestor while the root doesn't exist), so that runGooget's
	// snapshot of the scratch tree and the callers' snapshots of the root
	// show them, even when googet ignores the error that a denied write would
	// return.
	scratchEnv = childEnvPrefix + "SCRATCH"
	rootEnv    = childEnvPrefix + "ROOT"
	// writeProbeEnv, when set, makes the child write an empty file at the
	// path it holds instead of running googet. The child exits 0 if the write
	// works, probeDeniedCode if it fails with a permission error and 1 if it
	// fails otherwise. TestWriteConfinement uses it.
	writeProbeEnv = childEnvPrefix + "WRITE_PROBE"
)

// Exit codes and messages that a child uses to report its result.
const (
	probeDeniedCode = 3
	// confineFailCode and confineFailPrefix mark a child that couldn't
	// confine its writes.
	confineFailCode   = 125
	confineFailPrefix = "confining writes: "
)

// confineFailed reports whether a child exited because it couldn't confine
// its writes.
func confineFailed(code int, stderr string) bool {
	return code == confineFailCode && strings.Contains(stderr, confineFailPrefix)
}

// TestMain serves all tests in package main. When runChildEnv or
// writeProbeEnv is set, the test binary acts as a child process for
// runGooget or TestWriteConfinement instead of running the tests.
func TestMain(m *testing.M) {
	child, probe := os.Getenv(runChildEnv) == "1", os.Getenv(writeProbeEnv)
	if !child && probe == "" {
		os.Exit(m.Run())
	}
	if os.Getenv(confineEnv) != "" && os.Getenv(reexecGuardEnv) == "" {
		var allow []string
		if p := os.Getenv(childTraceEnv); p != "" {
			allow = append(allow, filepath.Dir(p))
		}
		if d := os.Getenv(scratchEnv); d != "" {
			allow = append(allow, d)
		}
		if d := os.Getenv(rootEnv); d != "" {
			// The root may not exist yet. Allow its nearest existing
			// ancestor, normally a test's own temporary
			// directory, so that creating the root shows up too.
			for !isDir(d) && filepath.Dir(d) != d {
				d = filepath.Dir(d)
			}
			// Never allow a whole volume.
			if filepath.Dir(d) != d {
				allow = append(allow, d)
			}
		}
		// A binary built with coverage writes its counters to GOCOVERDIR
		// when it exits.
		if d := os.Getenv("GOCOVERDIR"); d != "" {
			allow = append(allow, d)
		}
		err := confineWritesAndExec(allow, append(os.Environ(), reexecGuardEnv+"=1"))
		fmt.Fprintf(os.Stderr, "%s%v\n", confineFailPrefix, err)
		os.Exit(confineFailCode)
	}
	if probe != "" {
		err := os.WriteFile(probe, nil, 0644)
		switch {
		case err == nil:
			os.Exit(0)
		case errors.Is(err, fs.ErrPermission):
			fmt.Fprintf(os.Stderr, "write probe: %v\n", err)
			os.Exit(probeDeniedCode)
		default:
			fmt.Fprintf(os.Stderr, "write probe: %v\n", err)
			os.Exit(1)
		}
	}
	os.Exit(runChild())
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
//
// An unelevated run (admin unset) must not write any file, inside the googet
// root or anywhere else. Callers check the root. runGooget points the
// child's temporary, home and application data directories and its working
// directory into a scratch tree and fails the test if that tree changes.
// Where the kernel supports Landlock, the child also denies itself every
// filesystem write outside the scratch tree, the root (or the root's nearest
// existing ancestor while the root doesn't exist) and the directory of the
// childTraceEnv file, so that a write anywhere else fails in the child. On
// Linux without Landlock only the snapshots run, and the test fails instead
// when CI is set. A denied write whose error googet ignores goes unreported,
// and Landlock doesn't cover changes to a file's mode, owner, times or
// extended attributes.
func runGooget(t *testing.T, admin bool, args ...string) (int, string) {
	t.Helper()
	return runGoogetEnv(t, admin, nil, args...)
}

// runGoogetEnv is runGooget with extra environment variables for the child.
func runGoogetEnv(t *testing.T, admin bool, extraEnv []string, args ...string) (int, string) {
	t.Helper()
	c := newChild(t, admin, extraEnv, args)
	out, err := c.cmd.CombinedOutput()
	return c.finish(t, err, string(out)), string(out)
}

// runGoogetSplit is runGoogetEnv with the child's stdout and stderr returned
// separately.
func runGoogetSplit(t *testing.T, admin bool, extraEnv []string, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	c := newChild(t, admin, extraEnv, args)
	var outBuf, errBuf bytes.Buffer
	c.cmd.Stdout, c.cmd.Stderr = &outBuf, &errBuf
	err := c.cmd.Run()
	return c.finish(t, err, errBuf.String()), outBuf.String(), errBuf.String()
}

// childEnv returns this process's environment with the variables in set
// replaced by their values in set. GooGetRoot and every inherited variable
// whose name starts with childEnvPrefix are dropped unless set has them, so
// the parent's environment can't change how a child runs. Names are compared
// without regard to case, as Windows does.
func childEnv(set map[string]string) []string {
	skip := map[string]bool{strings.ToUpper(envVar): true}
	for k := range set {
		skip[strings.ToUpper(k)] = true
	}
	var env []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if uk := strings.ToUpper(k); !skip[uk] && !strings.HasPrefix(uk, childEnvPrefix) {
			env = append(env, kv)
		}
	}
	for k, v := range set {
		env = append(env, k+"="+v)
	}
	return env
}

// child is a googet child process set up as runGooget describes.
type child struct {
	cmd     *exec.Cmd
	args    []string
	scratch string            // Empty for an elevated child.
	before  map[string]string // Snapshot of scratch before the run.
}

// newChild returns a child that runs googet with args and the extra "K=V"
// environment variables in extraEnv, which may also override the ones below.
func newChild(t *testing.T, admin bool, extraEnv, args []string) *child {
	t.Helper()
	env := map[string]string{
		runChildEnv: "1",
		// The race detector otherwise sleeps for a second before each exit.
		"GORACE": strings.TrimSpace(os.Getenv("GORACE") + " atexit_sleep_ms=0"),
	}
	c := &child{cmd: exec.Command(os.Args[0], args...), args: args}
	if admin {
		env[childAdminEnv] = "1"
	} else {
		c.scratch = t.TempDir()
		env[scratchEnv] = c.scratch
		tmp, home, cwd := filepath.Join(c.scratch, "tmp"), filepath.Join(c.scratch, "home"), filepath.Join(c.scratch, "cwd")
		for _, d := range []string{tmp, home, cwd} {
			if err := os.Mkdir(d, 0755); err != nil {
				t.Fatal(err)
			}
		}
		for _, k := range []string{"TMPDIR", "TMP", "TEMP"} {
			env[k] = tmp
		}
		for _, k := range []string{"HOME", "USERPROFILE", "APPDATA", "LOCALAPPDATA", "XDG_CACHE_HOME", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME"} {
			env[k] = home
		}
		c.cmd.Dir = cwd
		if root, ok := rootArg(args); ok {
			env[rootEnv] = root
		}
		if writeConfinementSupported() {
			env[confineEnv] = "1"
		} else if runtime.GOOS == "linux" && os.Getenv("CI") != "" {
			t.Fatal("Landlock is not available, so unelevated runs can't be checked for writes outside the googet root")
		}
	}
	for _, kv := range extraEnv {
		k, v, _ := strings.Cut(kv, "=")
		env[k] = v
	}
	c.cmd.Env = childEnv(env)
	if c.scratch != "" {
		c.before = testutil.TreeSnapshot(t, c.scratch)
	}
	return c
}

// finish returns the child's exit code, given the error from running it and
// its stderr. It fails the test if the child couldn't confine its writes or,
// for an unelevated child, if the scratch tree changed.
func (c *child) finish(t *testing.T, err error, stderr string) int {
	t.Helper()
	var code int
	var exitErr *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &exitErr):
		code = exitErr.ExitCode()
	default:
		t.Fatalf("running googet %q: %v", c.args, err)
	}
	if confineFailed(code, stderr) {
		t.Fatalf("googet %q: %s", c.args, stderr)
	}
	if c.scratch != "" {
		if diff := cmp.Diff(c.before, testutil.TreeSnapshot(t, c.scratch)); diff != "" {
			t.Errorf("unelevated googet %q wrote outside the googet root (-before +after):\n%s", c.args, diff)
		}
	}
	return code
}

// rootArg returns the value of the global -root flag in args, the last one
// if it is repeated, as the flag package keeps. It handles the forms these
// tests use: one or two leading dashes, the value either after '=' or in the
// next argument, and it stops at "--" or at the first argument that isn't a
// flag. Unlike the flag package, it skips malformed flags such as "---root"
// instead of rejecting them.
func rootArg(args []string) (root string, ok bool) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" || len(a) < 2 || a[0] != '-' {
			break
		}
		name, val, hasVal := strings.Cut(strings.TrimPrefix(a[1:], "-"), "=")
		if name != "root" {
			continue
		}
		if !hasVal {
			if i+1 >= len(args) {
				return "", false
			}
			i++
			val = args[i]
		}
		root, ok = val, true
	}
	return root, ok
}

func TestRootArg(t *testing.T) {
	for _, tc := range []struct {
		args   []string
		want   string
		wantOK bool
	}{
		{[]string{"-root", "r", "installed"}, "r", true},
		{[]string{"--root", "r", "installed"}, "r", true},
		{[]string{"-root=r", "installed"}, "r", true},
		{[]string{"--root=r", "installed"}, "r", true},
		{[]string{"-noconfirm", "-root", "r", "installed"}, "r", true},
		{[]string{"-root", "a", "--root=b", "installed"}, "b", true},
		{[]string{"--root=a", "-root", "b", "installed"}, "b", true},
		{[]string{"-root"}, "", false},
		{[]string{"installed", "-root", "r"}, "", false},
		{[]string{"install", "root"}, "", false},
		{[]string{"install", "root=r"}, "", false},
		{[]string{"--", "-root", "r"}, "", false},
		{[]string{"---root", "r"}, "", false},
	} {
		if got, ok := rootArg(tc.args); got != tc.want || ok != tc.wantOK {
			t.Errorf("rootArg(%q) = %q, %v, want %q, %v", tc.args, got, ok, tc.want, tc.wantOK)
		}
	}
}

// TestWriteConfinement checks that the write confinement runGooget uses for
// unelevated runs allows writes only in the trace file's directory and under
// the root, or under the root's nearest existing ancestor while the root
// doesn't exist, and never under a whole volume.
func TestWriteConfinement(t *testing.T) {
	if !writeConfinementSupported() {
		t.Skip("write confinement is not supported on this system")
	}
	traceDir := t.TempDir()
	existingRoot := t.TempDir()
	missingRootParent := t.TempDir()
	missingRoot := filepath.Join(missingRootParent, "googet")
	volumeChildRoot := filepath.Join(string(filepath.Separator), "nonexistent-googet-test", "googet")
	for _, tc := range []struct {
		name      string
		path      string
		root      string
		wantWrite bool
	}{
		{"outside the trace directory", filepath.Join(t.TempDir(), "probe"), "", false},
		{"in the trace directory", filepath.Join(traceDir, "probe"), "", true},
		{"under an existing root", filepath.Join(existingRoot, "probe"), existingRoot, true},
		{"beside an existing root", filepath.Join(filepath.Dir(existingRoot), "probe-beside-root"), existingRoot, false},
		{"beside a missing root", filepath.Join(missingRootParent, "probe"), missingRoot, true},
		{"outside a missing root's parent", filepath.Join(t.TempDir(), "probe"), missingRoot, false},
		{"missing root whose only existing ancestor is the volume", filepath.Join(t.TempDir(), "probe"), volumeChildRoot, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			set := map[string]string{childTraceEnv: filepath.Join(traceDir, "trace.txt"), confineEnv: "1", writeProbeEnv: tc.path}
			if tc.root != "" {
				set[rootEnv] = tc.root
			}
			cmd := exec.Command(os.Args[0])
			cmd.Env = childEnv(set)
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			code := 0
			var exitErr *exec.ExitError
			if err := cmd.Run(); errors.As(err, &exitErr) {
				code = exitErr.ExitCode()
			} else if err != nil {
				t.Fatalf("running the write probe: %v", err)
			}
			if confineFailed(code, stderr.String()) {
				t.Fatalf("child failed to confine writes: %s", stderr.String())
			}
			wantCode := probeDeniedCode
			if tc.wantWrite {
				wantCode = 0
			}
			if code != wantCode {
				t.Errorf("write probe of %q exit code = %d, want %d (0: written, %d: permission denied); stderr: %s", tc.path, code, wantCode, probeDeniedCode, stderr.String())
			}
			_, statErr := os.Stat(tc.path)
			if wrote := statErr == nil; wrote != tc.wantWrite {
				t.Errorf("probe file %q exists: %v (Stat error %v), want %v", tc.path, wrote, statErr, tc.wantWrite)
			}
		})
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
		{"addrepo name https://example.com/repo", cli.Privileged},
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
	wantAllow := []string{"available", "install", "installed", "latest", "listrepos", "remove", "update"}
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
// unelevated, with a failing admin check, against a root with a stale repo
// index cache. Each must succeed without taking the lock, preparing or
// checking the root, write no googet.log and leave every name, byte and
// mtime under the root unchanged. Only the cli.LockFree commands skip the
// admin check too.
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
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			root := readOnlyRoot(t)
			before := testutil.TreeSnapshot(t, root)
			trace := filepath.Join(t.TempDir(), "trace")
			code, stdout, stderr := runGoogetSplit(t, false, []string{childTraceEnv + "=" + trace}, append([]string{"-root", root, "-noconfirm"}, tc.args...)...)
			if code != 0 {
				t.Errorf("unelevated googet %q exit code = %d, want 0; output:\n%s%s", tc.args, code, stdout, stderr)
			}
			if !strings.Contains(stdout, tc.wantOut) {
				t.Errorf("unelevated googet %q printed %q, want it to contain %q", tc.args, stdout, tc.wantOut)
			}
			steps := readTrace(t, trace)
			wantAdminCheck := !tc.lockFree
			if slices.Contains(steps, "isAdmin") != wantAdminCheck || slices.Contains(steps, "obtainLock") {
				t.Errorf("unelevated googet %q run() steps = %q, want isAdmin: %v and no lock", tc.args, steps, wantAdminCheck)
			}
			for _, step := range steps {
				if strings.HasPrefix(step, "prepareRoot") || strings.HasPrefix(step, "checkEntries") {
					t.Errorf("unelevated googet %q called %s, want no root preparation or entry checks", tc.args, step)
				}
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
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			root := readOnlyRoot(t)
			before := testutil.TreeSnapshot(t, root)
			trace := filepath.Join(t.TempDir(), "trace")
			code, out := runGoogetEnv(t, false, []string{childTraceEnv + "=" + trace}, append([]string{"-root", root, "-noconfirm"}, args...)...)
			if code != 1 {
				t.Errorf("unelevated googet %q exit code = %d, want 1; output:\n%s", args, code, out)
			}
			if steps := readTrace(t, trace); !slices.Equal(steps, []string{"isAdmin"}) {
				t.Errorf("unelevated googet %q run() steps = %q, want only isAdmin", args, steps)
			}
			if diff := cmp.Diff(before, testutil.TreeSnapshot(t, root)); diff != "" {
				t.Errorf("unelevated googet %q changed the root (-before +after):\n%s", args, diff)
			}
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
			if tc.privileged {
				skipUnlessElevatedOnWindows(t)
			}
			root := readOnlyRoot(t)
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
