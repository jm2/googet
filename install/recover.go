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

// Replacing a package is best-effort, not a transaction. Before a plan step
// removes the packages its package replaces, it records each one: its
// database row, read as the step starts removing them, and its cached archive,
// which the uninstall keeps. If the step fails before its package is
// installed, the recorded packages are installed again from those archives,
// newest removal first, and their rows are written back. Uninstall and install
// scripts, and installer side effects such as MSI changes, aren't undone; the
// recorded packages' install scripts simply run again.
//
// The record lives only in memory. If GooGet is killed or interrupted while a
// step runs, the removed packages stay removed. The replace_record line,
// printed and logged before the first removal, names them and their kept
// archives, from which they can be installed again; `googet clean` deletes
// those archives because they belong to no installed package.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/google/googet/v2/client"
	"github.com/google/googet/v2/goolib"
	"github.com/google/googet/v2/oswrap"
	"github.com/google/googet/v2/remove"
	"github.com/google/logger"
)

// replaceRecord holds what one plan step's replacement removed, until the
// step's package is recorded.
type replaceRecord struct {
	// by is the package that replaces the removed ones.
	by *goolib.PkgSpec
	// removed holds the packages uninstalled so far, in removal order.
	removed []recordEntry
}

// recordEntry is one removed package.
type recordEntry struct {
	// row is the package's database row as it was when the step started
	// removing packages.
	row client.PackageState
	// archive is the path of the package's kept archive, as
	// remove.LocalArchive resolved it, or "" if only the database changes.
	archive string
}

// removeReplaced uninstalls the installed packages step s replaces, in the
// plan's order, keeping their archives, and returns a record of each one
// uninstalled. Before the first removal, it prints and logs a replace_record
// line naming the packages and their archives. On failure, the record holds
// the packages uninstalled before it, and the package that failed to
// uninstall stays installed.
func (ri *repoInstall) removeReplaced(ctx context.Context, s planStep) (*replaceRecord, error) {
	rec := &replaceRecord{by: s.spec.PackageSpec}
	// Read each row as it is now, rather than as the plan saw it: a package
	// that an earlier step installed has its archive recorded only once that
	// step is done.
	var todo []recordEntry
	var rows, archives []string
	for _, r := range s.removed {
		key := r.PackageSpec.String()
		row, err := ri.db.FetchPkg(goolib.PackageInfo{Name: r.PackageSpec.Name, Arch: r.PackageSpec.Arch, Ver: r.PackageSpec.Version})
		if err != nil {
			return rec, err
		}
		if row.PackageSpec == nil || row.PackageSpec.String() != key {
			return rec, ri.divergence(s, "removal-row", fmt.Sprintf("%s is no longer installed", key))
		}
		archive := ""
		if !ri.opts.DBOnly {
			if archive, err = remove.LocalArchive(ctx, row, ri.downloader); err != nil {
				return rec, err
			}
			archives = append(archives, archive)
		}
		todo = append(todo, recordEntry{row: row, archive: archive})
		rows = append(rows, key)
	}
	line := fmt.Sprintf("replace_record replacer=%s rows=%s archives=%s", s.spec.PackageSpec, strings.Join(rows, ","), strings.Join(archives, ","))
	logger.Info(line)
	fmt.Println(line)
	for _, e := range todo {
		key := e.row.PackageSpec.String()
		if err := ri.atStage(stageBeforeUninstall, key); err != nil {
			return rec, err
		}
		logger.Infof("%s replaces %s, removing", s.spec.PackageSpec, key)
		if err := remove.UninstallRow(ctx, e.row, remove.Options{DBOnly: ri.opts.DBOnly, KeepCache: true, Downloader: ri.downloader}, ri.db); err != nil {
			return rec, err
		}
		rec.removed = append(rec.removed, e)
		if err := ri.atStage(stageAfterUninstall, key); err != nil {
			return rec, err
		}
	}
	return rec, nil
}

// recoverRemoved returns cause after installing again, newest removal first,
// every package rec says was removed. It runs even if ctx is cancelled. It
// reinstalls each package from its kept archive, downloading the archive
// again only if it is gone, and allows the package's files to overwrite other
// packages' files, as they did before the step began. A package that can't
// be reinstalled keeps its archive, which the returned error names. With no
// record, or nothing removed, it returns cause unchanged.
func (ri *repoInstall) recoverRemoved(ctx context.Context, rec *replaceRecord, cause error) error {
	if rec == nil || len(rec.removed) == 0 {
		return cause
	}
	ctx = context.WithoutCancel(ctx)
	opts := ri.opts
	opts.Force = true
	var restored, kept []string
	var errs []error
	for i := len(rec.removed) - 1; i >= 0; i-- {
		e := rec.removed[i]
		if err := ri.reinstall(ctx, e, opts); err != nil {
			logger.Errorf("Recovery: reinstalling %s failed: %v", e.row.PackageSpec, err)
			if e.archive != "" {
				errs = append(errs, fmt.Errorf("couldn't reinstall %s, which installing %s removed (archive %q): %w", e.row.PackageSpec, rec.by, e.archive, err))
				kept = append(kept, fmt.Sprintf("%s (archive %s)", e.row.PackageSpec, e.archive))
			} else {
				errs = append(errs, fmt.Errorf("couldn't reinstall %s, which installing %s removed: %w", e.row.PackageSpec, rec.by, err))
				kept = append(kept, e.row.PackageSpec.String())
			}
			continue
		}
		restored = append(restored, e.row.PackageSpec.String())
	}
	logger.Infof("replace_recovery replacer=%s reinstalled=%s failed=%d", rec.by, strings.Join(restored, ","), len(errs))
	if len(kept) > 0 {
		// The packages that couldn't be reinstalled stay removed; their
		// archives stay cached so that they can be installed again by hand.
		line := fmt.Sprintf("replace_record replacer=%s result=not_restored removed=%s", rec.by, strings.Join(kept, ","))
		logger.Error(line)
		fmt.Println(line)
	}
	err := cause
	if len(restored) > 0 {
		err = fmt.Errorf("%w; reinstalled the packages that installing %s had removed: %s", cause, rec.by, strings.Join(restored, ", "))
	}
	return errors.Join(append([]error{err}, errs...)...)
}

// reinstall installs the removed package e again and writes back its row,
// with the files the reinstall recorded.
func (ri *repoInstall) reinstall(ctx context.Context, e recordEntry, opts Options) error {
	row := e.row
	if !opts.DBOnly {
		if _, err := os.Stat(e.archive); err != nil {
			if _, err := remove.LocalArchive(ctx, row, ri.downloader); err != nil {
				return err
			}
		}
		rc := newRecorder(ri.db, row, recoverPolicy(), ri.db.AddPkgWithTransfers)
		if _, err := installPkg(e.archive, row.PackageSpec, opts, ri.db, rc.record); err != nil {
			return err
		}
		rc.finish()
		return nil
	}
	return recordInstall(ri.db, row, recoverPolicy(), ri.db.AddPkgWithTransfers)
}

// commit deletes the archives kept for the packages rec removed, now that the
// package that replaces them is recorded, except any archive in needed, which
// the current step installed from or a later step in the same plan still
// installs from. A nil record commits nothing.
func (rec *replaceRecord) commit(needed []fetched) {
	if rec == nil {
		return
	}
	for _, e := range rec.removed {
		if e.archive == "" || slices.ContainsFunc(needed, func(f fetched) bool { return f.path == e.archive }) {
			continue
		}
		if err := oswrap.RemoveAll(e.archive); err != nil {
			logger.Errorf("error removing package data from cache directory: %v", err)
		}
	}
}
