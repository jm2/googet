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

	"github.com/google/googet/v2/cli/internal/policy"
	"github.com/google/googet/v2/client"
	"github.com/google/googet/v2/goolib"
	"github.com/google/googet/v2/install"
	"github.com/google/googet/v2/internal/relation"
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
	// opReplace installs a package whose Replaces entries remove an installed
	// package that it also provides.
	opReplace
)

// String returns the word update logs for op.
func (op updateOp) String() string {
	switch op {
	case opDowngrade:
		return "Downgrade"
	case opReplace:
		return "Replace"
	default:
		return "Upgrade"
	}
}

// updateAction is one package that update installs.
type updateAction struct {
	// op is the kind of change.
	op updateOp
	// from is the installed package the action updates. For a replacement it
	// is the installed package whose evaluation chose the replacing package.
	from *goolib.PkgSpec
	// to is the package the action installs.
	to *goolib.PkgSpec
	// repo is the repository that offers to.
	repo string
	// replaced lists, for a replacement, the installed packages that to's
	// Replaces entries remove, in removal order.
	replaced []goolib.PackageInfo
	// removed lists, for a replacement, every installed package the
	// replacement removes, in removal order: the replaced packages and any
	// dependents removed with them.
	removed []goolib.PackageInfo
}

// info returns the name, arch and version of the package a installs.
func (a updateAction) info() goolib.PackageInfo {
	return specInfo(a.to)
}

// skipReason says why update doesn't replace an installed package with a
// package that declares Replaces for it.
type skipReason string

// The reasons a replacement is skipped.
const (
	// reasonMissingProvides means packages declare Replaces for the installed
	// package but none of them also provides its name, as when a package is
	// split into several.
	reasonMissingProvides skipReason = "missing_provides"
	// reasonPinnedPriority means the installed package's own best version
	// outranks every replacing package, or no repository offers the
	// installed package and every replacing package comes from a repository
	// below the default priority.
	reasonPinnedPriority skipReason = "pinned_priority"
	// reasonAmbiguous means more than one package name replaces and provides
	// the installed package at the best priority.
	reasonAmbiguous skipReason = "ambiguous_replacers"
	// reasonCycle means the replacing package is itself replaced by the
	// installed package's own best version, by an installed package, or by
	// a chain of replacing packages that leads back to the installed one.
	reasonCycle skipReason = "replaces_cycle"
	// reasonOrphans means the replacement would leave installed packages
	// with an unsatisfied dependency.
	reasonOrphans skipReason = "orphaned_dependents"
	// reasonRejected means planning the replacement failed for another
	// reason.
	reasonRejected skipReason = "plan_rejected"
)

// replaceSkip records that update leaves an installed package where it is
// instead of replacing it.
type replaceSkip struct {
	// pkg is the installed package.
	pkg *goolib.PkgSpec
	// replacer is the replacing package that was chosen, or nil if none was.
	replacer *goolib.PkgSpec
	// reason says why the replacement is skipped.
	reason skipReason
	// candidates lists the competing replacing packages for
	// reasonAmbiguous, or the packages that replace without providing for
	// reasonMissingProvides.
	candidates []*goolib.PkgSpec
	// err is the planning error for reasonOrphans and reasonRejected.
	err error
}

// diagKind names a structured diagnostic that planning records.
type diagKind string

// The diagnostics planning records.
const (
	// diagSplitIgnored means packages that replace the installed package
	// without providing it were ignored in favour of one that does both.
	diagSplitIgnored diagKind = "update_replace_split_ignored"
	// diagCoexistingCurrent means the package that replaces and provides the
	// installed package is installed at its best version already, so the
	// installed package is left alone instead of being updated in place.
	diagCoexistingCurrent diagKind = "update_replace_coexisting_current"
)

// replaceDiag is a structured diagnostic about a replacement.
type replaceDiag struct {
	kind diagKind
	// pkg is the installed package.
	pkg *goolib.PkgSpec
	// replacer is the package that replaces and provides pkg.
	replacer *goolib.PkgSpec
	// others lists, for diagSplitIgnored, the ignored packages.
	others []*goolib.PkgSpec
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
	// pkg is the name.arch key of the installed package the first pass
	// recorded the note for, or "" for notes the second pass records.
	pkg string
}

