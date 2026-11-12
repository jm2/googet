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

// This file generates random install scenarios and records what FromRepo
// does with them, so that tests can compare the install planner, and later
// install paths, against FromRepo's observable behaviour.

import (
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"

	"github.com/google/googet/v2/client"
	"github.com/google/googet/v2/goolib"
	"github.com/google/googet/v2/testutil"
)

// corpusArchs is the arch preference every corpus install uses.
var corpusArchs = []string{"noarch", "x86_64"}

// corpusCase is one random install scenario: the packages installed before
// the run, the packages a single repository offers, and what to install.
type corpusCase struct {
	Seed      uint64
	Installed []goolib.PkgSpec
	Available []goolib.PkgSpec
	Target    string
	Mode      ConflictMode
	DBOnly    bool
}

// genCorpusCase returns the scenario for seed. Package names include a dotted
// name and a virtual name that only Provides entries answer to; relation
// entries mix arch-less, arch-qualified and versioned forms. The repository
// never offers two packages with the same name, arch and version.
func genCorpusCase(seed uint64) corpusCase {
	rng := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
	names := []string{"A", "B", "C", "D", "P", "Q", "x.y"}
	pick := func(s []string) string { return s[rng.IntN(len(s))] }
	relation := func() string {
		n := pick(names)
		switch rng.IntN(5) {
		case 0:
			return n + "." + pick(corpusArchs)
		case 1:
			return n + ".." + pick([]string{"1", "2", "3"})
		case 2:
			return n + "." + pick(corpusArchs) + "." + pick([]string{"1", "2"})
		default:
			return n
		}
	}
	deps := func(max int) map[string]string {
		m := map[string]string{}
		for range rng.IntN(max + 1) {
			key := pick(append(slices.Clone(names), "V"))
			if rng.IntN(4) == 0 {
				key += "." + pick(corpusArchs)
			}
			m[key] = pick([]string{"1", "1", "2", "3"})
		}
		if len(m) == 0 {
			return nil
		}
		return m
	}
	list := func(prob int, gen func() string) []string {
		if rng.IntN(prob) != 0 {
			return nil
		}
		out := []string{gen()}
		if rng.IntN(3) == 0 {
			out = append(out, gen())
		}
		return out
	}
	provides := func() string {
		return pick([]string{"V", "V=1", "V=3", "P=3", "Q"})
	}

	c := corpusCase{Seed: seed, Mode: ConflictMode(rng.IntN(3)), DBOnly: rng.IntN(2) == 0}
	for _, n := range names {
		for _, a := range corpusArchs {
			for _, v := range []string{"2", "3"} {
				if rng.IntN(3) != 0 {
					continue
				}
				c.Available = append(c.Available, goolib.PkgSpec{
					Name: n, Arch: a, Version: v,
					PkgDependencies: deps(rng.IntN(2) + rng.IntN(2)),
					Replaces:        list(3, relation),
					Conflicts:       list(8, relation),
					Provides:        list(4, provides),
				})
			}
		}
	}
	for _, n := range names {
		for _, a := range corpusArchs {
			if rng.IntN(10) >= 3 {
				continue
			}
			v := pick([]string{"1", "2"})
			spec := goolib.PkgSpec{
				Name: n, Arch: a, Version: v,
				PkgDependencies: deps(1),
				Conflicts:       list(6, relation),
				Provides:        list(4, provides),
			}
			// An installed package with the same name, arch and version as
			// an available one has the same spec.
			if i := slices.IndexFunc(c.Available, func(ps goolib.PkgSpec) bool {
				return ps.Name == n && ps.Arch == a && ps.Version == v
			}); i >= 0 {
				spec = c.Available[i]
			}
			c.Installed = append(c.Installed, spec)
		}
	}
	targets := []string{"V"}
	for _, ps := range c.Available {
		targets = append(targets, ps.Name)
	}
	c.Target = pick(targets)
	return c
}

// trace is what an install did, or what a plan says it will do, in the terms
// tests compare.
type trace struct {
	// Err is the error text, or "" on success.
	Err string
	// Final lists name.arch.version for every installed package afterwards,
	// sorted. A plan that fails has no Final.
	Final []string
	// Installs lists name.arch.version of each package installed, in order.
	Installs []string
	// Removed lists the name of each package uninstalled, in order.
	Removed []string
	// Events lists the planning log lines and console messages, in order,
	// as "LEVEL: text".
	Events []string
}

// liveLine is one parsed logger line.
type liveLine struct {
	level, text string
}

