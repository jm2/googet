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
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode"

	"github.com/google/googet/v2/client"
	"github.com/google/googet/v2/googetdb"
	"github.com/google/googet/v2/goolib"
	"github.com/google/googet/v2/internal/ownership"
	"github.com/google/googet/v2/remove"
	"github.com/google/googet/v2/settings"
)

func randomizeWindowsSpelling(rng *rand.Rand, canonical string) string {
	var b strings.Builder
	b.Grow(len(canonical))
	for _, r := range canonical {
		if r == '\\' {
			// Keep drive root backslash after ':' valid while varying others.
			if rng.IntN(3) == 0 && b.Len() > 2 {
				b.WriteByte('/')
			} else {
				b.WriteByte('\\')
			}
			continue
		}
		if unicode.IsLetter(r) {
			if rng.IntN(2) == 0 {
				b.WriteRune(unicode.ToLower(r))
			} else {
				b.WriteRune(unicode.ToUpper(r))
			}
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

type simWindowsDisk struct {
	files map[string]bool
	dirs  map[string]bool
}

func newSimWindowsDisk() *simWindowsDisk {
	return &simWindowsDisk{
		files: make(map[string]bool),
		dirs:  make(map[string]bool),
	}
}

func (d *simWindowsDisk) mkdirParents(key string) {
	parts := strings.Split(key, `\`)
	for i := 2; i <= len(parts); i++ {
		d.dirs[strings.Join(parts[:i], `\`)] = true
	}
}

func (d *simWindowsDisk) addEntry(rawPath string, isDir bool) {
	k := ownership.FoldWindows(rawPath)
	if isDir {
		d.mkdirParents(k)
		return
	}
	idx := strings.LastIndexByte(k, '\\')
	if idx > 2 {
		d.mkdirParents(k[:idx])
	}
	d.files[k] = true
}

func (d *simWindowsDisk) hasChildren(dirKey string) bool {
	prefix := dirKey + `\`
	for fk := range d.files {
		if strings.HasPrefix(fk, prefix) {
			return true
		}
	}
	for dk := range d.dirs {
		if dk != dirKey && strings.HasPrefix(dk, prefix) {
			return true
		}
	}
	return false
}

func (d *simWindowsDisk) removeEntry(rawPath string) error {
	k := ownership.FoldWindows(rawPath)
	if d.files[k] {
		delete(d.files, k)
		return nil
	}
	if d.dirs[k] {
		if d.hasChildren(k) {
			return errors.New("directory not empty")
		}
		delete(d.dirs, k)
		return nil
	}
	return nil
}

func (d *simWindowsDisk) exists(rawPath string) bool {
	k := ownership.FoldWindows(rawPath)
	return d.files[k] || d.dirs[k]
}

// TestRandomOwnershipCascadesAndUpgradesWindowsRule exercises 550 seeded
// random co-listing states, upgrades, and dependency-cascade removals under
// WindowsRule, checking that no still-owned path is ever deleted and every
// unowned file and empty unowned directory is removed.
func TestRandomOwnershipCascadesAndUpgradesWindowsRule(t *testing.T) {
	t.Parallel()
	const iterations = 550

	canonicalDirs := []string{
		`C:\App`,
		`C:\App\Bin`,
		`C:\App\Bin\Plugins`,
		`C:\Shared`,
		`C:\Shared\Sub`,
		`C:\Vendor`,
		`C:\Vendor\Tools`,
	}
	canonicalFiles := []string{
		`C:\App\app.exe`,
		`C:\App\Bin\core.dll`,
		`C:\App\Bin\Plugins\ext.dll`,
		`C:\Shared\common.dll`,
		`C:\Shared\Sub\data.json`,
		`C:\Vendor\Tools\helper.exe`,
		`C:\Vendor\readme.txt`,
	}

	type rowSpec struct {
		name string
		arch string
	}
	pool := []rowSpec{
		{name: "pkgA", arch: "noarch"},
		{name: "pkgA", arch: "x86_64"},
		{name: "pkgB", arch: "noarch"},
		{name: "pkgC", arch: "noarch"},
		{name: "pkgD", arch: "noarch"},
		{name: "pkgE", arch: "x86_64"},
	}

	for seed := uint64(0); seed < iterations; seed++ {
		rng := rand.New(rand.NewPCG(seed+1, 0x9e3779b97f4a7c15))
		disk := newSimWindowsDisk()
		legacyFiles := make(map[string]bool)
		for _, f := range canonicalFiles {
			if rng.IntN(5) == 0 {
				legacyFiles[ownership.FoldWindows(f)] = true
			}
		}

		numRows := 3 + rng.IntN(len(pool)-2)
		var state client.GooGetState
		for i := 0; i < numRows; i++ {
			rs := pool[i]
			deps := make(map[string]string)
			for j := 0; j < i; j++ {
				if pool[j].name != rs.name && rng.IntN(3) == 0 {
					deps[pool[j].name+"."+pool[j].arch] = "1"
				}
			}

			ins := make(map[string]string)
			for _, cd := range canonicalDirs {
				if rng.IntN(2) == 0 {
					spelled := randomizeWindowsSpelling(rng, cd)
					ins[spelled] = ""
					disk.addEntry(spelled, true)
				}
			}
			for _, cf := range canonicalFiles {
				if rng.IntN(2) == 0 {
					spelled := randomizeWindowsSpelling(rng, cf)
					fk := ownership.FoldWindows(cf)
					sum := "sum-identical"
					if legacyFiles[fk] {
						sum = ""
					} else if rng.IntN(2) == 0 {
						sum = fmt.Sprintf("sum-%s-%s-%d", rs.name, rs.arch, i)
					}
					ins[spelled] = sum
					disk.addEntry(spelled, false)
				}
			}

			state = append(state, client.PackageState{
				PackageSpec: &goolib.PkgSpec{
					Name:            rs.name,
					Arch:            rs.arch,
					Version:         "1",
					PkgDependencies: deps,
				},
				InstalledFiles: ins,
			})
		}

		assertSurvivingOwned := func(phase string) {
			for _, ps := range state {
				for p := range ps.InstalledFiles {
					if !disk.exists(p) {
						t.Fatalf("seed %d (%s): path %q still owned by %s.%s was deleted", seed, phase, p, ps.PackageSpec.Name, ps.PackageSpec.Arch)
					}
				}
			}
		}

		// Perform two random package upgrades before running cascade removal.
		for up := 0; up < 2; up++ {
			idx := rng.IntN(len(state))
			oldRow := state[idx]
			rowID := ownership.RowID{Name: oldRow.PackageSpec.Name, Arch: oldRow.PackageSpec.Arch}

			v2Files := make(map[string]string)
			for _, cd := range canonicalDirs {
				if rng.IntN(2) == 0 {
					spelled := randomizeWindowsSpelling(rng, cd)
					v2Files[spelled] = ""
					disk.addEntry(spelled, true)
				}
			}
			for _, cf := range canonicalFiles {
				if rng.IntN(2) == 0 {
					spelled := randomizeWindowsSpelling(rng, cf)
					v2Files[spelled] = fmt.Sprintf("sum-v2-%d", up)
					disk.addEntry(spelled, false)
				}
			}

			ix := ownership.NewIndexWithRule(state, rowID, ownership.WindowsRule)
			dropped := ownership.Dropped(oldRow.InstalledFiles, v2Files, ix)
			plan := planCleanup(oldRow.InstalledFiles, v2Files, ix)
			for _, f := range plan.Files {
				_ = disk.removeEntry(f)
			}
			for _, d := range plan.Dirs {
				_ = disk.removeEntry(d)
			}

			newSpec := *oldRow.PackageSpec
			newSpec.Version = fmt.Sprintf("%d", up+2)
			state[idx] = client.PackageState{
				PackageSpec:    &newSpec,
				InstalledFiles: v2Files,
			}

			assertSurvivingOwned(fmt.Sprintf("upgrade-%d", up))
			allAfter := ownership.NewIndexWithRule(state, ownership.RowID{}, ownership.WindowsRule)
			for p := range dropped {
				if len(allAfter.Owners(p)) > 0 {
					continue
				}
				k := ownership.FoldWindows(p)
				if disk.files[k] {
					t.Fatalf("seed %d (upgrade-%d): unowned dropped file %q still on disk", seed, up, p)
				}
				if disk.dirs[k] && !disk.hasChildren(k) {
					t.Fatalf("seed %d (upgrade-%d): empty unowned dropped dir %q still on disk", seed, up, p)
				}
			}
		}

		// Execute a dependency cascade removal starting from a random root row.
		rootSpec := state[rng.IntN(len(state))].PackageSpec
		rootPI := goolib.PackageInfo{Name: rootSpec.Name, Arch: rootSpec.Arch}
		depOrder, err := remove.Plan(rootPI, state)
		if err != nil {
			t.Fatalf("seed %d: remove.Plan(%v): %v", seed, rootPI, err)
		}
		cascade := append(depOrder, rootPI.Name+"."+rootPI.Arch)

		for stepIdx, key := range cascade {
			dot := strings.LastIndexByte(key, '.')
			target := ownership.RowID{Name: key[:dot], Arch: key[dot+1:]}
			rowPos := -1
			for i, ps := range state {
				if ps.PackageSpec.Name == target.Name && ps.PackageSpec.Arch == target.Arch {
					rowPos = i
					break
				}
			}
			if rowPos < 0 {
				continue
			}
			removing := state[rowPos]
			ix := ownership.NewIndexWithRule(state, target, ownership.WindowsRule)
			plan := ownership.PlanDelete(removing.InstalledFiles, ix)
			for _, f := range plan.Files {
				_ = disk.removeEntry(f)
			}
			for _, d := range plan.Dirs {
				_ = disk.removeEntry(d)
			}
			state = append(state[:rowPos], state[rowPos+1:]...)

			phase := fmt.Sprintf("cascade-step-%d-%s", stepIdx, key)
			assertSurvivingOwned(phase)
			survivingIx := ownership.NewIndexWithRule(state, ownership.RowID{}, ownership.WindowsRule)
			for p := range removing.InstalledFiles {
				if len(survivingIx.Owners(p)) > 0 {
					continue
				}
				k := ownership.FoldWindows(p)
				if disk.files[k] {
					t.Fatalf("seed %d (%s): unowned removed file %q remained on disk", seed, phase, p)
				}
				if disk.dirs[k] && !disk.hasChildren(k) {
					t.Fatalf("seed %d (%s): empty unowned removed dir %q remained on disk", seed, phase, p)
				}
			}
		}
	}
}

// TestRandomOwnershipRealFilesystemAndSQLite exercises 60 seeded random
// co-listing, recordInstall upgrade, and remove.All cascade scenarios against a
// real temporary directory and SQLite database.
func TestRandomOwnershipRealFilesystemAndSQLite(t *testing.T) {
	const iterations = 60
	baseRoot := t.TempDir()
	downloader, err := client.NewDownloader("")
	if err != nil {
		t.Fatalf("client.NewDownloader: %v", err)
	}

	relDirs := []string{"app", "app/sub", "shared"}
	relFiles := []string{"app/bin", "app/sub/plugin", "shared/common", "shared/config"}

	for seed := uint64(0); seed < iterations; seed++ {
		rng := rand.New(rand.NewPCG(seed+1001, 0x517cc1b727220a95))
		iterDir := filepath.Join(baseRoot, fmt.Sprintf("iter-%d", seed))
		stateRoot := filepath.Join(iterDir, "state")
		cacheDir := filepath.Join(iterDir, "cache")
		dstDir := filepath.Join(iterDir, "dst")
		for _, d := range []string{stateRoot, cacheDir, dstDir} {
			if err := os.MkdirAll(d, 0755); err != nil {
				t.Fatal(err)
			}
		}

		settings.Initialize(stateRoot, false)
		db, err := googetdb.NewDB(settings.DBFile())
		if err != nil {
			t.Fatalf("seed %d: NewDB: %v", seed, err)
		}

		names := []string{"P0", "P1", "P2", "P3"}
		var initial client.GooGetState
		for i, name := range names {
			deps := make(map[string]string)
			for j := 0; j < i; j++ {
				if rng.IntN(2) == 0 {
					deps[names[j]] = "1"
				}
			}
			ins := make(map[string]string)
			for _, rd := range relDirs {
				if rng.IntN(2) == 0 {
					full := filepath.Join(dstDir, rd)
					if err := os.MkdirAll(full, 0755); err != nil {
						t.Fatal(err)
					}
					ins[full] = ""
				}
			}
			for _, rf := range relFiles {
				if rng.IntN(2) == 0 {
					full := filepath.Join(dstDir, rf)
					if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(full, []byte(fmt.Sprintf("seed-%d-%s", seed, name)), 0644); err != nil {
						t.Fatal(err)
					}
					ins[full] = fmt.Sprintf("sum-%s", name)
				}
			}
			spec := goolib.PkgSpec{
				Name:            name,
				Arch:            "noarch",
				Version:         "1",
				PkgDependencies: deps,
			}
			goo := filepath.Join(cacheDir, spec.String()+".goo")
			writeGooArchiveWithEntries(t, goo, spec, nil)
			initial = append(initial, client.PackageState{
				PackageSpec:    &spec,
				LocalPath:      goo,
				InstalledFiles: ins,
			})
		}
		if err := db.WriteStateToDB(initial); err != nil {
			db.Close()
			t.Fatalf("seed %d: WriteStateToDB: %v", seed, err)
		}

		// Upgrade P0 via recordInstall with a random subset of files.
		v2Ins := make(map[string]string)
		for _, rf := range relFiles {
			if rng.IntN(2) == 0 {
				full := filepath.Join(dstDir, rf)
				if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(full, []byte("v2"), 0644); err != nil {
					t.Fatal(err)
				}
				v2Ins[full] = "sum-v2"
			}
		}
		specV2 := goolib.PkgSpec{Name: "P0", Arch: "noarch", Version: "2"}
		gooV2 := filepath.Join(cacheDir, specV2.String()+".goo")
		writeGooArchiveWithEntries(t, gooV2, specV2, nil)
		rowV2 := client.PackageState{
			PackageSpec:    &specV2,
			LocalPath:      gooV2,
			InstalledFiles: v2Ins,
		}
		if err := recordInstall(db, rowV2, nil, applyStepPolicy(false), db.AddPkgWithTransfers); err != nil {
			db.Close()
			t.Fatalf("seed %d: recordInstall(P0 v2): %v", seed, err)
		}

		// Cascade-remove a random package via EnumerateDeps and remove.All.
		targetName := names[rng.IntN(len(names))]
		pi := goolib.PackageInfo{Name: targetName, Arch: "noarch"}
		deps, _, err := remove.EnumerateDeps(pi, db)
		if err != nil {
			db.Close()
			t.Fatalf("seed %d: EnumerateDeps(%s): %v", seed, targetName, err)
		}
		if err := remove.All(t.Context(), pi, deps, false, downloader, db); err != nil {
			db.Close()
			t.Fatalf("seed %d: remove.All(%s): %v", seed, targetName, err)
		}

		surviving, err := db.FetchPkgs("")
		db.Close()
		if err != nil {
			t.Fatalf("seed %d: FetchPkgs: %v", seed, err)
		}

		for _, ps := range surviving {
			for p := range ps.InstalledFiles {
				if _, statErr := os.Lstat(p); statErr != nil {
					t.Fatalf("seed %d: path %q still owned by %s missing on disk: %v", seed, p, ps.PackageSpec, statErr)
				}
			}
		}

		survivingIx := ownership.NewIndex(surviving, ownership.RowID{})
		for _, rf := range relFiles {
			full := filepath.Join(dstDir, rf)
			if len(survivingIx.Owners(full)) == 0 {
				if _, statErr := os.Lstat(full); !os.IsNotExist(statErr) {
					// Only check files that were tracked by at least one initial or v2 row.
					wasTracked := rowV2.InstalledFiles[full] != ""
					for _, initPS := range initial {
						if _, ok := initPS.InstalledFiles[full]; ok {
							wasTracked = true
						}
					}
					if wasTracked {
						t.Fatalf("seed %d: unowned tracked file %q still exists on disk", seed, full)
					}
				}
			}
		}
	}
}
