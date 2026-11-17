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

package system

import (
	"os/exec"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/googet/v2/goolib"
)

func TestInstallerEnvStripAndAppend(t *testing.T) {
	baseEntries := []string{
		`=C:=C:\work`,
		`=ExitCode=00000000`,
		`=GOOGET_X=1`,
		`ENTRY_WITHOUT_EQUALS`,
		`GOOGET_WITHOUT_EQUALS`,
		`GooGetRoot=C:\ProgramData\GooGet`,
		`GOOGETROOT=C:\ProgramData\GooGet`,
		`GOOGET=bare`,
		`PATH=/usr/bin:/bin`,
		`GOOGET_X=upper`,
		`googet_x=lower`,
		`GooGet_Replaced_By=stale.pkg.1`,
		`GOOGET_ACTION=replace`,
		`googet_action=remove`,
		`GOOGET_PACKAGE=stale.noarch.0`,
		`GOOGET_PREVIOUS_VERSION=0.0.1`,
		`GOOGET_SOURCES=https://repo.example/index`,
		`GOOGET_SHA256=deadbeef`,
		`GOOGET_=empty_key_suffix`,
	}

	keptPrefix := []string{
		`=C:=C:\work`,
		`=ExitCode=00000000`,
		`=GOOGET_X=1`,
		`ENTRY_WITHOUT_EQUALS`,
		`GOOGET_WITHOUT_EQUALS`,
		`GooGetRoot=C:\ProgramData\GooGet`,
		`GOOGETROOT=C:\ProgramData\GooGet`,
		`GOOGET=bare`,
		`PATH=/usr/bin:/bin`,
	}

	tests := []struct {
		name         string
		ic           goolib.InstallerContext
		wantAppended []string
	}{
		{
			name: "install_empty_previous_ignores_replaced_by",
			ic: goolib.InstallerContext{
				Action:          goolib.ActionInstall,
				Package:         "foo.x86_64.1.2.0",
				PreviousVersion: "",
				ReplacedBy:      "ignored.x86_64.9.9.9",
			},
			wantAppended: []string{
				"GOOGET_ACTION=install",
				"GOOGET_PACKAGE=foo.x86_64.1.2.0",
				"GOOGET_PREVIOUS_VERSION=",
			},
		},
		{
			name: "upgrade_includes_previous_version",
			ic: goolib.InstallerContext{
				Action:          goolib.ActionUpgrade,
				Package:         "foo.x86_64.2.0.0",
				PreviousVersion: "1.2.0",
				ReplacedBy:      "ignored.x86_64.9.9.9",
			},
			wantAppended: []string{
				"GOOGET_ACTION=upgrade",
				"GOOGET_PACKAGE=foo.x86_64.2.0.0",
				"GOOGET_PREVIOUS_VERSION=1.2.0",
			},
		},
		{
			name: "replace_appends_replaced_by",
			ic: goolib.InstallerContext{
				Action:          goolib.ActionReplace,
				Package:         "legacy.noarch.1.0.0",
				PreviousVersion: "",
				ReplacedBy:      "modern.noarch.2.0.0",
			},
			wantAppended: []string{
				"GOOGET_ACTION=replace",
				"GOOGET_PACKAGE=legacy.noarch.1.0.0",
				"GOOGET_PREVIOUS_VERSION=",
				"GOOGET_REPLACED_BY=modern.noarch.2.0.0",
			},
		},
		{
			name: "remove_dependent_omits_replaced_by",
			ic: goolib.InstallerContext{
				Action:          goolib.ActionRemoveDependent,
				Package:         "dep.noarch.1.0.0",
				PreviousVersion: "",
				ReplacedBy:      "modern.noarch.2.0.0",
			},
			wantAppended: []string{
				"GOOGET_ACTION=remove-dependent",
				"GOOGET_PACKAGE=dep.noarch.1.0.0",
				"GOOGET_PREVIOUS_VERSION=",
			},
		},
		{
			name: "nul_bytes_removed_from_all_appended_values",
			ic: goolib.InstallerContext{
				Action:          goolib.ActionReplace,
				Package:         "pkg\x00.noarch.\x001.0.0",
				PreviousVersion: "0.\x009.0",
				ReplacedBy:      "rep\x00.noarch.2.0.0\x00",
			},
			wantAppended: []string{
				"GOOGET_ACTION=replace",
				"GOOGET_PACKAGE=pkg.noarch.1.0.0",
				"GOOGET_PREVIOUS_VERSION=0.9.0",
				"GOOGET_REPLACED_BY=rep.noarch.2.0.0",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Give base spare capacity so in-place append bugs would corrupt trailing elements.
			backing := make([]string, len(baseEntries)+4)
			copy(backing, baseEntries)
			for i := len(baseEntries); i < len(backing); i++ {
				backing[i] = "SENTINEL=untouched"
			}
			backingBefore := slices.Clone(backing)
			base := backing[:len(baseEntries)]

			got := installerEnv(base, tc.ic)
			want := append(slices.Clone(keptPrefix), tc.wantAppended...)
			if diff := cmp.Diff(want, got); diff != "" {
				t.Errorf("installerEnv() mismatch (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(backingBefore, backing); diff != "" {
				t.Errorf("installerEnv() mutated base backing array (-want +got):\n%s", diff)
			}

			// Mutating the returned slice must not mutate base.
			got[0] = "MUTATED=1"
			if diff := cmp.Diff(backingBefore, backing); diff != "" {
				t.Errorf("mutating installerEnv() result mutated base (-want +got):\n%s", diff)
			}
		})
	}
}

func TestInstallerEnvLegacyActionsPreserveBase(t *testing.T) {
	base := []string{
		"PATH=/usr/bin",
		"GOOGET_ACTION=inherited",
		"GOOGET_SOURCES=https://repo.example",
	}

	for _, tc := range []struct {
		name   string
		action goolib.Action
	}{
		{name: "unspecified", action: goolib.ActionUnspecified},
		{name: "negative_out_of_range", action: goolib.Action(-1)},
		{name: "positive_out_of_range", action: goolib.Action(99)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := slices.Clone(base)
			got := installerEnv(base, goolib.InstallerContext{
				Action:          tc.action,
				Package:         "pkg.noarch.1.0.0",
				PreviousVersion: "0.9.0",
				ReplacedBy:      "other.noarch.2.0.0",
			})
			if diff := cmp.Diff(before, got); diff != "" {
				t.Errorf("installerEnv() with legacy action %d mismatch (-want +got):\n%s", tc.action, diff)
			}
			got[0] = "MUTATED=1"
			if diff := cmp.Diff(before, base); diff != "" {
				t.Errorf("mutating installerEnv() legacy result mutated base (-want +got):\n%s", diff)
			}
		})
	}
}

