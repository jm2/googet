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
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/google/googet/v2/client"
	"github.com/google/googet/v2/googetdb"
	"github.com/google/googet/v2/internal/ownership"
	"github.com/google/googet/v2/oswrap"
	"github.com/google/logger"
)

// stateReader provides a snapshot of installed package rows for a recorder.
// *googetdb.GooDB satisfies stateReader.
type stateReader interface {
	FetchPkgs(string) (client.GooGetState, error)
}

// recordPolicy configures how a recorder reconciles the previous installed
// row when it records row and when it finishes. It is the single place
// commit-path behaviour differs across install callers.
type recordPolicy struct {
	// cleanup reconciles dropped files from the previous exact name.arch row.
	cleanup bool
	// removeArchive removes the previous exact name.arch row's cached archive
	// and unpack directory.
	removeArchive bool
	// carryDirs carries forward previous empty-checksum directory entries that
	// exist as directories on disk: from captured when non-nil, otherwise from
	// the previous exact name.arch row, leaving out the directories that
	// cleanup is expected to delete.
	carryDirs bool
	// transfers strips overwritten files with differing checksums from other
	// installed rows in the same database transaction.
	transfers bool
}

func applyStepPolicy(dbOnly bool) recordPolicy {
	return recordPolicy{
		cleanup:       !dbOnly,
		removeArchive: true,
		carryDirs:     !dbOnly,
		transfers:     !dbOnly,
	}
}

func fromDiskPolicy(dbOnly bool) recordPolicy {
	return recordPolicy{
		carryDirs: !dbOnly,
		transfers: !dbOnly,
	}
}

// recoverPolicy returns the commit policy for replacement recovery: recovery
// restores the captured pre-replacement row (carrying forward its existing
// recorded directories on non-db_only runs) and never transfers ownership away
// from surviving packages.
func recoverPolicy(dbOnly bool) recordPolicy {
	return recordPolicy{
		carryDirs: !dbOnly,
	}
}

func reinstallPolicy() recordPolicy {
	return recordPolicy{
		cleanup:   true,
		carryDirs: true,
		transfers: true,
	}
}

func (p recordPolicy) needsSnapshot(captured *client.PackageState) bool {
	return p.cleanup || p.removeArchive || p.transfers || (p.carryDirs && captured == nil)
}

func findExactRow(state client.GooGetState, name, arch string) client.PackageState {
	for _, st := range state {
		if st.PackageSpec != nil && st.PackageSpec.Name == name && st.PackageSpec.Arch == arch {
			return st
		}
	}
	return client.PackageState{}
}

func removeOldArchives(old client.PackageState) {
	if old.LocalPath != "" && oswrap.RemoveAll(old.LocalPath) != nil {
		logger.Error("Unable to remove old local path")
	}
	if old.UnpackDir != "" && oswrap.RemoveAll(old.UnpackDir) != nil {
		logger.Error("Unable to remove old unpack dir")
	}
}

// planCleanup computes the ownership-aware deletion plan for entries in old
// that insFiles no longer lists under ix's rule.
func planCleanup(old, insFiles map[string]string, ix *ownership.Index) ownership.DeletePlan {
	return ownership.PlanDelete(ownership.Dropped(old, insFiles, ix), ix)
}

// cleanOldFiles deletes dropped entries from oldState that no other installed
// row in ix still owns, using rm and logging through lg.
func cleanOldFiles(oldState client.PackageState, insFiles map[string]string, ix *ownership.Index, rm func(string) (string, error), lg ownership.Logger) {
	executeCleanup(oldState, planCleanup(oldState.InstalledFiles, insFiles, ix), rm, lg)
}

// executeCleanup runs plan, the cleanup of files the previous version old
// installed, using rm and logging through lg. It does nothing if old
// recorded no files.
func executeCleanup(old client.PackageState, plan ownership.DeletePlan, rm func(string) (string, error), lg ownership.Logger) {
	if len(old.InstalledFiles) == 0 {
		return
	}
	var pkg string
	if old.PackageSpec != nil {
		pkg = old.PackageSpec.String()
	}
	ownership.Execute(plan, "upgrade", pkg, rm, lg)
}

// carryDirs merges empty-checksum entries from candidates that still exist as
// directories on disk into insFiles unless insFiles already lists an
// equivalent key under ix.
//
// Carry-forward preserves created parent directories (as well as dropped
// payload directories kept non-empty by user or other packages' files) across
// upgrades, FromDisk installs, replacement recovery, and reinstalls so
// uninstalling the package later removes them once empty. If package A creates
// a parent directory and package B later installs files under that already
// existing parent, only A's row records the parent; removing A first leaves
// the non-empty parent for B, and removing B later leaves that unrecorded
// parent on disk.
func carryDirs(candidates, insFiles map[string]string, ix *ownership.Index) map[string]string {
	return carryDirsExcept(candidates, insFiles, ix, nil)
}

