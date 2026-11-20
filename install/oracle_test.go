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
	"context"

	"github.com/google/googet/v2/client"
	"github.com/google/googet/v2/googetdb"
	"github.com/google/googet/v2/goolib"
	"github.com/google/googet/v2/internal/relation"
	"github.com/google/googet/v2/remove"
	"github.com/google/logger"
)

// This file holds database-backed forms of install decisions that production
// code now makes over a snapshot of the installed packages, for the tests
// that call them. isSatisfied and minInstalled read the database directly,
// so they also serve as independent references for satisfiedIn.

// repoExclusions returns the initial exclusion set for installing ps from a
// repository, covering ps itself and the single row each Replaces entry picks.
func repoExclusions(ps *goolib.PkgSpec, db *googetdb.GooDB) (exclusions, error) {
	state, err := db.FetchPkgs("")
	if err != nil {
		return exclusions{}, err
	}
	return repoExclusionsIn(ps, state)
}

// installedSpecs returns the non-nil package specs currently recorded in db.
func installedSpecs(db *googetdb.GooDB) ([]*goolib.PkgSpec, error) {
	state, err := db.FetchPkgs("")
	if err != nil {
		return nil, err
	}
	return specsFromState(state), nil
}

// resolveConflicts fails if any of ps.Conflicts matches an installed row not
// excluded by ex. It returns the entries that passed only because a Replaces
// victim was exempt; callers must recheck them after resolveReplacements runs.
func resolveConflicts(ps *goolib.PkgSpec, ex exclusions, db *googetdb.GooDB) ([]relation.Requirement, error) {
	if len(ps.Conflicts) == 0 {
		return nil, nil
	}
	state, err := db.FetchPkgs("")
	if err != nil {
		return nil, err
	}
	return resolveConflictsIn(ps, ex, state, liveSink{})
}

// resolveReverseConflicts checks installed packages' Conflicts against ps
// under ex and mode, and returns the name.arch keys of any excluded victim
// rows that matched ps so they can be re-verified after replacements run.
func resolveReverseConflicts(ps *goolib.PkgSpec, ex exclusions, mode ConflictMode, db *googetdb.GooDB) (map[string]bool, error) {
	installed, err := installedSpecs(db)
	if err != nil {
		return nil, err
	}
	return resolveReverseConflictsIn(ps, ex, mode, installed, liveSink{})
}

// checkConflicts evaluates both ps's forward Conflicts and installed packages'
// reverse Conflicts under ex and mode, returning any checks deferred onto
// Replaces removal.
func checkConflicts(ps *goolib.PkgSpec, ex exclusions, mode ConflictMode, db *googetdb.GooDB) (deferredChecks, error) {
	state, err := db.FetchPkgs("")
	if err != nil {
		return deferredChecks{}, err
	}
	return checkConflictsIn(ps, ex, mode, state, liveSink{})
}

// recheckBeforeReplacements simulates resolveReplacements on the
// post-dependency database snapshot and re-evaluates deferred forward entries
// and (in ConflictError mode) deferred reverse victim rows against the
// simulated post-replacement state with self-only exclusions, failing before
// resolveReplacements removes any package whenever the post-replacement recheck
// would fail. If simulation does not complete cleanly, it logs at info level
// and defers to resolveReplacements and recheck.
func (d deferredChecks) recheckBeforeReplacements(ps *goolib.PkgSpec, mode ConflictMode, db *googetdb.GooDB) error {
	if !d.needsPrecheck(mode) {
		return nil
	}
	state, err := db.FetchPkgs("")
	if err != nil {
		return err
	}
	return d.recheckBeforeReplacementsIn(ps, mode, state, liveSink{})
}

// needsPrecheck reports whether recheckBeforeReplacements has anything to
// check under mode.
func (d deferredChecks) needsPrecheck(mode ConflictMode) bool {
	return len(d.fwd) > 0 || (mode == ConflictError && len(d.rev) > 0)
}

// recheck re-evaluates deferred forward and reverse conflict checks with
// self-only exclusions after resolveReplacements completes, ensuring no
// exempted victim survived.
func (d deferredChecks) recheck(ps *goolib.PkgSpec, mode ConflictMode, db *googetdb.GooDB) error {
	if len(d.fwd) == 0 && len(d.rev) == 0 {
		return nil
	}
	state, err := db.FetchPkgs("")
	if err != nil {
		return err
	}
	return d.recheckIn(ps, mode, state, liveSink{})
}

// minInstalled reports whether the package r names is installed at r's
// minimum version or later. It checks the single installed row that
// db.FetchPkg picks for r.
func minInstalled(r relation.Requirement, db *googetdb.GooDB) (bool, error) {
	_, ok, err := minInstalledRow(r, db)
	return ok, err
}

// isSatisfied reports whether the package dependency r is satisfied by any
// installed database row matching r's name, arch, and minimum version, or by
// any installed package that provides r. It queries SQLite directly without
// calling relation.Resolve so it serves as an independent reference for
// satisfiedIn.
func isSatisfied(r relation.Requirement, db *googetdb.GooDB) (bool, error) {
	pkgs, err := db.FetchPkgs("")
	if err != nil {
		return false, err
	}
	matched := false
	for _, p := range pkgs {
		if p.PackageSpec == nil {
			continue
		}
		ps := p.PackageSpec
		if ps.Name == r.Name && (r.Arch == "" || ps.Arch == r.Arch) {
			c, err := goolib.Compare(r.Ver, ps.Version)
			if err != nil {
				return false, err
			}
			if c < 1 {
				matched = true
			}
		}
	}
	if matched {
		return true, nil
	}
	for _, p := range pkgs {
		if p.PackageSpec == nil {
			continue
		}
		for _, prov := range p.PackageSpec.Provides {
			if client.SatisfiesProvider(prov, r.Name, r.Ver) {
				return true, nil
			}
		}
	}
	return false, nil
}

// minInstalledRow returns the installed row db.FetchPkg picks for r if it
// satisfies r's minimum version. Keep its selection and version gate in sync
// with simulateReplacements (covered by
// TestSimulateReplacementsMatchesResolveReplacements).
func minInstalledRow(r relation.Requirement, db *googetdb.GooDB) (client.PackageState, bool, error) {
	p, err := db.FetchPkg(r.PackageInfo())
	if err != nil {
		return client.PackageState{}, false, err
	}
	if p.PackageSpec == nil {
		return client.PackageState{}, false, nil
	}
	ok, err := r.MinInstalled(p.PackageSpec.Name, p.PackageSpec.Arch, p.PackageSpec.Version)
	if err != nil {
		return client.PackageState{}, false, err
	}
	if !ok {
		return client.PackageState{}, false, nil
	}
	return p, true, nil
}

// resolveReplacements removes the concrete installed rows that ps replaces,
// along with their cascaded name-based orphans ordered via remove.OrderSet,
// executing against db via remove.UninstallRow.
func (ri *repoInstall) resolveReplacements(ctx context.Context, ps *goolib.PkgSpec) error {
	db := ri.db
	state, err := db.FetchPkgs("")
	if err != nil {
		return err
	}
	out, err := planReplacement(ps, state)
	if err != nil {
		return err
	}
	for _, row := range out.removed {
		logger.Infof("%s replaces %s, removing", ps, row.PackageSpec)
		if err := remove.UninstallRow(ctx, row, remove.Options{DBOnly: ri.opts.DBOnly, Downloader: ri.downloader}, db); err != nil {
			return err
		}
	}
	return nil
}
