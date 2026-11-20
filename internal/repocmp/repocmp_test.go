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

package repocmp_test

import (
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/googet/v2/goolib"
	"github.com/google/googet/v2/internal/repocmp"
	"github.com/google/googet/v2/priority"
)

func cand(name, arch, ver, repo string, prio priority.Value) repocmp.Candidate {
	return repocmp.Candidate{Spec: &goolib.PkgSpec{Name: name, Arch: arch, Version: ver}, Repo: repo, Priority: prio}
}

func sign(n int) int {
	switch {
	case n < 0:
		return -1
	case n > 0:
		return 1
	}
	return 0
}

// TestCompareCandidates checks each key of the candidate order in turn:
// priority, version, arch preference and repository URL, and the fallback
// order and error for versions that can't be parsed.
func TestCompareCandidates(t *testing.T) {
	pref := repocmp.ArchPreference([]string{"x86_64", "noarch"})
	for _, tc := range []struct {
		name    string
		a, b    repocmp.Candidate
		want    int
		wantErr bool
	}{
		{name: "higher priority first despite lower version", a: cand("P", "x86_64", "1.0.0", "b", 1000), b: cand("P", "x86_64", "9.0.0", "a", 500), want: -1},
		{name: "lower priority last", a: cand("P", "x86_64", "9.0.0", "a", 500), b: cand("P", "x86_64", "1.0.0", "b", 1000), want: 1},
		{name: "higher version first", a: cand("P", "noarch", "2.0.0", "b", 500), b: cand("P", "x86_64", "1.0.0", "a", 500), want: -1},
		{name: "preferred arch first", a: cand("P", "x86_64", "1.0.0", "b", 500), b: cand("P", "noarch", "1.0.0", "a", 500), want: -1},
		{name: "unknown arch ranks with the first arch", a: cand("P", "arm64", "1.0.0", "b", 500), b: cand("P", "x86_64", "1.0.0", "a", 500), want: 1},
		{name: "smaller repository URL first", a: cand("P", "x86_64", "1.0.0", "a", 500), b: cand("P", "x86_64", "1.0.0", "b", 500), want: -1},
		{name: "exact tie", a: cand("P", "x86_64", "1.0.0", "a", 500), b: cand("Q", "x86_64", "1.0.0", "a", 500), want: 0},
		{name: "parseable before unparseable", a: cand("P", "noarch", "1.0.0", "b", 500), b: cand("P", "x86_64", "bad", "a", 500), want: -1, wantErr: true},
		{name: "unparseable after parseable", a: cand("P", "x86_64", "bad", "a", 500), b: cand("P", "noarch", "1.0.0", "b", 500), want: 1, wantErr: true},
		{name: "two unparseable by descending string", a: cand("P", "noarch", "zzz", "b", 500), b: cand("P", "x86_64", "aaa", "a", 500), want: -1, wantErr: true},
		{name: "equal unparseable by arch then repository", a: cand("P", "x86_64", "bad", "b", 500), b: cand("P", "noarch", "bad", "a", 500), want: -1, wantErr: true},
		{name: "priority decides before versions are parsed", a: cand("P", "x86_64", "bad", "a", 1000), b: cand("P", "x86_64", "1.0.0", "a", 500), want: -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := repocmp.CompareCandidates(tc.a, tc.b, pref)
			if sign(got) != tc.want || (err != nil) != tc.wantErr {
				t.Errorf("CompareCandidates = (%d, %v), want sign %d and error %t", got, err, tc.want, tc.wantErr)
			}
		})
	}
}

