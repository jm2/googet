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
	"errors"
	"fmt"
	"slices"

	"github.com/google/googet/v2/client"
	"github.com/google/googet/v2/goolib"
	"github.com/google/googet/v2/internal/relation"
)

// pkgKey returns the database identity key (name.arch) for ps.
func pkgKey(ps *goolib.PkgSpec) string {
	return ps.Name + "." + ps.Arch
}

// exclusions identifies installed packages, keyed as name.arch, that conflict
// checks skip because installing the incoming package removes them.
type exclusions struct {
	// self is the incoming package's own name.arch key.
	self string
	// victims holds the name.arch keys of rows that Replaces entries remove
	// during a repository install.
	victims map[string]bool
}

// selfExclusion returns an exclusion set containing only ps itself.
func selfExclusion(ps *goolib.PkgSpec) exclusions {
	return exclusions{self: pkgKey(ps)}
}

// victimsIn returns every installed row in st that ps's Replaces entries select
// via relation.Resolve, deduplicated by name.arch.
func victimsIn(ps *goolib.PkgSpec, st client.GooGetState) ([]client.PackageState, error) {
	if len(ps.Replaces) == 0 {
		return nil, nil
	}
	specs := specsFromState(st)
	byKey := make(map[string]client.PackageState, len(st))
	for _, row := range st {
		if row.PackageSpec != nil {
			byKey[pkgKey(row.PackageSpec)] = row
		}
	}
	seen := make(map[string]bool)
	var out []client.PackageState
	for _, raw := range ps.Replaces {
		matches, err := relation.Resolve(relation.ParseRelation(raw), specs)
		if err != nil {
			return nil, err
		}
		for _, m := range matches {
			k := pkgKey(m)
			if !seen[k] {
				seen[k] = true
				out = append(out, byKey[k])
			}
		}
	}
	return out, nil
}

// repoExclusionsIn returns the initial exclusion set for installing ps from a
// repository over the installed state st, covering ps itself and every row ps's
// Replaces entries select.
func repoExclusionsIn(ps *goolib.PkgSpec, st client.GooGetState) (exclusions, error) {
	ex := selfExclusion(ps)
	victims, err := victimsIn(ps, st)
	if err != nil {
		return exclusions{}, err
	}
	for _, v := range victims {
		if ex.victims == nil {
			ex.victims = make(map[string]bool, len(victims))
		}
		ex.victims[pkgKey(v.PackageSpec)] = true
	}
	return ex, nil
}

// has reports whether key is excluded from conflict checks.
func (e exclusions) has(key string) bool {
	return key != "" && (key == e.self || e.victims[key])
}

// specsFromState extracts the non-nil PkgSpec pointers from state.
func specsFromState(state client.GooGetState) []*goolib.PkgSpec {
	specs := make([]*goolib.PkgSpec, 0, len(state))
	for _, st := range state {
		if st.PackageSpec != nil {
			specs = append(specs, st.PackageSpec)
		}
	}
	return specs
}

// providesRequirement reports whether ps declares a Provides entry that
// satisfies r.
func providesRequirement(ps *goolib.PkgSpec, r relation.Requirement) bool {
	for _, prov := range ps.Provides {
		if client.SatisfiesProvider(prov, r.Name, r.Ver) {
			return true
		}
	}
	return false
}

// checkForwardConflict evaluates a single Conflicts requirement r against all
// installed package specs under ex. It returns whether an unexcluded conflict
// exists and any excluded rows that matched r.
func checkForwardConflict(r relation.Requirement, installed []*goolib.PkgSpec, ex exclusions) (hit bool, exempted []*goolib.PkgSpec, err error) {
	matches, err := relation.Resolve(r, installed)
	if err != nil {
		return false, nil, err
	}
	for _, m := range matches {
		if !ex.has(pkgKey(m)) {
			return true, exempted, nil
		}
		exempted = append(exempted, m)
	}
	for _, inst := range installed {
		if inst == nil || !providesRequirement(inst, r) {
			continue
		}
		if !ex.has(pkgKey(inst)) {
			return true, exempted, nil
		}
		exempted = append(exempted, inst)
	}
	return false, exempted, nil
}

