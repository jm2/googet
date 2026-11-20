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
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/googet/v2/client"
	"github.com/google/googet/v2/googetdb"
	"github.com/google/googet/v2/goolib"
	"github.com/google/googet/v2/internal/ownership"
	"github.com/google/googet/v2/testutil"
)

// TestPlannedDirDeleted checks which directories of a cleanup plan the
// predicate expects cleanup to delete, given what is in them.
func TestPlannedDirDeleted(t *testing.T) {
	root := filepath.Join(string(filepath.Separator), "root")
	a := filepath.Join(root, "a")
	b := filepath.Join(a, "b")
	f := filepath.Join(b, "f")
	g := filepath.Join(a, "g")
	user := filepath.Join(b, "user")
	added := filepath.Join(a, "added")
	shared := filepath.Join(a, "shared")
	for _, tc := range []struct {
		desc     string
		plan     ownership.DeletePlan
		children map[string][]string
		want     map[string]bool
	}{
		{
			desc:     "chain of planned directories holding only planned files",
			plan:     ownership.DeletePlan{Files: []string{f, g}, Dirs: []string{b, a}},
			children: map[string][]string{a: {b, g}, b: {f}},
			want:     map[string]bool{a: true, b: true},
		},
		{
			desc:     "untracked file in a deeper planned directory",
			plan:     ownership.DeletePlan{Files: []string{f, g}, Dirs: []string{b, a}},
			children: map[string][]string{a: {b, g}, b: {f, user}},
			want:     map[string]bool{a: false, b: false},
		},
		{
			desc:     "deeper directory that isn't planned for deletion",
			plan:     ownership.DeletePlan{Files: []string{g}, Dirs: []string{a}},
			children: map[string][]string{a: {g, b}},
			want:     map[string]bool{a: false, b: false},
		},
		{
			desc:     "file the new version installed",
			plan:     ownership.DeletePlan{Files: []string{g}, Dirs: []string{a}},
			children: map[string][]string{a: {g, added}},
			want:     map[string]bool{a: false},
		},
		{
			desc:     "file another package owns",
			plan:     ownership.DeletePlan{Files: []string{g}, Dirs: []string{a}, Kept: []ownership.Kept{{Path: shared, Owners: []ownership.RowID{{Name: "other", Arch: "noarch"}}}}},
			children: map[string][]string{a: {g, shared}},
			want:     map[string]bool{a: false},
		},
		{
			desc:     "empty planned directories",
			plan:     ownership.DeletePlan{Dirs: []string{b, a}},
			children: map[string][]string{a: {}},
			want:     map[string]bool{a: true, b: true},
		},
		{
			desc:     "directory that isn't in the plan",
			plan:     ownership.DeletePlan{Files: []string{f}, Dirs: []string{b}},
			children: map[string][]string{a: {b}, b: {f}},
			want:     map[string]bool{a: false, b: true},
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			calls := map[string]int{}
			list := func(dir string) ([]string, error) {
				calls[dir]++
				return tc.children[dir], nil
			}
			deleted := plannedDirDeleted(tc.plan, ownership.NewIndex(nil, ownership.RowID{}), list)
			got := map[string]bool{}
			for p := range tc.want {
				got[p] = deleted(p)
			}
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("deleted (-want +got):\n%s", diff)
			}
			for dir, n := range calls {
				if n > 1 {
					t.Errorf("listed %s %d times, want at most once", dir, n)
				}
			}
		})
	}

	t.Run("windows case and separator folding", func(t *testing.T) {
		plan := ownership.DeletePlan{Files: []string{`C:\ROOT\A\F.TXT`}, Dirs: []string{`C:\ROOT\A\B`, `C:\ROOT\A`}}
		children := map[string][]string{
			`c:\root\a`:   {`c:\root\a\b`, `c:\root\a\f.txt`},
			`c:\root\a\b`: {},
		}
		deleted := plannedDirDeleted(plan, ownership.NewIndexWithRule(nil, ownership.RowID{}, ownership.WindowsRule), func(dir string) ([]string, error) { return children[dir], nil })
		if !deleted(`c:\root\a`) {
			t.Errorf(`deleted(c:\root\a) = false, want true: its only entries are planned under another case`)
		}
		if deleted(`c:\root\x`) {
			t.Errorf(`deleted(c:\root\x) = true, want false: it isn't in the plan`)
		}
	})
}