// TestCompareCandidatesIsStrictWeakOrder checks antisymmetry and
// transitivity of CompareCandidates over every pair and triple of a mix of
// parseable and unparseable candidates, so that sorting is well defined.
func TestCompareCandidatesIsStrictWeakOrder(t *testing.T) {
	pref := repocmp.ArchPreference([]string{"x86_64", "noarch"})
	var all []repocmp.Candidate
	for _, prio := range []priority.Value{500, 1000} {
		for _, ver := range []string{"1.0.0", "2.0.0", "bad", "worse", "1.0.0@2"} {
			for _, arch := range []string{"x86_64", "noarch"} {
				for _, repo := range []string{"a", "b"} {
					all = append(all, cand("P", arch, ver, repo, prio))
				}
			}
		}
	}
	c := func(a, b repocmp.Candidate) int {
		n, _ := repocmp.CompareCandidates(a, b, pref)
		return sign(n)
	}
	for _, a := range all {
		for _, b := range all {
			if c(a, b) != -c(b, a) {
				t.Fatalf("not antisymmetric: %v %v", a, b)
			}
			for _, d := range all {
				if c(a, b) < 0 && c(b, d) < 0 && c(a, d) >= 0 {
					t.Fatalf("not transitive: %v < %v < %v but not %v < %v", a, b, d, a, d)
				}
				if c(a, b) == 0 && c(b, d) == 0 && c(a, d) != 0 {
					t.Fatalf("ties not transitive: %v, %v, %v", a, b, d)
				}
			}
		}
	}
}

// TestSortCandidates checks that SortCandidates orders best first, keeps
// tied candidates in their original order, and returns comparison errors
// only when an unparseable version had to be compared.
func TestSortCandidates(t *testing.T) {
	pref := repocmp.ArchPreference([]string{"x86_64", "noarch"})
	list := []repocmp.Candidate{
		cand("A", "noarch", "1.0.0", "a", 500),
		cand("B", "x86_64", "1.0.0", "a", 500),
		cand("C", "x86_64", "1.0.0", "a", 500),
		cand("D", "x86_64", "2.0.0", "a", 500),
		cand("E", "x86_64", "0.1.0", "a", 1000),
	}
	if errs := repocmp.SortCandidates(list, pref); len(errs) != 0 {
		t.Errorf("SortCandidates errors = %v, want none", errs)
	}
	var got []string
	for _, c := range list {
		got = append(got, c.Spec.Name)
	}
	if diff := cmp.Diff([]string{"E", "D", "B", "C", "A"}, got); diff != "" {
		t.Errorf("order (-want +got):\n%s", diff)
	}

	bad := []repocmp.Candidate{cand("P", "x86_64", "bad", "a", 500), cand("P", "x86_64", "1.0.0", "a", 500)}
	if errs := repocmp.SortCandidates(bad, pref); len(errs) == 0 {
		t.Error("SortCandidates with an unparseable version returned no errors")
	}
	if bad[0].Spec.Version != "1.0.0" {
		t.Errorf("best = %s, want the parseable 1.0.0", bad[0].Spec.Version)
	}
}

// TestArchAllowed checks the LockArch rule: only a locked installed package
// moving to another arch is held back, and only by a candidate that locks
// its own arch.
func TestArchAllowed(t *testing.T) {
	for _, tc := range []struct {
		installedArch string
		isLocked      bool
		candArch      string
		candLock      bool
		want          bool
	}{
		{"x86_64", false, "noarch", true, true},
		{"x86_64", true, "x86_64", true, true},
		{"x86_64", true, "noarch", false, true},
		{"x86_64", true, "noarch", true, false},
		{"x86_64", false, "x86_64", false, true},
	} {
		if got := repocmp.ArchAllowed(tc.installedArch, tc.isLocked, tc.candArch, tc.candLock); got != tc.want {
			t.Errorf("ArchAllowed(%q, %t, %q, %t) = %t, want %t", tc.installedArch, tc.isLocked, tc.candArch, tc.candLock, got, tc.want)
		}
	}
}

// pkg returns a package spec with the given name, arch and version.
func pkg(name, arch, ver string) *goolib.PkgSpec {
	return &goolib.PkgSpec{Name: name, Arch: arch, Version: ver}
}

