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
	"strings"

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

// repoExclusionsIn returns the initial exclusion set for installing ps from a
// repository over the installed state st, covering ps itself and the single
// row each Replaces entry picks.
func repoExclusionsIn(ps *goolib.PkgSpec, st client.GooGetState) (exclusions, error) {
	ex := selfExclusion(ps)
	for _, pkg := range ps.Replaces {
		v, ok, err := minInstalledRowIn(relation.ParseRelation(pkg), st)
		if err != nil {
			return exclusions{}, err
		}
		if !ok {
			continue
		}
		if ex.victims == nil {
			ex.victims = make(map[string]bool)
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

// ConflictMode controls how reverse package conflicts are handled.
type ConflictMode int

// String returns the canonical lowercase configuration name for m.
func (m ConflictMode) String() string {
	switch m {
	case ConflictWarn:
		return "warn"
	case ConflictError:
		return "error"
	case ConflictIgnore:
		return "ignore"
	default:
		return fmt.Sprintf("ConflictMode(%d)", int(m))
	}
}

// ParseConflictMode parses a case-insensitive conflict mode name ("warn",
// "error", or "ignore") after trimming surrounding whitespace.
func ParseConflictMode(s string) (ConflictMode, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "warn":
		return ConflictWarn, nil
	case "error":
		return ConflictError, nil
	case "ignore":
		return ConflictIgnore, nil
	default:
		return ConflictWarn, fmt.Errorf("invalid package conflict mode %q: want warn, error, or ignore", s)
	}
}

// ErrPackageConflict marks an installation failure caused by an installed
// package's Conflicts entry under ConflictError mode.
var ErrPackageConflict = errors.New("package conflict")

// reverseHit is a single reverse-conflict result between an installed package
// and an incoming package.
type reverseHit struct {
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

// reverseConflicts evaluates all Conflicts entries of non-excluded specs in
// installed against incoming package ps, returning at most one hit per
// (installed package, entry) pair.
func reverseConflicts(ps *goolib.PkgSpec, installed []*goolib.PkgSpec, ex exclusions) []reverseHit {
	var hits []reverseHit
	for _, inst := range installed {
		if inst == nil || ex.has(pkgKey(inst)) {
			continue
		}
		for _, c := range inst.Conflicts {
			matched, unparseable := matchReverseEntry(c, ps)
			if matched {
				hits = append(hits, reverseHit{installed: inst, entry: c})
			} else if unparseable {
				hits = append(hits, reverseHit{installed: inst, entry: c, unparseable: true})
			}
		}
	}
	return hits
}

// conflictLine formats the structured telemetry log line for a reverse package
// conflict event.
func conflictLine(mode ConflictMode, result string, installed *goolib.PkgSpec, entry string, incoming *goolib.PkgSpec) string {
	return fmt.Sprintf("package_conflict direction=reverse mode=%s result=%s installed=%s entry=%q incoming=%s",
		mode, result, installed, entry, incoming)
}

// applyReverseConflicts logs each hit in hits to sink according to mode,
// prints user warnings in warn mode, and returns an error on real conflicts in
// error mode.
func applyReverseConflicts(ps *goolib.PkgSpec, hits []reverseHit, mode ConflictMode, sink eventSink) error {
	var firstErr error
	for _, hit := range hits {
		if hit.unparseable {
			sink.Warning(conflictLine(mode, "unparseable", hit.installed, hit.entry, ps))
			continue
		}
		switch mode {
		case ConflictError:
			sink.Error(conflictLine(mode, "failed", hit.installed, hit.entry, ps))
			if firstErr == nil {
				firstErr = fmt.Errorf("%w: cannot install %s: conflict with installed package %s (entry %q; package conflict mode is %s)", ErrPackageConflict, ps, hit.installed, hit.entry, mode)
			}
		case ConflictIgnore:
			sink.Infof("%s", conflictLine(mode, "ignored", hit.installed, hit.entry, ps))
		default:
			sink.Warning(conflictLine(ConflictWarn, "warned", hit.installed, hit.entry, ps))
			sink.Printf("Warning: installed package %s conflicts with %s (entry %q); installing anyway (package_conflicts=warn)\n", hit.installed, ps, hit.entry)
		}
	}
	return firstErr
}

// resolveReverseConflictsIn checks the installed specs' Conflicts against ps
// under ex and mode, sending its log lines and warnings to sink, and returns
// the name.arch keys of any excluded victim rows that matched ps so they can
// be re-verified once the replacements are removed.
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
	// rev maps name.arch keys of exempted victims whose Conflicts matched ps.
	rev map[string]bool
}

// checkConflictsIn evaluates both ps's forward Conflicts and the installed
// packages' reverse Conflicts over the installed state st under ex and mode,
// sending log lines and warnings to sink, and returns any checks deferred
// until the replacements are removed.
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