// TestCarryDirsExcept checks that carryDirsExcept carries existing recorded
// directories except those the predicate reports deleted, and that a nil
// predicate carries as carryDirs does.
func TestCarryDirsExcept(t *testing.T) {
	root := t.TempDir()
	kept, gone, missing := filepath.Join(root, "kept"), filepath.Join(root, "gone"), filepath.Join(root, "missing")
	for _, d := range []string{kept, gone} {
		if err := os.Mkdir(d, 0755); err != nil {
			t.Fatal(err)
		}
	}
	file := filepath.Join(root, "file")
	candidates := map[string]string{kept: "", gone: "", missing: "", file: "sum"}
	ix := ownership.NewIndex(nil, ownership.RowID{})

	got := carryDirsExcept(candidates, map[string]string{file: "sum"}, ix, func(p string) bool { return p == gone })
	if diff := cmp.Diff(map[string]string{file: "sum", kept: ""}, got); diff != "" {
		t.Errorf("carryDirsExcept with a predicate (-want +got):\n%s", diff)
	}
	got = carryDirsExcept(candidates, map[string]string{file: "sum"}, ix, nil)
	if diff := cmp.Diff(map[string]string{file: "sum", kept: "", gone: ""}, got); diff != "" {
		t.Errorf("carryDirsExcept with no predicate (-want +got):\n%s", diff)
	}
}

// TestUpgradeRecordsOnlyDroppedDirectoriesCleanupKeeps checks, through a
// repository upgrade, that a directory the old version shipped and the new
// one doesn't is deleted and left out of the new row when nothing else is in
// it, and is kept and carried forward when an untracked file is in it.
func TestUpgradeRecordsOnlyDroppedDirectoriesCleanupKeeps(t *testing.T) {
	f := newRelFixture(t, nil, nil)
	gone, kept := filepath.Join(f.dst, "gone"), filepath.Join(f.dst, "kept")
	publishRepoSpec(f, buildRepoArchiveWithFiles(t, f.gooDir, goolib.PkgSpec{
		Name: "A", Arch: "noarch", Version: "1",
		Files: map[string]string{"gone": gone, "kept": kept},
	}, map[string]string{"gone/a.txt": "a", "kept/b.txt": "b"}))
	if err := f.install(t, "A"); err != nil {
		t.Fatalf("install A.noarch.1: %v", err)
	}
	untracked := filepath.Join(kept, "untracked")
	if err := os.WriteFile(untracked, []byte("user"), 0644); err != nil {
		t.Fatal(err)
	}
	publishRepoSpec(f, buildRepoArchiveWithFiles(t, f.gooDir, goolib.PkgSpec{
		Name: "A", Arch: "noarch", Version: "2",
		Files: map[string]string{"c.txt": filepath.Join(f.dst, "c.txt")},
	}, map[string]string{"c.txt": "c"}))
	if err := f.install(t, "A"); err != nil {
		t.Fatalf("install A.noarch.2: %v", err)
	}
	row := f.rows(t)["A"]
	if row.PackageSpec.Version != "2" {
		t.Fatalf("A's installed version = %q, want 2", row.PackageSpec.Version)
	}
	if sum, ok := row.InstalledFiles[gone]; ok {
		t.Errorf("A.noarch.2's row lists %s (%q), which cleanup deleted", gone, sum)
	}
	if fileThere(gone) {
		t.Errorf("%s is still there, want it deleted with the file in it", gone)
	}
	if sum, ok := row.InstalledFiles[kept]; !ok || sum != "" {
		t.Errorf("A.noarch.2's row lists %s as (%q, %t), want it carried forward as a directory", kept, sum, ok)
	}
	if !fileThere(untracked) || fileThere(filepath.Join(kept, "b.txt")) {
		t.Errorf("want %s kept and %s deleted", untracked, filepath.Join(kept, "b.txt"))
	}
	for p, sum := range row.InstalledFiles {
		if sum == "" && !isDir(p) {
			t.Errorf("A.noarch.2's row lists directory %s, which isn't on disk", p)
		}
	}
}