// carryDirsExcept is carryDirs, except that it also leaves out every candidate
// for which deleted reports true.
func carryDirsExcept(candidates, insFiles map[string]string, ix *ownership.Index, deleted func(string) bool) map[string]string {
	if len(candidates) == 0 {
		return insFiles
	}
	if insFiles == nil {
		insFiles = make(map[string]string)
	}
	present := ix.KeySet(insFiles)
	paths := make([]string, 0, len(candidates))
	for p, sum := range candidates {
		if sum == "" {
			paths = append(paths, p)
		}
	}
	slices.Sort(paths)
	for _, p := range paths {
		if present.Has(p) || !isDir(p) || (deleted != nil && deleted(p)) {
			continue
		}
		present.Add(p)
		insFiles[p] = ""
	}
	return insFiles
}

// plannedDirDeleted returns a predicate reporting whether running plan is
// expected to delete the directory p: p is one of plan's directories, and
// every entry listChildren finds in it is one of plan's files or a directory
// below it that plan is itself expected to delete. A directory that keeps
// anything else, such as files the new version installed, files another
// package owns, or files no package records, stays in place, because cleanup
// only removes empty directories. A directory listChildren can't list is
// conservatively expected to stay, so the caller keeps recording it even
// though cleanup may still remove it; a recorded directory that is gone is
// harmless, because carry-forward and uninstall both skip missing paths.
func plannedDirDeleted(plan ownership.DeletePlan, ix *ownership.Index, listChildren func(string) ([]string, error)) func(string) bool {
	set := func(paths []string) ownership.PathSet {
		m := make(map[string]string, len(paths))
		for _, p := range paths {
			m[p] = ""
		}
		return ix.KeySet(m)
	}
	files, dirs := set(plan.Files), set(plan.Dirs)
	memo := make(map[string]bool)
	var deleted func(string) bool
	deleted = func(p string) bool {
		if !dirs.Has(p) {
			return false
		}
		if v, ok := memo[p]; ok {
			return v
		}
		children, err := listChildren(p)
		if err != nil {
			memo[p] = false
			return false
		}
		empty := true
		for _, c := range children {
			if !files.Has(c) && !deleted(c) {
				empty = false
				break
			}
		}
		memo[p] = empty
		return empty
	}
	return deleted
}

// listDir returns the paths of the entries in the directory dir.
func listDir(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, filepath.Join(dir, e.Name()))
	}
	return out, nil
}

// transfersFor returns, for each other row in ix, the file entries that row's
// install overwrote with different content. Identical-checksum files remain
// co-owned by both rows so removing either package keeps the shared file on
// disk, and empty-checksum entries never transfer.
func transfersFor(row client.PackageState, ix *ownership.Index) []googetdb.FileTransfer {
	if ix == nil || len(row.InstalledFiles) == 0 {
		return nil
	}
	var selfName, selfArch string
	if row.PackageSpec != nil {
		selfName, selfArch = row.PackageSpec.Name, row.PackageSpec.Arch
	}
	byRow := make(map[ownership.RowID]map[string]struct{})
	for path, newSum := range row.InstalledFiles {
		if newSum == "" {
			continue
		}
		for _, o := range ix.Owners(path) {
			if o.Row.Name == selfName && o.Row.Arch == selfArch {
				continue
			}
			if o.Checksum == "" || o.Checksum == newSum {
				continue
			}
			set := byRow[o.Row]
			if set == nil {
				set = make(map[string]struct{})
				byRow[o.Row] = set
			}
			set[o.Path] = struct{}{}
		}
	}
	if len(byRow) == 0 {
		return nil
	}
	ids := make([]ownership.RowID, 0, len(byRow))
	for id := range byRow {
		ids = append(ids, id)
	}
	slices.SortFunc(ids, func(a, b ownership.RowID) int {
		return cmp.Or(strings.Compare(a.Name, b.Name), strings.Compare(a.Arch, b.Arch))
	})
	out := make([]googetdb.FileTransfer, 0, len(ids))
	for _, id := range ids {
		files := make([]string, 0, len(byRow[id]))
		for f := range byRow[id] {
			files = append(files, f)
		}
		slices.Sort(files)
		out = append(out, googetdb.FileTransfer{
			Name:  id.Name,
			Arch:  id.Arch,
			Files: files,
		})
	}
	return out
}