// resolveConflictsIn fails if any of ps.Conflicts matches a row of the
// installed state st not excluded by ex, sending its log lines to sink. It
// returns the entries that passed only because a Replaces victim was exempt;
// callers must recheck them once the replacements are removed.
func resolveConflictsIn(ps *goolib.PkgSpec, ex exclusions, st client.GooGetState, sink eventSink) ([]relation.Requirement, error) {
	if len(ps.Conflicts) == 0 {
		return nil, nil
	}
	// Check for any conflicting packages.
	// TODO(ajackura): Make sure no conflicting packages are listed as
	// dependencies or subdependancies.
	installed := specsFromState(st)
	selfOnly := exclusions{self: ex.self}
	logged := make(map[string]bool)
	var deferred []relation.Requirement
	for _, pkg := range ps.Conflicts {
		r := relation.ParseRelation(pkg)
		// Check if the conflicting package or a provider of it is installed.
		hit, exempted, err := checkForwardConflict(r, installed, ex)
		if err != nil {
			return nil, err
		}
		if hit {
			return nil, fmt.Errorf("cannot install, conflict with installed package or provider: %s", r.PackageInfo())
		}
		for _, exPkg := range exempted {
			k := pkgKey(exPkg)
			if !logged[k] {
				logged[k] = true
				sink.Infof("conflict %q with %s skipped: installing %s removes it", pkg, exPkg, ps)
			}
		}
		if len(ex.victims) > 0 {
			selfHit, _, err := checkForwardConflict(r, installed, selfOnly)
			if err != nil {
				return nil, err
			}
			if selfHit {
				deferred = append(deferred, r)
			}
		}
	}
	return deferred, nil
}

// ErrPackageConflict marks an installation failure under ConflictError mode
// caused by an installed package's Conflicts entry matching the incoming
// package, or by an installed package's Replaces entry naming it.
var ErrPackageConflict = errors.New("package conflict")

// reverseKind identifies which relation field of an installed package a
// reverse hit comes from.
type reverseKind int

const (
	// revConflict is a hit from an installed package's Conflicts entry.
	revConflict reverseKind = iota
	// revReplaces is a hit from an installed package's Replaces entry.
	revReplaces
)

// direction returns the direction field of the structured log line for k.
func (k reverseKind) direction() string {
	if k == revReplaces {
		return "reverse_replaces"
	}
	return "reverse"
}

// reverseHit is a single reverse relation result between an installed package
// and an incoming package: either an installed package's Conflicts entry
// matches the incoming package, or its Replaces entry names it.
type reverseHit struct {
	kind        reverseKind
	installed   *goolib.PkgSpec
	entry       string
	unparseable bool
}

// matchReverseEntry evaluates a single Conflicts entry c of an installed
// package against incoming package ps across ps's name and all of ps.Provides.
// A valid match takes precedence over an unparseable sibling Provides version.
func matchReverseEntry(c string, ps *goolib.PkgSpec) (matched, unparseable bool) {
	r := relation.ParseRelation(c)
	ok, err := r.MinInstalled(ps.Name, ps.Arch, ps.Version)
	if ok && err == nil {
		return true, false
	}
	hadErr := err != nil
	for _, prov := range ps.Provides {
		pOK, pErr := r.SatisfiedBy(relation.ParseProvide(prov))
		if pErr != nil {
			hadErr = true
			continue
		}
		if pOK {
			return true, false
		}
	}
	return false, hadErr
}

// matchReverseReplaces evaluates a single Replaces entry e of an installed
// package against incoming package ps's own name, arch and version only, as
// Replaces selects its victims, never against ps.Provides. An entry that names
// ps's identity but whose version can't be compared is reported as
// unparseable.
func matchReverseReplaces(e string, ps *goolib.PkgSpec) (matched, unparseable bool) {
	r := relation.ParseRelation(e)
	ok, err := r.MinInstalled(ps.Name, ps.Arch, ps.Version)
	if err != nil {
		return false, r.SameIdentity(ps.Name, ps.Arch)
	}
	return ok, false
}

// reverseConflicts evaluates all Conflicts and Replaces entries of
// non-excluded specs in installed against incoming package ps, returning at
// most one hit per (installed package, relation kind, entry) triple. For each
// installed package its Conflicts hits come before its Replaces hits.
func reverseConflicts(ps *goolib.PkgSpec, installed []*goolib.PkgSpec, ex exclusions) []reverseHit {
	var hits []reverseHit
	for _, inst := range installed {
		if inst == nil || ex.has(pkgKey(inst)) {
			continue
		}
		for _, c := range inst.Conflicts {
			matched, unparseable := matchReverseEntry(c, ps)
			if matched || unparseable {
				hits = append(hits, reverseHit{kind: revConflict, installed: inst, entry: c, unparseable: !matched})
			}
		}
		for _, e := range inst.Replaces {
			matched, unparseable := matchReverseReplaces(e, ps)
			if matched || unparseable {
				hits = append(hits, reverseHit{kind: revReplaces, installed: inst, entry: e, unparseable: !matched})
			}
		}
	}
	return hits
}

