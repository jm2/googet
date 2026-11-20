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
	"github.com/google/googet/v2/internal/snapshot"
)

// planPolicy holds the choices that change what a plan contains.
type planPolicy struct {
	// conflicts is how reverse package conflicts are handled.
	conflicts ConflictMode
}

// planTarget names the package a plan installs: either a repository package,
// as FromRepo receives it, or a local package's spec, as FromDisk reads it.
type planTarget struct {
	// pi names a repository package. An empty Ver means the latest available
	// version.
	pi goolib.PackageInfo
	// repo is the repository that holds pi when pi.Ver is set.
	repo string
	// spec, if set, is a local package to install instead of pi. A local
	// install uses no repository: every dependency must already be satisfied,
	// and it refuses to install a package that replaces an installed one.
	spec *goolib.PkgSpec
}

// planStep is one package that a plan installs, together with the installed
// packages that its Replaces entries remove first.
type planStep struct {
	// spec is the package to install.
	spec goolib.RepoSpec
	// repo is the repository spec comes from.
	repo string
	// old is the installed package with spec's name and arch, which
	// installing spec overwrites, or nil.
	old *client.PackageState
	// removed lists the installed packages that spec's Replaces entries
	// remove before spec is installed, in removal order.
	removed []client.PackageState
	// recheck holds the conflict checks that passed only because a package
	// that removed lists was exempt; they must still pass once it is gone.
	recheck deferredChecks
}

// plan is the ordered list of steps that installing one package performs.
type plan struct {
	// steps are in install order: every dependency before the package that
	// needs it.
	steps []planStep
	// final is the installed state once every step has run.
	final client.GooGetState
}

// buildPlan works out, without side effects, what FromRepo does when it
// installs t, or what FromDisk does when t is a local package: which packages it installs, in which order, and which installed
// packages each one's Replaces entries remove first. It reads installed as a
// snapshot and never modifies it, and it sends the log lines and console
// messages that FromRepo emits while resolving packages to sink, in the same
// order. It returns the error that FromRepo would return, if any, and no plan.
//
// buildPlan evaluates each package in turn, exactly as FromRepo does: against
// the installed state as it stands after the packages installed before it,
// checking its conflicts first, then installing its dependencies in sorted
// order, then applying its replacements. Conflicts are deliberately checked
// against that sequential state rather than against the final state, as
// FromRepo checks them, so that a dependency is never exempted from a
// conflict with a package that a later step replaces.
//
// buildPlan doesn't log anything itself, but the client lookups it shares
// with FromRepo log version comparison errors, as they do during an install.
func buildPlan(t planTarget, rm client.RepoMap, installed client.GooGetState, archs []string, pol planPolicy, sink eventSink) (*plan, error) {
	p := &planner{rm: rm, archs: archs, pol: pol, sink: sink, state: slices.Clone(installed)}
	var err error
	if t.spec != nil {
		err = p.diskNode(t.spec)
	} else {
		err = p.repoNode(t.pi, t.repo, nil)
	}
	if err != nil {
		return nil, err
	}
	return &plan{steps: p.steps, final: p.state}, nil
}

// planner holds the working state of buildPlan.
type planner struct {
	rm    client.RepoMap
	archs []string
	pol   planPolicy
	sink  eventSink
	// state is the installed state after the steps planned so far.
	state client.GooGetState
	steps []planStep
}

// repoNode plans the install of pi from a repository. chain lists the packages, as name.arch, whose dependencies are being
// planned further up.
func (p *planner) repoNode(pi goolib.PackageInfo, repo string, chain []string) error {
	p.sink.Infof("Starting install of %s.%s.%s", pi.Name, pi.Arch, pi.Ver)
	p.sink.Printf("Installing %s.%s.%s and dependencies...\n", pi.Name, pi.Arch, pi.Ver)
	if pi.Ver == "" {
		installedArch, isLocked := snapshot.LockState(pi.Name, p.state)
		spec, repoURL, _, err := client.FindRepoLatest(pi, p.rm, p.archs, installedArch, isLocked)
		if err != nil {
			return err
		}
		repo = repoURL
		pi.Name = spec.Name
		pi.Arch = spec.Arch
		pi.Ver = spec.Version
	}
	rs, err := client.FindRepoSpec(pi, p.rm[repo])
	if err != nil {
		return err
	}
	key := rs.PackageSpec.Name + "." + rs.PackageSpec.Arch
	if at := slices.Index(chain, key); at >= 0 {
		return fmt.Errorf("cannot install %s: dependency cycle %s", chain[0], strings.Join(append(slices.Clone(chain[at:]), key), " -> "))
	}
	removed, recheck, err := p.depsAndReplacements(rs.PackageSpec, append(slices.Clone(chain), key))
	if err != nil {
		return err
	}
	p.addStep(planStep{spec: rs, repo: repo, removed: removed, recheck: recheck}, client.PackageState{
		SourceRepo:  repo,
		Checksum:    rs.Checksum,
		PackageSpec: rs.PackageSpec,
	})
	return nil
}