// recorder records one install according to its policy. Its record method
// is installPkg's record hook: it reads the installed rows at most once and
// writes the installed package's row and any file ownership transfers in one
// database transaction, before installPkg commits the install's file changes.
// Its finish method then cleans up the version the install overwrote, which
// must happen only once the new version is recorded and its file changes are
// committed.
type recorder struct {
	r        stateReader
	row      client.PackageState
	captured *client.PackageState
	policy   recordPolicy
	write    func(client.PackageState, []googetdb.FileTransfer) error
	// old is the installed row with row's name and arch before the write,
	// and plan is the cleanup of the files it installed that row doesn't;
	// both are computed only when the policy reconciles the previous row.
	old  client.PackageState
	plan ownership.DeletePlan
	// written reports that write succeeded.
	written bool
}

// newRecorder returns a recorder that records row, with the files the install
// placed, according to p, writing it with write. captured, if not nil, is
// the row as it was before a replacement removed it, whose recorded
// directories recovery carries forward.
func newRecorder(r stateReader, row client.PackageState, captured *client.PackageState, p recordPolicy, write func(client.PackageState, []googetdb.FileTransfer) error) *recorder {
	return &recorder{r: r, row: row, captured: captured, policy: p, write: write}
}

// record writes the recorder's row with insFiles, plus any recorded
// directories the policy carries forward, as its installed files, together
// with the ownership transfers the policy asks for, and logs a
// file_owner_transfer line for each transferred file once the write
// succeeds. It doesn't change insFiles.
//
// Cleanup runs only in finish, after the write, but carry-forward needs to
// know which of the previous version's directories survive it: a dropped
// directory is carried forward only if cleanup is expected to leave it in
// place, because something cleanup doesn't delete is still in it.
func (rc *recorder) record(insFiles map[string]string) error {
	rc.row.InstalledFiles = maps.Clone(insFiles)
	var transfers []googetdb.FileTransfer
	if rc.policy.needsSnapshot(rc.captured) {
		state, err := rc.r.FetchPkgs("")
		if err != nil {
			return err
		}
		var name, arch string
		if rc.row.PackageSpec != nil {
			name, arch = rc.row.PackageSpec.Name, rc.row.PackageSpec.Arch
		}
		rc.old = findExactRow(state, name, arch)
		ix := ownership.NewIndex(state, ownership.RowID{Name: name, Arch: arch})
		var deleted func(string) bool
		if rc.policy.cleanup {
			rc.plan = planCleanup(rc.old.InstalledFiles, rc.row.InstalledFiles, ix)
			deleted = plannedDirDeleted(rc.plan, ix, listDir)
		}
		if rc.policy.carryDirs && rc.captured == nil {
			rc.row.InstalledFiles = carryDirsExcept(rc.old.InstalledFiles, rc.row.InstalledFiles, ix, deleted)
		}
		if rc.policy.transfers {
			transfers = transfersFor(rc.row, ix)
		}
	}
	if rc.policy.carryDirs && rc.captured != nil {
		rc.row.InstalledFiles = carryDirs(rc.captured.InstalledFiles, rc.row.InstalledFiles, nil)
	}
	if err := rc.write(rc.row, transfers); err != nil {
		return err
	}
	rc.written = true
	if len(transfers) > 0 {
		var toPkg string
		if rc.row.PackageSpec != nil {
			toPkg = rc.row.PackageSpec.String()
		}
		for _, tr := range transfers {
			for _, f := range tr.Files {
				logger.Infof("file_owner_transfer from=%s.%s to=%s path=%q", tr.Name, tr.Arch, toPkg, f)
			}
		}
	}
	return nil
}

// finish cleans up the version the recorded install overwrote, as the policy
// asks. It does nothing unless record wrote the row. Failures are logged.
func (rc *recorder) finish() {
	if !rc.written {
		return
	}
	if rc.policy.cleanup {
		executeCleanup(rc.old, rc.plan, client.RemoveOrRename, ownership.Logger{
			Infof:  logger.Infof,
			Errorf: logger.Errorf,
		})
	}
	if rc.policy.removeArchive {
		removeOldArchives(rc.old)
	}
}

// recordInstall records row according to p, writing it with write, and then
// cleans up the version it overwrote. It is for callers whose file changes
// are already committed, or that change no files.
func recordInstall(r stateReader, row client.PackageState, captured *client.PackageState, p recordPolicy, write func(client.PackageState, []googetdb.FileTransfer) error) error {
	rc := newRecorder(r, row, captured, p, write)
	if err := rc.record(row.InstalledFiles); err != nil {
		return err
	}
	rc.finish()
	return nil
}
