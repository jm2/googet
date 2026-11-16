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
	"github.com/google/googet/v2/client"
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
}

func applyStepPolicy(dbOnly bool) recordPolicy {
	return recordPolicy{
		cleanup:       !dbOnly,
		removeArchive: true,
	}
}

func fromDiskPolicy(_ bool) recordPolicy {
	return recordPolicy{}
}

func recoverPolicy() recordPolicy {
	return recordPolicy{}
}

func (p recordPolicy) needsSnapshot() bool {
	return p.cleanup || p.removeArchive
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

// recorder records one install according to its policy. Its record method
// is installPkg's record hook: it reads the installed rows at most once and
// writes the installed package's row, before installPkg commits the
// install's file changes. Its finish method then cleans up the version the
// install overwrote, which must happen only once the new version is recorded
// and its file changes are committed.
type recorder struct {
	r      stateReader
	row    client.PackageState
	policy recordPolicy
	write  func(client.PackageState) error
	// old is the installed row with row's name and arch before the write,
	// and ix indexes the other installed rows' files; both are read only
	// when the policy reconciles the previous row.
	old client.PackageState
	ix  *ownership.Index
	// written reports that write succeeded.
	written bool
}

// newRecorder returns a recorder that records row, with the files the install
// placed, according to p, writing it with write.
func newRecorder(r stateReader, row client.PackageState, p recordPolicy, write func(client.PackageState) error) *recorder {
	return &recorder{r: r, row: row, policy: p, write: write}
}

// record writes the recorder's row with insFiles as its installed files.
func (rc *recorder) record(insFiles map[string]string) error {
	rc.row.InstalledFiles = insFiles
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
	}
	if err := rc.write(rc.row); err != nil {
		return err
	}
	rc.written = true
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
func recordInstall(r stateReader, row client.PackageState, p recordPolicy, write func(client.PackageState) error) error {
	rc := newRecorder(r, row, p, write)
	if err := rc.record(row.InstalledFiles); err != nil {
		return err
	}
	rc.finish()
	return nil
}
