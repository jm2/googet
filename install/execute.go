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
	"errors"
	"fmt"
	"os"
	"slices"

	"github.com/google/googet/v2/client"
	"github.com/google/googet/v2/download"
	"github.com/google/googet/v2/googetdb"
	"github.com/google/googet/v2/goolib"
	"github.com/google/googet/v2/internal/relation"
	"github.com/google/googet/v2/oswrap"
	"github.com/google/googet/v2/remove"
	"github.com/google/logger"
)

// applyStage names a point in applying a plan step. Tests use it to inject
// failures at that point.
type applyStage int

const (
	// stageFetched follows fetching every archive the plan needs, before
	// anything changes.
	stageFetched applyStage = iota
	// stageBeforeReplace precedes removing the packages a step replaces.
	stageBeforeReplace
	// stageBeforeUninstall precedes uninstalling one replaced package; the
	// hook's step is that package.
	stageBeforeUninstall
	// stageAfterUninstall follows uninstalling one replaced package; the
	// hook's step is that package.
	stageAfterUninstall
	// stageAfterReplace follows removing them.
	stageAfterReplace
	// stageAfterInstall follows installing a step's package, while its file
	// changes can still be rolled back and before its database row is
	// written.
	stageAfterInstall
	// stageAddPkg precedes each attempt to write a step's database row. An
	// error from the hook fails that attempt in place of the database.
	stageAddPkg
	// stageCommitted follows writing a step's database row.
	stageCommitted
)

// errPlanDivergence marks an install that stopped because the installed
// packages no longer matched what its plan expected.
var errPlanDivergence = errors.New("installed packages changed while the install ran")

// fetched is where fetch put a plan step's archive.
type fetched struct {
	// path is the archive in the cache.
	path string
	// url is where the archive was downloaded from.
	url string
}

// fromRepo installs pi from a repository in three phases. It plans the
// install over a snapshot of the installed packages, which fails, changing
// nothing, if the install can't proceed. Then it fetches and verifies the
// archive of every package the plan installs and, unless only the database
// changes, makes sure the archive of every package it removes is cached, so
// that a missing or corrupt download also changes nothing. Only then does it
// apply the plan, one step at a time.
//
// Planning writes the log lines and console messages for every package it
// resolves before anything is downloaded or installed.
func (ri *repoInstall) fromRepo(ctx context.Context, pi goolib.PackageInfo, repo string) error {
	state, err := ri.db.FetchPkgs("")
	if err != nil {
		return err
	}
	p, err := buildPlan(planTarget{pi: pi, repo: repo}, ri.rm, state, ri.archs, planPolicy{conflicts: ri.opts.PackageConflicts, orphans: ri.opts.OrphanedDependents}, liveSink{})
	if err != nil {
		return err
	}
	archives, err := ri.fetch(ctx, p)
	if err != nil {
		return err
	}
	if err := ri.atStage(stageFetched, ""); err != nil {
		return err
	}
	for i, s := range p.steps {
		if err := ri.applyStep(ctx, s, archives[i:]); err != nil {
			return err
		}
	}
	return nil
}

// fetch downloads and verifies the archive of every package p installs, in
// install order, and returns where each one is. Unless only the database
// changes, it also makes sure the cached archive of every package p removes
// is present, since uninstalling a package needs its archive. It changes
// nothing outside the cache. Archives verified before a failure stay cached,
// and a later install reuses them.
func (ri *repoInstall) fetch(ctx context.Context, p *plan) ([]fetched, error) {
	var out []fetched
	for _, s := range p.steps {
		dst, url, err := download.FromRepo(ctx, s.spec, s.repo, ri.cache, ri.downloader)
		if err != nil {
			return nil, err
		}
		out = append(out, fetched{path: dst, url: url})
	}
	if ri.opts.DBOnly {
		return out, nil
	}
	for i, s := range p.steps {
		for _, r := range s.removed {
			if installedEarlier(p.steps[:i], r.row) {
				// Its archive is one fetched above.
				continue
			}
			if _, err := remove.LocalArchive(ctx, r.row, ri.downloader); err != nil {
				rmArg := r.row.PackageSpec.Name
				if r.row.PackageSpec.Arch != "" {
					rmArg = pkgKey(r.row.PackageSpec)
				}
				return nil, fmt.Errorf("cannot install %s: installing it removes %s, which has no usable cached archive to uninstall it with: %v; restore the archive at %s, or remove %s first with \"googet remove -db_only %s\", which doesn't run its uninstall script", s.spec.PackageSpec, r.row.PackageSpec, err, archivePath(r.row), pkgKey(r.row.PackageSpec), rmArg)
			}
		}
	}
	return out, nil
}

