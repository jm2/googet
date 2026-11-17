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
	"cmp"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"

	"github.com/google/googet/v2/client"
	"github.com/google/googet/v2/goolib"
	"github.com/google/googet/v2/internal/relation"
	"github.com/google/googet/v2/remove"
)

// ErrOrphanedDependents marks a planning failure where replacing installed
// packages would leave one or more installed packages with an unsatisfied
// dependency under OrphanFail mode.
var ErrOrphanedDependents = errors.New("replacing would leave installed packages with an unsatisfied dependency")

// OrphanedDependent describes one installed package whose dependency would be
// left unsatisfied by a replacement step.
type OrphanedDependent struct {
	// Package is the installed dependent package (Name, Arch, Ver).
	Package goolib.PackageInfo
	// Requirement is the unsatisfied dependency requirement (Name, Arch, and
	// minimum Ver).
	Requirement goolib.PackageInfo
	// Removed lists the installed packages whose removal leaves Package
	// unsatisfied.
	Removed []goolib.PackageInfo
	// ReplacedBy is the incoming package whose Replaces entries trigger the
	// removal.
	ReplacedBy goolib.PackageInfo
}

// OrphanedDependentsError reports every installed package whose name-based
// dependency would become unsatisfied if an install proceeded under OrphanFail
// mode.
type OrphanedDependentsError struct {
	Dependents []OrphanedDependent
}

// Error formats the unsatisfied dependents and notes that satisfaction is
// evaluated as each replacement step completes, before any packages installed
// by later steps in the same run.
func (e *OrphanedDependentsError) Error() string {
	if e == nil || len(e.Dependents) == 0 {
		return ErrOrphanedDependents.Error()
	}
	parts := make([]string, 0, len(e.Dependents))
	for _, d := range e.Dependents {
		removed := make([]string, 0, len(d.Removed))
		for _, r := range d.Removed {
			removed = append(removed, r.String())
		}
		parts = append(parts, fmt.Sprintf("%s (requires %s, broken by removing %s for %s)",
			d.Package, relation.FormatPackageRequirement(d.Requirement), strings.Join(removed, ", "), d.ReplacedBy))
	}
	return fmt.Sprintf("%s: %s (evaluated at each replacement step before later packages in the same run are installed)",
		ErrOrphanedDependents, strings.Join(parts, "; "))
}

// Unwrap returns ErrOrphanedDependents so errors.Is matches the sentinel.
func (e *OrphanedDependentsError) Unwrap() error {
	return ErrOrphanedDependents
}

// removal is one installed package a plan step removes.
type removal struct {
	// row is the installed package row uninstalled at this step.
	row client.PackageState
	// victim reports whether one of the step's Replaces entries names row
	// directly; false indicates a dependent cascaded with the replaced packages.
	victim bool
}

// replaceOutcome holds the pure planning result of applying one package's
// Replaces entries over the working installed state.
type replaceOutcome struct {
	// post is the installed state after removing victims and cascaded
	// name-based orphans (before addStep records the incoming package).
	post client.GooGetState
	// removed lists the installed rows uninstalled in topological removal order.
	removed []removal
	// orphans lists unsatisfied name-based dependency entries at this step.
	orphans []OrphanedDependent
	// listed holds Provides-only dependents whose dependencies become
	// unsatisfied; they are reported and never removed.
	listed []relation.Orphan
}

// simulateReplacements returns the installed state after ps's replacements
// and cascaded name-based orphans are removed, as planReplacement works it
// out, and true, or nil and false if removing them would fail.
func simulateReplacements(ps *goolib.PkgSpec, installed client.GooGetState) (client.GooGetState, bool) {
	out, err := planReplacement(ps, installed)
	if err != nil {
		return nil, false
	}
	return out.post, true
}