// conflictLine formats the structured telemetry log line for a reverse package
// conflict event of kind k.
func conflictLine(k reverseKind, mode ConflictMode, result string, installed *goolib.PkgSpec, entry string, incoming *goolib.PkgSpec) string {
	return fmt.Sprintf("package_conflict direction=%s mode=%s result=%s installed=%s entry=%q incoming=%s",
		k.direction(), mode, result, installed, entry, incoming)
}

// applyReverseConflicts logs each hit in hits to sink according to mode,
// prints user warnings in warn mode, and returns an error on real conflicts in
// error mode.
func applyReverseConflicts(ps *goolib.PkgSpec, hits []reverseHit, mode ConflictMode, sink eventSink) error {
	var firstErr error
	for _, hit := range hits {
		if hit.unparseable {
			sink.Warning(conflictLine(hit.kind, mode, "unparseable", hit.installed, hit.entry, ps))
			continue
		}
		switch mode {
		case ConflictError:
			sink.Error(conflictLine(hit.kind, mode, "failed", hit.installed, hit.entry, ps))
			if firstErr == nil {
				firstErr = reverseError(hit, ps, mode)
			}
		case ConflictIgnore:
			sink.Infof("%s", conflictLine(hit.kind, mode, "ignored", hit.installed, hit.entry, ps))
		default:
			sink.Warning(conflictLine(hit.kind, ConflictWarn, "warned", hit.installed, hit.entry, ps))
			if hit.kind == revReplaces {
				sink.Printf("Warning: installed package %s replaces %s (entry %q); installing anyway (package_conflicts=warn)\n", hit.installed, ps, hit.entry)
			} else {
				sink.Printf("Warning: installed package %s conflicts with %s (entry %q); installing anyway (package_conflicts=warn)\n", hit.installed, ps, hit.entry)
			}
		}
	}
	return firstErr
}

// reverseError returns the ErrPackageConflict error that hit causes when ps is
// installed under mode.
func reverseError(hit reverseHit, ps *goolib.PkgSpec, mode ConflictMode) error {
	if hit.kind == revReplaces {
		return fmt.Errorf("%w: cannot install %s: installed package %s replaces it (entry %q; package conflict mode is %s)", ErrPackageConflict, ps, hit.installed, hit.entry, mode)
	}
	return fmt.Errorf("%w: cannot install %s: conflict with installed package %s (entry %q; package conflict mode is %s)", ErrPackageConflict, ps, hit.installed, hit.entry, mode)
}

// resolveReverseConflictsIn checks the installed specs' Conflicts and Replaces
// against ps under ex and mode, sending its log lines and warnings to sink,
// and returns the name.arch keys of any excluded victim rows that matched ps
// so they can be re-verified once the replacements are removed.
func resolveReverseConflictsIn(ps *goolib.PkgSpec, ex exclusions, mode ConflictMode, installed []*goolib.PkgSpec, sink eventSink) (map[string]bool, error) {
	if err := applyReverseConflicts(ps, reverseConflicts(ps, installed, ex), mode, sink); err != nil {
		return nil, err
	}
	if len(ex.victims) == 0 {
		return nil, nil
	}
	var victims []*goolib.PkgSpec
	for _, inst := range installed {
		if inst != nil {
			k := pkgKey(inst)
			if ex.victims[k] && k != ex.self {
				victims = append(victims, inst)
			}
		}
	}
	victimHits := reverseConflicts(ps, victims, exclusions{self: ex.self})
	if len(victimHits) == 0 {
		return nil, nil
	}
	deferred := make(map[string]bool, len(victimHits))
	for _, h := range victimHits {
		deferred[pkgKey(h.installed)] = true
	}
	return deferred, nil
}

// deferredChecks holds forward and reverse conflict checks that passed before
// dependency installation only because a Replaces victim was exempted, so
// callers can verify after replacements run that no conflicting row survived.
type deferredChecks struct {
	// fwd holds parsed ps.Conflicts requirements that passed only via a victim exemption.
	fwd []relation.Requirement
	// rev maps name.arch keys of exempted victims whose Conflicts matched ps
	// or whose Replaces named it.
	rev map[string]bool
}