// parseLogs splits logger output captured by testutil.CaptureLogs into lines,
// keeping continuation lines with the line they belong to.
func parseLogs(out string) []liveLine {
	var lines []liveLine
	for _, l := range strings.Split(strings.TrimSuffix(out, "\n"), "\n") {
		level := ""
		for _, prefix := range []string{"INFO : ", "WARN : ", "ERROR: "} {
			if strings.HasPrefix(l, prefix) {
				level = strings.TrimRight(strings.TrimSuffix(prefix, ": "), " ")
			}
		}
		if level == "" {
			if len(lines) > 0 {
				lines[len(lines)-1].text += "\n" + l
			}
			continue
		}
		// Drop the date, time and file:line fields.
		fields := strings.SplitN(l[len("INFO : "):], " ", 4)
		if len(fields) < 4 {
			continue
		}
		lines = append(lines, liveLine{level: level, text: fields[3]})
	}
	return lines
}

// isPlanningLog reports whether a logger line is one that resolving packages
// produces, as opposed to downloading, installing or removing them.
func isPlanningLog(text string) bool {
	for _, prefix := range []string{
		"Starting install of ",
		"Resolving conflicts and dependencies for ",
		"Dependency met: ",
		"Dependency found: ",
		"package_conflict direction=reverse ",
		"Replacement simulation for ",
	} {
		if strings.HasPrefix(text, prefix) {
			return true
		}
	}
	return strings.HasPrefix(text, `conflict "`) && strings.Contains(text, " skipped: installing ")
}

// isPlanningOutput reports whether a console line is one that resolving
// packages produces.
func isPlanningOutput(line string) bool {
	return (strings.HasPrefix(line, "Installing ") && strings.HasSuffix(line, " and dependencies...")) ||
		strings.HasPrefix(line, "Warning: installed package ")
}

// runCase builds a fresh fixture for c, plans c's target with buildPlan, then
// installs it with FromRepo, and returns what the plan predicted and what
// FromRepo did.
func runCase(t *testing.T, c corpusCase) (planned, live trace) {
	t.Helper()
	f := newRelFixture(t, c.Installed, c.Available)
	state, err := f.db.FetchPkgs("")
	if err != nil {
		t.Fatal(err)
	}
	return runPlan(c, f.rm, state), runLive(t, c, f)
}

// runLive installs c's target with FromRepo in fixture f and returns what it
// did.
func runLive(t *testing.T, c corpusCase, f *relFixture) trace {
	t.Helper()
	logs := testutil.CaptureLogs(t)
	var err error
	stdout := captureStdout(t, func() {
		ins := New(f.cache, f.db, Options{DBOnly: c.DBOnly, PackageConflicts: c.Mode})
		err = ins.FromRepo(t.Context(), goolib.PackageInfo{Name: c.Target}, "", f.rm, corpusArchs, f.downloader)
	})
	var tr trace
	var planLog []string
	for _, l := range parseLogs(logs.String()) {
		switch {
		case isPlanningLog(l.text):
			planLog = append(planLog, l.level+": "+l.text)
		case strings.HasPrefix(l.text, "Installation of ") && strings.HasSuffix(l.text, " completed"):
			tr.Installs = append(tr.Installs, strings.TrimSuffix(strings.TrimPrefix(l.text, "Installation of "), " completed"))
		case strings.HasPrefix(l.text, "Executing removal of package "):
			tr.Removed = append(tr.Removed, strings.TrimPrefix(l.text, "Executing removal of package "))
		}
	}
	tr.Events = planLog
	for _, l := range strings.Split(stdout, "\n") {
		if isPlanningOutput(l) {
			tr.Events = append(tr.Events, "STDOUT: "+l)
		}
	}
	if err != nil {
		tr.Err = err.Error()
	}
	tr.Final = f.installed(t)
	return tr
}

// runPlan plans c's target with buildPlan over the installed state and
// repositories and returns what the plan says will happen.
func runPlan(c corpusCase, rm client.RepoMap, state client.GooGetState) trace {
	sink := &recordingSink{}
	p, err := buildPlan(planTarget{pi: goolib.PackageInfo{Name: c.Target}}, rm, state, corpusArchs, planPolicy{conflicts: c.Mode}, sink)
	var tr trace
	var stdout []string
	for _, e := range sink.events {
		switch e.level {
		case levelStdout:
			stdout = append(stdout, "STDOUT: "+strings.TrimSuffix(e.text, "\n"))
		case levelWarning:
			tr.Events = append(tr.Events, "WARN: "+e.text)
		default:
			tr.Events = append(tr.Events, string(e.level)+": "+e.text)
		}
	}
	tr.Events = append(tr.Events, stdout...)
	if err != nil {
		tr.Err = err.Error()
		return tr
	}
	for _, s := range p.steps {
		for _, r := range s.removed {
			tr.Removed = append(tr.Removed, fmt.Sprintf("%q", r.PackageSpec.Name))
		}
		tr.Installs = append(tr.Installs, s.spec.PackageSpec.String())
	}
	for _, st := range p.final {
		tr.Final = append(tr.Final, st.PackageSpec.String())
	}
	slices.Sort(tr.Final)
	return tr
}
