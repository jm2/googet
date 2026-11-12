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

package client

import (
	"fmt"
	"testing"

	"github.com/google/googet/v2/goolib"
	"github.com/google/googet/v2/priority"
)

// tiedRepos returns repos at the same priority that each carry an identical
// foo.noarch.1 and a provider of virt at the same version.
func tiedRepos() RepoMap {
	rm := RepoMap{}
	for _, u := range []string{"https://c.example/repo", "https://a.example/repo", "https://b.example/repo"} {
		rm[u] = Repo{Priority: priority.Default, Packages: []goolib.RepoSpec{
			{Source: u, PackageSpec: &goolib.PkgSpec{Name: "foo", Arch: "noarch", Version: "1"}},
			{Source: u, PackageSpec: &goolib.PkgSpec{Name: "prov", Arch: "noarch", Version: "1", Provides: []string{"virt"}}},
		}}
	}
	return rm
}

// TestRepoChoiceIsDeterministicOnTies checks that when several repos carry the
// same package at the same priority, FindRepoLatest and WhatRepo always pick
// the repo with the smallest URL.
func TestRepoChoiceIsDeterministicOnTies(t *testing.T) {
	const want = "https://a.example/repo"
	for i := 0; i < 50; i++ {
		rm := tiedRepos()
		for _, name := range []string{"foo", "virt"} {
			_, repo, _, err := FindRepoLatest(goolib.PackageInfo{Name: name}, rm, []string{"noarch"}, "", false)
			if err != nil {
				t.Fatalf("FindRepoLatest(%s): %v", name, err)
			}
			if repo != want {
				t.Fatalf("iteration %d: FindRepoLatest(%s) repo = %q, want %q", i, name, repo, want)
			}
		}
		repo, err := WhatRepo(goolib.PackageInfo{Name: "foo", Arch: "noarch", Ver: "1"}, rm)
		if err != nil {
			t.Fatalf("WhatRepo: %v", err)
		}
		if repo != want {
			t.Fatalf("iteration %d: WhatRepo = %q, want %q", i, repo, want)
		}
	}
}

// TestRepoChoicePriorityAndVersionBeatURL checks that the URL only breaks
// exact ties: a higher priority or a higher version still wins.
func TestRepoChoicePriorityAndVersionBeatURL(t *testing.T) {
	for i := 0; i < 20; i++ {
		rm := tiedRepos()
		hi := "https://z.example/repo"
		rm[hi] = Repo{Priority: priority.Default + 1, Packages: []goolib.RepoSpec{
			{Source: hi, PackageSpec: &goolib.PkgSpec{Name: "foo", Arch: "noarch", Version: "1"}},
		}}
		newer := "https://y.example/repo"
		rm[newer] = Repo{Priority: priority.Default, Packages: []goolib.RepoSpec{
			{Source: newer, PackageSpec: &goolib.PkgSpec{Name: "prov", Arch: "noarch", Version: "2", Provides: []string{"virt"}}},
		}}
		if _, repo, _, err := FindRepoLatest(goolib.PackageInfo{Name: "foo"}, rm, []string{"noarch"}, "", false); err != nil || repo != hi {
			t.Fatalf("FindRepoLatest(foo) = %q, %v; want %q", repo, err, hi)
		}
		if repo, err := WhatRepo(goolib.PackageInfo{Name: "foo", Arch: "noarch", Ver: "1"}, rm); err != nil || repo != hi {
			t.Fatalf("WhatRepo(foo) = %q, %v; want %q", repo, err, hi)
		}
		if _, repo, _, err := FindRepoLatest(goolib.PackageInfo{Name: "virt"}, rm, []string{"noarch"}, "", false); err != nil || repo != newer {
			t.Fatalf("FindRepoLatest(virt) = %q, %v; want %q", repo, err, newer)
		}
	}
	if _, err := WhatRepo(goolib.PackageInfo{Name: "missing", Arch: "noarch", Ver: "1"}, tiedRepos()); err == nil {
		t.Errorf("WhatRepo(missing) = nil error, want an error")
	} else if want := fmt.Sprintf("package %s %s version %s not found in any repo", "noarch", "missing", "1"); err.Error() != want {
		t.Errorf("WhatRepo(missing) error = %q, want %q", err, want)
	}
}

// TestFindRepoLatestDeterministicWithUnparseableCandidate checks that an
// unparseable version in one repository never breaks strict weak ordering or
// wins over valid parseable versions when pi.Ver is empty, and that all
// unparseable candidates still tie-break deterministically.
func TestFindRepoLatestDeterministicWithUnparseableCandidate(t *testing.T) {
	for _, tc := range []struct {
		name     string
		versions [3]string // Assigned to a.example, b.example, c.example in URL order.
		wantVer  string
		wantRepo string
	}{
		{name: "unparseable_first", versions: [3]string{"not-a-semver", "1", "2"}, wantVer: "2", wantRepo: "https://c.example/repo"},
		{name: "unparseable_middle", versions: [3]string{"1", "not-a-semver", "2"}, wantVer: "2", wantRepo: "https://c.example/repo"},
		{name: "unparseable_last", versions: [3]string{"2", "1", "not-a-semver"}, wantVer: "2", wantRepo: "https://a.example/repo"},
		{name: "all_unparseable_version_tiebreak", versions: [3]string{"bad-a", "bad-c", "bad-b"}, wantVer: "bad-c", wantRepo: "https://b.example/repo"},
		{name: "all_unparseable_equal_version_url_tiebreak", versions: [3]string{"bad-same", "bad-same", "bad-same"}, wantVer: "bad-same", wantRepo: "https://a.example/repo"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rm := RepoMap{
				"https://a.example/repo": {Priority: priority.Default, Packages: []goolib.RepoSpec{{Source: "a", PackageSpec: &goolib.PkgSpec{Name: "B", Arch: "noarch", Version: tc.versions[0]}}}},
				"https://b.example/repo": {Priority: priority.Default, Packages: []goolib.RepoSpec{{Source: "b", PackageSpec: &goolib.PkgSpec{Name: "B", Arch: "noarch", Version: tc.versions[1]}}}},
				"https://c.example/repo": {Priority: priority.Default, Packages: []goolib.RepoSpec{{Source: "c", PackageSpec: &goolib.PkgSpec{Name: "B", Arch: "noarch", Version: tc.versions[2]}}}},
			}
			spec, repo, _, err := FindRepoLatest(goolib.PackageInfo{Name: "B"}, rm, []string{"noarch"}, "", false)
			if err != nil {
				t.Fatalf("FindRepoLatest(B) unexpected error: %v", err)
			}
			if spec.Version != tc.wantVer || repo != tc.wantRepo {
				t.Errorf("FindRepoLatest(B) = (%s, %s), want (%s, %s)", spec.Version, repo, tc.wantVer, tc.wantRepo)
			}
		})
	}
}
