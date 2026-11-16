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
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/googet/v2/client"
	"github.com/google/googet/v2/goolib"
	"github.com/google/googet/v2/internal/ownership"
	"github.com/google/googet/v2/settings"
	"github.com/google/googet/v2/testutil"
)

func setStrictConflicts(t *testing.T, v bool) {
	t.Helper()
	old := settings.StrictConflicts
	settings.StrictConflicts = v
	t.Cleanup(func() { settings.StrictConflicts = old })
}

func setPackageConflicts(t *testing.T, v string) {
	t.Helper()
	old := settings.PackageConflicts
	settings.PackageConflicts = v
	t.Cleanup(func() { settings.PackageConflicts = old })
}

func TestOptionsFromSettings(t *testing.T) {
	for _, dbOnly := range []bool{false, true} {
		for _, strict := range []bool{false, true} {
			for _, force := range []bool{false, true} {
				for _, tc := range []struct {
					conf       string
					want       ConflictMode
					wantErrLog int
				}{
					{conf: "", want: ConflictWarn},
					{conf: "warn", want: ConflictWarn},
					{conf: "error", want: ConflictError},
					{conf: "ignore", want: ConflictIgnore},
					{conf: "bogus", want: ConflictWarn, wantErrLog: 1},
				} {
					name := fmt.Sprintf("dbOnly=%t/strict=%t/force=%t/conf=%s", dbOnly, strict, force, tc.conf)
					t.Run(name, func(t *testing.T) {
						logs := testutil.CaptureLogs(t)
						setStrictConflicts(t, strict)
						setPackageConflicts(t, tc.conf)
						got := OptionsFromSettings(dbOnly, force)
						want := Options{DBOnly: dbOnly, Force: force, StrictConflicts: strict, PackageConflicts: tc.want}
						if got != want {
							t.Errorf("StrictConflicts=%t, PackageConflicts=%q: OptionsFromSettings(%t, %t) = %+v, want %+v", strict, tc.conf, dbOnly, force, got, want)
						}
						if errLogs := strings.Count(logs.String(), "ERROR:"); errLogs != tc.wantErrLog {
							t.Errorf("PackageConflicts=%q: ERROR log count = %d, want %d; logs:\n%s", tc.conf, errLogs, tc.wantErrLog, logs.String())
						}
					})
				}
			}
		}
	}
}

// TestSettingsPackageConflictsContract verifies the cross-package contract
// between settings.Initialize and ParseConflictMode: every value settings
// accepts is accepted by ParseConflictMode, and every other value is normalized
// to "".
func TestSettingsPackageConflictsContract(t *testing.T) {
	t.Cleanup(settings.Snapshot())

	for _, tc := range []struct {
		raw      string
		accepted bool
		wantMode ConflictMode
	}{
		{raw: "warn", accepted: true, wantMode: ConflictWarn},
		{raw: "  WARN  ", accepted: true, wantMode: ConflictWarn},
		{raw: "error", accepted: true, wantMode: ConflictError},
		{raw: "ErRoR", accepted: true, wantMode: ConflictError},
		{raw: "ignore", accepted: true, wantMode: ConflictIgnore},
		{raw: " IGNORE ", accepted: true, wantMode: ConflictIgnore},
		{raw: ""},
		{raw: "   "},
		{raw: "bogus"},
		{raw: "strict"},
		{raw: "false"},
	} {
		dir := t.TempDir()
		yaml := "packageconflicts: \"" + tc.raw + "\"\n"
		if err := os.WriteFile(filepath.Join(dir, "googet.conf"), []byte(yaml), 0644); err != nil {
			t.Fatal(err)
		}
		settings.Initialize(dir, false)
		if !tc.accepted {
			if settings.PackageConflicts != "" {
				t.Errorf("raw %q: settings.PackageConflicts = %q, want empty", tc.raw, settings.PackageConflicts)
			}
			continue
		}
		if settings.PackageConflicts == "" {
			t.Errorf("raw %q: settings.PackageConflicts = empty, want accepted value", tc.raw)
			continue
		}
		got, err := ParseConflictMode(settings.PackageConflicts)
		if err != nil || got != tc.wantMode {
			t.Errorf("raw %q -> settings.PackageConflicts %q: ParseConflictMode = (%v, %v), want (%v, nil)", tc.raw, settings.PackageConflicts, got, err, tc.wantMode)
		}
	}
}

