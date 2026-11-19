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

package update

// The tests in this file pin how update treats Replaces today. Each case says
// whether the behaviour it pins is intended, so the case guards against
// regressions, or known to be wrong, in which case it says what a fix should
// expect instead. A change that fixes a known-wrong behaviour should update
// the matching case, so that the change in semantics is visible in review.

import (
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/googet/v2/client"
	"github.com/google/googet/v2/goolib"
	"github.com/google/googet/v2/priority"
	"github.com/google/googet/v2/settings"
)

// TestCharacterizeUpdatesReplaces pins that updates() never reads Replaces.
// A replacing package is only reached through the Provides fallback, which
// runs once the installed package is gone from every repo, and that fallback
// picks the top-ranked provider without checking whether it is the only one.
func TestCharacterizeUpdatesReplaces(t *testing.T) {
	installed := client.GooGetState{
		{PackageSpec: &goolib.PkgSpec{Name: "P", Version: "1.0", Arch: "x86_32"}},
	}
	replacer := func(name, version string, provides ...string) goolib.RepoSpec {
		return goolib.RepoSpec{PackageSpec: &goolib.PkgSpec{
			Name:     name,
			Version:  version,
			Arch:     "x86_32",
			Replaces: []string{"P"},
			Provides: provides,
		}}
	}
	publishedP := func(version string) goolib.RepoSpec {
		return goolib.RepoSpec{PackageSpec: &goolib.PkgSpec{Name: "P", Version: version, Arch: "x86_32"}}
	}

	// A change that makes update follow Replaces should leave these cases
	// unchanged unless the caller explicitly asks for it.
	const noFollowIntent = "Intended: update doesn't follow Replaces while the installed package is still published."

	for _, tc := range []struct {
		name     string
		intent   string
		packages []goolib.RepoSpec
		want     []goolib.PackageInfo
	}{
		{
			name:     "replacer ignored while P is published at the installed version",
			intent:   noFollowIntent,
			packages: []goolib.RepoSpec{publishedP("1.0"), replacer("R", "2.0", "P")},
			want:     nil,
		},
		{
			name:     "P upgrades in place while a replacer is published",
			intent:   noFollowIntent,
			packages: []goolib.RepoSpec{publishedP("1.1"), replacer("R", "2.0", "P")},
			want:     []goolib.PackageInfo{{Name: "P", Arch: "x86_32", Ver: "1.1"}},
		},
		{
			name:     "replacer without Provides is never reached once P is retired",
			intent:   "Intended: a package that splits P into parts declares Replaces but not Provides, and must not be picked as P's successor.",
			packages: []goolib.RepoSpec{replacer("R", "2.0")},
			want:     nil,
		},
		{
			name:     "single replacer that Provides P is reached once P is retired",
			intent:   "Intended: this is the rename path through Provides.",
			packages: []goolib.RepoSpec{replacer("R", "2.0", "P")},
			want:     []goolib.PackageInfo{{Name: "R", Arch: "x86_32", Ver: "2.0"}},
		},
		{
			name:     "top-ranked of several replacers is chosen once P is retired",
			intent:   "Known wrong: when more than one package name both Replaces and Provides P, a fix should skip the migration and log why.",
			packages: []goolib.RepoSpec{replacer("R1", "2.0", "P"), replacer("R2", "3.0", "P")},
			want:     []goolib.PackageInfo{{Name: "R2", Arch: "x86_32", Ver: "3.0"}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			settings.Initialize(t.TempDir(), false)
			settings.Archs = []string{"x86_32", "noarch"}
			rm := client.RepoMap{"stable": client.Repo{Priority: priority.Default, Packages: tc.packages}}

			var got []goolib.PackageInfo
			captureStdout(func() {
				got = updates(installed, rm)
			})
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("updates() mismatch (-want +got):\n%s\n%s", diff, tc.intent)
			}
		})
	}
}
