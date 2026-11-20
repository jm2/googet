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

package remove

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/googet/v2/client"
	"github.com/google/googet/v2/googetdb"
	"github.com/google/googet/v2/goolib"
)

// oldRemovalOrder is removalOrder as it was before removalOrderSet existed,
// kept verbatim as the reference for TestRemovalOrderMatchesOld.
func oldRemovalOrder(deps DepMap, root string) (order, rootCycle []string, err error) {
	pending := make(DepMap, len(deps))
	for pkg, dependents := range deps {
		pending[pkg] = slices.Clone(dependents)
		// A dependent that isn't a key would otherwise never be removed.
		for _, d := range dependents {
			if _, ok := deps[d]; !ok {
				pending[d] = nil
			}
		}
	}
	if _, ok := pending[root]; ok {
		for _, pkg := range slices.Sorted(maps.Keys(pending)) {
			if slices.Contains(pending[pkg], root) {
				rootCycle = append(rootCycle, pkg)
			}
		}
		pending.remove(root)
	}
	for len(pending) > 0 {
		var ready []string
		for pkg, dependents := range pending {
			if len(dependents) == 0 {
				ready = append(ready, pkg)
			}
		}
		if len(ready) == 0 {
			remaining := slices.Sorted(maps.Keys(pending))
			return nil, nil, fmt.Errorf("cannot remove %s: these packages depend on each other in a cycle: %s; remove one of them first", root, strings.Join(remaining, ", "))
		}
		slices.Sort(ready)
		for _, pkg := range ready {
			pending.remove(pkg)
		}
		order = append(order, ready...)
	}
	return order, rootCycle, nil
}

// TestRemovalOrderMatchesOld checks that removalOrder, now built on
// removalOrderSet, orders random dependency maps exactly as before, including
// maps with cycles through the root and elsewhere.
func TestRemovalOrderMatchesOld(t *testing.T) {
	rng := rand.New(rand.NewPCG(11, 12))
	names := []string{"R.noarch", "A.noarch", "B.noarch", "C.noarch", "D.noarch", "E.noarch"}
	for i := range 2000 {
		deps := DepMap{}
		for _, n := range names[:2+rng.IntN(len(names)-1)] {
			if rng.IntN(5) == 0 && n != "R.noarch" {
				continue
			}
			var dependents []string
			for _, d := range names {
				if d != n && rng.IntN(4) == 0 {
					dependents = append(dependents, d)
				}
			}
			deps[n] = dependents
		}
		root := names[rng.IntN(2)]
		wantOrder, wantCycle, wantErr := oldRemovalOrder(deps, root)
		gotOrder, gotCycle, gotErr := removalOrder(deps, root)
		if fmt.Sprint(gotErr) != fmt.Sprint(wantErr) || !slices.Equal(gotOrder, wantOrder) || !slices.Equal(gotCycle, wantCycle) {
			t.Fatalf("case %d removalOrder(%v, %s) = %v, %v, %v; want %v, %v, %v", i, deps, root, gotOrder, gotCycle, gotErr, wantOrder, wantCycle, wantErr)
		}
	}
}