// recorderFixture is an installed package pkg.noarch.1 whose recorded
// directories and files exercise each way cleanup can leave a directory, an
// installed package other.noarch.1 that owns files in them, and the files
// pkg.noarch.2 installs in its place.
type recorderFixture struct {
	reader   *countingStateReader
	row      client.PackageState
	insFiles map[string]string
	archive  string
	// gone are the paths cleanup deletes, and kept the ones it leaves.
	gone, kept []string
	// wantFiles is the row's installed files once recorded.
	wantFiles map[string]string
}

func newRecorderFixture(t *testing.T) *recorderFixture {
	t.Helper()
	root := t.TempDir()
	p := func(rel string) string { return filepath.Join(root, filepath.FromSlash(rel)) }
	for _, d := range []string{"empty", "outer/inner", "user", "shared", "newdir"} {
		if err := os.MkdirAll(p(d), 0755); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{"empty/f", "outer/inner/f", "user/f", "user/untracked", "shared/f", "shared/other", "newdir/f", "newdir/added", "keep", "pkg.noarch.1.goo"} {
		if err := os.WriteFile(p(f), []byte(f), 0644); err != nil {
			t.Fatal(err)
		}
	}
	old := client.PackageState{
		LocalPath:   p("pkg.noarch.1.goo"),
		PackageSpec: &goolib.PkgSpec{Name: "pkg", Arch: "noarch", Version: "1"},
		InstalledFiles: map[string]string{
			p("empty"): "", p("empty/f"): "sum",
			p("outer"): "", p("outer/inner"): "", p("outer/inner/f"): "sum",
			p("user"): "", p("user/f"): "sum",
			p("shared"): "", p("shared/f"): "sum",
			p("newdir"): "", p("newdir/f"): "sum",
			p("keep"): "sum-old",
		},
	}
	other := client.PackageState{
		PackageSpec:    &goolib.PkgSpec{Name: "other", Arch: "noarch", Version: "1"},
		InstalledFiles: map[string]string{p("shared/other"): "sum-other", p("keep"): "sum-other"},
	}
	return &recorderFixture{
		reader:   &countingStateReader{state: client.GooGetState{old, other}},
		row:      client.PackageState{PackageSpec: &goolib.PkgSpec{Name: "pkg", Arch: "noarch", Version: "2"}},
		insFiles: map[string]string{p("keep"): "sum-new", p("newdir/added"): "sum-added"},
		archive:  p("pkg.noarch.1.goo"),
		gone:     []string{p("empty/f"), p("empty"), p("outer/inner/f"), p("outer/inner"), p("outer"), p("user/f"), p("shared/f"), p("newdir/f"), p("pkg.noarch.1.goo")},
		kept:     []string{p("user"), p("user/untracked"), p("shared"), p("shared/other"), p("newdir"), p("newdir/added"), p("keep")},
		wantFiles: map[string]string{
			p("keep"): "sum-new", p("newdir/added"): "sum-added",
			p("user"): "", p("shared"): "", p("newdir"): "",
		},
	}
}

func (f *recorderFixture) checkAllExist(t *testing.T, when string) {
	t.Helper()
	for _, p := range append(append([]string{}, f.gone...), f.kept...) {
		if _, err := os.Lstat(p); err != nil {
			t.Errorf("%s: %s: %v, want it still there", when, p, err)
		}
	}
}

// TestRecorderCarriesOnlyDirectoriesCleanupKeeps checks that recording an
// upgrade carries forward exactly the previous version's directories that
// the later cleanup leaves in place, that nothing is cleaned up until finish,
// and that the row never lists a directory that finish deletes.
func TestRecorderCarriesOnlyDirectoriesCleanupKeeps(t *testing.T) {
	f := newRecorderFixture(t)
	insFiles := maps.Clone(f.insFiles)
	logs := testutil.CaptureLogs(t)
	var written []client.PackageState
	var transfers [][]googetdb.FileTransfer
	rc := newRecorder(f.reader, f.row, nil, applyStepPolicy(false), func(row client.PackageState, tr []googetdb.FileTransfer) error {
		written = append(written, row)
		transfers = append(transfers, tr)
		return nil
	})
	if err := rc.record(insFiles); err != nil {
		t.Fatalf("record() = %v", err)
	}
	if f.reader.calls != 1 {
		t.Errorf("FetchPkgs called %d times, want 1", f.reader.calls)
	}
	if len(written) != 1 {
		t.Fatalf("write called %d times, want 1", len(written))
	}
	if diff := cmp.Diff(f.wantFiles, written[0].InstalledFiles); diff != "" {
		t.Errorf("recorded installed files (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff(f.insFiles, insFiles); diff != "" {
		t.Errorf("record() changed its argument (-before +after):\n%s", diff)
	}
	wantTransfers := []googetdb.FileTransfer{{Name: "other", Arch: "noarch", Files: []string{f.kept[len(f.kept)-1]}}}
	if diff := cmp.Diff(wantTransfers, transfers[0]); diff != "" {
		t.Errorf("transfers (-want +got):\n%s", diff)
	}
	if !strings.Contains(logs.String(), "file_owner_transfer from=other.noarch to=pkg.noarch.2") {
		t.Errorf("logs missing the file_owner_transfer line; got:\n%s", logs)
	}
	f.checkAllExist(t, "after record")

	rc.finish()
	for _, p := range f.gone {
		if _, err := os.Lstat(p); !os.IsNotExist(err) {
			t.Errorf("after finish: %s: Lstat error = %v, want it deleted", p, err)
		}
	}
	for _, p := range f.kept {
		if _, err := os.Lstat(p); err != nil {
			t.Errorf("after finish: %s: %v, want it kept", p, err)
		}
	}
	for p, sum := range written[0].InstalledFiles {
		if sum == "" && !isDir(p) {
			t.Errorf("the row lists directory %s, which finish deleted", p)
		}
	}
}

// TestRecorderFailedWriteChangesNothing checks that when the write fails,
// record returns its error without logging transfers, and finish then cleans
// up nothing.
func TestRecorderFailedWriteChangesNothing(t *testing.T) {
	f := newRecorderFixture(t)
	logs := testutil.CaptureLogs(t)
	full := errors.New("disk full")
	rc := newRecorder(f.reader, f.row, nil, applyStepPolicy(false), func(client.PackageState, []googetdb.FileTransfer) error {
		return full
	})
	if err := rc.record(maps.Clone(f.insFiles)); !errors.Is(err, full) {
		t.Fatalf("record() = %v, want %v", err, full)
	}
	rc.finish()
	f.checkAllExist(t, "after a failed write and finish")
	if strings.Contains(logs.String(), "file_owner_transfer") {
		t.Errorf("logged a transfer that wasn't written; got:\n%s", logs)
	}
}

// TestRecorderFinishBeforeRecord checks that finish does nothing when record
// never ran or failed before writing.
func TestRecorderFinishBeforeRecord(t *testing.T) {
	f := newRecorderFixture(t)
	writes := 0
	write := func(client.PackageState, []googetdb.FileTransfer) error {
		writes++
		return nil
	}
	newRecorder(f.reader, f.row, nil, applyStepPolicy(false), write).finish()
	f.checkAllExist(t, "after finish without record")

	f.reader.err = errors.New("corrupt row")
	rc := newRecorder(f.reader, f.row, nil, applyStepPolicy(false), write)
	if err := rc.record(maps.Clone(f.insFiles)); !errors.Is(err, f.reader.err) {
		t.Fatalf("record() = %v, want %v", err, f.reader.err)
	}
	rc.finish()
	f.checkAllExist(t, "after a failed read and finish")
	if writes != 0 {
		t.Errorf("write called %d times, want 0", writes)
	}
}

// TestPlannedDirDeletedListingErrors checks that a planned directory that
// can't be listed is expected to stay, along with every planned directory
// above it, and that the failed listing is remembered rather than retried.
func TestPlannedDirDeletedListingErrors(t *testing.T) {
	root := filepath.Join(string(filepath.Separator), "root")
	a := filepath.Join(root, "a")
	b := filepath.Join(a, "b")
	f := filepath.Join(b, "f")
	g := filepath.Join(a, "g")
	plan := ownership.DeletePlan{Files: []string{f, g}, Dirs: []string{b, a}}
	children := map[string][]string{a: {b, g}, b: {f}}
	for _, tc := range []struct {
		desc       string
		unreadable string
		want       map[string]bool
	}{
		{desc: "top planned directory", unreadable: a, want: map[string]bool{a: false, b: true}},
		{desc: "nested planned directory", unreadable: b, want: map[string]bool{a: false, b: false}},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			calls := map[string]int{}
			list := func(dir string) ([]string, error) {
				calls[dir]++
				if dir == tc.unreadable {
					return nil, fs.ErrPermission
				}
				return children[dir], nil
			}
			deleted := plannedDirDeleted(plan, ownership.NewIndex(nil, ownership.RowID{}), list)
			for round := 1; round <= 2; round++ {
				for _, p := range []string{a, b} {
					if got := deleted(p); got != tc.want[p] {
						t.Errorf("round %d: deleted(%s) = %t, want %t", round, p, got, tc.want[p])
					}
				}
			}
			if n := calls[tc.unreadable]; n != 1 {
				t.Errorf("listed the unreadable %s %d times, want once", tc.unreadable, n)
			}
		})
	}
}