// installedEarlier reports whether one of steps installs the package that r
// describes, rather than r being installed before the plan runs.
func installedEarlier(steps []planStep, r client.PackageState) bool {
	return slices.ContainsFunc(steps, func(s planStep) bool { return s.spec.PackageSpec == r.PackageSpec })
}

// archivePath returns where the installed package ps's cached archive should
// be, as remove.LocalArchive looks for it.
func archivePath(ps client.PackageState) string {
	if ps.LocalPath == "" && ps.UnpackDir != "" {
		return ps.UnpackDir + ".goo"
	}
	return ps.LocalPath
}

// archiveIntact reports whether the file at path exists and has the SHA-256
// checksum sum.
func archiveIntact(path, sum string) bool {
	f, err := oswrap.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	return goolib.Checksum(f) == sum
}

// applyStep performs one plan step: it removes the packages the step
// replaces, checks that no conflict the plan deferred has appeared, installs
// the step's package from its fetched archive, cleans up the version it
// overwrote and records it in the database.
//
// Replacing is best-effort, not a transaction. Until the step's package is
// recorded, a failure, including a failure to record it, rolls back the
// files the package placed and reinstalls the packages the step removed,
// from the archives kept for that purpose. Install scripts and any installer
// side effects aren't undone.
func (ri *repoInstall) applyStep(ctx context.Context, s planStep, remaining []fetched) error {
	a := remaining[0]
	db := ri.db
	ps := s.spec.PackageSpec
	var rec *replaceRecord
	if len(s.removed) > 0 {
		if err := ri.checkRemovals(s); err != nil {
			return err
		}
		if err := ri.atStage(stageBeforeReplace, ps.String()); err != nil {
			return err
		}
		if len(s.listed) > 0 {
			writeListedOrphans(os.Stdout, ps, s.listed)
			for _, o := range s.listed {
				logger.Warningf("dependency_unsatisfied op=replace result=listed dependent=%s requirement=%q removed=%s replacer=%s",
					o.Dependent, o.Requirement, relation.JoinSpecs(o.Removed, ","), ps)
			}
		}
		var err error
		rec, err = ri.removeReplaced(ctx, s)
		if err != nil {
			return ri.recoverRemoved(ctx, rec, err)
		}
		if err := ri.atStage(stageAfterReplace, ps.String()); err != nil {
			return ri.recoverRemoved(ctx, rec, err)
		}
	}
	if err := ri.recheckConflicts(s); err != nil {
		return ri.recoverRemoved(ctx, rec, err)
	}

	// Download the archive again only if it is gone or no longer matches its
	// checksum, so that an intact archive isn't downloaded or logged twice.
	if !archiveIntact(a.path, s.spec.Checksum) {
		if _, _, err := download.FromRepo(ctx, s.spec, s.repo, ri.cache, ri.downloader); err != nil {
			return ri.recoverRemoved(ctx, rec, err)
		}
	}
	rc := newRecorder(db, client.PackageState{
		SourceRepo:  s.repo,
		DownloadURL: a.url,
		Checksum:    s.spec.Checksum,
		LocalPath:   a.path,
		PackageSpec: ps,
	}, nil, applyStepPolicy(ri.opts.DBOnly), func(row client.PackageState, transfers []googetdb.FileTransfer) error {
		if err := ri.addPkg(row, transfers); err != nil {
			if rec == nil {
				return err
			}
			// Failing now rolls back the package's files and reinstalls the
			// packages the step removed, so try once more to record it.
			logger.Errorf("Recording %s failed, trying again: %v", ps, err)
			return ri.addPkg(row, transfers)
		}
		return nil
	})
	if _, err := installPkg(a.path, ps, ri.opts, db, func(insFiles map[string]string) error {
		if err := ri.atStage(stageAfterInstall, ps.String()); err != nil {
			return err
		}
		return rc.record(insFiles)
	}); err != nil {
		// installPkg has already put back the files it changed.
		return ri.recoverRemoved(ctx, rec, err)
	}

	logger.Infof("Installation of %s.%s.%s completed", ps.Name, ps.Arch, ps.Version)
	fmt.Printf("Installation of %s.%s.%s and all dependencies completed\n", ps.Name, ps.Arch, ps.Version)
	// The version this install overwrote is cleaned up only once the new one
	// is recorded.
	rc.finish()
	rec.commit(remaining)
	return ri.atStage(stageCommitted, ps.String())
}

