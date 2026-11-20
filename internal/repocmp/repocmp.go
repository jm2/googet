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

// Package repocmp ranks and selects package specs offered by repositories, so
// that every caller picks the same candidate. It neither logs nor reads
// settings: callers decide what to do with the comparison errors it returns.
package repocmp

import (
	"fmt"
	"slices"
	"strings"

	"github.com/google/googet/v2/goolib"
	"github.com/google/googet/v2/internal/relation"
	"github.com/google/googet/v2/priority"
)

// Candidate is a package spec that a repository offers, together with the
// repository's URL and priority.
type Candidate struct {
	// Spec is the offered package spec.
	Spec *goolib.PkgSpec
	// Repo is the URL of the repository that offers Spec.
	Repo string
	// Priority is the priority of Repo.
	Priority priority.Value
}

// ArchPreference maps each arch in archs to its position in archs, so that
// an arch listed earlier is preferred.
func ArchPreference(archs []string) map[string]int {
	pref := make(map[string]int, len(archs))
	for i, a := range archs {
		pref[a] = i
	}
	return pref
}

// CompareCandidates returns a negative number when a ranks before b, a
// positive number when b ranks before a, and zero when they tie. Candidates
// rank by repository priority, then version, both highest first, then by arch
// preference in archPref, then by ascending repository URL, so that exact
// ties across repositories always resolve the same way.
//
// When the versions must be compared and one of them can't be parsed,
// CompareCandidates returns the comparison error together with an order that
// keeps sorting well defined: a parseable version ranks before an unparseable
// one, and two unparseable versions rank by descending string before arch and
// repository. The order is valid either way; the error only tells the caller
// that it fell back.
func CompareCandidates(a, b Candidate, archPref map[string]int) (int, error) {
	c, err := goolib.ComparePriorityVersion(a.Priority, a.Spec.Version, b.Priority, b.Spec.Version)
	if err != nil {
		// ComparePriorityVersion only parses versions when priorities are
		// equal. Preserve a strict weak ordering so an unparseable candidate
		// never breaks transitivity between valid candidates or outranks
		// them.
		_, aErr := goolib.ParseVersion(a.Spec.Version)
		_, bErr := goolib.ParseVersion(b.Spec.Version)
		switch {
		case aErr == nil && bErr != nil:
			return -1, err
		case aErr != nil && bErr == nil:
			return 1, err
		default:
			if vc := strings.Compare(b.Spec.Version, a.Spec.Version); vc != 0 {
				return vc, err
			}
		}
	} else if c != 0 {
		// Higher priority and version rank first.
		return -c, nil
	}
	if archPref[a.Spec.Arch] < archPref[b.Spec.Arch] {
		return -1, err
	}
	if archPref[a.Spec.Arch] > archPref[b.Spec.Arch] {
		return 1, err
	}
	return strings.Compare(a.Repo, b.Repo), err
}

// SortCandidates stably sorts list best first using CompareCandidates and
// returns every comparison error in the order the sort met them.
func SortCandidates(list []Candidate, archPref map[string]int) []error {
	var errs []error
	slices.SortStableFunc(list, func(a, b Candidate) int {
		c, err := CompareCandidates(a, b, archPref)
		if err != nil {
			errs = append(errs, err)
		}
		return c
	})
	return errs
}

// ArchAllowed reports whether a candidate with arch candArch and LockArch
// candLockArch may replace an installed package with arch installedArch whose
// arch is locked when isLocked is set: a locked installed package only moves
// to another arch when the candidate doesn't lock its own arch.
func ArchAllowed(installedArch string, isLocked bool, candArch string, candLockArch bool) bool {
	return !(isLocked && candArch != installedArch && candLockArch)
}

// Repo is a repository as Latest reads it: its URL, its priority and the
// package specs it offers.
type Repo struct {
	// URL identifies the repository.
	URL string
	// Priority is the repository's priority.
	Priority priority.Value
	// Specs are the package specs the repository offers.
	Specs []*goolib.PkgSpec
}

// Latest returns the best package that repos offer for name at one of archs:
// the highest-ranked spec named name whose version satisfies minVer, or, when
// there is none that LockArch allows, the highest-ranked spec of another name
// that provides name at minVer or later. Specs are ranked with
// CompareCandidates, preferring archs in the order given, and an installed
// package of arch installedArch, locked when isLocked is set, only moves to
// what ArchAllowed allows. Repos are read in the order given, so callers list
// them by URL.
//
// Latest also returns, in the order it met them, the version comparisons that
// failed, each wrapped with the versions or the ranking step involved. A
// candidate whose version can't be checked against minVer is left out; one
// that ranking can't compare is ordered as CompareCandidates describes.
func Latest(repos []Repo, name, minVer string, archs []string, installedArch string, isLocked bool) (Candidate, bool, []error) {
	archPref := ArchPreference(archs)
	req := relation.Requirement{Name: name, Ver: minVer}
	var problems []error
	var direct, providers []Candidate
	for _, r := range repos {
		for _, ps := range r.Specs {
			if _, ok := archPref[ps.Arch]; !ok {
				continue
			}
			cand := Candidate{Spec: ps, Repo: r.URL, Priority: r.Priority}
			if ps.Name == name {
				ok, err := req.Satisfies(ps.Version)
				if err != nil {
					problems = append(problems, fmt.Errorf("comparing versions %s vs %s: %w", ps.Version, minVer, err))
				} else if ok {
					direct = append(direct, cand)
				}
				continue
			}
			for _, raw := range ps.Provides {
				p := relation.ParseProvide(raw)
				ok, err := req.SatisfiedBy(p)
				if err != nil {
					problems = append(problems, fmt.Errorf("comparing versions %s vs %s: %w", p.Ver, minVer, err))
					continue
				}
				if ok {
					providers = append(providers, cand)
					break
				}
			}
		}
	}
	for _, list := range [][]Candidate{direct, providers} {
		if len(list) == 0 {
			continue
		}
		for _, err := range SortCandidates(list, archPref) {
			problems = append(problems, fmt.Errorf("comparing priority/version: %w", err))
		}
		for _, c := range list {
			if ArchAllowed(installedArch, isLocked, c.Spec.Arch, c.Spec.LockArch) {
				return c, true, problems
			}
		}
	}
	return Candidate{}, false, problems
}
