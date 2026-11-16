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
	// transfers strips overwritten files with differing checksums from other
	// installed rows in the same database transaction.
	transfers bool
}

func applyStepPolicy(dbOnly bool) recordPolicy {
	return recordPolicy{
		cleanup:       !dbOnly,
		removeArchive: true,
		transfers:     !dbOnly,
	}
}

func fromDiskPolicy(dbOnly bool) recordPolicy {
	return recordPolicy{
		transfers: !dbOnly,
	}
}

// recoverPolicy returns the commit policy for replacement recovery: recovery
// restores the captured pre-replacement row and never transfers ownership away
// from surviving packages.
func recoverPolicy() recordPolicy {
	return recordPolicy{}
}

func (p recordPolicy) needsSnapshot() bool {
	return p.cleanup || p.removeArchive || p.transfers
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
	if len(oldState.InstalledFiles) == 0 {
		return
	}
	var pkg string
	if oldState.PackageSpec != nil {
		pkg = oldState.PackageSpec.String()
	}
	ownership.Execute(planCleanup(oldState.InstalledFiles, insFiles, ix), "upgrade", pkg, rm, lg)
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
	r      stateReader
	row    client.PackageState
	policy recordPolicy
	write  func(client.PackageState, []googetdb.FileTransfer) error
	// old is the installed row with row's name and arch before the write,
	// and ix indexes the other installed rows' files; both are read only
	// when the policy reconciles the previous row or transfers ownership.
	old client.PackageState
	ix  *ownership.Index
	// written reports that write succeeded.
	written bool
}

// newRecorder returns a recorder that records row, with the files the install
// placed, according to p, writing it with write.
func newRecorder(r stateReader, row client.PackageState, p recordPolicy, write func(client.PackageState, []googetdb.FileTransfer) error) *recorder {
	return &recorder{r: r, row: row, policy: p, write: write}
}

// record writes the recorder's row with insFiles as its installed files,
// together with the ownership transfers the policy asks for, and logs a
// file_owner_transfer line for each transferred file once the write succeeds.
func (rc *recorder) record(insFiles map[string]string) error {
	rc.row.InstalledFiles = insFiles
	var transfers []googetdb.FileTransfer
	if rc.policy.needsSnapshot() {
		state, err := rc.r.FetchPkgs("")
		if err != nil {
			return err
		}
		var name, arch string
		if rc.row.PackageSpec != nil {
			name, arch = rc.row.PackageSpec.Name, rc.row.PackageSpec.Arch
		}
		rc.old = findExactRow(state, name, arch)
		rc.ix = ownership.NewIndex(state, ownership.RowID{Name: name, Arch: arch})
		if rc.policy.transfers {
			transfers = transfersFor(rc.row, rc.ix)
		}
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
		cleanOldFiles(rc.old, rc.row.InstalledFiles, rc.ix, client.RemoveOrRename, ownership.Logger{
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
func recordInstall(r stateReader, row client.PackageState, p recordPolicy, write func(client.PackageState, []googetdb.FileTransfer) error) error {
	rc := newRecorder(r, row, p, write)
	if err := rc.record(row.InstalledFiles); err != nil {
		return err
	}
	rc.finish()
	return nil
}