// TestRemovalOrderSet checks how removalOrderSet orders several roots.
func TestRemovalOrderSet(t *testing.T) {
	for _, tc := range []struct {
		name      string
		deps      DepMap
		roots     []string
		want      []string
		wantCycle []string
		wantErr   string
	}{
		{
			name:  "a root that depends on another root comes first",
			deps:  DepMap{"A.noarch": {"B.noarch", "D.noarch"}, "B.noarch": nil, "D.noarch": nil},
			roots: []string{"A.noarch", "B.noarch"},
			want:  []string{"B.noarch", "D.noarch", "A.noarch"},
		},
		{
			name:      "a dependent that a root depends on is still removed first",
			deps:      DepMap{"A.noarch": {"D.noarch"}, "D.noarch": {"A.noarch"}},
			roots:     []string{"A.noarch"},
			want:      []string{"D.noarch", "A.noarch"},
			wantCycle: []string{"D.noarch"},
		},
		{
			name:  "roots in a cycle are removed in sorted order",
			deps:  DepMap{"A.noarch": {"B.noarch"}, "B.noarch": {"A.noarch"}},
			roots: []string{"B.noarch", "A.noarch"},
			want:  []string{"A.noarch", "B.noarch"},
		},
		{
			name:  "a root outside a cycle of roots still comes after a root that depends on it",
			deps:  DepMap{"A.noarch": {"B.noarch"}, "B.noarch": {"C.noarch"}, "C.noarch": {"B.noarch"}},
			roots: []string{"A.noarch", "B.noarch", "C.noarch"},
			want:  []string{"B.noarch", "A.noarch", "C.noarch"},
		},
		{
			name:    "other packages in a cycle are an error",
			deps:    DepMap{"A.noarch": {"C.noarch"}, "C.noarch": {"D.noarch"}, "D.noarch": {"C.noarch"}},
			roots:   []string{"A.noarch"},
			wantErr: "cannot remove A.noarch: these packages depend on each other in a cycle: C.noarch, D.noarch; remove one of them first",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := maps.Clone(tc.deps)
			got, cycle, err := removalOrderSet(tc.deps, tc.roots)
			if tc.wantErr != "" {
				if err == nil || err.Error() != tc.wantErr {
					t.Fatalf("removalOrderSet = %v, want error %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("removalOrderSet: %v", err)
			}
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("order (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(tc.wantCycle, cycle); diff != "" {
				t.Errorf("rootCycle (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(before, tc.deps); diff != "" {
				t.Errorf("deps modified (-before +after):\n%s", diff)
			}
		})
	}
}

// TestLocalArchive checks where LocalArchive finds an installed package's
// archive, when it downloads it again, and its errors.
func TestLocalArchive(t *testing.T) {
	content := []byte("archive contents")
	sum := sha256.Sum256(content)
	checksum := hex.EncodeToString(sum[:])
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/pkg.goo" {
			http.NotFound(w, r)
			return
		}
		w.Write(content)
	}))
	t.Cleanup(srv.Close)
	downloader, err := client.NewDownloader("")
	if err != nil {
		t.Fatal(err)
	}
	spec := &goolib.PkgSpec{Name: "foo", Arch: "noarch", Version: "1"}
	for _, tc := range []struct {
		name     string
		existing []byte // When nil, no file is written at the archive path.
		state    func(dir string) client.PackageState
		wantPath func(dir string) string
		wantErr  string
	}{
		{
			name: "missing package spec",
			state: func(dir string) client.PackageState {
				return client.PackageState{}
			},
			wantErr: "missing package spec",
		},
		{
			name:     "archive present",
			existing: content,
			state: func(dir string) client.PackageState {
				return client.PackageState{PackageSpec: spec, LocalPath: filepath.Join(dir, "a.goo"), Checksum: checksum}
			},
			wantPath: func(dir string) string { return filepath.Join(dir, "a.goo") },
		},
		{
			name: "archive missing is downloaded again",
			state: func(dir string) client.PackageState {
				return client.PackageState{PackageSpec: spec, LocalPath: filepath.Join(dir, "a.goo"), Checksum: checksum, DownloadURL: srv.URL + "/pkg.goo"}
			},
			wantPath: func(dir string) string { return filepath.Join(dir, "a.goo") },
		},
		{
			name:     "checksum mismatch is downloaded again",
			existing: []byte("corrupt"),
			state: func(dir string) client.PackageState {
				return client.PackageState{PackageSpec: spec, LocalPath: filepath.Join(dir, "a.goo"), Checksum: checksum, DownloadURL: srv.URL + "/pkg.goo"}
			},
			wantPath: func(dir string) string { return filepath.Join(dir, "a.goo") },
		},
		{
			name:     "older GooGet records only the unpack directory",
			existing: content,
			state: func(dir string) client.PackageState {
				return client.PackageState{PackageSpec: spec, UnpackDir: filepath.Join(dir, "a"), Checksum: checksum}
			},
			wantPath: func(dir string) string { return filepath.Join(dir, "a.goo") },
		},
		{
			name: "no path recorded",
			state: func(dir string) client.PackageState {
				return client.PackageState{PackageSpec: spec}
			},
			wantErr: `no local path available for package "foo"`,
		},
		{
			name: "archive missing and no download URL",
			state: func(dir string) client.PackageState {
				return client.PackageState{PackageSpec: spec, LocalPath: filepath.Join(dir, "a.goo"), Checksum: checksum}
			},
			wantErr: "can not redownload foo.noarch.1, DownloadURL not saved",
		},
		{
			name: "archive missing and gone from the repo",
			state: func(dir string) client.PackageState {
				return client.PackageState{PackageSpec: spec, LocalPath: filepath.Join(dir, "a.goo"), Checksum: checksum, DownloadURL: srv.URL + "/gone.goo"}
			},
			wantErr: "error redownloading foo.noarch.1, package may no longer exist in the repo",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if tc.existing != nil {
				if err := os.WriteFile(filepath.Join(dir, "a.goo"), tc.existing, 0644); err != nil {
					t.Fatal(err)
				}
			}
			got, err := LocalArchive(t.Context(), tc.state(dir), downloader)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("LocalArchive = %q, %v; want error containing %q", got, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("LocalArchive: %v", err)
			}
			if want := tc.wantPath(dir); got != want {
				t.Errorf("LocalArchive = %q, want %q", got, want)
			}
			if b, err := os.ReadFile(got); err != nil || string(b) != string(content) {
				t.Errorf("archive at %s = %q, %v; want %q", got, b, err, content)
			}
		})
	}
}

// TestUninstallRow checks that UninstallRow removes exactly the row it is
// given, refuses a row whose version is no longer installed, and keeps the
// cached archive when asked to.
func TestUninstallRow(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "foo.noarch.1.goo")
	writeTestGoo(t, archive)
	db, err := googetdb.NewDB(filepath.Join(dir, "googet.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	row := client.PackageState{PackageSpec: &goolib.PkgSpec{Name: "foo", Arch: "noarch", Version: "1"}, LocalPath: archive}
	if err := db.WriteStateToDB(client.GooGetState{row}); err != nil {
		t.Fatal(err)
	}

	stale := row
	stale.PackageSpec = &goolib.PkgSpec{Name: "foo", Arch: "noarch", Version: "2"}
	if err := UninstallRow(t.Context(), stale, Options{KeepCache: true}, db); err == nil || !strings.Contains(err.Error(), "foo.noarch.2 is no longer installed") {
		t.Errorf("UninstallRow(foo.noarch.2) = %v, want a no-longer-installed error", err)
	}
	if state, _ := db.FetchPkgs(""); len(state) != 1 {
		t.Fatalf("installed packages after refusing = %v, want foo still installed", state)
	}

	if err := UninstallRow(t.Context(), row, Options{KeepCache: true}, db); err != nil {
		t.Fatalf("UninstallRow(foo.noarch.1): %v", err)
	}
	if state, _ := db.FetchPkgs(""); len(state) != 0 {
		t.Errorf("installed packages = %v, want none", state)
	}
	if _, err := os.Stat(archive); err != nil {
		t.Errorf("archive: %v, want it kept", err)
	}
}

// writeTestGoo writes an empty package archive to path.
func writeTestGoo(t *testing.T, path string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	gw := gzip.NewWriter(f)
	tw := tar.NewWriter(gw)
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}
