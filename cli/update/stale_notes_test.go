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

import (
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/googet/v2/client"
	"github.com/google/googet/v2/goolib"
	"github.com/google/googet/v2/priority"
	"github.com/google/googet/v2/testutil"
	"github.com/google/subcommands"
)

// TestPrunesNotesOfPackagesALaterReplacementRemoves checks that a note the
// first pass recorded for an installed package, saying why that package is
// left alone, is dropped once a replacement for another installed package
// removes it, whichever order the installed packages come in. Notes the
// second pass records for packages no replacement touches are kept.
func TestPrunesNotesOfPackagesALaterReplacementRemoves(t *testing.T) {
	a2, p := spec("A.x86_64.2.0.0"), spec("P.x86_64.1.0.0")
	rp := spec("RP.x86_64.2.0.0", repl("P", "A"), prov("P"))
	for _, tc := range []struct {
		name  string
		state []*goolib.PkgSpec
		rm    client.RepoMap
		want  string
		// wantNotes lists the notes that survive, in order.
		wantNotes []string
	}{
		{
			name:  "highest priority version already installed",
			state: []*goolib.PkgSpec{a2, p},
			rm:    stable(spec("RA.x86_64.1.0.0", repl("A"), prov("A")), rp),
			want:  "Replace P.x86_64.1.0.0 -> RP.x86_64.2.0.0 from stable replaced=[A.x86_64.2.0.0, P.x86_64.1.0.0] removed=[A.x86_64.2.0.0, P.x86_64.1.0.0]",
		},
		{
			name:  "same version installed",
			state: []*goolib.PkgSpec{a2, p},
			rm: client.RepoMap{
				"canary": repoOf(priority.Canary, spec("RA.x86_64.2.0.0", repl("A"), prov("A"))),
				"stable": repoOf(priority.Default, rp),
			},
			want: "Replace P.x86_64.1.0.0 -> RP.x86_64.2.0.0 from stable replaced=[A.x86_64.2.0.0, P.x86_64.1.0.0] removed=[A.x86_64.2.0.0, P.x86_64.1.0.0]",
		},
		{
			name:      "replacing package already installed",
			state:     []*goolib.PkgSpec{spec("A.x86_64.1.0.0"), p, spec("W.x86_64.1.0.0", repl("A"), prov("A"))},
			rm:        stable(spec("W.x86_64.1.0.0", repl("A"), prov("A")), rp),
			want:      "Replace P.x86_64.1.0.0 -> RP.x86_64.2.0.0 from stable replaced=[A.x86_64.1.0.0, P.x86_64.1.0.0] removed=[A.x86_64.1.0.0, P.x86_64.1.0.0]",
			wantNotes: []string{"W.x86_64 - highest priority version already installed"},
		},
	} {
		reversed := slices.Clone(tc.state)
		slices.Reverse(reversed)
		for order, specs := range map[string][]*goolib.PkgSpec{"forward": tc.state, "reversed": reversed} {
			t.Run(tc.name+"/"+order, func(t *testing.T) {
				plan := planUpdates(stateOf(specs...), tc.rm, gateCfg)
				if diff := cmp.Diff([]string{tc.want}, summarize(plan)); diff != "" {
					t.Errorf("planUpdates (-want +got):\n%s", diff)
				}
				if diff := cmp.Diff(tc.wantNotes, notes(plan)); diff != "" {
					t.Errorf("planUpdates notes (-want +got):\n%s", diff)
				}
			})
		}
	}
}

// TestUpdateCmdDryRunOmitsNotesOfRemovedPackages checks that an update dry
// run never logs that an installed package is left alone when the plan it
// prints removes that package as part of another package's replacement.
func TestUpdateCmdDryRunOmitsNotesOfRemovedPackages(t *testing.T) {
	rp := goolib.PkgSpec{Name: "RP", Arch: "x86_64", Version: "2.0.0", Replaces: []string{"P", "A"}, Provides: []string{"P"}}
	for _, tc := range []struct {
		name      string
		installed []goolib.PkgSpec
		repo      []goolib.PkgSpec
		wantLine  string
		staleNote string
	}{
		{
			name: "highest priority version already installed",
			installed: []goolib.PkgSpec{
				{Name: "A", Arch: "x86_64", Version: "2.0.0"},
				{Name: "P", Arch: "x86_64", Version: "1.0.0"},
			},
			repo:      []goolib.PkgSpec{{Name: "RA", Arch: "x86_64", Version: "1.0.0", Replaces: []string{"A"}, Provides: []string{"A"}}, rp},
			wantLine:  "  P.x86_64, 1.0.0 --> RP.x86_64, 2.0.0 from %s (replaces A.x86_64.2.0.0, P.x86_64.1.0.0)\n",
			staleNote: "A.x86_64 - highest priority version already installed",
		},
		{
			name: "replacing package already installed",
			installed: []goolib.PkgSpec{
				{Name: "A", Arch: "x86_64", Version: "1.0.0"},
				{Name: "P", Arch: "x86_64", Version: "1.0.0"},
				{Name: "W", Arch: "x86_64", Version: "1.0.0", Replaces: []string{"A"}, Provides: []string{"A"}},
			},
			repo:      []goolib.PkgSpec{{Name: "W", Arch: "x86_64", Version: "1.0.0", Replaces: []string{"A"}, Provides: []string{"A"}}, rp},
			wantLine:  "  P.x86_64, 1.0.0 --> RP.x86_64, 2.0.0 from %s (replaces A.x86_64.1.0.0, P.x86_64.1.0.0)\n",
			staleNote: "W.x86_64 - 1.0.0 already installed",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, repoURL := testutil.SetupCLIRepo(t, "", tc.installed, tc.repo)
			logs := testutil.CaptureLogs(t)
			status, out := runUpdate(t, "-sources="+repoURL, "-dry_run")
			if status != subcommands.ExitSuccess {
				t.Fatalf("update -dry_run = %v; output:\n%s", status, out)
			}
			if want := strings.Replace(tc.wantLine, "%s", repoURL, 1); !strings.Contains(out, want) {
				t.Errorf("dry run output missing %q:\n%s", want, out)
			}
			if !strings.Contains(logs.String(), "Replace for package P.x86_64, 1.0.0 installed") {
				t.Fatalf("dry run didn't log the replacement of P; logs:\n%s", logs.String())
			}
			if strings.Contains(logs.String(), tc.staleNote) {
				t.Errorf("dry run logged %q for a package its plan removes; logs:\n%s", tc.staleNote, logs.String())
			}
		})
	}
}
