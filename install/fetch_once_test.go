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
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/googet/v2/goolib"
	"github.com/google/googet/v2/testutil"
)

// TestEachArchiveFetchedOnce checks that an install downloads each archive it
// needs exactly once, never downloads the archives of the packages it
// replaces while they are cached, and logs reusing a cached archive only for
// an archive that was cached before the install.
func TestEachArchiveFetchedOnce(t *testing.T) {
	f := newRelFixture(t,
		[]goolib.PkgSpec{
			{Name: "P", Arch: "noarch", Version: "1"},
			{Name: "Q", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"P": "1"}},
		},
		[]goolib.PkgSpec{
			{Name: "T", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"C": "1", "D": "1", "E": "1"}, Replaces: []string{"P"}},
			{Name: "C", Arch: "noarch", Version: "1"},
			{Name: "D", Arch: "noarch", Version: "1"},
			{Name: "E", Arch: "noarch", Version: "1"},
		})
	copyFile(t, f.archive(t, "C"), filepath.Join(f.cache, "C.noarch.1.goo"))
	logs := testutil.CaptureLogs(t)
	if err := f.install(t, "T"); err != nil {
		t.Fatalf("install T: %v", err)
	}
	if diff := cmp.Diff([]string{"C.noarch.1", "D.noarch.1", "E.noarch.1", "T.noarch.1"}, f.installed(t)); diff != "" {
		t.Errorf("installed packages (-want +got):\n%s", diff)
	}
	for a, want := range map[string]int{"C.noarch.1.goo": 0, "D.noarch.1.goo": 1, "E.noarch.1.goo": 1, "T.noarch.1.goo": 1, "P.noarch.1.goo": 0, "Q.noarch.1.goo": 0} {
		if n := f.getCount(a); n != want {
			t.Errorf("%s downloaded %d times, want %d", a, n, want)
		}
	}
	var reused []string
	for _, l := range parseLogs(logs.String()) {
		if rest, ok := strings.CutPrefix(l.text, "using existing file: "); ok {
			path, _, _ := strings.Cut(rest, " (sum = ")
			reused = append(reused, filepath.Base(path))
		}
	}
	if diff := cmp.Diff([]string{"C.noarch.1.goo"}, reused); diff != "" {
		t.Errorf("archives logged as reused from the cache (-want +got):\n%s", diff)
	}
}

// TestCorpusFetchesEachArchiveOnce checks, for every recorded install
// scenario, that each archive is either downloaded or reused from the cache,
// once: none is downloaded twice, verified again and logged as reused after
// being downloaded, or reused twice.
func TestCorpusFetchesEachArchiveOnce(t *testing.T) {
	for _, s := range goldenScenarios() {
		c := s.corpusCase()
		f := newRelFixture(t, c.Installed, c.Available)
		logs := testutil.CaptureLogs(t)
		captureStdout(t, func() {
			New(f.cache, f.db, Options{DBOnly: c.DBOnly, PackageConflicts: c.Mode}).FromRepo(t.Context(), goolib.PackageInfo{Name: c.Target}, "", f.rm, corpusArchs, f.downloader)
		})
		fetches := map[string]int{}
		f.mu.Lock()
		for archive, n := range f.gets {
			fetches[archive] += n
		}
		f.mu.Unlock()
		for _, l := range parseLogs(logs.String()) {
			if rest, ok := strings.CutPrefix(l.text, "using existing file: "); ok {
				path, _, _ := strings.Cut(rest, " (sum = ")
				fetches[filepath.Base(path)]++
			}
		}
		for archive, n := range fetches {
			if n > 1 {
				t.Errorf("scenario %q seed %d: %s downloaded or reused %d times, want once", s.Name, s.Seed, archive, n)
			}
		}
	}
}