// TestLatest checks how Latest selects among the packages of several
// repositories: packages of the requested name before packages that provide
// it, each tier in candidate order, a minimum version, the installable archs
// and the LockArch rule, and the problem lines it returns for versions that
// can't be compared.
func TestLatest(t *testing.T) {
	archs := []string{"x86_64", "noarch"}
	lockedNoarch := pkg("P", "noarch", "3.0.0")
	lockedNoarch.LockArch = true
	provP := pkg("Q", "x86_64", "1.0.0")
	provP.Provides = []string{"P"}
	provP2 := pkg("S", "x86_64", "1.0.0")
	provP2.Provides = []string{"P=2.0.0"}
	selfProv := pkg("P", "x86_64", "1.0.0")
	selfProv.Provides = []string{"P=2.0.0"}
	provBad := pkg("T", "x86_64", "1.0.0")
	provBad.Provides = []string{"P=worse"}
	for _, tc := range []struct {
		name     string
		repos    []repocmp.Repo
		minVer   string
		isLocked bool
		want     *goolib.PkgSpec
		wantRepo string
		// wantProblems lists the prefix of each problem's message, in order.
		wantProblems []string
	}{
		{name: "no repositories"},
		{name: "empty repository", repos: []repocmp.Repo{{URL: "a", Priority: 500}}},
		{
			name:  "nothing of that name",
			repos: []repocmp.Repo{{URL: "a", Priority: 500, Specs: []*goolib.PkgSpec{pkg("Q", "x86_64", "1.0.0")}}},
		},
		{
			name: "package of the name before a provider at a higher priority",
			repos: []repocmp.Repo{
				{URL: "a", Priority: 500, Specs: []*goolib.PkgSpec{pkg("P", "x86_64", "1.0.0")}},
				{URL: "b", Priority: 1000, Specs: []*goolib.PkgSpec{provP}},
			},
			want: pkg("P", "x86_64", "1.0.0"), wantRepo: "a",
		},
		{
			name:  "provider when no package has the name",
			repos: []repocmp.Repo{{URL: "a", Priority: 500, Specs: []*goolib.PkgSpec{provP}}},
			want:  provP, wantRepo: "a",
		},
		{
			name: "priority before version",
			repos: []repocmp.Repo{
				{URL: "a", Priority: 500, Specs: []*goolib.PkgSpec{pkg("P", "x86_64", "2.0.0")}},
				{URL: "b", Priority: 1000, Specs: []*goolib.PkgSpec{pkg("P", "x86_64", "1.0.0")}},
			},
			want: pkg("P", "x86_64", "1.0.0"), wantRepo: "b",
		},
		{
			name:  "version before arch",
			repos: []repocmp.Repo{{URL: "a", Priority: 500, Specs: []*goolib.PkgSpec{pkg("P", "x86_64", "1.0.0"), pkg("P", "noarch", "2.0.0")}}},
			want:  pkg("P", "noarch", "2.0.0"), wantRepo: "a",
		},
		{
			name:  "preferred arch",
			repos: []repocmp.Repo{{URL: "a", Priority: 500, Specs: []*goolib.PkgSpec{pkg("P", "noarch", "1.0.0"), pkg("P", "x86_64", "1.0.0")}}},
			want:  pkg("P", "x86_64", "1.0.0"), wantRepo: "a",
		},
		{
			name: "first repository URL on an exact tie",
			repos: []repocmp.Repo{
				{URL: "a", Priority: 500, Specs: []*goolib.PkgSpec{pkg("P", "x86_64", "1.0.0")}},
				{URL: "b", Priority: 500, Specs: []*goolib.PkgSpec{pkg("P", "x86_64", "1.0.0")}},
			},
			want: pkg("P", "x86_64", "1.0.0"), wantRepo: "a",
		},
		{
			name:  "arch that isn't installable is ignored",
			repos: []repocmp.Repo{{URL: "a", Priority: 500, Specs: []*goolib.PkgSpec{pkg("P", "x86_32", "9.0.0"), pkg("P", "noarch", "1.0.0")}}},
			want:  pkg("P", "noarch", "1.0.0"), wantRepo: "a",
		},
		{
			name:   "minimum version",
			repos:  []repocmp.Repo{{URL: "a", Priority: 1000, Specs: []*goolib.PkgSpec{pkg("P", "x86_64", "1.0.0")}}, {URL: "b", Priority: 500, Specs: []*goolib.PkgSpec{pkg("P", "x86_64", "2.0.0")}}},
			minVer: "1.5.0",
			want:   pkg("P", "x86_64", "2.0.0"), wantRepo: "b",
		},
		{
			name:   "provider when no package of the name is recent enough",
			repos:  []repocmp.Repo{{URL: "a", Priority: 500, Specs: []*goolib.PkgSpec{pkg("P", "x86_64", "1.0.0"), provP2}}},
			minVer: "1.5.0",
			want:   provP2, wantRepo: "a",
		},
		{
			name:   "package of the name never counts as its own provider",
			repos:  []repocmp.Repo{{URL: "a", Priority: 500, Specs: []*goolib.PkgSpec{selfProv}}},
			minVer: "1.5.0",
		},
		{
			name:     "locked package skips a locked candidate for another arch",
			repos:    []repocmp.Repo{{URL: "a", Priority: 500, Specs: []*goolib.PkgSpec{lockedNoarch, pkg("P", "x86_64", "1.0.0")}}},
			isLocked: true,
			want:     pkg("P", "x86_64", "1.0.0"), wantRepo: "a",
		},
		{
			name:  "unlocked package takes a locked candidate for another arch",
			repos: []repocmp.Repo{{URL: "a", Priority: 500, Specs: []*goolib.PkgSpec{lockedNoarch, pkg("P", "x86_64", "1.0.0")}}},
			want:  lockedNoarch, wantRepo: "a",
		},
		{
			name:     "provider when the LockArch rule rejects every package of the name",
			repos:    []repocmp.Repo{{URL: "a", Priority: 500, Specs: []*goolib.PkgSpec{lockedNoarch, provP}}},
			isLocked: true,
			want:     provP, wantRepo: "a",
		},
		{
			name:     "nothing when the LockArch rule rejects every candidate",
			repos:    []repocmp.Repo{{URL: "a", Priority: 500, Specs: []*goolib.PkgSpec{lockedNoarch}}},
			isLocked: true,
		},
		{
			name: "problem lines for versions that can't be compared, in the order met",
			repos: []repocmp.Repo{
				{URL: "a", Priority: 500, Specs: []*goolib.PkgSpec{provBad, pkg("P", "x86_64", "bad"), pkg("P", "x86_64", "2.0.0")}},
			},
			minVer:       "1.0.0",
			want:         pkg("P", "x86_64", "2.0.0"),
			wantRepo:     "a",
			wantProblems: []string{"comparing versions worse vs 1.0.0: ", "comparing versions bad vs 1.0.0: "},
		},
		{
			name: "problem lines for candidates the sort can't compare",
			repos: []repocmp.Repo{
				{URL: "a", Priority: 500, Specs: []*goolib.PkgSpec{pkg("P", "x86_64", "bad"), pkg("P", "x86_64", "1.0.0")}},
			},
			want:         pkg("P", "x86_64", "1.0.0"),
			wantRepo:     "a",
			wantProblems: []string{"comparing priority/version: "},
		},
		{
			name:         "problem lines without a candidate",
			repos:        []repocmp.Repo{{URL: "a", Priority: 500, Specs: []*goolib.PkgSpec{pkg("P", "x86_64", "bad")}}},
			minVer:       "1.0.0",
			wantProblems: []string{"comparing versions bad vs 1.0.0: "},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok, problems := repocmp.Latest(tc.repos, "P", tc.minVer, archs, "x86_64", tc.isLocked)
			if ok != (tc.want != nil) {
				t.Fatalf("Latest found = %t (%v), want %t", ok, got.Spec, tc.want != nil)
			}
			if ok {
				if diff := cmp.Diff(tc.want, got.Spec); diff != "" || got.Repo != tc.wantRepo {
					t.Errorf("Latest = %v from %q, want %v from %q", got.Spec, got.Repo, tc.want, tc.wantRepo)
				}
				for _, r := range tc.repos {
					if r.URL == tc.wantRepo && got.Priority != r.Priority {
						t.Errorf("Latest priority = %d, want %d", got.Priority, r.Priority)
					}
				}
			} else if got != (repocmp.Candidate{}) {
				t.Errorf("Latest = %+v without a match, want the zero Candidate", got)
			}
			if len(problems) != len(tc.wantProblems) {
				t.Fatalf("Latest problems = %q, want messages starting with %q", problems, tc.wantProblems)
			}
			for i, p := range problems {
				if !strings.HasPrefix(p.Error(), tc.wantProblems[i]) {
					t.Errorf("Latest problem %d = %q, want prefix %q", i, p, tc.wantProblems[i])
				}
			}
		})
	}
}
