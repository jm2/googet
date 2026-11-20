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

package googetdb

import (
	"math/rand/v2"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/googet/v2/client"
	"github.com/google/googet/v2/goolib"
	"github.com/google/googet/v2/internal/snapshot"
)

// scaledIterations returns n, or a tenth of n (at least 1) on Windows, where a
// SQLite write transaction takes about a hundred times longer on CI runners
// than on Linux ones. It mirrors testutil.ScaledIterations, which this package
// can't import because testutil imports googetdb.
func scaledIterations(n int) int {
	if runtime.GOOS != "windows" {
		return n
	}
	return max(n/10, 1)
}

// openSnapshotTestDB opens a fresh database in a temporary directory.
func openSnapshotTestDB(t *testing.T) *GooDB {
	t.Helper()
	db, err := NewDB(filepath.Join(t.TempDir(), "googet.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// TestPickMatchesFetchPkg checks snapshot.Pick against FetchPkg across
// randomized multi-arch, multi-version database states.
func TestPickMatchesFetchPkg(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	names := []string{"P", "Q", "R", "dotted.pkg"}
	archs := []string{"noarch", "x86_64", "x86_32", "arm64", "arm"}
	vers := []string{"1", "1.0.0", "2.0.0", "1.2.3@4", "10.0.0", "0.0.1-rc1", "notaver"}
	qNames := []string{"P", "Q", "R", "dotted.pkg", "nonexistent", ""}
	qArchs := []string{"", "noarch", "x86_64", "x86_32", "arm64", "arm", "unknown"}
	qVers := []string{"", "1", "1.0.0", "2.0.0", "1.2.3@4", "99.9.9"}

	numStates := scaledIterations(200)
	const queriesPerState = 25
	for i := range numStates {
		db := openSnapshotTestDB(t)
		count := rng.IntN(9)
		var state client.GooGetState
		for range count {
			ps := &goolib.PkgSpec{
				Name:    names[rng.IntN(len(names))],
				Arch:    archs[rng.IntN(len(archs))],
				Version: vers[rng.IntN(len(vers))],
			}
			state = append(state, client.PackageState{PackageSpec: ps})
		}
		if err := db.WriteStateToDB(state); err != nil {
			t.Fatal(err)
		}
		snap, err := db.FetchPkgs("")
		if err != nil {
			t.Fatal(err)
		}
		for q := range queriesPerState {
			pi := goolib.PackageInfo{
				Name: qNames[rng.IntN(len(qNames))],
				Arch: qArchs[rng.IntN(len(qArchs))],
				Ver:  qVers[rng.IntN(len(qVers))],
			}
			want, err := db.FetchPkg(pi)
			if err != nil {
				t.Fatalf("state %d query %d FetchPkg(%+v): %v", i, q, pi, err)
			}
			got, gotOK := snapshot.Pick(pi, snap)
			wantOK := want.PackageSpec != nil
			if gotOK != wantOK {
				t.Errorf("state %d query %d (%+v): Pick ok=%t, want %t", i, q, pi, gotOK, wantOK)
			}
			if diff := cmp.Diff(want, got); diff != "" {
				t.Errorf("state %d query %d (%+v) mismatch (-want +got):\n%s", i, q, pi, diff)
			}
		}
	}
}

// TestLockStateMatchesInstalledLockState checks snapshot.LockState against
// InstalledLockState, whose name lookup is a SQLite LIKE pattern: "_" and "%"
// are wildcards, "_" matches one UTF-8 character, only ASCII letters fold
// case, a backslash is literal, and an empty name selects every package.
func TestLockStateMatchesInstalledLockState(t *testing.T) {
	rng := rand.New(rand.NewPCG(3, 4))
	names := []string{"a_b", "axb", "AXB", "aXb", "a%b", `a\b`, "ab", "é", "É", "éx", "日本", "日x", "x_y", "foo", "Foo", "foo.bar"}
	archs := []string{"noarch", "x86_64", "arm64", "X86_64"}
	queries := []string{"a_b", "A_B", "a%", "%b", "_", "__", "%", "", `a\b`, `a\_b`, "é", "É", "_x", "日_", "_本", "%x%", "foo", "FOO", "foo%", "fo_.bar", "nonexistent", "a%%b", "%_%"}

	numStates := scaledIterations(300)
	for i := range numStates {
		db := openSnapshotTestDB(t)
		var state client.GooGetState
		for range rng.IntN(7) {
			state = append(state, client.PackageState{PackageSpec: &goolib.PkgSpec{
				Name:     names[rng.IntN(len(names))],
				Arch:     archs[rng.IntN(len(archs))],
				Version:  "1",
				LockArch: rng.IntN(2) == 0,
			}})
		}
		if err := db.WriteStateToDB(state); err != nil {
			t.Fatal(err)
		}
		snap, err := db.FetchPkgs("")
		if err != nil {
			t.Fatal(err)
		}
		// The snapshot's order must not matter.
		rng.Shuffle(len(snap), func(a, b int) { snap[a], snap[b] = snap[b], snap[a] })
		for _, q := range queries {
			wantArch, wantLocked, _, _, err := db.InstalledLockState(q)
			if err != nil {
				t.Fatalf("state %d InstalledLockState(%q): %v", i, q, err)
			}
			gotArch, gotLocked := snapshot.LockState(q, snap)
			if gotArch != wantArch || gotLocked != wantLocked {
				t.Errorf("state %d (%v) LockState(%q) = (%q, %t), want (%q, %t)", i, specNames(snap), q, gotArch, gotLocked, wantArch, wantLocked)
			}
		}
	}
}

// specNames returns name.arch.version for each entry of st.
func specNames(st client.GooGetState) []string {
	var out []string
	for _, ps := range st {
		out = append(out, ps.PackageSpec.String())
	}
	return out
}
