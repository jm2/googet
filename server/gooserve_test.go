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

package main

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/googet/v2/goolib"
	"github.com/google/googet/v2/testutil"
)

// TestRunSyncLogsRelationProblemsAndClosesReaders checks that runSync logs
// fatal relation syntax at Error and warnings at Warning with pkgPath and spec
// context, indexes all packages regardless of relation problems, and closes
// archive file handles so directory cleanup succeeds.
func TestRunSyncLogsRelationProblemsAndClosesReaders(t *testing.T) {
	logs := testutil.CaptureLogs(t)
	rootDir := t.TempDir()
	pkgDir := filepath.Join(rootDir, "packages")
	if err := os.MkdirAll(pkgDir, 0755); err != nil {
		t.Fatal(err)
	}
	dstDir := t.TempDir()

	cleanSpec := goolib.PkgSpec{Name: "clean", Arch: "noarch", Version: "1.0.0"}
	warnSpec := goolib.PkgSpec{Name: "warnpkg", Arch: "noarch", Version: "1.0.0", Conflicts: []string{"old"}, Replaces: []string{"old"}}
	// Arch-qualified operator Conflicts remain fatal syntax because older
	// clients fail closed when an installed package provides the base name.
	fatalSpec := goolib.PkgSpec{Name: "fatalpkg", Arch: "noarch", Version: "1.0.0", Conflicts: []string{"dep.noarch>=2.0"}}

	var gooPaths []string
	for _, ps := range []goolib.PkgSpec{cleanSpec, warnSpec, fatalSpec} {
		rs := testutil.GenGoo(t, pkgDir, dstDir, ps)
		gooPaths = append(gooPaths, filepath.Join(pkgDir, rs.Source))
	}

	if err := runSync(context.Background(), rootDir, "packages"); err != nil {
		t.Fatalf("runSync: %v", err)
	}

	var indexed []string
	for _, rs := range repoContents.rs {
		indexed = append(indexed, rs.PackageSpec.String())
	}
	slices.Sort(indexed)
	wantIndexed := []string{"clean.noarch.1.0.0", "fatalpkg.noarch.1.0.0", "warnpkg.noarch.1.0.0"}
	if diff := cmp.Diff(wantIndexed, indexed); diff != "" {
		t.Errorf("indexed packages (-want +got):\n%s", diff)
	}

	logText := logs.String()
	warnPath := filepath.Join(pkgDir, "warnpkg.noarch.1.0.0.goo")
	fatalPath := filepath.Join(pkgDir, "fatalpkg.noarch.1.0.0.goo")

	var sawWarn, sawFatal bool
	for _, line := range strings.Split(logText, "\n") {
		if strings.HasPrefix(line, "WARN : ") && strings.Contains(line, warnPath) && strings.Contains(line, "warnpkg.noarch.1.0.0") {
			sawWarn = true
		}
		if strings.HasPrefix(line, "ERROR: ") && strings.Contains(line, fatalPath) && strings.Contains(line, "fatalpkg.noarch.1.0.0") {
			sawFatal = true
		}
		if strings.Contains(line, "clean.noarch.1.0.0") && (strings.HasPrefix(line, "WARN : ") || strings.HasPrefix(line, "ERROR: ")) {
			t.Errorf("unexpected warning or error logged for clean package: %s", line)
		}
	}
	if !sawWarn {
		t.Errorf("logs missing WARN line attributed to %s and warnpkg.noarch.1.0.0; logs:\n%s", warnPath, logText)
	}
	if !sawFatal {
		t.Errorf("logs missing ERROR line attributed to %s and fatalpkg.noarch.1.0.0; logs:\n%s", fatalPath, logText)
	}

	// Verify that no open file descriptors still reference pkgDir on systems
	// with /proc/self/fd, and that every .goo file can be removed immediately.
	if entries, err := os.ReadDir("/proc/self/fd"); err == nil {
		for _, entry := range entries {
			target, err := os.Readlink(filepath.Join("/proc/self/fd", entry.Name()))
			if err == nil && strings.HasPrefix(target, pkgDir) {
				t.Errorf("leaked open file descriptor %s -> %s after runSync", entry.Name(), target)
			}
		}
	}
	for _, p := range gooPaths {
		if err := os.Remove(p); err != nil {
			t.Errorf("Remove(%q) after runSync failed: %v", p, err)
		}
	}
}
