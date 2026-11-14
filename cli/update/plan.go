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
	"fmt"
	"maps"
	"slices"

	"github.com/google/googet/v2/client"
	"github.com/google/googet/v2/goolib"
	"github.com/google/googet/v2/internal/repocmp"
	"github.com/google/googet/v2/priority"
)

// updateOp is the kind of change an update action makes to an installed
// package.
type updateOp int

const (
	// opUpgrade installs a later version of an installed package, or of a
	// package that provides it.
	opUpgrade updateOp = iota
	// opDowngrade installs an earlier version from a repository whose
	// priority outranks the installed version.
	opDowngrade
)

// String returns the word update logs for op.
func (op updateOp) String() string {
	if op == opDowngrade {
		return "Downgrade"
	}
	return "Upgrade"
}

// updateAction is one package that update installs.
type updateAction struct {
	// op is the kind of change.
	op updateOp
	// from is the installed package the action updates.
	from *goolib.PkgSpec
	// to is the package the action installs.
	to *goolib.PkgSpec
	// repo is the repository that offers to.
	repo string
}

// info returns the name, arch and version of the package a installs.
func (a updateAction) info() goolib.PackageInfo {
	return goolib.PackageInfo{Name: a.to.Name, Arch: a.to.Arch, Ver: a.to.Version}
}

// noteLevel is the log level of a planning note.
type noteLevel int

const (
	// noteInfo is an informational log line.
	noteInfo noteLevel = iota
	// noteError is an error log line.
	noteError
)

// planNote is a log line that planning recorded for renderUpdates to emit.
type planNote struct {
	level noteLevel
	text  string
}

// planEntry is either an action or a note. Entries keep the order in which
// planning produced them, so that rendering emits them in that order.
type planEntry struct {
	action *updateAction
	note   *planNote
}

// updatePlan is what an update would do: the actions to run, in order, and
// the log lines that explain the packages it leaves alone.
type updatePlan struct {
	entries []planEntry
}

// actions returns the plan's actions in the order they run.
func (p *updatePlan) actions() []updateAction {
	var out []updateAction
	for _, e := range p.entries {
		if e.action != nil {
			out = append(out, *e.action)
		}
	}
	return out
}

// packageInfos returns the name, arch and version of the package each action
// installs, in the order they run, or nil if there are none.
func (p *updatePlan) packageInfos() []goolib.PackageInfo {
	var out []goolib.PackageInfo
	for _, a := range p.actions() {
		out = append(out, a.info())
	}
	return out
}

// addAction appends a to the plan.
func (p *updatePlan) addAction(a updateAction) {
	p.entries = append(p.entries, planEntry{action: &a})
}

// addNote appends n to the plan.
func (p *updatePlan) addNote(n planNote) {
	p.entries = append(p.entries, planEntry{note: &n})
}

// infof appends an informational note to the plan.
func (p *updatePlan) infof(format string, args ...any) {
	p.entries = append(p.entries, planEntry{note: &planNote{level: noteInfo, text: fmt.Sprintf(format, args...)}})
}

// errorf appends an error note to the plan.
func (p *updatePlan) errorf(format string, args ...any) {
	p.entries = append(p.entries, planEntry{note: &planNote{level: noteError, text: fmt.Sprintf(format, args...)}})
}

// planUpdates works out, without side effects, which installed packages in
// state update would change, using the packages rm offers. It neither logs
// nor prints: it returns the actions and the log lines that explain them for
// renderUpdates to emit.
func planUpdates(state client.GooGetState, rm client.RepoMap) updatePlan {
	var plan updatePlan
	repos := repoList(rm)
	for _, p := range state {
		if p.PackageSpec == nil {
			continue
		}
		planInPlace(&plan, p.PackageSpec, state, repos)
	}
	return plan
}

// planInPlace records the update of the installed package p to the best
// package repos offer for p's name and arch, or a note saying why there is
// none.
func planInPlace(plan *updatePlan, p *goolib.PkgSpec, state client.GooGetState, repos []repocmp.Repo) {
	best, ok, problems := latestFor(p, repos)
	for _, err := range problems {
		plan.errorf("Error %v", err)
	}
	if !ok {
		// This installed package is not available in a repo.
		plan.infof("no package found satisfying %s.%s in any repo", p.Name, p.Arch)
		return
	}
	spec := best.Spec
	if p.Name != spec.Name {
		// Check if package identified via "Provides" is already installed (at any version).
		if _, err := state.GetPackageState(goolib.PackageInfo{Name: spec.Name}); err == nil {
			plan.infof("%s.%s - %s already installed", spec.Name, spec.Arch, spec.Version)
			return
		}
	}
	op, ok, why := compareInstalled(best, p)
	if !ok {
		plan.addNote(why)
		return
	}
	plan.addAction(updateAction{op: op, from: p, to: spec, repo: best.Repo})
}

// compareInstalled decides, without recording anything, whether best should
// update the installed package p in place: best must have a higher
// (priority, version) than p's version at the default priority, and a
// different version. It returns the kind of update, or the note that says why
// there is none.
func compareInstalled(best repocmp.Candidate, p *goolib.PkgSpec) (updateOp, bool, planNote) {
	c, err := goolib.ComparePriorityVersion(best.Priority, best.Spec.Version, priority.Default, p.Version)
	if err != nil {
		return 0, false, planNote{level: noteError, text: err.Error()}
	}
	if c < 1 {
		return 0, false, planNote{level: noteInfo, text: fmt.Sprintf("%s.%s - highest priority version already installed", p.Name, p.Arch)}
	}
	// The versions might actually be the same even though the priorities are different,
	// so do another check to skip reinstall of the same version.
	c, err = goolib.Compare(best.Spec.Version, p.Version)
	if err != nil {
		return 0, false, planNote{level: noteError, text: err.Error()}
	}
	switch c {
	case 0:
		return 0, false, planNote{level: noteInfo, text: fmt.Sprintf("%s.%s - same version installed", p.Name, p.Arch)}
	case -1:
		return opDowngrade, true, planNote{}
	default:
		return opUpgrade, true, planNote{}
	}
}

// latestFor returns the best package repos offer for the installed package p,
// as client.FindRepoLatest picks it for p's name and arch: the highest-ranked
// package of p's name and arch, or else the highest-ranked package of p's
// arch that provides p's name. It also returns the version problems that
// ranking met, which FindRepoLatest logs.
func latestFor(p *goolib.PkgSpec, repos []repocmp.Repo) (repocmp.Candidate, bool, []error) {
	return repocmp.Latest(repos, p.Name, "", []string{p.Arch}, p.Arch, p.LockArch)
}

// repoList returns the repositories in rm, ordered by URL, as repocmp reads
// them.
func repoList(rm client.RepoMap) []repocmp.Repo {
	var repos []repocmp.Repo
	for _, u := range slices.Sorted(maps.Keys(rm)) {
		r := repocmp.Repo{URL: u, Priority: rm[u].Priority}
		for _, rs := range rm[u].Packages {
			r.Specs = append(r.Specs, rs.PackageSpec)
		}
		repos = append(repos, r)
	}
	return repos
}