// planReplacement computes, without side effects, the victims, cascaded
// name-based orphans, Provides-only unsatisfied dependents, and topological
// removal order for ps's Replaces entries over installed. Dependents are
// evaluated against the state right after ps commits (installed minus victims
// and ps's overwritten same-arch row, plus ps). A replacer that depends on its
// own victim without providing it fails with a hard error rather than an
// orphan entry.
func planReplacement(ps *goolib.PkgSpec, installed client.GooGetState) (replaceOutcome, error) {
	state := make(client.GooGetState, 0, len(installed))
	byKey := make(map[string]client.PackageState, len(installed))
	for _, st := range installed {
		if st.PackageSpec != nil {
			state = append(state, st)
			byKey[pkgKey(st.PackageSpec)] = st
		}
	}
	victims, err := victimsIn(ps, state)
	if err != nil {
		return replaceOutcome{}, err
	}
	if len(victims) == 0 {
		return replaceOutcome{post: state}, nil
	}

	selfKey := pkgKey(ps)
	removingKeys := make(map[string]bool, len(victims))
	var rootKeys []string
	var removingSpecs []*goolib.PkgSpec
	for _, v := range victims {
		k := pkgKey(v.PackageSpec)
		removingKeys[k] = true
		rootKeys = append(rootKeys, k)
		removingSpecs = append(removingSpecs, v.PackageSpec)
	}

	var remainingSpecs []*goolib.PkgSpec
	for _, st := range state {
		k := pkgKey(st.PackageSpec)
		if removingKeys[k] || k == selfKey {
			continue
		}
		remainingSpecs = append(remainingSpecs, st.PackageSpec)
	}
	remainingSpecs = append(remainingSpecs, ps)

	type orphanKey struct {
		dependent string
		req       relation.Requirement
	}
	orphanByKey := make(map[orphanKey]*OrphanedDependent)
	var orphanOrder []orphanKey
	var listed []relation.Orphan
	for {
		unsat, err := relation.Unsatisfied(removingSpecs, remainingSpecs)
		if err != nil {
			return replaceOutcome{}, err
		}
		for _, o := range unsat {
			if o.Dependent.Name == ps.Name && o.Dependent.Arch == ps.Arch {
				if slices.Contains(rootKeys, pkgKey(o.Removed[0])) {
					return replaceOutcome{}, fmt.Errorf("cannot install %s: it depends on %s, which it replaces and doesn't provide; nothing was changed", ps, o.Removed[0])
				}
				return replaceOutcome{}, fmt.Errorf("cannot install %s: it depends on %s, which depends on a package %s replaces and isn't satisfied afterwards; nothing was changed", ps, o.Removed[0], ps)
			}
		}
		added := false
		for _, o := range unsat {
			if !o.ByName {
				continue
			}
			dk := pkgKey(o.Dependent)
			rk := orphanKey{dependent: dk, req: o.Requirement}
			remInfos := specsToInfos(o.Removed)
			if existing, ok := orphanByKey[rk]; ok {
				for _, ri := range remInfos {
					if !slices.Contains(existing.Removed, ri) {
						existing.Removed = append(existing.Removed, ri)
					}
				}
				slices.SortFunc(existing.Removed, comparePackageInfo)
			} else {
				orphanByKey[rk] = &OrphanedDependent{
					Package:     specInfo(o.Dependent),
					Requirement: o.Requirement.PackageInfo(),
					Removed:     remInfos,
					ReplacedBy:  specInfo(ps),
				}
				orphanOrder = append(orphanOrder, rk)
			}
			if !removingKeys[dk] {
				removingKeys[dk] = true
				removingSpecs = append(removingSpecs, o.Dependent)
				remainingSpecs = slices.DeleteFunc(remainingSpecs, func(s *goolib.PkgSpec) bool {
					return pkgKey(s) == dk
				})
				added = true
			}
		}
		if !added {
			for _, o := range unsat {
				if !o.ByName {
					listed = append(listed, o)
				}
			}
			break
		}
	}

	allKeys := slices.Sorted(maps.Keys(removingKeys))
	order, _, err := remove.OrderSet(allKeys, rootKeys, state)
	if err != nil {
		return replaceOutcome{}, err
	}
	removed := make([]removal, 0, len(order))
	for _, k := range order {
		removed = append(removed, removal{
			row:    byKey[k],
			victim: slices.Contains(rootKeys, k),
		})
	}
	orphans := make([]OrphanedDependent, 0, len(orphanOrder))
	for _, rk := range orphanOrder {
		orphans = append(orphans, *orphanByKey[rk])
	}
	post := slices.DeleteFunc(state, func(st client.PackageState) bool {
		return removingKeys[pkgKey(st.PackageSpec)]
	})
	return replaceOutcome{
		post:    post,
		removed: removed,
		orphans: orphans,
		listed:  listed,
	}, nil
}

// writeListedOrphans writes the human-readable Provides-only unsatisfied
// dependency section for a single replacement step to w when listed is
// non-empty.
func writeListedOrphans(w io.Writer, replacer *goolib.PkgSpec, listed []relation.Orphan) {
	if len(listed) == 0 {
		return
	}
	fmt.Fprintln(w, listedOrphansHeader)
	for _, o := range listed {
		writeListedOrphanLine(w, replacer, o)
	}
}

// writeStepListedOrphans writes a single human-readable Provides-only
// unsatisfied dependency section across steps to w when any step has listed
// entries.
func writeStepListedOrphans(w io.Writer, steps []planStep) {
	listing := false
	for _, s := range steps {
		for _, o := range s.listed {
			if !listing {
				fmt.Fprintln(w, listedOrphansHeader)
				listing = true
			}
			writeListedOrphanLine(w, s.spec.PackageSpec, o)
		}
	}
}

const listedOrphansHeader = "The following packages will have an unsatisfied dependency (not removed):"

// writeListedOrphanLine formats a single Provides-only unsatisfied dependent
// line.
func writeListedOrphanLine(w io.Writer, replacer *goolib.PkgSpec, o relation.Orphan) {
	fmt.Fprintf(w, "  %s (requires %s, provided by %s; replaced by %s)\n",
		o.Dependent, o.Requirement, relation.JoinSpecs(o.Removed, ", "), replacer)
}

// specsToInfos converts specs into sorted PackageInfo values.
func specsToInfos(specs []*goolib.PkgSpec) []goolib.PackageInfo {
	out := make([]goolib.PackageInfo, 0, len(specs))
	for _, ps := range specs {
		info := specInfo(ps)
		if !slices.Contains(out, info) {
			out = append(out, info)
		}
	}
	slices.SortFunc(out, comparePackageInfo)
	return out
}

// comparePackageInfo orders PackageInfo values by Name, Arch, and Ver.
func comparePackageInfo(a, b goolib.PackageInfo) int {
	return cmp.Or(strings.Compare(a.Name, b.Name), strings.Compare(a.Arch, b.Arch), strings.Compare(a.Ver, b.Ver))
}