// TestListDir checks that listDir returns the paths of a directory's entries
// and an error for a path it can't list as a directory.
func TestListDir(t *testing.T) {
	dir := t.TempDir()
	file, sub := filepath.Join(dir, "file"), filepath.Join(dir, "sub")
	if err := os.WriteFile(file, nil, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(sub, 0755); err != nil {
		t.Fatal(err)
	}
	got, err := listDir(dir)
	if err != nil {
		t.Fatalf("listDir(%s) = %v", dir, err)
	}
	if diff := cmp.Diff([]string{file, sub}, got); diff != "" {
		t.Errorf("listDir(%s) (-want +got):\n%s", dir, diff)
	}
	for _, p := range []string{filepath.Join(dir, "missing"), file} {
		if got, err := listDir(p); err == nil {
			t.Errorf("listDir(%s) = %q, nil; want an error", p, got)
		}
	}
}

// TestExecuteCleanup checks that executeCleanup runs the plan under the old
// row's package name, or under no name when the row has no spec, and that it
// runs nothing when the old row recorded no files.
func TestExecuteCleanup(t *testing.T) {
	plan := ownership.DeletePlan{
		Files: []string{"f"},
		Dirs:  []string{"d"},
		Kept:  []ownership.Kept{{Path: "k", Owners: []ownership.RowID{{Name: "other", Arch: "noarch"}}}},
	}
	files := map[string]string{"f": "sum", "d": ""}
	spec := &goolib.PkgSpec{Name: "pkg", Arch: "noarch", Version: "1"}
	for _, tc := range []struct {
		desc        string
		old         client.PackageState
		wantRemoved []string
		wantLog     string
	}{
		{desc: "old row recorded no files", old: client.PackageState{PackageSpec: spec}},
		{desc: "old row with a spec", old: client.PackageState{PackageSpec: spec, InstalledFiles: files}, wantRemoved: []string{"f", "d"}, wantLog: `file_kept op=upgrade pkg=pkg.noarch.1 path="k"`},
		{desc: "old row without a spec", old: client.PackageState{InstalledFiles: files}, wantRemoved: []string{"f", "d"}, wantLog: `file_kept op=upgrade pkg= path="k"`},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			var removed, logs []string
			rm := func(p string) (string, error) {
				removed = append(removed, p)
				return "", nil
			}
			logf := func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) }
			executeCleanup(tc.old, plan, rm, ownership.Logger{Infof: logf, Errorf: logf})
			if diff := cmp.Diff(tc.wantRemoved, removed); diff != "" {
				t.Errorf("removed (-want +got):\n%s", diff)
			}
			if tc.wantLog == "" {
				if len(logs) != 0 {
					t.Errorf("logged %q, want nothing", logs)
				}
				return
			}
			if !slices.ContainsFunc(logs, func(l string) bool { return strings.HasPrefix(l, tc.wantLog) }) {
				t.Errorf("logs %q have no line starting with %q", logs, tc.wantLog)
			}
		})
	}
}