// planEntry is an action, a skipped replacement, a diagnostic or a note.
// Entries keep the order in which planning produced them, so that rendering
// emits them in that order.
type planEntry struct {
	action *updateAction
	skip   *replaceSkip
	diag   *replaceDiag
	note   *planNote
}

// updatePlan is what an update would do: the actions to run, in order, and
// the skipped replacements and log lines that explain the packages it leaves
// alone.
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

// addAction appends a to the plan.
func (p *updatePlan) addAction(a updateAction) {
	p.entries = append(p.entries, planEntry{action: &a})
}

// addSkip appends s to the plan.
func (p *updatePlan) addSkip(s replaceSkip) {
	p.entries = append(p.entries, planEntry{skip: &s})
}

// addDiag appends d to the plan.
func (p *updatePlan) addDiag(d replaceDiag) {
	p.entries = append(p.entries, planEntry{diag: &d})
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

// updateConfig holds what planUpdates needs besides the installed state and
// the repositories, so that it never reads settings itself.
type updateConfig struct {
	// archs are the installable archs, most preferred first.
	archs []string
	// opts are the install options a replacement is planned with; only the
	// package conflict and orphaned dependent policies matter.
	opts install.Options
	// followReplaces makes update replace an installed package that a
	// repository still offers with a package that declares both Replaces and
	// Provides for it.
	followReplaces bool
}

// planUpdates works out, without side effects, which installed packages in
// state update would change, using the packages rm offers. It neither logs
// nor prints: it returns the actions, the skipped replacements and the log
// lines that explain them for renderUpdates to emit.
//
// Planning runs in two passes. The first pass looks for replacements of each
// installed package P: always when no repository offers P for its arch any
// more and the package that would replace it through its Provides also
// declares Replaces for P, and for every P when cfg.followReplaces is set. A
// replacement must pass every rule in selectReplacer, and its install is
// planned with install.PlanState over the installed state as the earlier
// replacements leave it. A replacement that passes is scheduled, and every
// package it removes or installs is left out of the rest of the planning;
// one that fails is skipped and recorded with its reason. Replacements are
// scheduled greedily in the order of state, which googetdb lists by name and
// arch, so when two replacements exclude each other the first one wins. Once
// the first pass ends, the skips, diagnostics and notes it recorded for
// packages that a later replacement removes or installs are dropped. The
// second pass plans in-place updates for the remaining packages exactly as
// update always has, including moving to a package that provides P without
// declaring Replaces for it. A package whose replacement was skipped because
// of the Provides fallback is left installed, while one whose replacement
// cfg.followReplaces asked for is still updated in place.
func planUpdates(state client.GooGetState, rm client.RepoMap, cfg updateConfig) updatePlan {
	pl := newPlanner(state, rm, cfg)
	for _, row := range state {
		if row.PackageSpec == nil || pl.suppressed[pkgKey(row.PackageSpec)] {
			continue
		}
		pl.planReplacement(row.PackageSpec)
	}
	pl.pruneStale()
	for _, row := range state {
		p := row.PackageSpec
		if p == nil || pl.suppressed[pkgKey(p)] || pl.handled[pkgKey(p)] {
			continue
		}
		planInPlace(&pl.plan, p, pl.installedOrScheduled(), pl.repos)
	}
	return pl.plan
}

// planner holds the working state of planUpdates.
type planner struct {
	state client.GooGetState
	rm    client.RepoMap
	cfg   updateConfig
	// archPref ranks cfg.archs, most preferred first.
	archPref map[string]int
	// repos lists the repositories in rm, ordered by URL.
	repos []repocmp.Repo
	// offered lists every package rm offers for one of cfg.archs, with
	// repositories in URL order.
	offered []repocmp.Candidate
	// working is the installed state as the replacements scheduled so far
	// leave it.
	working client.GooGetState
	// suppressed holds the name.arch keys of installed packages that a
	// scheduled replacement removes or installs, which planning leaves
	// alone from then on.
	suppressed map[string]bool
	// handled holds the name.arch keys of installed packages whose update
	// the first pass decided, which the second pass leaves alone.
	handled map[string]bool
	// installs holds the name.arch keys of the packages that scheduled
	// replacements install, which a later replacement must not remove.
	installs map[string]bool
	// removes holds the name.arch keys of the packages that scheduled
	// replacements remove, which a later replacement must not install again.
	removes map[string]bool
	// stale holds the name.arch keys of installed packages that a scheduled
	// replacement for another installed package removes or installs, whose
	// earlier skips and notes no longer describe what the update does.
	stale map[string]bool
	plan  updatePlan
}

// newPlanner returns a planner for state and rm under cfg.
func newPlanner(state client.GooGetState, rm client.RepoMap, cfg updateConfig) *planner {
	pl := &planner{
		state:      state,
		rm:         rm,
		cfg:        cfg,
		archPref:   repocmp.ArchPreference(cfg.archs),
		working:    slices.Clone(state),
		suppressed: make(map[string]bool),
		handled:    make(map[string]bool),
		installs:   make(map[string]bool),
		removes:    make(map[string]bool),
		stale:      make(map[string]bool),
	}
	pl.repos = repoList(rm)
	for _, r := range pl.repos {
		for _, ps := range r.Specs {
			if ps != nil && slices.Contains(cfg.archs, ps.Arch) {
				pl.offered = append(pl.offered, repocmp.Candidate{Spec: ps, Repo: r.URL, Priority: r.Priority})
			}
		}
	}
	return pl
}

// planReplacement decides, in the first pass, whether the installed package
// p is replaced, and records the outcome in the plan.
func (pl *planner) planReplacement(p *goolib.PkgSpec) {
	legacy, ok, _ := latestFor(p, pl.repos)
	fallback := ok && legacy.Spec.Name != p.Name && replaces(legacy.Spec, p)
	if !fallback && !pl.cfg.followReplaces {
		// Either a repository still offers p for its arch, or the package
		// that would replace p through its Provides doesn't declare
		// Replaces for it: the second pass updates p as before.
		return
	}
	var fallbackSpec *goolib.PkgSpec
	if fallback {
		// The package that provides p also replaces it, so the update goes
		// through the replacement rules and never falls back to installing
		// that package unchecked.
		pl.handled[pkgKey(p)] = true
		fallbackSpec = legacy.Spec
	}
	winner, ok := pl.selectReplacer(p, fallbackSpec)
	if !ok {
		return
	}
	if inst := pl.installedRow(winner.Spec); inst != nil {
		if _, pending, _ := compareInstalled(winner, inst); !pending {
			// The replacing package is installed at its best version
			// already. An installed replacing package with a pending
			// update is replaced below, so that its update removes p and
			// runs only once.
			if pl.cfg.followReplaces && replaces(inst, p) && providesName(inst, p.Name) && !replaces(p, inst) {
				// Updating p in place would only install a package that the
				// installed replacing package replaces.
				pl.suppressed[pkgKey(p)] = true
				pl.plan.addDiag(replaceDiag{kind: diagCoexistingCurrent, pkg: p, replacer: inst})
				return
			}
			pl.plan.addNote(planNote{level: noteInfo, text: fmt.Sprintf("%s.%s - %s already installed", winner.Spec.Name, winner.Spec.Arch, winner.Spec.Version), pkg: pkgKey(p)})
			return
		}
	}
	// Following Replaces may move to a lower version, as when a renamed
	// package restarts its numbering; the Provides fallback keeps requiring
	// a higher priority or version.
	if !pl.cfg.followReplaces {
		if _, ok, why := compareInstalled(winner, p); !ok {
			why.pkg = pkgKey(p)
			pl.plan.addNote(why)
			return
		}
	}
	pl.scheduleReplacement(p, winner)
}

// selectReplacer picks the package that replaces the installed package p, or
// records why there is none. Every package name rm offers for p's arch or
// noarch, honouring LockArch, counts once, at its highest-ranked version;
// older versions are ignored. A name qualifies when that version
// (a) declares Replaces for p at p's version and (b) provides p's name. A
// qualifying package is chosen only if (c) it is the only qualifying name at
// the best priority among them, p's own best version doesn't come from a
// higher priority, and, when no repository offers p, the best priority isn't
// below the default; and (d) neither p's best version for any arch, nor an
// installed package, nor a chain of qualifying packages starting from it
// replaces it in turn. Names that replace p without providing it never
// qualify: with cfg.followReplaces set, they are reported as a skip when
// nothing qualifies, and as a diagnostic when something else does.
//
// fallback, if not nil, is the package the Provides fallback would install
// for p, which replaces p. selectReplacer returns false when it picks
// nothing, and then records a skip that says why, except when it was only
// asked because cfg.followReplaces is set and no package replaces p at all.
func (pl *planner) selectReplacer(p, fallback *goolib.PkgSpec) (repocmp.Candidate, bool) {
	var full, split []repocmp.Candidate
	var pBest *repocmp.Candidate
	for _, c := range pl.bestPerName(pl.admissible(p)) {
		switch {
		case c.Spec.Name == p.Name:
			pBest = &c
		case !replaces(c.Spec, p):
			// A name that doesn't replace p plays no part.
		case providesName(c.Spec, p.Name):
			full = append(full, c)
		default:
			split = append(split, c)
		}
	}
	if len(full) == 0 {
		switch {
		case len(split) > 0 && pl.cfg.followReplaces:
			pl.plan.addSkip(replaceSkip{pkg: p, reason: reasonMissingProvides, candidates: specsOf(split)})
		case fallback != nil:
			// No version of the provider that the rules consider still
			// replaces and provides p, for example because its best version
			// dropped them. p stays installed, and the skip says why.
			pl.plan.addSkip(replaceSkip{pkg: p, replacer: fallback, reason: reasonRejected,
				err: fmt.Errorf("the best version of %s offered for %s or noarch doesn't both replace and provide %s", fallback.Name, p.Arch, p.Name)})
		}
		return repocmp.Candidate{}, false
	}
	if len(split) > 0 {
		pl.plan.addDiag(replaceDiag{kind: diagSplitIgnored, pkg: p, replacer: full[0].Spec, others: specsOf(split)})
	}
	maxPrio := full[0].Priority
	for _, c := range full[1:] {
		maxPrio = max(maxPrio, c.Priority)
	}
	var top []repocmp.Candidate
	for _, c := range full {
		if c.Priority == maxPrio {
			top = append(top, c)
		}
	}
	if (pBest != nil && pBest.Priority > maxPrio) || (pBest == nil && maxPrio < priority.Default) {
		pl.plan.addSkip(replaceSkip{pkg: p, replacer: top[0].Spec, reason: reasonPinnedPriority})
		return repocmp.Candidate{}, false
	}
	if len(top) > 1 {
		pl.plan.addSkip(replaceSkip{pkg: p, reason: reasonAmbiguous, candidates: specsOf(top)})
		return repocmp.Candidate{}, false
	}
	winner := top[0]
	if pl.cyclic(p, winner.Spec) {
		pl.plan.addSkip(replaceSkip{pkg: p, replacer: winner.Spec, reason: reasonCycle})
		return repocmp.Candidate{}, false
	}
	return winner, true
}

// admissible returns the offered packages that may update the installed
// package p: those for p's arch or noarch that p's LockArch allows.
func (pl *planner) admissible(p *goolib.PkgSpec) []repocmp.Candidate {
	var out []repocmp.Candidate
	for _, c := range pl.offered {
		if (c.Spec.Arch == p.Arch || c.Spec.Arch == "noarch") && repocmp.ArchAllowed(p.Arch, p.LockArch, c.Spec.Arch, c.Spec.LockArch) {
			out = append(out, c)
		}
	}
	return out
}

// bestPerName returns the highest-ranked candidate of each package name in
// cands, ordered by name.
func (pl *planner) bestPerName(cands []repocmp.Candidate) []repocmp.Candidate {
	byName := make(map[string][]repocmp.Candidate)
	for _, c := range cands {
		byName[c.Spec.Name] = append(byName[c.Spec.Name], c)
	}
	var out []repocmp.Candidate
	for _, name := range slices.Sorted(maps.Keys(byName)) {
		list := slices.Clone(byName[name])
		// The order is well defined even when a version can't be parsed, so
		// the comparison errors are not needed here; the second pass
		// reports them for the packages it looks up.
		repocmp.SortCandidates(list, pl.archPref)
		out = append(out, list[0])
	}
	return out
}

// cyclic reports whether replacing the installed package p with r would be
// undone by another replacement: when p's best offered version for any of
// cfg.archs, or an installed package other than r's own row, declares
// Replaces for r, or when following qualifying replacing packages from r
// leads back to p's name or around a loop.
func (pl *planner) cyclic(p, r *goolib.PkgSpec) bool {
	var own []repocmp.Candidate
	for _, c := range pl.offered {
		if c.Spec.Name == p.Name {
			own = append(own, c)
		}
	}
	byArch := make(map[string][]repocmp.Candidate)
	for _, c := range own {
		byArch[c.Spec.Arch] = append(byArch[c.Spec.Arch], c)
	}
	for _, list := range byArch {
		if replaces(pl.bestPerName(list)[0].Spec, r) {
			return true
		}
	}
	for _, row := range pl.working {
		if row.PackageSpec != nil && pkgKey(row.PackageSpec) != pkgKey(r) && replaces(row.PackageSpec, r) {
			return true
		}
	}
	best := pl.bestPerName(pl.offered)
	onPath := map[string]bool{p.Name: true}
	done := make(map[string]bool)
	var walk func(x *goolib.PkgSpec) bool
	walk = func(x *goolib.PkgSpec) bool {
		if onPath[x.Name] {
			return true
		}
		if done[x.Name] {
			return false
		}
		onPath[x.Name] = true
		defer func() { onPath[x.Name] = false; done[x.Name] = true }()
		for _, c := range best {
			if c.Spec.Name != x.Name && replaces(c.Spec, x) && providesName(c.Spec, x.Name) && walk(c.Spec) {
				return true
			}
		}
		return false
	}
	return walk(r)
}

// installedRow returns the working state's installed package with ps's name
// and arch, or nil.
func (pl *planner) installedRow(ps *goolib.PkgSpec) *goolib.PkgSpec {
	for _, row := range pl.working {
		if row.PackageSpec != nil && pkgKey(row.PackageSpec) == pkgKey(ps) {
			return row.PackageSpec
		}
	}
	return nil
}

// installedOrScheduled returns the installed packages together with the
// packages the replacements scheduled so far install, so that the Provides
// fallback of the second pass never installs a package a second time.
func (pl *planner) installedOrScheduled() client.GooGetState {
	return append(slices.Clone(pl.state), pl.working...)
}

// scheduleReplacement plans the install of winner over the working state to
// replace the installed package p. If the install plans cleanly and removes
// p, it schedules the replacement, advances the working state and leaves
// every package the replacement removes or installs out of the rest of the
// planning. Otherwise it records why the replacement is skipped.
func (pl *planner) scheduleReplacement(p *goolib.PkgSpec, winner repocmp.Candidate) {
	ip, err := install.PlanState(specInfo(winner.Spec), winner.Repo, pl.rm, pl.working, pl.cfg.archs, pl.cfg.opts)
	if err != nil {
		reason := reasonRejected
		if policy.OrphanOnly(err) {
			reason = reasonOrphans
		}
		pl.plan.addSkip(replaceSkip{pkg: p, replacer: winner.Spec, reason: reason, err: err})
		return
	}
	removed, installs := ip.Removals(), ip.Installs()
	// This guard is defensive: a winner that matched p's Replaces entry always
	// removes p, but the replacement must never be scheduled unless it does.
	if !slices.ContainsFunc(removed, func(pi goolib.PackageInfo) bool { return infoKey(pi) == pkgKey(p) }) {
		pl.plan.addSkip(replaceSkip{pkg: p, replacer: winner.Spec, reason: reasonRejected, err: fmt.Errorf("installing %s doesn't remove %s", winner.Spec, p)})
		return
	}
	// A later replacement must not undo an earlier one in the same update,
	// in either direction: removing a package an earlier replacement
	// installs, or installing again a package an earlier replacement
	// removes.
	for _, pi := range removed {
		if pl.installs[infoKey(pi)] {
			pl.plan.addSkip(replaceSkip{pkg: p, replacer: winner.Spec, reason: reasonRejected, err: fmt.Errorf("installing %s removes %s, which an earlier replacement installs", winner.Spec, pi)})
			return
		}
	}
	for _, pi := range installs {
		if pl.removes[infoKey(pi)] {
			pl.plan.addSkip(replaceSkip{pkg: p, replacer: winner.Spec, reason: reasonRejected, err: fmt.Errorf("installing %s installs %s again, which an earlier replacement removes", winner.Spec, pi)})
			return
		}
	}
	pl.plan.addAction(updateAction{op: opReplace, from: p, to: winner.Spec, repo: winner.Repo, replaced: ip.Replaced(), removed: removed})
	pl.working = ip.State()
	pl.suppressed[pkgKey(p)] = true
	for _, pi := range removed {
		pl.removes[infoKey(pi)] = true
	}
	for _, pi := range installs {
		pl.installs[infoKey(pi)] = true
	}
	for _, pi := range append(slices.Clone(removed), installs...) {
		k := infoKey(pi)
		pl.suppressed[k] = true
		if k != pkgKey(p) {
			pl.stale[k] = true
		}
	}
}

// pruneStale drops the skips, diagnostics and notes recorded for installed
// packages that a later replacement for another installed package removes or
// installs, so that a package the update removes or installs is never also
// reported as not replaced or as left alone. A package's own replacement, or its
// update_replace_coexisting_current diagnostic, never makes its entries
// stale. Which of two replacements that exclude each other is scheduled
// still follows the order of the installed packages.
func (pl *planner) pruneStale() {
	pl.plan.entries = slices.DeleteFunc(pl.plan.entries, func(e planEntry) bool {
		switch {
		case e.skip != nil:
			return pl.stale[pkgKey(e.skip.pkg)]
		case e.diag != nil:
			return pl.stale[pkgKey(e.diag.pkg)]
		case e.note != nil:
			return pl.stale[e.note.pkg]
		}
		return false
	})
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

// providesName reports whether ps has a Provides entry for name, at any
// version.
func providesName(ps *goolib.PkgSpec, name string) bool {
	ok, _ := relation.ProvidedBy(ps, relation.Requirement{Name: name})
	return ok
}

// replaces reports whether one of ps's Replaces entries names the package
// target at a version that meets the entry's version constraint. Entries
// whose versions can't be compared don't count.
func replaces(ps, target *goolib.PkgSpec) bool {
	return relation.ReplacesInstalled(ps.Replaces, target.Name, target.Arch, target.Version)
}

// specsOf returns the specs of cands, in order.
func specsOf(cands []repocmp.Candidate) []*goolib.PkgSpec {
	out := make([]*goolib.PkgSpec, 0, len(cands))
	for _, c := range cands {
		out = append(out, c.Spec)
	}
	return out
}

// pkgKey returns ps's name.arch key.
func pkgKey(ps *goolib.PkgSpec) string {
	return ps.Name + "." + ps.Arch
}

// infoKey returns pi's name.arch key.
func infoKey(pi goolib.PackageInfo) string {
	return pi.Name + "." + pi.Arch
}

// specInfo returns ps's name, arch and version.
func specInfo(ps *goolib.PkgSpec) goolib.PackageInfo {
	return goolib.PackageInfo{Name: ps.Name, Arch: ps.Arch, Ver: ps.Version}
}
