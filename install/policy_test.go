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
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/googet/v2/client"
	"github.com/google/googet/v2/goolib"
	"github.com/google/googet/v2/settings"
	"github.com/google/googet/v2/testutil"
)

func setStrictConflicts(t *testing.T, v bool) {
	t.Helper()
	old := settings.StrictConflicts
	settings.StrictConflicts = v
	t.Cleanup(func() { settings.StrictConflicts = old })
}

func TestOptionsFromSettings(t *testing.T) {
	for _, dbOnly := range []bool{false, true} {
		for _, strict := range []bool{false, true} {
			for _, force := range []bool{false, true} {
				setStrictConflicts(t, strict)
				got := OptionsFromSettings(dbOnly, force)
				want := Options{DBOnly: dbOnly, Force: force, StrictConflicts: strict}
				if got != want {
					t.Errorf("StrictConflicts=%t: OptionsFromSettings(%t, %t) = %+v, want %+v", strict, dbOnly, force, got, want)
				}
			}
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
	owners := map[string]string{filepath.Join(dst, "shared"): "A"}

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
	for _, tc := range []struct {
		name           string
		settingsStrict bool
		optsStrict     bool
		wantErr        bool
	}{
		{name: "permissive Options overrides strict settings", settingsStrict: true, optsStrict: false},
		{name: "strict Options overrides permissive settings", settingsStrict: false, optsStrict: true, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRelFixture(t, nil, []goolib.PkgSpec{{Name: "B", Arch: "noarch", Version: "1"}})
			// Package A owns the destination file that B installs.
			goo := filepath.Join(f.cache, "A.noarch.1.goo")
			writeGoo(t, goo, map[string]string{"x": "from A"})
			ps := &goolib.PkgSpec{
				Name:    "A",
				Arch:    "noarch",
				Version: "1",
				Files:   map[string]string{"x": filepath.Join(f.dst, "B")},
			}
			if _, err := installPkg(goo, ps, Options{}, f.db, func(insFiles map[string]string) error {
				return f.db.AddPkg(client.PackageState{LocalPath: goo, PackageSpec: ps, InstalledFiles: insFiles})
			}); err != nil {
				t.Fatal(err)
			}

			setStrictConflicts(t, tc.settingsStrict)
			ins := New(f.cache, f.db, Options{StrictConflicts: tc.optsStrict})
			err := ins.FromRepo(t.Context(), goolib.PackageInfo{Name: "B"}, "", f.rm, []string{"noarch"}, f.downloader)
			if (err != nil) != tc.wantErr {
				t.Fatalf("Installer.FromRepo error = %v, want error: %t", err, tc.wantErr)
			}
		})
	}
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