// TestUpgradeKeepsRecordingUnlistableDroppedDirectory checks, through a
// repository upgrade, that a directory the old version recorded and the new
// one drops stays recorded when it can't be listed and still holds an
// untracked file, because cleanup can't delete it.
func TestUpgradeKeepsRecordingUnlistableDroppedDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chmod can't deny listing a directory on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root can list any directory")
	}
	f := newRelFixture(t, nil, nil)
	p := filepath.Join(f.dst, "p")
	publishRepoSpec(f, buildRepoArchiveWithFiles(t, f.gooDir, goolib.PkgSpec{
		Name: "A", Arch: "noarch", Version: "1",
		Files: map[string]string{"p": p},
	}, map[string]string{"p/a.txt": "a"}))
	if err := f.install(t, "A"); err != nil {
		t.Fatalf("install A.noarch.1: %v", err)
	}
	if sum, ok := f.rows(t)["A"].InstalledFiles[p]; !ok || sum != "" {
		t.Fatalf("A.noarch.1's row lists %s as (%q, %t), want it recorded as a directory", p, sum, ok)
	}
	untracked := filepath.Join(p, "untracked")
	if err := os.WriteFile(untracked, []byte("user"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, 0300); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(p, 0755) })
	if _, err := os.ReadDir(p); err == nil {
		t.Skip("mode 0300 doesn't deny listing a directory here")
	}
	publishRepoSpec(f, buildRepoArchiveWithFiles(t, f.gooDir, goolib.PkgSpec{
		Name: "A", Arch: "noarch", Version: "2",
		Files: map[string]string{"c.txt": filepath.Join(f.dst, "c.txt")},
	}, map[string]string{"c.txt": "c"}))
	if err := f.install(t, "A"); err != nil {
		t.Fatalf("install A.noarch.2: %v", err)
	}
	if err := os.Chmod(p, 0755); err != nil {
		t.Fatal(err)
	}
	row := f.rows(t)["A"]
	if row.PackageSpec.Version != "2" {
		t.Fatalf("A's installed version = %q, want 2", row.PackageSpec.Version)
	}
	if sum, ok := row.InstalledFiles[p]; !ok || sum != "" {
		t.Errorf("A.noarch.2's row lists %s as (%q, %t), want it carried forward as a directory", p, sum, ok)
	}
	if !isDir(p) || !fileThere(untracked) {
		t.Errorf("want %s and %s kept", p, untracked)
	}
	if fileThere(filepath.Join(p, "a.txt")) {
		t.Errorf("%s is still there, want cleanup to delete it", filepath.Join(p, "a.txt"))
	}
}
