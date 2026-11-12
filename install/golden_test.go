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
	"encoding/json"
	"flag"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/googet/v2/goolib"
)

var updateGolden = flag.Bool("update_golden", false, "rewrite "+goldenFile+" from what FromRepo does now")

// goldenFile holds install scenarios and what FromRepo did with each one when
// the file was recorded. It was recorded from FromRepo's original recursive
// implementation, so it stays an independent record of that behaviour after
// FromRepo is rebuilt on the install planner. Rewrite it with -update_golden
// only to record a deliberate behaviour change.
const goldenFile = "testdata/golden_installs.json"

// goldenSeeds is the number of generated scenarios in goldenFile, which
// also holds the scenarios goldenExtras returns.
const goldenSeeds = 120

// goldenSpec is a package spec as goldenFile stores it.
type goldenSpec struct {
	Name, Arch, Version string
	LockArch            bool              `json:",omitempty"`
	PkgDependencies     map[string]string `json:",omitempty"`
	Replaces            []string          `json:",omitempty"`
	Conflicts           []string          `json:",omitempty"`
	Provides            []string          `json:",omitempty"`
}

// goldenScenario is a corpusCase as goldenFile stores it.
type goldenScenario struct {
	Name      string `json:",omitempty"`
	Seed      uint64 `json:",omitempty"`
	Installed []goldenSpec
	Available []goldenSpec
	Target    string
	Mode      ConflictMode
	DBOnly    bool
}

// goldenCase is one recorded scenario and what FromRepo did with it.
type goldenCase struct {
	Scenario goldenScenario
	Trace    trace
}

func toGoldenSpecs(specs []goolib.PkgSpec) []goldenSpec {
	var out []goldenSpec
	for _, ps := range specs {
		out = append(out, goldenSpec{ps.Name, ps.Arch, ps.Version, ps.LockArch, ps.PkgDependencies, ps.Replaces, ps.Conflicts, ps.Provides})
	}
	return out
}

func fromGoldenSpecs(specs []goldenSpec) []goolib.PkgSpec {
	var out []goolib.PkgSpec
	for _, gs := range specs {
		out = append(out, goolib.PkgSpec{Name: gs.Name, Arch: gs.Arch, Version: gs.Version, LockArch: gs.LockArch, PkgDependencies: gs.PkgDependencies, Replaces: gs.Replaces, Conflicts: gs.Conflicts, Provides: gs.Provides})
	}
	return out
}

func (s goldenScenario) corpusCase() corpusCase {
	return corpusCase{Seed: s.Seed, Installed: fromGoldenSpecs(s.Installed), Available: fromGoldenSpecs(s.Available), Target: s.Target, Mode: s.Mode, DBOnly: s.DBOnly}
}

func toGoldenScenario(name string, c corpusCase) goldenScenario {
	return goldenScenario{Name: name, Seed: c.Seed, Installed: toGoldenSpecs(c.Installed), Available: toGoldenSpecs(c.Available), Target: c.Target, Mode: c.Mode, DBOnly: c.DBOnly}
}