func TestWithContextHermeticEnv(t *testing.T) {
	// Intentionally omit t.Parallel because t.Setenv mutates the process environment.
	t.Setenv("GOOGET_ACTION", "stale_replace")
	t.Setenv("googet_extra", "stale_lower")
	t.Setenv("GooGet_Replaced_By", "stale.pkg.1")
	t.Setenv("GOOGET_SOURCES", "https://repo.example")
	t.Setenv("GooGetRoot", "/var/lib/googet-root")

	if got := withContext(nil, goolib.InstallerContext{Action: goolib.ActionInstall}); got != nil {
		t.Fatalf("withContext(nil, ...) = %v, want nil", got)
	}

	for _, tc := range []struct {
		name   string
		action goolib.Action
	}{
		{name: "zero_action", action: goolib.ActionUnspecified},
		{name: "out_of_range_action", action: goolib.Action(42)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command("/bin/sh", "-c", "true")
			got := withContext(cmd, goolib.InstallerContext{
				Action:  tc.action,
				Package: "pkg.noarch.1.0.0",
			})
			if got != cmd {
				t.Fatalf("withContext() returned %p, want %p", got, cmd)
			}
			if cmd.Env != nil {
				t.Errorf("withContext() with action %d set cmd.Env = %v, want nil", tc.action, cmd.Env)
			}
		})
	}

	t.Run("applies_over_configured_cmd_environ", func(t *testing.T) {
		workDir := t.TempDir()
		cmd := exec.Command("/bin/sh", "-c", "true")
		cmd.Dir = workDir

		got := withContext(cmd, goolib.InstallerContext{
			Action:          goolib.ActionUpgrade,
			Package:         "widget.x86_64.2.1.0",
			PreviousVersion: "2.0.0",
			ReplacedBy:      "ignored.x86_64.3.0.0",
		})
		if got != cmd {
			t.Fatalf("withContext() returned %p, want %p", got, cmd)
		}
		if cmd.Env == nil {
			t.Fatal("withContext() left cmd.Env nil, want populated slice")
		}

		var googetVars []string
		var sawGooGetRoot, sawWorkDirPWD bool
		for _, entry := range cmd.Env {
			if entry == "GooGetRoot=/var/lib/googet-root" {
				sawGooGetRoot = true
			}
			if entry == "PWD="+workDir {
				sawWorkDirPWD = true
			}
			if !strings.HasPrefix(entry, "=") {
				if k, _, ok := strings.Cut(entry, "="); ok && strings.HasPrefix(strings.ToUpper(k), "GOOGET_") {
					googetVars = append(googetVars, entry)
				}
			}
		}

		if !sawGooGetRoot {
			t.Errorf("cmd.Env missing GooGetRoot=/var/lib/googet-root: %v", cmd.Env)
		}
		if runtime.GOOS != "windows" && runtime.GOOS != "plan9" && !sawWorkDirPWD {
			t.Errorf("cmd.Env missing PWD=%s synthesized from cmd.Dir: %v", workDir, cmd.Env)
		}
		wantGooGetVars := []string{
			"GOOGET_ACTION=upgrade",
			"GOOGET_PACKAGE=widget.x86_64.2.1.0",
			"GOOGET_PREVIOUS_VERSION=2.0.0",
		}
		if diff := cmp.Diff(wantGooGetVars, googetVars); diff != "" {
			t.Errorf("withContext() GOOGET_* entries mismatch (-want +got):\n%s", diff)
		}
	})

	t.Run("preserves_pseudo_keys_in_explicit_cmd_env", func(t *testing.T) {
		cmd := exec.Command("/bin/sh", "-c", "true")
		cmd.Env = []string{
			`=C:=C:\Users\Admin`,
			`=ExitCode=00000000`,
			`=GOOGET_PSEUDO=keep`,
			`GooGetRoot=C:\ProgramData\GooGet`,
			`GOOGET_ACTION=stale`,
		}
		withContext(cmd, goolib.InstallerContext{
			Action:     goolib.ActionReplace,
			Package:    "old.x86_64.1.0.0",
			ReplacedBy: "new.x86_64.2.0.0",
		})

		for _, wantEntry := range []string{
			`=C:=C:\Users\Admin`,
			`=ExitCode=00000000`,
			`=GOOGET_PSEUDO=keep`,
			`GooGetRoot=C:\ProgramData\GooGet`,
			`GOOGET_ACTION=replace`,
			`GOOGET_PACKAGE=old.x86_64.1.0.0`,
			`GOOGET_PREVIOUS_VERSION=`,
			`GOOGET_REPLACED_BY=new.x86_64.2.0.0`,
		} {
			if !slices.Contains(cmd.Env, wantEntry) {
				t.Errorf("cmd.Env = %v, missing %q", cmd.Env, wantEntry)
			}
		}
		if slices.Contains(cmd.Env, "GOOGET_ACTION=stale") {
			t.Errorf("cmd.Env retained stale GOOGET_ACTION=stale: %v", cmd.Env)
		}
	})
}
