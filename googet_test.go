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
	"flag"
	"io/ioutil"
	"path/filepath"
	"testing"

	"github.com/google/googet/v2/oswrap"
	"github.com/google/subcommands"
)

func TestRotateLog(t *testing.T) {
	tempDir, err := ioutil.TempDir("", "")
	if err != nil {
		t.Fatalf("error creating temp directory: %v", err)
	}
	defer oswrap.RemoveAll(tempDir)

	table := []struct {
		name    string
		size    int64
		rotated bool
	}{
		{"test1.log", 10 * 1024, true},
		{"test2.log", 9 * 1024, false},
	}

	for _, tt := range table {
		logPath := filepath.Join(tempDir, tt.name)
		f, err := oswrap.Create(logPath)
		if err != nil {
			t.Fatalf("error creating log file: %v", err)
		}

		if err := f.Truncate(tt.size); err != nil {
			t.Fatalf("error truncating log file: %v", err)
		}

		if err := f.Close(); err != nil {
			t.Fatalf("error closing log file: %v", err)
		}

		if err := rotateLog(logPath, 10*1024); err != nil {
			t.Errorf("error running rotateLog: %v", err)
		}

		switch tt.rotated {
		case true:
			if _, err := oswrap.Stat(logPath); err == nil {
				t.Error("rotateLog did not rotate log as expected, old log file still exists")
			}
			if _, err := oswrap.Stat(logPath + ".old"); err != nil {
				t.Error("rotateLog did not rotate log as expected, .old file does not exist")
			}
		case false:
			if _, err := oswrap.Stat(logPath); err != nil {
				t.Error("rotateLog rotated a log we didn't expect")
			}
		}
	}
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

// TestInstallAndUpdatePackageConflictsFlagParity checks that install and
// update register -package_conflicts and -force with identical defaults and
// usage strings.
func TestInstallAndUpdatePackageConflictsFlagParity(t *testing.T) {
	flagsByCmd := make(map[string]*flag.FlagSet)
	subcommands.DefaultCommander.VisitCommands(func(_ *subcommands.CommandGroup, cmd subcommands.Command) {
		switch cmd.Name() {
		case "install", "update":
			fs := flag.NewFlagSet(cmd.Name(), flag.ContinueOnError)
			cmd.SetFlags(fs)
			flagsByCmd[cmd.Name()] = fs
		}
	})
	if len(flagsByCmd) != 2 {
		t.Fatalf("found commands %v, want install and update", flagsByCmd)
	}
	instPC := flagsByCmd["install"].Lookup("package_conflicts")
	updPC := flagsByCmd["update"].Lookup("package_conflicts")
	if instPC == nil || updPC == nil {
		t.Fatalf("package_conflicts lookup: install=%v, update=%v; want both non-nil", instPC, updPC)
	}
	if instPC.DefValue != "warn" || updPC.DefValue != "warn" {
		t.Errorf("DefValue: install=%q, update=%q; want warn for both", instPC.DefValue, updPC.DefValue)
	}
	if instPC.Usage != updPC.Usage {
		t.Errorf("Usage mismatch:\ninstall: %q\nupdate:  %q", instPC.Usage, updPC.Usage)
	}
	instForce := flagsByCmd["install"].Lookup("force")
	updForce := flagsByCmd["update"].Lookup("force")
	if instForce == nil || updForce == nil || instForce.Usage != updForce.Usage {
		t.Errorf("force Usage mismatch: install=%v, update=%v", instForce, updForce)
	}
}
