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

package client

import (
	"fmt"
	"math/rand/v2"
	"testing"

	"github.com/google/googet/v2/goolib"
	"github.com/google/googet/v2/priority"
)

// TestTiedProvidersKeepRepoOrder checks that when several packages in one
// repo provide a name at the same version and priority, FindRepoLatest picks
// the first of them in the repo's package list, even when another repo with
// a larger URL offers the same providers in the opposite order. There are
// enough providers that an unstable sort would reorder them.
func TestTiedProvidersKeepRepoOrder(t *testing.T) {
	const n = 40
	var forward, backward []goolib.RepoSpec
	for i := range n {
		forward = append(forward, goolib.RepoSpec{PackageSpec: &goolib.PkgSpec{Name: fmt.Sprintf("prov%02d", i), Arch: "noarch", Version: "1", Provides: []string{"virt"}}})
	}
	for i := n - 1; i >= 0; i-- {
		backward = append(backward, forward[i])
	}
	for _, tc := range []struct {
		name     string
		packages []goolib.RepoSpec
		want     string
	}{
		{name: "forward", packages: forward, want: "prov00"},
		{name: "backward", packages: backward, want: fmt.Sprintf("prov%02d", n-1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			other := backward
			if tc.name == "backward" {
				other = forward
			}
			rm := RepoMap{
				"https://a.example/repo": Repo{Priority: priority.Default, Packages: tc.packages},
				"https://b.example/repo": Repo{Priority: priority.Default, Packages: other},
			}
			for i := 0; i < 20; i++ {
				spec, repo, _, err := FindRepoLatest(goolib.PackageInfo{Name: "virt"}, rm, []string{"noarch"}, "", false)
				if err != nil {
					t.Fatalf("FindRepoLatest(virt): %v", err)
				}
				if spec.Name != tc.want || repo != "https://a.example/repo" {
					t.Fatalf("iteration %d: FindRepoLatest(virt) = %s from %s, want %s from https://a.example/repo", i, spec.Name, repo, tc.want)
				}
			}
		})
	}
}

// TestWhatRepoAgreesWithFindRepoLatest checks, over random repo maps, that
// WhatRepo returns the repo FindRepoLatest picked for the package it picked,
// so that the repo googet install resolves a package to is the one the
// install then uses. A package with a given name, arch and version has the
// same spec in every repo that carries it.
func TestWhatRepoAgreesWithFindRepoLatest(t *testing.T) {
	rng := rand.New(rand.NewPCG(3, 4))
	archs := []string{"noarch", "x86_64"}
	urls := []string{"https://a.example/r", "https://b.example/r", "https://c.example/r", "https://d.example/r"}
	for i := range 3000 {
		provides := map[string]bool{}
		for _, name := range []string{"foo", "bar"} {
			for _, arch := range archs {
				for _, ver := range []string{"1", "2", "3"} {
					provides[name+"."+arch+"."+ver] = rng.IntN(3) == 0
				}
			}
		}
		rm := RepoMap{}
		for _, u := range urls[:1+rng.IntN(len(urls))] {
			var pkgs []goolib.RepoSpec
			for _, name := range []string{"foo", "bar"} {
				for _, arch := range archs {
					for _, ver := range []string{"1", "2", "3"} {
						if rng.IntN(3) != 0 {
							continue
						}
						ps := &goolib.PkgSpec{Name: name, Arch: arch, Version: ver}
						if provides[ps.String()] {
							ps.Provides = []string{"virt"}
						}
						pkgs = append(pkgs, goolib.RepoSpec{PackageSpec: ps})
					}
				}
			}
			rm[u] = Repo{Priority: priority.Value(500 + 500*rng.IntN(2)), Packages: pkgs}
		}
		for _, name := range []string{"foo", "virt"} {
			spec, repo, arch, err := FindRepoLatest(goolib.PackageInfo{Name: name}, rm, archs, "", false)
			if err != nil {
				continue
			}
			got, err := WhatRepo(goolib.PackageInfo{Name: spec.Name, Arch: arch, Ver: spec.Version}, rm)
			if err != nil || got != repo {
				t.Fatalf("case %d: FindRepoLatest(%s) = %s.%s.%s from %s, but WhatRepo = %q, %v", i, name, spec.Name, arch, spec.Version, repo, got, err)
			}
		}
	}
}
