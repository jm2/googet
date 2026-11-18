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
	"maps"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/googet/v2/client"
	"github.com/google/googet/v2/googetdb"
	"github.com/google/googet/v2/goolib"
	"github.com/google/googet/v2/internal/ownership"
	"github.com/google/googet/v2/oswrap"
	"github.com/google/googet/v2/remove"
)

func dirAncestorsBetween(root, target string) []string {
	var out []string
	for d := target; d != root && filepath.Dir(d) != d; d = filepath.Dir(d) {
		out = append(out, d)
	}
	slices.Reverse(out)
	return out
}

func isDirNonEmpty(path string) bool {
	entries, err := os.ReadDir(path)
	return err == nil && len(entries) > 0
}

// TestRandomCreatedParentsTreesUpgradeRollbackAndRemove runs 520 seeded
// random directory-tree iterations exercising pre-existing and missing
// intermediates, lone-file and package-directory installs, injected part-way
// MkdirAll and post-walk failures, upgrades with carry-forward, co-owning
// packages, untracked user files, and removals.
func TestRandomCreatedParentsTreesUpgradeRollbackAndRemove(t *testing.T) {
	t.Parallel()
	const iterations = 520

	segments := [][]string{
		{"alpha", "beta", "gamma"},
		{"suite", "runtime", "plugins"},
		{"bin", "lib", "cfg"},
		{"leaf1", "leaf2"},
	}

	baseTmp := t.TempDir()
	for seed := uint64(1); seed <= iterations; seed++ {
		rng := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
		iterRoot := filepath.Join(baseTmp, "s_"+strings.ReplaceAll(filepath.Base(baseTmp), "/", "")+"_"+strconv.FormatUint(seed, 10))
		if err := os.MkdirAll(iterRoot, 0755); err != nil {
			t.Fatalf("seed %d: MkdirAll(iterRoot): %v", seed, err)
		}

		dstRoot := filepath.Join(iterRoot, "dst")
		srcV1 := filepath.Join(iterRoot, "src_v1")
		srcV2 := filepath.Join(iterRoot, "src_v2")
		for _, d := range []string{dstRoot, srcV1, srcV2} {
			if err := os.MkdirAll(d, 0755); err != nil {
				t.Fatalf("seed %d: MkdirAll(%q): %v", seed, d, err)
			}
		}

		// Generate candidate relative directory chains of depth 1..4.
		var candidateDirs []string
		for _, s0 := range segments[0] {
			candidateDirs = append(candidateDirs, s0)
			for _, s1 := range segments[1][:2] {
				p2 := filepath.Join(s0, s1)
				candidateDirs = append(candidateDirs, p2)
				for _, s2 := range segments[2][:2] {
					p3 := filepath.Join(p2, s2)
					candidateDirs = append(candidateDirs, p3)
					p4 := filepath.Join(p3, segments[3][rng.IntN(len(segments[3]))])
					candidateDirs = append(candidateDirs, p4)
				}
			}
		}

		// Pre-create a random prefix-closed subset of directories and optional
		// pre-existing user files.
		preExistingDirs := map[string]bool{dstRoot: true}
		for _, rel := range candidateDirs {
			if rng.IntN(4) == 0 {
				full := filepath.Join(dstRoot, rel)
				if err := os.MkdirAll(full, 0755); err != nil {
					t.Fatalf("seed %d: MkdirAll(pre %q): %v", seed, full, err)
				}
				for _, anc := range dirAncestorsBetween(dstRoot, full) {
					preExistingDirs[anc] = true
				}
				if rng.IntN(3) == 0 {
					uf := filepath.Join(full, "pre_user.txt")
					if err := os.WriteFile(uf, []byte("pre-user"), 0644); err != nil {
						t.Fatalf("seed %d: WriteFile(%q): %v", seed, uf, err)
					}
				}
			}
		}
		preTree := tree(t, dstRoot)

		// Choose 2..4 install mappings for v1.
		type mapping struct {
			srcRel string
			dstAbs string
			isDir  bool
		}
		numMappings := 2 + rng.IntN(3)
		usedDst := map[string]bool{}
		var v1Mappings []mapping
		mappedPayloadDirs := map[string]bool{}
		for mIdx := range numMappings {
			relDir := candidateDirs[rng.IntN(len(candidateDirs))]
			if rng.IntN(2) == 0 {
				dstFile := filepath.Join(dstRoot, relDir, "f_"+strconv.Itoa(mIdx)+".dat")
				if usedDst[dstFile] {
					continue
				}
				usedDst[dstFile] = true
				srcFile := "lone_" + strconv.Itoa(mIdx) + ".dat"
				if err := os.WriteFile(filepath.Join(srcV1, srcFile), []byte("v1-"+srcFile), 0644); err != nil {
					t.Fatalf("seed %d: WriteFile(srcV1): %v", seed, err)
				}
				v1Mappings = append(v1Mappings, mapping{srcRel: srcFile, dstAbs: dstFile, isDir: false})
			} else {
				dstDir := filepath.Join(dstRoot, relDir, "pdir_"+strconv.Itoa(mIdx))
				if usedDst[dstDir] {
					continue
				}
				usedDst[dstDir] = true
				mappedPayloadDirs[dstDir] = true
				srcSub := "pdir_" + strconv.Itoa(mIdx)
				if err := os.MkdirAll(filepath.Join(srcV1, srcSub), 0755); err != nil {
					t.Fatalf("seed %d: MkdirAll(srcSub): %v", seed, err)
				}
				if err := os.WriteFile(filepath.Join(srcV1, srcSub, "item.dat"), []byte("v1-dir-"+srcSub), 0644); err != nil {
					t.Fatalf("seed %d: WriteFile(srcSub/item): %v", seed, err)
				}
				v1Mappings = append(v1Mappings, mapping{srcRel: srcSub, dstAbs: dstDir, isDir: true})
			}
		}

		mode := seed % 5
		switch mode {
		case 0:
			// Inject a part-way MkdirAll failure (either >255-byte component
			// after 2..3 missing levels, or PATH_MAX overflow) and verify full
			// rollback back to preTree.
			var badDst string
			// Windows long paths have no PATH_MAX, so only an over-long
			// component fails there.
			if rng.IntN(2) == 0 || runtime.GOOS == "windows" {
				badDst = filepath.Join(dstRoot, "fail_a_"+strconv.FormatUint(seed, 10), "fail_b", strings.Repeat("x", 300), "bad.dat")
			} else {
				parts := []string{dstRoot, "ovf_a_" + strconv.FormatUint(seed, 10), "ovf_b"}
				for range 22 {
					parts = append(parts, "seg_"+strings.Repeat("q", 200))
				}
				parts = append(parts, "bad.dat")
				badDst = filepath.Join(parts...)
			}
			srcBad := "bad.dat"
			if err := os.WriteFile(filepath.Join(srcV1, srcBad), []byte("bad"), 0644); err != nil {
				t.Fatalf("seed %d: WriteFile(bad): %v", seed, err)
			}
			v1Mappings = append(v1Mappings, mapping{srcRel: srcBad, dstAbs: badDst, isDir: false})

			var u undo
			insFiles := make(map[string]string)
			var walkErr error
			for _, m := range v1Mappings {
				s := filepath.Join(srcV1, m.srcRel)
				if err := oswrap.Walk(s, makeInstallFunction(s, m.dstAbs, insFiles, Options{}, conflictMap{}, &u)); err != nil {
					walkErr = err
					break
				}
			}
			if walkErr == nil {
				t.Fatalf("seed %d: expected part-way MkdirAll error, got nil", seed)
			}
			u.rollback()
			if diff := cmp.Diff(preTree, tree(t, dstRoot)); diff != "" {
				t.Fatalf("seed %d (part-way MkdirAll rollback): dstRoot tree mismatch (-want +got):\n%s", seed, diff)
			}

		case 1:
			// Simulate post-walk script failure and verify rollback restores
			// preTree without leaving created parents behind.
			var u undo
			insFiles := make(map[string]string)
			for _, m := range v1Mappings {
				s := filepath.Join(srcV1, m.srcRel)
				if err := oswrap.Walk(s, makeInstallFunction(s, m.dstAbs, insFiles, Options{}, conflictMap{}, &u)); err != nil {
					t.Fatalf("seed %d: Walk(v1) unexpected error: %v", seed, err)
				}
			}
			u.rollback()
			if diff := cmp.Diff(preTree, tree(t, dstRoot)); diff != "" {
				t.Fatalf("seed %d (script failure rollback): dstRoot tree mismatch (-want +got):\n%s", seed, diff)
			}

		default:
			// Successful v1 install, optional upgrade to v2, and full removal.
			var u1 undo
			v1Files := make(map[string]string)
			for _, m := range v1Mappings {
				s := filepath.Join(srcV1, m.srcRel)
				if err := oswrap.Walk(s, makeInstallFunction(s, m.dstAbs, v1Files, Options{}, conflictMap{}, &u1)); err != nil {
					t.Fatalf("seed %d: Walk(v1) unexpected error: %v", seed, err)
				}
			}
			u1.recordDirs(v1Files)
			u1.commit()

			// Compute exact set of directories created by v1.
			createdSet := map[string]bool{}
			for _, m := range v1Mappings {
				leafDir := m.dstAbs
				if !m.isDir {
					leafDir = filepath.Dir(m.dstAbs)
				}
				for _, d := range dirAncestorsBetween(dstRoot, leafDir) {
					if !preExistingDirs[d] {
						createdSet[d] = true
					}
				}
			}

			// Invariant 1: No pre-existing directory outside mapped payload
			// roots is ever recorded in InstalledFiles.
			for p, sum := range v1Files {
				if sum == "" && preExistingDirs[p] && !mappedPayloadDirs[p] {
					t.Fatalf("seed %d: pre-existing parent %q recorded in v1 InstalledFiles", seed, p)
				}
			}
			for d := range createdSet {
				if sum, ok := v1Files[d]; !ok || sum != "" {
					t.Fatalf("seed %d: created directory %q missing from v1 InstalledFiles: %v", seed, d, v1Files)
				}
			}

			// Optionally drop a post-install untracked user file in one created
			// directory, and optionally co-list one created directory on pkgB.
			var postUserFile string
			var coOwnedDir string
			for d := range createdSet {
				if postUserFile == "" && rng.IntN(3) == 0 {
					postUserFile = filepath.Join(d, "post_user.txt")
					if err := os.WriteFile(postUserFile, []byte("keep-me"), 0644); err != nil {
						t.Fatalf("seed %d: WriteFile(postUserFile): %v", seed, err)
					}
				} else if coOwnedDir == "" && rng.IntN(4) == 0 {
					coOwnedDir = d
				}
			}

			var otherRows client.GooGetState
			if coOwnedDir != "" {
				otherRows = append(otherRows, client.PackageState{
					PackageSpec:    &goolib.PkgSpec{Name: "pkgB", Arch: "noarch", Version: "1"},
					InstalledFiles: map[string]string{coOwnedDir: ""},
				})
			}

			activeFiles := maps.Clone(v1Files)
			if mode >= 3 {
				// Upgrade v1 -> v2 with a fresh destination mapping, checking
				// carry-forward and cleanup.
				relDir2 := candidateDirs[rng.IntN(len(candidateDirs))]
				dstV2File := filepath.Join(dstRoot, relDir2, "v2_only.dat")
				if err := os.WriteFile(filepath.Join(srcV2, "v2_only.dat"), []byte("v2-body"), 0644); err != nil {
					t.Fatalf("seed %d: WriteFile(v2_only): %v", seed, err)
				}
				for _, d := range dirAncestorsBetween(dstRoot, filepath.Dir(dstV2File)) {
					if !preExistingDirs[d] && !createdSet[d] {
						if _, err := os.Stat(d); os.IsNotExist(err) {
							createdSet[d] = true
						}
					}
				}

				var u2 undo
				v2Raw := make(map[string]string)
				s2 := filepath.Join(srcV2, "v2_only.dat")
				if err := oswrap.Walk(s2, makeInstallFunction(s2, dstV2File, v2Raw, Options{}, conflictMap{}, &u2)); err != nil {
					t.Fatalf("seed %d: Walk(v2): %v", seed, err)
				}
				u2.recordDirs(v2Raw)
				u2.commit()

				reader := &countingStateReader{
					state: append(client.GooGetState{{
						PackageSpec:    &goolib.PkgSpec{Name: "pkgA", Arch: "noarch", Version: "1"},
						InstalledFiles: v1Files,
					}}, otherRows...),
				}
				var committedRow client.PackageState
				if err := recordInstall(reader, client.PackageState{
					PackageSpec:    &goolib.PkgSpec{Name: "pkgA", Arch: "noarch", Version: "2"},
					InstalledFiles: v2Raw,
				}, nil, applyStepPolicy(false), func(r client.PackageState, _ []googetdb.FileTransfer) error {
					committedRow = r
					return nil
				}); err != nil {
					t.Fatalf("seed %d: recordInstall(v2): %v", seed, err)
				}
				activeFiles = committedRow.InstalledFiles

				for p, sum := range activeFiles {
					if sum == "" && preExistingDirs[p] && !mappedPayloadDirs[p] {
						t.Fatalf("seed %d: pre-existing parent %q recorded in v2 InstalledFiles", seed, p)
					}
				}
			}

			// Remove pkgA against otherRows.
			ix := ownership.NewIndex(otherRows, ownership.RowID{Name: "pkgA", Arch: "noarch"})
			ownership.Execute(ownership.PlanDelete(activeFiles, ix), "remove", "pkgA.noarch", client.RemoveOrRename, ownership.Logger{})

			// Every pre-existing directory (except an empty mapped payload
			// root) and every pre-existing user file must still exist.
			for preDir := range preExistingDirs {
				if mappedPayloadDirs[preDir] {
					continue
				}
				if _, err := os.Stat(preDir); err != nil {
					t.Fatalf("seed %d: pre-existing directory %q was deleted: %v", seed, preDir, err)
				}
			}
			if postUserFile != "" {
				if _, err := os.Stat(postUserFile); err != nil {
					t.Fatalf("seed %d: untracked user file %q was deleted: %v", seed, postUserFile, err)
				}
			}
			if coOwnedDir != "" {
				if _, err := os.Stat(coOwnedDir); err != nil {
					t.Fatalf("seed %d: co-owned directory %q was deleted while pkgB still owned it: %v", seed, coOwnedDir, err)
				}
			}

			// No created directory may remain on disk unless it is non-empty or
			// kept alive as an ancestor/self of coOwnedDir.
			for d := range createdSet {
				_, err := os.Stat(d)
				if os.IsNotExist(err) {
					continue
				}
				if err != nil {
					t.Fatalf("seed %d: Stat(%q): %v", seed, d, err)
				}
				keptByCoOwner := coOwnedDir != "" && (d == coOwnedDir || strings.HasPrefix(coOwnedDir, d+string(os.PathSeparator)))
				if !isDirNonEmpty(d) && !keptByCoOwner {
					t.Fatalf("seed %d: empty unowned created directory %q remained after remove", seed, d)
				}
			}
		}

		if err := os.RemoveAll(iterRoot); err != nil {
			t.Fatalf("seed %d: cleanup iterRoot: %v", seed, err)
		}
	}
}

