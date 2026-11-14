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
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/google/googet/v2/client"
	"github.com/google/googet/v2/goolib"
	"github.com/google/googet/v2/internal/relation"
)

// Plan describes what (*Installer).FromRepo would do to install one package:
// the packages it installs, in order, the installed packages removed by
// Replaces entries or cascaded dependent removal, and any Provides-only
// dependents left unsatisfied. It is unrelated to remove.Plan, which orders
// a removal.
//
// The zero value and a nil *Plan describe an install that does nothing.
type Plan struct {
	p *plan
	// warnings are the console warnings planning produced, such as reverse
	// package conflicts that are allowed.
	warnings []string
}

// Plan works out what FromRepo would do to install pi from rm, without
// downloading or changing anything. Its arguments mean what they mean for
// FromRepo, and it returns the error FromRepo would return before changing
// anything. Like FromRepo, it doesn't check whether pi is already installed:
// a package installed at the same version is planned as a reinstall, so
// callers that skip installed packages should call NeedsInstallation first.
// Plan writes nothing to the log or the console.
func (ins *Installer) Plan(pi goolib.PackageInfo, repo string, rm client.RepoMap, archs []string) (*Plan, error) {
	state, err := ins.db.FetchPkgs("")
	if err != nil {
		return nil, err
	}
	return PlanState(pi, repo, rm, state, archs, ins.opts)
}

// PlanState works out what FromRepo would do to install pi from rm over the
// installed state state, under the package conflict and orphaned dependent
// policies in opts, without a database, downloads or changes of any kind. It
// is what (*Installer).Plan does after reading the installed state, so a
// caller can plan several installs in a row by passing each plan's State to
// the next call. It never modifies state, and like Plan it writes nothing to
// the log or the console.
func PlanState(pi goolib.PackageInfo, repo string, rm client.RepoMap, state client.GooGetState, archs []string, opts Options) (*Plan, error) {
	sink := &recordingSink{}
	p, err := buildPlan(planTarget{pi: pi, repo: repo}, rm, state, archs, planPolicy{conflicts: opts.PackageConflicts, orphans: opts.OrphanedDependents}, sink)
	if err != nil {
		return nil, err
	}
	out := &Plan{p: p}
	for _, e := range sink.events {
		if e.level == levelStdout && strings.HasPrefix(e.text, "Warning: ") {
			out.warnings = append(out.warnings, strings.TrimSuffix(e.text, "\n"))
		}
	}
	return out, nil
}

// State returns the installed state the plan leaves once every step has run.
// The rows, their package specs, the specs' relation lists and dependency
// maps, and the rows' installed file maps are copies, so the caller may
// modify them without changing the plan. The zero value and a nil *Plan
// return nil.
func (p *Plan) State() client.GooGetState {
	if p == nil || p.p == nil {
		return nil
	}
	return cloneState(p.p.final)
}

// Installs returns the packages the plan installs, in install order.
func (p *Plan) Installs() []goolib.PackageInfo {
	if p == nil || p.p == nil {
		return nil
	}
	var out []goolib.PackageInfo
	for _, s := range p.p.steps {
		out = append(out, specInfo(s.spec.PackageSpec))
	}
	return out
}

// Removals returns the installed packages the plan removes, in removal
// order.
func (p *Plan) Removals() []goolib.PackageInfo {
	if p == nil || p.p == nil {
		return nil
	}
	var out []goolib.PackageInfo
	for _, s := range p.p.steps {
		for _, r := range s.removed {
			out = append(out, specInfo(r.PackageSpec))
		}
	}
	return out
}

// Replaced returns the installed packages that the Replaces entries of the
// package removing them name, in removal order. Removals also lists the
// dependents removed with them.
func (p *Plan) Replaced() []goolib.PackageInfo {
	if p == nil || p.p == nil {
		return nil
	}
	var out []goolib.PackageInfo
	for _, s := range p.p.steps {
		for _, r := range s.removed {
			if replacedBy(s, r) {
				out = append(out, specInfo(r.PackageSpec))
			}
		}
	}
	return out
}

// String describes the plan for a person: the packages it installs, then the
// packages it removes and why, then any Provides-only dependents left with an
// unsatisfied dependency, then any warnings. A plan that does nothing is
// described by an empty string.
func (p *Plan) String() string {
	if p == nil || p.p == nil {
		return ""
	}
	var b strings.Builder
	if len(p.p.steps) > 0 {
		fmt.Fprintln(&b, "The following packages will be installed:")
		for _, s := range p.p.steps {
			fmt.Fprintf(&b, "  %s%s\n", s.spec.PackageSpec, versionChange(s))
		}
	}
	removing := false
	for _, s := range p.p.steps {
		for _, r := range s.removed {
			if !removing {
				fmt.Fprintln(&b, "The following packages will be removed:")
				removing = true
			}
			fmt.Fprintf(&b, "  %s (%s)\n", r.PackageSpec, removalReason(s, r))
		}
	}
	writeStepListedOrphans(&b, p.p.steps)
	for _, w := range p.warnings {
		fmt.Fprintln(&b, w)
	}
	return b.String()
}

// cloneState returns a copy of st whose rows, package specs, relation lists,
// dependency maps and installed file maps don't share memory with st.
func cloneState(st client.GooGetState) client.GooGetState {
	if st == nil {
		return nil
	}
	out := make(client.GooGetState, len(st))
	for i, row := range st {
		if row.PackageSpec != nil {
			ps := *row.PackageSpec
			ps.PkgDependencies = maps.Clone(ps.PkgDependencies)
			ps.Replaces = slices.Clone(ps.Replaces)
			ps.Conflicts = slices.Clone(ps.Conflicts)
			ps.Provides = slices.Clone(ps.Provides)
			row.PackageSpec = &ps
		}
		row.InstalledFiles = maps.Clone(row.InstalledFiles)
		out[i] = row
	}
	return out
}

// specInfo returns ps's name, arch and version.
func specInfo(ps *goolib.PkgSpec) goolib.PackageInfo {
	return goolib.PackageInfo{Name: ps.Name, Arch: ps.Arch, Ver: ps.Version}
}

// versionChange describes how step changes the installed version of its
// package, or returns "" for a fresh install.
func versionChange(s planStep) string {
	if s.old == nil {
		return ""
	}
	from := s.old.PackageSpec.Version
	c, err := goolib.Compare(s.spec.PackageSpec.Version, from)
	switch {
	case err != nil:
		return fmt.Sprintf(" (replacing version %s)", from)
	case c > 0:
		return fmt.Sprintf(" (upgrade from %s)", from)
	case c < 0:
		return fmt.Sprintf(" (downgrade from %s)", from)
	default:
		return " (reinstall)"
	}
}

// removalReason says why step removes the installed package r: either one of
// step's Replaces entries names it at its minimum version or later, or it
// depends on a package they name.
func removalReason(s planStep, r client.PackageState) string {
	if replacedBy(s, r) {
		return "replaced by " + s.spec.PackageSpec.String()
	}
	return "depends on a package " + s.spec.PackageSpec.String() + " replaces"
}

// replacedBy reports whether one of the Replaces entries of step's package
// names the installed package r at its minimum version or later.
func replacedBy(s planStep, r client.PackageState) bool {
	return relation.ReplacesInstalled(s.spec.PackageSpec.Replaces, r.PackageSpec.Name, r.PackageSpec.Arch, r.PackageSpec.Version)
}
