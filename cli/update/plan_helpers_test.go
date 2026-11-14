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
	"strings"

	"github.com/google/googet/v2/client"
	"github.com/google/googet/v2/goolib"
	"github.com/google/googet/v2/priority"
)

// planArchs are the installable archs the planner tests use, most preferred
// first, as settings.Archs lists them on a 64-bit Windows host.
var planArchs = []string{"noarch", "x86_64", "x86_32"}

// specOpt modifies a package spec built by spec.
type specOpt func(*goolib.PkgSpec)

// repl sets the spec's Replaces entries.
func repl(entries ...string) specOpt { return func(ps *goolib.PkgSpec) { ps.Replaces = entries } }

// prov sets the spec's Provides entries.
func prov(entries ...string) specOpt { return func(ps *goolib.PkgSpec) { ps.Provides = entries } }

// conf sets the spec's Conflicts entries.
func conf(entries ...string) specOpt { return func(ps *goolib.PkgSpec) { ps.Conflicts = entries } }

// dep adds a dependency on name at minimum version ver.
func dep(name, ver string) specOpt {
	return func(ps *goolib.PkgSpec) {
		if ps.PkgDependencies == nil {
			ps.PkgDependencies = map[string]string{}
		}
		ps.PkgDependencies[name] = ver
	}
}

// locked sets the spec's LockArch.
func locked(ps *goolib.PkgSpec) { ps.LockArch = true }

// spec builds a package spec from an id of the form name.arch.version.
func spec(id string, opts ...specOpt) *goolib.PkgSpec {
	parts := strings.SplitN(id, ".", 3)
	ps := &goolib.PkgSpec{Name: parts[0], Arch: parts[1], Version: parts[2]}
	for _, o := range opts {
		o(ps)
	}
	return ps
}

// stateOf returns an installed state holding specs, in order.
func stateOf(specs ...*goolib.PkgSpec) client.GooGetState {
	var st client.GooGetState
	for _, ps := range specs {
		st = append(st, client.PackageState{PackageSpec: ps})
	}
	return st
}

// repoOf returns a repository at priority prio offering specs.
func repoOf(prio priority.Value, specs ...*goolib.PkgSpec) client.Repo {
	r := client.Repo{Priority: prio}
	for _, ps := range specs {
		r.Packages = append(r.Packages, goolib.RepoSpec{PackageSpec: ps})
	}
	return r
}

// stable returns a repository map with one repository, named stable, at the
// default priority offering specs.
func stable(specs ...*goolib.PkgSpec) client.RepoMap {
	return client.RepoMap{"stable": repoOf(priority.Default, specs...)}
}

// joinSpecs formats specs as a bracketed, comma-separated list.
func joinSpecs(specs []*goolib.PkgSpec) string {
	parts := make([]string, 0, len(specs))
	for _, ps := range specs {
		parts = append(parts, ps.String())
	}
	return "[" + strings.Join(parts, ",") + "]"
}

// summarize describes the actions and skipped replacements of plan, in
// order, one string each, leaving out diagnostics and notes.
func summarize(plan updatePlan) []string {
	var out []string
	for _, e := range plan.entries {
		switch {
		case e.action != nil:
			a := e.action
			s := fmt.Sprintf("%s %s -> %s from %s", a.op, a.from, a.to, a.repo)
			if a.op == opReplace {
				s += fmt.Sprintf(" replaced=[%s] removed=[%s]", joinInfos(a.replaced), joinInfos(a.removed))
			}
			out = append(out, s)
		case e.skip != nil:
			s := fmt.Sprintf("skip %s reason=%s", e.skip.pkg, e.skip.reason)
			if e.skip.replacer != nil {
				s += " replacer=" + e.skip.replacer.String()
			}
			if len(e.skip.candidates) > 0 {
				s += " candidates=" + joinSpecs(e.skip.candidates)
			}
			out = append(out, s)
		}
	}
	return out
}

// notes returns the text of plan's notes, in order.
func notes(plan updatePlan) []string {
	var out []string
	for _, e := range plan.entries {
		if e.note != nil {
			out = append(out, e.note.text)
		}
	}
	return out
}

// gateCfg is the planner configuration of a default update, which doesn't
// follow Replaces.
var gateCfg = updateConfig{archs: planArchs}