func TestParseConflictMode(t *testing.T) {
	for _, tc := range []struct {
		in      string
		want    ConflictMode
		wantErr bool
	}{
		{in: "warn", want: ConflictWarn},
		{in: "WARN", want: ConflictWarn},
		{in: "  Warn \t", want: ConflictWarn},
		{in: "error", want: ConflictError},
		{in: "ERROR", want: ConflictError},
		{in: "ignore", want: ConflictIgnore},
		{in: "Ignore", want: ConflictIgnore},
		{in: "", wantErr: true},
		{in: "   ", wantErr: true},
		{in: "bogus", wantErr: true},
	} {
		got, err := ParseConflictMode(tc.in)
		if (err != nil) != tc.wantErr {
			t.Errorf("ParseConflictMode(%q) error = %v, want error: %t", tc.in, err, tc.wantErr)
			continue
		}
		if !tc.wantErr && got != tc.want {
			t.Errorf("ParseConflictMode(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
	for _, mode := range []ConflictMode{ConflictWarn, ConflictError, ConflictIgnore} {
		got, err := ParseConflictMode(mode.String())
		if err != nil || got != mode {
			t.Errorf("ParseConflictMode(%q.String()) = %v, %v; want %v, nil", mode, got, err, mode)
		}
	}
}

// TestMakeInstallFunctionUsesOptions checks that the file-conflict check
// follows the Options it is given rather than the global settings.
func TestMakeInstallFunctionUsesOptions(t *testing.T) {
	src := filepath.Join(t.TempDir(), "src")
	dst := t.TempDir()
	if err := os.MkdirAll(src, 0755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(src, "shared")
	if err := os.WriteFile(file, []byte("new"), 0644); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}
	owners := newConflictMap(ownership.NewIndex(client.GooGetState{
		{
			PackageSpec:    &goolib.PkgSpec{Name: "A", Arch: "noarch", Version: "1"},
			InstalledFiles: map[string]string{filepath.Join(dst, "shared"): "chksum"},
		},
	}, ownership.RowID{}), "B")

	for _, tc := range []struct {
		settings bool
		opts     Options
		wantErr  bool
	}{
		{settings: true, opts: Options{}},
		{settings: false, opts: Options{StrictConflicts: true}, wantErr: true},
	} {
		setStrictConflicts(t, tc.settings)
		fn := makeInstallFunction(src, dst, map[string]string{}, tc.opts, owners, &undo{})
		if err := fn(file, fi, nil); (err != nil) != tc.wantErr {
			t.Errorf("StrictConflicts=%t, opts %+v: install function error = %v, want error: %t", tc.settings, tc.opts, err, tc.wantErr)
		}
	}
}

// TestInstallerNewIgnoresSettings checks that New and Installer follow Options
// exclusively and never read settings globals.
func TestInstallerNewIgnoresSettings(t *testing.T) {
	t.Run("permissive Options overrides strict settings", func(t *testing.T) {
		f := newRelFixture(t, nil, []goolib.PkgSpec{{Name: "B", Arch: "noarch", Version: "1"}})
		// Package A owns B's file and also declares a reverse conflict with B.
		goo := filepath.Join(f.cache, "A.noarch.1.goo")
		writeGoo(t, goo, map[string]string{"x": "from A"})
		ps := &goolib.PkgSpec{
			Name:      "A",
			Arch:      "noarch",
			Version:   "1",
			Conflicts: []string{"B"},
			Files:     map[string]string{"x": filepath.Join(f.dst, "B")},
		}
		if _, err := installPkg(goo, ps, Options{}, f.db, func(insFiles map[string]string) error {
			return f.db.AddPkg(client.PackageState{LocalPath: goo, PackageSpec: ps, InstalledFiles: insFiles})
		}); err != nil {
			t.Fatal(err)
		}

		setStrictConflicts(t, true)
		setPackageConflicts(t, "error")

		ins := New(f.cache, f.db, Options{StrictConflicts: false, PackageConflicts: ConflictIgnore})
		if err := ins.FromRepo(t.Context(), goolib.PackageInfo{Name: "B"}, "", f.rm, []string{"noarch"}, f.downloader); err != nil {
			t.Fatalf("Installer.FromRepo with permissive Options failed despite settings: %v", err)
		}
	})

	t.Run("strict Options overrides permissive settings", func(t *testing.T) {
		f := newRelFixture(t,
			[]goolib.PkgSpec{{Name: "A", Arch: "noarch", Version: "1", Conflicts: []string{"B"}}},
			[]goolib.PkgSpec{{Name: "B", Arch: "noarch", Version: "1"}})

		setStrictConflicts(t, false)
		setPackageConflicts(t, "ignore")

		ins := New(f.cache, f.db, Options{StrictConflicts: true, PackageConflicts: ConflictError})
		err := ins.FromRepo(t.Context(), goolib.PackageInfo{Name: "B"}, "", f.rm, []string{"noarch"}, f.downloader)
		if err == nil || !strings.Contains(err.Error(), "conflict") {
			t.Fatalf("Installer.FromRepo with ConflictError Options = %v, want conflict error", err)
		}
	})
}

// TestEntryPointsHonourStrictConflicts checks that FromRepo, FromDisk and
// Reinstall each apply StrictConflicts and -force to the files they install.
func TestEntryPointsHonourStrictConflicts(t *testing.T) {
	// Each case installs B over a file that A owns, and returns the error.
	for _, tc := range []struct {
		name    string
		install func(t *testing.T, f *relFixture, force bool) error
	}{
		{
			name: "FromRepo",
			install: func(t *testing.T, f *relFixture, force bool) error {
				return FromRepo(t.Context(), goolib.PackageInfo{Name: "B"}, "", f.cache, f.rm, []string{"noarch"}, false, force, f.downloader, f.db)
			},
		},
		{
			name: "FromDisk",
			install: func(t *testing.T, f *relFixture, force bool) error {
				dir := t.TempDir()
				rs := testutil.GenGoo(t, dir, f.dst, goolib.PkgSpec{Name: "B", Arch: "noarch", Version: "1"})
				return FromDisk(filepath.Join(dir, rs.Source), f.cache, false, force, false, f.db)
			},
		},
		{
			name: "Reinstall",
			install: func(t *testing.T, f *relFixture, force bool) error {
				dir := t.TempDir()
				rs := testutil.GenGoo(t, dir, f.dst, goolib.PkgSpec{Name: "B", Arch: "noarch", Version: "1"})
				ps, err := f.db.FetchPkg(goolib.PackageInfo{Name: "A", Arch: "noarch"})
				if err != nil {
					t.Fatal(err)
				}
				ps.PackageSpec = rs.PackageSpec
				ps.LocalPath = filepath.Join(dir, rs.Source)
				ps.Checksum = ""
				return Reinstall(t.Context(), ps, false, force, f.downloader, f.db)
			},
		},
	} {
		for _, strict := range []bool{false, true} {
			for _, force := range []bool{false, true} {
				f := newRelFixture(t, nil, []goolib.PkgSpec{{Name: "B", Arch: "noarch", Version: "1"}})
				// A owns the file that B installs.
				f.installArchive(t, "A", map[string]string{"x": "from A"}, map[string]string{"x": filepath.Join(f.dst, "B")})
				setStrictConflicts(t, strict)
				err := tc.install(t, f, force)
				wantErr := strict && !force
				if (err != nil) != wantErr {
					t.Errorf("%s with StrictConflicts=%t, force=%t: error = %v, want error: %t", tc.name, strict, force, err, wantErr)
				} else if wantErr && !strings.Contains(err.Error(), "already owned by package A") {
					t.Errorf("%s with StrictConflicts=%t, force=%t: error = %v, want a file conflict with A", tc.name, strict, force, err)
				}
			}
		}
	}
}