// addPkg records row and transfers in the database, unless the test hook fails
// the stageAddPkg stage in its place.
func (ri *repoInstall) addPkg(row client.PackageState, transfers []googetdb.FileTransfer) error {
	if err := ri.atStage(stageAddPkg, row.PackageSpec.String()); err != nil {
		return err
	}
	return addPkg(ri.db, row, transfers)
}

// checkRemovals fails, changing nothing, unless removing the packages step s
// replaces from the installed packages as they are now would remove exactly
// the packages the plan expects, whose archives fetch made sure of.
func (ri *repoInstall) checkRemovals(s planStep) error {
	state, err := ri.db.FetchPkgs("")
	if err != nil {
		return err
	}
	out, err := planReplacement(s.spec.PackageSpec, state)
	if err == nil && (ri.opts.OrphanedDependents != OrphanFail || len(out.orphans) == 0) && slices.EqualFunc(out.removed, s.removed, func(a, b removal) bool {
		return pkgKey(a.row.PackageSpec) == pkgKey(b.row.PackageSpec) && a.victim == b.victim
	}) {
		return nil
	}
	return ri.divergence(s, "removal-set", fmt.Sprintf("planned to remove %s, would now remove %s (%v)", rowKeys(s.removed), rowKeys(out.removed), err))
}

// recheckConflicts fails if a conflict check that the plan deferred until
// step s's replacements are removed fails against the installed packages as
// they are now. The plan already checked it against the state it expected,
// so a failure means the installed packages diverged from the plan.
func (ri *repoInstall) recheckConflicts(s planStep) error {
	if len(s.recheck.fwd) == 0 && len(s.recheck.rev) == 0 {
		return nil
	}
	state, err := ri.db.FetchPkgs("")
	if err != nil {
		return err
	}
	// The plan already logged what this check finds when it passes.
	if err := s.recheck.recheckIn(s.spec.PackageSpec, ri.opts.PackageConflicts, state, &recordingSink{}); err != nil {
		return ri.divergence(s, "conflict-recheck", err.Error())
	}
	return nil
}

// divergence logs and returns an error for a step whose installed packages no
// longer match the plan.
func (ri *repoInstall) divergence(s planStep, check, detail string) error {
	logger.Errorf("plan_divergence step=%s check=%s: %s", s.spec.PackageSpec, check, detail)
	return fmt.Errorf("%w: installing %s: %s", errPlanDivergence, s.spec.PackageSpec, detail)
}

// rowKeys returns name.arch for each removal row.
func rowKeys(rows []removal) []string {
	var out []string
	for _, r := range rows {
		out = append(out, pkgKey(r.row.PackageSpec))
	}
	return out
}

// atStage calls the test hook, if any, for stage.
func (ri *repoInstall) atStage(stage applyStage, step string) error {
	if ri.stageHook == nil {
		return nil
	}
	return ri.stageHook(stage, step)
}
