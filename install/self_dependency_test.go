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
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/googet/v2/client"
	"github.com/google/googet/v2/googetdb"
	"github.com/google/googet/v2/goolib"
	"github.com/google/googet/v2/priority"
	"github.com/google/googet/v2/settings"
)

// selfDependencyCases are installs whose dependencies name the package being
// installed. A dependency is rejected as resolving to the package itself only
// when no installed package satisfies it and the repository resolves it to
// the package's own name and arch.
var selfDependencyCases = []struct {
	desc      string
	installed []*goolib.PkgSpec
	available []*goolib.PkgSpec // available[0] is the package to install.
	wantSelf  bool
	wantSteps []string // wantSteps lists the planned installs as name.arch.version.
}{
	{
		desc: "depends on itself, already satisfied by the installed version",
		installed: []*goolib.PkgSpec{
			{Name: "self_pkg", Version: "1.0.0", Arch: "noarch"},
		},
		available: []*goolib.PkgSpec{
			{Name: "self_pkg", Version: "2.0.0", Arch: "noarch",
				PkgDependencies: map[string]string{"self_pkg": "1.0.0"}},
		},
		wantSteps: []string{"self_pkg.noarch.2.0.0"},
	},
	{
		desc: "depends on a name it provides, already provided by the installed version",
		installed: []*goolib.PkgSpec{
			{Name: "self_pkg", Version: "1.0.0", Arch: "noarch", Provides: []string{"virt"}},
		},
		available: []*goolib.PkgSpec{
			{Name: "self_pkg", Version: "2.0.0", Arch: "noarch", Provides: []string{"virt"},
				PkgDependencies: map[string]string{"virt": "0.0.0"}},
		},
		wantSteps: []string{"self_pkg.noarch.2.0.0"},
	},
	{
		desc: "depends on itself at a version the installed version doesn't meet",
		installed: []*goolib.PkgSpec{
			{Name: "self_pkg", Version: "1.0.0", Arch: "noarch"},
		},
		available: []*goolib.PkgSpec{
			{Name: "self_pkg", Version: "3.0.0", Arch: "noarch",
				PkgDependencies: map[string]string{"self_pkg": "2.0.0"}},
		},
		wantSelf: true,
	},
	{
		desc: "depends on a name it provides, with nothing installed",
		available: []*goolib.PkgSpec{
			{Name: "self_pkg", Version: "2.0.0", Arch: "noarch", Provides: []string{"virt"},
				PkgDependencies: map[string]string{"virt": "0.0.0"}},
		},
		wantSelf: true,
	},
	{
		desc: "depends on another arch of itself",
		available: []*goolib.PkgSpec{
			{Name: "self_pkg", Version: "1.0.0", Arch: "x86_64",
				PkgDependencies: map[string]string{"self_pkg.x86_32": "0.0.0"}},
			{Name: "self_pkg", Version: "1.0.0", Arch: "x86_32"},
		},
		wantSteps: []string{"self_pkg.x86_32.1.0.0", "self_pkg.x86_64.1.0.0"},
	},
}

const selfDependencyErr = "resolves to the package itself"

var selfDependencyArchs = []string{"x86_64", "x86_32", "noarch"}

func selfDependencyRepo(available []*goolib.PkgSpec) client.RepoMap {
	var specs []goolib.RepoSpec
	for _, ps := range available {
		specs = append(specs, goolib.RepoSpec{PackageSpec: ps})
	}
	return client.RepoMap{"repo1": client.Repo{Priority: priority.Value(500), Packages: specs}}
}

// TestBuildPlanSelfDependencyPlacement checks that the planner rejects a
// dependency that resolves to the package itself only after checking whether
// an installed package already satisfies it, and allows a dependency on
// another arch of the same name.
func TestBuildPlanSelfDependencyPlacement(t *testing.T) {
	for _, tc := range selfDependencyCases {
		t.Run(tc.desc, func(t *testing.T) {
			var state client.GooGetState
			for _, ps := range tc.installed {
				state = append(state, client.PackageState{PackageSpec: ps})
			}
			target := planTarget{pi: goolib.PackageInfo{Name: tc.available[0].Name, Arch: tc.available[0].Arch}}
			p, err := buildPlan(target, selfDependencyRepo(tc.available), state, selfDependencyArchs, planPolicy{}, &recordingSink{})
			if gotSelf := err != nil && strings.Contains(err.Error(), selfDependencyErr); gotSelf != tc.wantSelf {
				t.Fatalf("buildPlan() error = %v; want an error containing %q: %t", err, selfDependencyErr, tc.wantSelf)
			}
			if tc.wantSelf {
				return
			}
			if err != nil {
				t.Fatalf("buildPlan() = %v, want a plan", err)
			}
			var got []string
			for _, s := range p.steps {
				got = append(got, s.spec.PackageSpec.String())
			}
			if diff := cmp.Diff(tc.wantSteps, got); diff != "" {
				t.Errorf("planned installs (-want +got):\n%s", diff)
			}
		})
	}
}

// TestFromRepoSelfDependencyPlacement checks that a repository install
// applies the same rule to the installed packages in the database. The
// installs that pass resolution then fail to download, because the test
// repository has no package files. The case with nothing installed that
// passes resolution is left to TestBuildPlanSelfDependencyPlacement, because
// a failing download retries and so is slow.
func TestFromRepoSelfDependencyPlacement(t *testing.T) {
	for _, tc := range selfDependencyCases {
		if len(tc.installed) == 0 && !tc.wantSelf {
			continue
		}
		t.Run(tc.desc, func(t *testing.T) {
			settings.Initialize(t.TempDir(), false)
			db, err := googetdb.NewDB(settings.DBFile())
			if err != nil {
				t.Fatalf("googetdb.NewDB: %v", err)
			}
			defer db.Close()
			for _, ps := range tc.installed {
				if err := db.AddPkg(client.PackageState{PackageSpec: ps}); err != nil {
					t.Fatalf("db.AddPkg: %v", err)
				}
			}
			downloader, err := client.NewDownloader("")
			if err != nil {
				t.Fatalf("client.NewDownloader: %v", err)
			}
			pi := goolib.PackageInfo{Name: tc.available[0].Name, Arch: tc.available[0].Arch}
			err = New(t.TempDir(), db, Options{}).FromRepo(t.Context(), pi, "", selfDependencyRepo(tc.available), selfDependencyArchs, downloader)
			if gotSelf := err != nil && strings.Contains(err.Error(), selfDependencyErr); gotSelf != tc.wantSelf {
				t.Errorf("FromRepo() = %v; want an error containing %q: %t", err, selfDependencyErr, tc.wantSelf)
			}
		})
	}
}
