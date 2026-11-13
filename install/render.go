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
	sink := &recordingSink{}
	p, err := buildPlan(planTarget{pi: pi, repo: repo}, rm, state, archs, planPolicy{conflicts: ins.opts.PackageConflicts, orphans: ins.opts.OrphanedDependents}, sink)
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
	for _, raw := range s.spec.PackageSpec.Replaces {
		if ok, err := relation.ParseRelation(raw).MinInstalled(r.PackageSpec.Name, r.PackageSpec.Arch, r.PackageSpec.Version); err == nil && ok {
			return "replaced by " + s.spec.PackageSpec.String()
		}
	}
	return "depends on a package " + s.spec.PackageSpec.String() + " replaces"
}