// goldenExtras returns hand-written scenarios that the generated ones rarely
// cover: failures after some packages were installed, replacements with
// dependents, and selection quirks.
func goldenExtras() map[string]corpusCase {
	return map[string]corpusCase{
		"dependency installed before a later dependency fails to resolve": {
			Target: "A",
			Available: []goolib.PkgSpec{
				{Name: "A", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"B": "1", "Z": "1"}},
				{Name: "B", Arch: "noarch", Version: "1"},
			},
		},
		"dependency installed before the next one's reverse conflict fails": {
			Target:    "A",
			Mode:      ConflictError,
			Installed: []goolib.PkgSpec{{Name: "X", Arch: "noarch", Version: "1", Conflicts: []string{"C"}}},
			Available: []goolib.PkgSpec{
				{Name: "A", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"B": "1", "C": "1"}},
				{Name: "B", Arch: "noarch", Version: "1"},
				{Name: "C", Arch: "noarch", Version: "1"},
			},
		},
		"dependency installs a row that a Replaces entry doesn't remove": {
			Target:    "R",
			Installed: []goolib.PkgSpec{{Name: "P", Arch: "noarch", Version: "1"}},
			Available: []goolib.PkgSpec{
				{Name: "P", Arch: "x86_64", Version: "1"},
				{Name: "R", Arch: "noarch", Version: "1", Conflicts: []string{"P"}, Replaces: []string{"P.noarch"}, PkgDependencies: map[string]string{"P.x86_64": "1"}},
			},
		},
		"replacement removes dependents first": {
			Target: "R",
			Installed: []goolib.PkgSpec{
				{Name: "P", Arch: "noarch", Version: "1"},
				{Name: "D", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"P": "1"}},
				{Name: "E", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"D": "1"}},
			},
			Available: []goolib.PkgSpec{{Name: "R", Arch: "noarch", Version: "1", Replaces: []string{"P"}, Provides: []string{"P=1"}}},
		},
		"replacement in a dependency's step": {
			Target:    "A",
			Installed: []goolib.PkgSpec{{Name: "P", Arch: "noarch", Version: "1"}},
			Available: []goolib.PkgSpec{
				{Name: "A", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"R": "1"}},
				{Name: "R", Arch: "noarch", Version: "1", Replaces: []string{"P"}},
			},
		},
		"replaced package selected again after its dependents": {
			Target: "R",
			DBOnly: true,
			Installed: []goolib.PkgSpec{
				{Name: "P", Arch: "noarch", Version: "1"},
				{Name: "P", Arch: "x86_64", Version: "3", PkgDependencies: map[string]string{"P": "1"}},
			},
			Available: []goolib.PkgSpec{{Name: "R", Arch: "noarch", Version: "1", Replaces: []string{"P..2"}}},
		},
		"replacement in a dependency cycle": {
			Target: "R",
			Installed: []goolib.PkgSpec{
				{Name: "P", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"D": "1"}},
				{Name: "D", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"P": "1"}},
			},
			Available: []goolib.PkgSpec{{Name: "R", Arch: "noarch", Version: "1", Replaces: []string{"P"}}},
		},
		"lock state sees an earlier step": {
			Target: "T",
			Available: []goolib.PkgSpec{
				{Name: "T", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"aAb": "1", "a_b": "1"}},
				{Name: "aAb", Arch: "x86_64", Version: "1", LockArch: true},
				{Name: "a_b", Arch: "noarch", Version: "1", LockArch: true},
				{Name: "a_b", Arch: "x86_64", Version: "1", LockArch: true},
			},
		},
		"reverse conflict warning and upgrade": {
			Target: "A",
			Installed: []goolib.PkgSpec{
				{Name: "A", Arch: "noarch", Version: "1"},
				{Name: "X", Arch: "noarch", Version: "1", Conflicts: []string{"A"}},
			},
			Available: []goolib.PkgSpec{{Name: "A", Arch: "noarch", Version: "2"}},
		},
	}
}

// goldenScenarios returns every scenario goldenFile records, in file order.
func goldenScenarios() []goldenScenario {
	var out []goldenScenario
	for seed := uint64(1); seed <= goldenSeeds; seed++ {
		out = append(out, toGoldenScenario("", genCorpusCase(seed)))
	}
	extras := goldenExtras()
	for _, name := range slices.Sorted(maps.Keys(extras)) {
		out = append(out, toGoldenScenario(name, extras[name]))
	}
	return out
}

// TestGoldenInstalls checks that FromRepo still does, for every recorded
// scenario, what it did when the scenario was recorded: the same error, the
// same installed packages afterwards, the same install and removal order, and
// the same resolution log lines and console messages.
func TestGoldenInstalls(t *testing.T) {
	if *updateGolden {
		var lines []string
		for _, s := range goldenScenarios() {
			c := s.corpusCase()
			b, err := json.Marshal(goldenCase{Scenario: s, Trace: runLive(t, c, newRelFixture(t, c.Installed, c.Available))})
			if err != nil {
				t.Fatal(err)
			}
			lines = append(lines, string(b))
		}
		if err := os.MkdirAll(filepath.Dir(goldenFile), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(goldenFile, []byte("[\n"+strings.Join(lines, ",\n")+"\n]\n"), 0644); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote %d scenarios to %s", len(lines), goldenFile)
		return
	}
	b, err := os.ReadFile(goldenFile)
	if err != nil {
		t.Fatal(err)
	}
	var cases []goldenCase
	if err := json.Unmarshal(b, &cases); err != nil {
		t.Fatal(err)
	}
	if want := len(goldenScenarios()); len(cases) != want {
		t.Fatalf("%s has %d scenarios, want %d", goldenFile, len(cases), want)
	}
	for _, gc := range cases {
		c := gc.Scenario.corpusCase()
		got := runLive(t, c, newRelFixture(t, c.Installed, c.Available))
		if diff := cmp.Diff(gc.Trace, got); diff != "" {
			t.Errorf("scenario %q seed %d: FromRepo differs from the recording (-recorded +now):\n%s", gc.Scenario.Name, gc.Scenario.Seed, diff)
		}
	}
}