// depsAndReplacements plans ps's conflict checks, dependencies and
// replacements, and returns the installed packages that ps's Replaces entries
// remove, in removal order, and the conflict checks that must still pass once
// they are gone.
func (p *planner) depsAndReplacements(ps *goolib.PkgSpec, chain []string) ([]client.PackageState, deferredChecks, error) {
	p.sink.Infof("Resolving conflicts and dependencies for %s %s version %s", ps.Arch, ps.Name, ps.Version)
	ex, err := repoExclusionsIn(ps, p.state)
	if err != nil {
		return nil, deferredChecks{}, err
	}
	deferred, err := checkConflictsIn(ps, ex, p.pol.conflicts, p.state, p.sink)
	if err != nil {
		return nil, deferredChecks{}, err
	}
	for _, dep := range slices.Sorted(maps.Keys(ps.PkgDependencies)) {
		ver := ps.PkgDependencies[dep]
		r := relation.ParseDependency(dep, ver)
		ok, err := satisfiedIn(r, p.state)
		if err != nil {
			return nil, deferredChecks{}, err
		}
		if ok {
			p.sink.Infof("Dependency met: %s.%s with version greater than %s installed or provided", r.Name, r.Arch, ver)
			continue
		}
		installedArch, isLocked := snapshot.LockState(r.Name, p.state)
		spec, repo, _, err := client.FindRepoLatest(goolib.PackageInfo{Name: r.Name, Arch: r.Arch, Ver: ver}, p.rm, p.archs, installedArch, isLocked)
		if err != nil {
			return nil, deferredChecks{}, err
		}
		if spec.Name == ps.Name && spec.Arch == ps.Arch {
			return nil, deferredChecks{}, fmt.Errorf("dependency %s of %s.%s resolves to the package itself", dep, ps.Name, ps.Arch)
		}
		p.sink.Infof("Dependency found: %s.%s %s (provides %s) is available", spec.Name, spec.Arch, spec.Version, r.Name)
		if err := p.repoNode(goolib.PackageInfo{Name: spec.Name, Arch: spec.Arch, Ver: spec.Version}, repo, chain); err != nil {
			return nil, deferredChecks{}, err
		}
	}
	if err := deferred.recheckBeforeReplacementsIn(ps, p.pol.conflicts, p.state, p.sink); err != nil {
		return nil, deferredChecks{}, err
	}
	post, removed, err := replaceRows(ps, p.state)
	if err != nil {
		return nil, deferredChecks{}, err
	}
	if err := deferred.recheckIn(ps, p.pol.conflicts, post, p.sink); err != nil {
		return nil, deferredChecks{}, err
	}
	p.state = post
	return removed, deferred, nil
}

// diskNode plans the install of the local package ps, as FromDisk performs
// it after reading the package: conflicts first, then dependencies, which
// must all be satisfied already, then a refusal if any installed package
// qualifies for one of ps's Replaces entries.
func (p *planner) diskNode(ps *goolib.PkgSpec) error {
	if _, err := checkConflictsIn(ps, selfExclusion(ps), p.pol.conflicts, p.state, p.sink); err != nil {
		return err
	}
	for _, dep := range slices.Sorted(maps.Keys(ps.PkgDependencies)) {
		ver := ps.PkgDependencies[dep]
		r := relation.ParseDependency(dep, ver)
		ok, err := satisfiedIn(r, p.state)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("package dependency %s %s (min version %s) not installed and not provided", r.Name, r.Arch, ver)
		}
		p.sink.Infof("Dependency met: %s.%s with version greater than %s installed or provided", r.Name, r.Arch, ver)
	}
	// Refuse if any installed arch of a replaced package qualifies, not just
	// the row that FetchPkg would select.
	installed := specsFromState(p.state)
	for _, pkg := range ps.Replaces {
		r := relation.ParseRelation(pkg)
		rows, err := relation.Resolve(r, installed)
		if err != nil {
			return err
		}
		if len(rows) > 0 {
			return fmt.Errorf("cannot install, replaces installed package, remove first then try installation again: %s (installed: %s)", r.PackageInfo(), rows[0])
		}
	}
	p.addStep(planStep{spec: goolib.RepoSpec{PackageSpec: ps}}, client.PackageState{PackageSpec: ps})
	return nil
}

// addStep records step and updates the working state: the package step
// installs takes the place of any installed package with its name and arch.
func (p *planner) addStep(step planStep, row client.PackageState) {
	name, arch := row.PackageSpec.Name, row.PackageSpec.Arch
	if i := slices.IndexFunc(p.state, func(st client.PackageState) bool {
		return st.PackageSpec != nil && st.PackageSpec.Name == name && st.PackageSpec.Arch == arch
	}); i >= 0 {
		old := p.state[i]
		step.old = &old
		p.state = slices.Delete(p.state, i, i+1)
	}
	p.state = append(p.state, row)
	p.steps = append(p.steps, step)
}