// TestRandomCreatedParentsEndToEndSQLite runs 40 seeded end-to-end iterations
// over FromRepo, Reinstall, and remove.All backed by SQLite and .goo archives.
func TestRandomCreatedParentsEndToEndSQLite(t *testing.T) {
	const iterations = 40
	for seed := uint64(1); seed <= iterations; seed++ {
		rng := rand.New(rand.NewPCG(seed+10_000, seed^0x517cc1b727220a95))
		f := newRelFixture(t, nil, nil)

		preDir := filepath.Join(f.dst, "pre_keep")
		if err := os.MkdirAll(preDir, 0755); err != nil {
			t.Fatalf("seed %d: MkdirAll(preDir): %v", seed, err)
		}

		var base string
		if rng.IntN(2) == 0 {
			base = preDir
		} else {
			base = f.dst
		}
		c1 := filepath.Join(base, "c1_"+strconv.FormatUint(seed, 10))
		c2 := filepath.Join(c1, "c2")
		v1Target := filepath.Join(c2, "v1", "app.bin")

		rsV1 := buildRepoArchiveWithFiles(t, f.gooDir, goolib.PkgSpec{
			Name: "P", Arch: "noarch", Version: "1",
			Files: map[string]string{"app.bin": v1Target},
		}, map[string]string{"app.bin": "v1"})
		publishRepoSpec(f, rsV1)
		if err := f.install(t, "P"); err != nil {
			t.Fatalf("seed %d: install(P v1): %v", seed, err)
		}

		if rng.IntN(2) == 0 {
			v2Target := filepath.Join(c2, "v2", "app2.bin")
			rsV2 := buildRepoArchiveWithFiles(t, f.gooDir, goolib.PkgSpec{
				Name: "P", Arch: "noarch", Version: "2",
				Files: map[string]string{"app2.bin": v2Target},
			}, map[string]string{"app2.bin": "v2"})
			publishRepoSpec(f, rsV2)
			if err := f.install(t, "P"); err != nil {
				t.Fatalf("seed %d: upgrade install(P v2): %v", seed, err)
			}
		} else {
			if err := Reinstall(t.Context(), f.rows(t)["P"], false, false, f.downloader, f.db); err != nil {
				t.Fatalf("seed %d: Reinstall(P): %v", seed, err)
			}
		}

		st := f.rows(t)["P"]
		pi := goolib.PackageInfo{Name: "P", Arch: "noarch", Ver: st.PackageSpec.Version}
		if err := remove.All(t.Context(), pi, remove.DepMap{}, false, f.downloader, f.db); err != nil {
			t.Fatalf("seed %d: remove.All(P): %v", seed, err)
		}

		if _, err := os.Stat(c1); !os.IsNotExist(err) {
			t.Fatalf("seed %d: created parent %q remained after remove: %v", seed, c1, err)
		}
		if _, err := os.Stat(preDir); err != nil {
			t.Fatalf("seed %d: pre-existing directory %q removed: %v", seed, preDir, err)
		}
	}
}