// checkConflictsIn evaluates both ps's forward Conflicts and the installed
// packages' reverse Conflicts and Replaces over the installed state st under
// ex and mode, sending log lines and warnings to sink, and returns any checks
// deferred until the replacements are removed.
func checkConflictsIn(ps *goolib.PkgSpec, ex exclusions, mode ConflictMode, st client.GooGetState, sink eventSink) (deferredChecks, error) {
	fwd, err := resolveConflictsIn(ps, ex, st, sink)
	if err != nil {
		return deferredChecks{}, err
	}
	rev, err := resolveReverseConflictsIn(ps, ex, mode, specsFromState(st), sink)
	if err != nil {
		return deferredChecks{}, err
	}
	return deferredChecks{fwd: fwd, rev: rev}, nil
}

// revSpecs returns the specs in installed whose name.arch key is recorded in d.rev.
func (d deferredChecks) revSpecs(installed []*goolib.PkgSpec) []*goolib.PkgSpec {
	var out []*goolib.PkgSpec
	for _, inst := range installed {
		if inst != nil && d.rev[pkgKey(inst)] {
			out = append(out, inst)
		}
	}
	return out
}

// recheckBeforeReplacementsIn simulates ps's replacements over the
// post-dependency installed state st and re-evaluates deferred forward
// entries and (in ConflictError mode) deferred reverse victim rows against the
// simulated post-replacement state with self-only exclusions, failing before
// any package would be removed whenever the post-replacement recheck would
// fail. If the simulation can't complete, it logs to sink at info level and
// leaves the checks to recheckIn.
func (d deferredChecks) recheckBeforeReplacementsIn(ps *goolib.PkgSpec, mode ConflictMode, st client.GooGetState, sink eventSink) error {
	needFwd := len(d.fwd) > 0
	needRev := mode == ConflictError && len(d.rev) > 0
	if !needFwd && !needRev {
		return nil
	}
	post, ok := simulateReplacements(ps, st)
	if !ok {
		sink.Infof("Replacement simulation for %s did not complete; deferring conflict checks until after replacements", ps)
		return nil
	}
	postSpecs := specsFromState(post)
	if needFwd {
		if err := recheckForwardEntries(d.fwd, ps, postSpecs, ""); err != nil {
			return err
		}
	}
	if !needRev {
		return nil
	}
	// Unparseable conflict versions never fail installation even in
	// ConflictError mode; drop them here so all-unparseable rows defer to
	// recheck and mixed rows do not log unparseable warnings twice.
	hits := slices.DeleteFunc(reverseConflicts(ps, d.revSpecs(postSpecs), selfExclusion(ps)), func(h reverseHit) bool {
		return h.unparseable
	})
	if len(hits) == 0 {
		return nil
	}
	return applyReverseConflicts(ps, hits, ConflictError, sink)
}

// recheckIn re-evaluates deferred forward and reverse conflict checks with
// self-only exclusions over the post-replacement installed state st, sending
// its log lines and warnings to sink, to make sure no exempted victim
// survived.
func (d deferredChecks) recheckIn(ps *goolib.PkgSpec, mode ConflictMode, st client.GooGetState, sink eventSink) error {
	if len(d.fwd) == 0 && len(d.rev) == 0 {
		return nil
	}
	installed := specsFromState(st)
	if len(d.fwd) > 0 {
		if err := recheckForwardEntries(d.fwd, ps, installed, " (still installed after replacing)"); err != nil {
			return err
		}
	}
	surviving := d.revSpecs(installed)
	if len(surviving) == 0 {
		return nil
	}
	return applyReverseConflicts(ps, reverseConflicts(ps, surviving, selfExclusion(ps)), mode, sink)
}

// recheckForwardEntries checks each requirement in reqs against installed
// under selfExclusion(ps) without repeating exemption logs, appending suffix
// to the conflict error on a hit.
func recheckForwardEntries(reqs []relation.Requirement, ps *goolib.PkgSpec, installed []*goolib.PkgSpec, suffix string) error {
	selfOnly := selfExclusion(ps)
	for _, r := range reqs {
		hit, _, err := checkForwardConflict(r, installed, selfOnly)
		if err != nil {
			return err
		}
		if hit {
			return fmt.Errorf("cannot install, conflict with installed package or provider: %s%s", r.PackageInfo(), suffix)
		}
	}
	return nil
}
