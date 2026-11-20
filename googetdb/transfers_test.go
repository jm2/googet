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

package googetdb

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/google/googet/v2/client"
	"github.com/google/googet/v2/goolib"
)

// frozen07fd257InstalledApplication is a standalone copy of
// client.InstalledApplication frozen at commit 07fd257 (GooGet 3.4.2) so
// backward-compatibility decoding tests never share live production types.
type frozen07fd257InstalledApplication struct {
	Name string
	Reg  string
}

// frozen07fd257ExecFile is a standalone copy of goolib.ExecFile frozen at
// commit 07fd257 (GooGet 3.4.2).
type frozen07fd257ExecFile struct {
	Path      string   `json:",omitempty"`
	Args      []string `json:",omitempty"`
	ExitCodes []int    `json:",omitempty"`
}

// frozen07fd257PkgSpec is a standalone copy of goolib.PkgSpec frozen at commit
// 07fd257 (GooGet 3.4.2).
type frozen07fd257PkgSpec struct {
	Name                string
	Version             string
	Arch                string
	LockArch            bool              `json:",omitempty"`
	ReleaseNotes        []string          `json:",omitempty"`
	Description         string            `json:",omitempty"`
	License             string            `json:",omitempty"`
	Authors             string            `json:",omitempty"`
	Owners              string            `json:",omitempty"`
	Source              string            `json:",omitempty"`
	Tags                map[string][]byte `json:",omitempty"`
	PkgDependencies     map[string]string `json:",omitempty"`
	Replaces            []string
	Conflicts           []string
	Provides            []string
	Install             frozen07fd257ExecFile
	Uninstall           frozen07fd257ExecFile
	Verify              frozen07fd257ExecFile
	Files               map[string]string `json:",omitempty"`
	ExternalProgramName string
	ExecutableNames     []string `json:",omitempty"`
	InstallPath         string   `json:",omitempty"`
}

// frozen07fd257PackageState is a standalone copy of client.PackageState frozen
// at commit 07fd257 (GooGet 3.4.2).
type frozen07fd257PackageState struct {
	SourceRepo, DownloadURL, Checksum, LocalPath, UnpackDir string
	PackageSpec                                             *frozen07fd257PkgSpec
	InstalledFiles                                          map[string]string
	InstallDate                                             int64
	InstalledApp                                            frozen07fd257InstalledApplication
}

func openTestDB(t *testing.T) (*GooDB, string) {
	t.Helper()
	dbFile := filepath.Join(t.TempDir(), "googet.db")
	db, err := NewDB(dbFile)
	if err != nil {
		t.Fatalf("NewDB(%q) unexpected error: %v", dbFile, err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db, dbFile
}

type rawRowMeta struct {
	id      int64
	pkgVer  string
	pkgJSON []byte
}

func readRawRow(t *testing.T, db *GooDB, name, arch string) (rawRowMeta, bool) {
	t.Helper()
	var m rawRowMeta
	err := db.db.QueryRow(
		`SELECT id, pkg_ver, pkg_json FROM InstalledPackages WHERE pkg_name = ? AND pkg_arch = ?`,
		name, arch,
	).Scan(&m.id, &m.pkgVer, &m.pkgJSON)
	if err != nil {
		return rawRowMeta{}, false
	}
	return m, true
}

// TestInstalledFilesKey verifies that the installedFilesKey constant matches
// the JSON object key produced by marshaling client.PackageState.InstalledFiles.
func TestInstalledFilesKey(t *testing.T) {
	t.Parallel()
	ps := client.PackageState{
		InstalledFiles: map[string]string{"sentinel-path": "sentinel-checksum"},
	}
	raw, err := json.Marshal(ps)
	if err != nil {
		t.Fatalf("json.Marshal(PackageState) unexpected error: %v", err)
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatalf("json.Unmarshal(PackageState) unexpected error: %v", err)
	}

	var matchedKeys []string
	for k, v := range obj {
		var m map[string]string
		if err := json.Unmarshal(v, &m); err == nil && m["sentinel-path"] == "sentinel-checksum" {
			matchedKeys = append(matchedKeys, k)
		}
	}
	if len(matchedKeys) != 1 {
		t.Fatalf("matched JSON keys for InstalledFiles = %v, want exactly 1 key", matchedKeys)
	}
	if matchedKeys[0] != installedFilesKey {
		t.Errorf("marshaled InstalledFiles JSON key = %q, want %q", matchedKeys[0], installedFilesKey)
	}
}

// TestAddPkgWithTransfersAtomicityBeforeInsertTrigger verifies that when a
// BEFORE INSERT trigger aborts inserting the new package row after an owner
// row's UPDATE has run, the single transaction rolls back completely, leaving
// the owner row unchanged and the new package absent.
func TestAddPkgWithTransfersAtomicityBeforeInsertTrigger(t *testing.T) {
	t.Parallel()
	db, _ := openTestDB(t)

	owner := client.PackageState{
		PackageSpec: &goolib.PkgSpec{Name: "pkgA", Arch: "noarch", Version: "1.0.0"},
		InstalledFiles: map[string]string{
			`C:\App\shared.dll`: "sum-a",
			`C:\App\keep.dll`:   "sum-keep",
		},
		InstallDate: 1700000000,
	}
	if err := db.AddPkg(owner); err != nil {
		t.Fatalf("AddPkg(pkgA) unexpected error: %v", err)
	}
	beforeA, ok := readRawRow(t, db, "pkgA", "noarch")
	if !ok {
		t.Fatal("pkgA row missing after AddPkg")
	}

	if _, err := db.db.Exec(`
		CREATE TRIGGER abort_pkg_b_insert
		BEFORE INSERT ON InstalledPackages
		WHEN NEW.pkg_name = 'pkgB'
		BEGIN
			SELECT RAISE(ABORT, 'simulated insert failure');
		END;
	`); err != nil {
		t.Fatalf("creating BEFORE INSERT trigger: %v", err)
	}

	pkgB := client.PackageState{
		PackageSpec: &goolib.PkgSpec{Name: "pkgB", Arch: "noarch", Version: "1.0.0"},
		InstalledFiles: map[string]string{
			`C:\App\shared.dll`: "sum-b",
		},
		InstallDate: 1700000100,
	}
	err := db.AddPkgWithTransfers(pkgB, []FileTransfer{
		{Name: "pkgA", Arch: "noarch", Files: []string{`C:\App\shared.dll`}},
	})
	if err == nil {
		t.Fatal("AddPkgWithTransfers() = nil, want error from BEFORE INSERT trigger")
	}

	afterA, ok := readRawRow(t, db, "pkgA", "noarch")
	if !ok {
		t.Fatal("pkgA row missing after failed AddPkgWithTransfers")
	}
	if diff := cmp.Diff(string(beforeA.pkgJSON), string(afterA.pkgJSON)); diff != "" {
		t.Errorf("pkgA pkg_json changed despite failed insert (-want +got):\n%s", diff)
	}
	if afterA.id != beforeA.id || afterA.pkgVer != beforeA.pkgVer {
		t.Errorf("pkgA (id, pkg_ver) = (%d, %q), want (%d, %q)", afterA.id, afterA.pkgVer, beforeA.id, beforeA.pkgVer)
	}
	if _, exists := readRawRow(t, db, "pkgB", "noarch"); exists {
		t.Error("pkgB row exists in InstalledPackages, want absent after rollback")
	}
}

// TestAddPkgWithTransfersAtomicityUpdateFailure verifies that neither a BEFORE
// UPDATE trigger failure on an owner row nor a corrupt/non-object owner
// pkg_json inserts the new package or alters existing rows.
func TestAddPkgWithTransfersAtomicityUpdateFailure(t *testing.T) {
	t.Parallel()

	t.Run("before_update_trigger_aborts", func(t *testing.T) {
		t.Parallel()
		db, _ := openTestDB(t)

		owner1 := client.PackageState{
			PackageSpec:    &goolib.PkgSpec{Name: "pkgA1", Arch: "noarch", Version: "1.0.0"},
			InstalledFiles: map[string]string{`C:\App\s1.dll`: "sum-a1"},
			InstallDate:    1700000000,
		}
		owner2 := client.PackageState{
			PackageSpec:    &goolib.PkgSpec{Name: "pkgA2", Arch: "noarch", Version: "1.0.0"},
			InstalledFiles: map[string]string{`C:\App\s2.dll`: "sum-a2"},
			InstallDate:    1700000001,
		}
		for _, st := range []client.PackageState{owner1, owner2} {
			if err := db.AddPkg(st); err != nil {
				t.Fatalf("AddPkg(%s) unexpected error: %v", st.PackageSpec.Name, err)
			}
		}
		beforeA1, _ := readRawRow(t, db, "pkgA1", "noarch")
		beforeA2, _ := readRawRow(t, db, "pkgA2", "noarch")

		if _, err := db.db.Exec(`
			CREATE TRIGGER abort_pkg_a2_update
			BEFORE UPDATE ON InstalledPackages
			WHEN OLD.pkg_name = 'pkgA2'
			BEGIN
				SELECT RAISE(ABORT, 'simulated update failure');
			END;
		`); err != nil {
			t.Fatalf("creating BEFORE UPDATE trigger: %v", err)
		}

		pkgB := client.PackageState{
			PackageSpec: &goolib.PkgSpec{Name: "pkgB", Arch: "noarch", Version: "1.0.0"},
			InstalledFiles: map[string]string{
				`C:\App\s1.dll`: "sum-b1",
				`C:\App\s2.dll`: "sum-b2",
			},
			InstallDate: 1700000100,
		}
		err := db.AddPkgWithTransfers(pkgB, []FileTransfer{
			{Name: "pkgA1", Arch: "noarch", Files: []string{`C:\App\s1.dll`}},
			{Name: "pkgA2", Arch: "noarch", Files: []string{`C:\App\s2.dll`}},
		})
		if err == nil {
			t.Fatal("AddPkgWithTransfers() = nil, want error from BEFORE UPDATE trigger")
		}

		afterA1, _ := readRawRow(t, db, "pkgA1", "noarch")
		afterA2, _ := readRawRow(t, db, "pkgA2", "noarch")
		if string(afterA1.pkgJSON) != string(beforeA1.pkgJSON) {
			t.Errorf("pkgA1 pkg_json = %s, want unchanged %s", afterA1.pkgJSON, beforeA1.pkgJSON)
		}
		if string(afterA2.pkgJSON) != string(beforeA2.pkgJSON) {
			t.Errorf("pkgA2 pkg_json = %s, want unchanged %s", afterA2.pkgJSON, beforeA2.pkgJSON)
		}
		if _, exists := readRawRow(t, db, "pkgB", "noarch"); exists {
			t.Error("pkgB row exists in InstalledPackages, want absent after failed update")
		}
	})

	for _, tc := range []struct {
		name    string
		rawJSON string
	}{
		{name: "corrupt_non_json_owner_row", rawJSON: "{not-valid-json"},
		{name: "json_null_owner_row", rawJSON: "null"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			db, _ := openTestDB(t)
			if _, err := db.db.Exec(
				stateQuery, "pkgCorrupt", "1.0.0", "noarch", []byte(tc.rawJSON),
			); err != nil {
				t.Fatalf("seeding corrupt row: %v", err)
			}

			pkgB := client.PackageState{
				PackageSpec:    &goolib.PkgSpec{Name: "pkgB", Arch: "noarch", Version: "1.0.0"},
				InstalledFiles: map[string]string{`C:\App\shared.dll`: "sum-b"},
				InstallDate:    1700000100,
			}
			err := db.AddPkgWithTransfers(pkgB, []FileTransfer{
				{Name: "pkgCorrupt", Arch: "noarch", Files: []string{`C:\App\shared.dll`}},
			})
			if err == nil {
				t.Fatalf("AddPkgWithTransfers() = nil for %s, want error", tc.name)
			}
			afterCorrupt, ok := readRawRow(t, db, "pkgCorrupt", "noarch")
			if !ok {
				t.Fatal("pkgCorrupt row missing")
			}
			if string(afterCorrupt.pkgJSON) != tc.rawJSON {
				t.Errorf("pkgCorrupt pkg_json = %q, want %q", afterCorrupt.pkgJSON, tc.rawJSON)
			}
			if _, exists := readRawRow(t, db, "pkgB", "noarch"); exists {
				t.Error("pkgB row exists in InstalledPackages, want absent")
			}
		})
	}
}

// TestAddPkgWithTransfersRejectsSelfTransfer verifies that any transfer naming
// pkgState's own (Name, Arch) is rejected with an error before starting a
// transaction or writing anything, while transfers from a different
// architecture of the same package name succeed.
func TestAddPkgWithTransfersRejectsSelfTransfer(t *testing.T) {
	t.Parallel()
	db, _ := openTestDB(t)

	otherOwner := client.PackageState{
		PackageSpec:    &goolib.PkgSpec{Name: "pkgA", Arch: "noarch", Version: "1.0.0"},
		InstalledFiles: map[string]string{`C:\App\shared.dll`: "sum-a"},
		InstallDate:    1700000000,
	}
	existingSelf := client.PackageState{
		PackageSpec:    &goolib.PkgSpec{Name: "pkgB", Arch: "x86_64", Version: "1.0.0"},
		InstalledFiles: map[string]string{`C:\App\b.dll`: "sum-b-v1"},
		InstallDate:    1700000001,
	}
	otherArch := client.PackageState{
		PackageSpec: &goolib.PkgSpec{Name: "pkgB", Arch: "x86_32", Version: "1.0.0"},
		InstalledFiles: map[string]string{
			`C:\App\cross_arch.dll`: "sum-b32",
			`C:\App\keep32.dll`:     "sum-keep32",
		},
		InstallDate: 1700000002,
	}
	for _, st := range []client.PackageState{otherOwner, existingSelf, otherArch} {
		if err := db.AddPkg(st); err != nil {
			t.Fatalf("AddPkg(%s.%s): %v", st.PackageSpec.Name, st.PackageSpec.Arch, err)
		}
	}
	beforeA, _ := readRawRow(t, db, "pkgA", "noarch")
	beforeB, _ := readRawRow(t, db, "pkgB", "x86_64")

	// Reject self-transfer before writing anything, even when paired with a
	// valid transfer on pkgA or an empty file list.
	candidateB := client.PackageState{
		PackageSpec: &goolib.PkgSpec{Name: "pkgB", Arch: "x86_64", Version: "2.0.0"},
		InstalledFiles: map[string]string{
			`C:\App\shared.dll`: "sum-b-v2",
			`C:\App\b.dll`:      "sum-b-v2",
		},
		InstallDate: 1700000200,
	}
	for _, tc := range []struct {
		name      string
		transfers []FileTransfer
	}{
		{
			name: "self_transfer_with_files",
			transfers: []FileTransfer{
				{Name: "pkgA", Arch: "noarch", Files: []string{`C:\App\shared.dll`}},
				{Name: "pkgB", Arch: "x86_64", Files: []string{`C:\App\b.dll`}},
			},
		},
		{
			name: "self_transfer_with_empty_files",
			transfers: []FileTransfer{
				{Name: "pkgB", Arch: "x86_64"},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := db.AddPkgWithTransfers(candidateB, tc.transfers); err == nil {
				t.Fatal("AddPkgWithTransfers() = nil, want self-transfer error")
			}
			afterA, _ := readRawRow(t, db, "pkgA", "noarch")
			afterB, _ := readRawRow(t, db, "pkgB", "x86_64")
			if string(afterA.pkgJSON) != string(beforeA.pkgJSON) {
				t.Errorf("pkgA row modified on rejected self-transfer: got %s, want %s", afterA.pkgJSON, beforeA.pkgJSON)
			}
			if string(afterB.pkgJSON) != string(beforeB.pkgJSON) {
				t.Errorf("pkgB row modified on rejected self-transfer: got %s, want %s", afterB.pkgJSON, beforeB.pkgJSON)
			}
		})
	}

	// Transferring from another architecture of the same package name is valid.
	crossArchB := client.PackageState{
		PackageSpec: &goolib.PkgSpec{Name: "pkgB", Arch: "x86_64", Version: "2.0.0"},
		InstalledFiles: map[string]string{
			`C:\App\cross_arch.dll`: "sum-b64",
			`C:\App\b.dll`:          "sum-b-v2",
		},
		InstallDate: 1700000300,
	}
	if err := db.AddPkgWithTransfers(crossArchB, []FileTransfer{
		{Name: "pkgB", Arch: "x86_32", Files: []string{`C:\App\cross_arch.dll`}},
	}); err != nil {
		t.Fatalf("AddPkgWithTransfers(other arch) unexpected error: %v", err)
	}
	gotB32, err := db.FetchPkg(goolib.PackageInfo{Name: "pkgB", Arch: "x86_32"})
	if err != nil {
		t.Fatalf("FetchPkg(pkgB.x86_32): %v", err)
	}
	if diff := cmp.Diff(map[string]string{`C:\App\keep32.dll`: "sum-keep32"}, gotB32.InstalledFiles); diff != "" {
		t.Errorf("pkgB.x86_32 InstalledFiles mismatch (-want +got):\n%s", diff)
	}
}

// TestAddPkgWithTransfersSkipsMissingAbsentNullUnlistedAndEmptiesMap verifies
// skipping of non-existent rows, rows with absent or null InstalledFiles, and
// unlisted paths, as well as clean decoding when all entries are removed from
// an owner's InstalledFiles map.
func TestAddPkgWithTransfersSkipsMissingAbsentNullUnlistedAndEmptiesMap(t *testing.T) {
	t.Parallel()
	db, _ := openTestDB(t)

	rawAbsent := `{"PackageSpec":{"Name":"pkgAbsent","Version":"1.0.0","Arch":"noarch"},"InstallDate":1700000000}`
	rawNull := `{"PackageSpec":{"Name":"pkgNull","Version":"1.0.0","Arch":"noarch"},"InstalledFiles":null,"InstallDate":1700000000}`
	for _, seed := range []struct{ name, raw string }{
		{"pkgAbsent", rawAbsent},
		{"pkgNull", rawNull},
	} {
		if _, err := db.db.Exec(stateQuery, seed.name, "1.0.0", "noarch", []byte(seed.raw)); err != nil {
			t.Fatalf("seeding %s: %v", seed.name, err)
		}
	}

	if err := db.AddPkg(client.PackageState{
		PackageSpec:    &goolib.PkgSpec{Name: "pkgUnlisted", Arch: "noarch", Version: "1.0.0"},
		InstalledFiles: map[string]string{`C:\App\only.dll`: "sum-only"},
		InstallDate:    1700000001,
	}); err != nil {
		t.Fatalf("AddPkg(pkgUnlisted): %v", err)
	}
	if err := db.AddPkg(client.PackageState{
		PackageSpec: &goolib.PkgSpec{Name: "pkgEmptied", Arch: "noarch", Version: "1.0.0"},
		InstalledFiles: map[string]string{
			`C:\App\move1.dll`: "sum-1",
			`C:\App\move2.dll`: "sum-2",
		},
		InstallDate: 1700000002,
	}); err != nil {
		t.Fatalf("AddPkg(pkgEmptied): %v", err)
	}
	beforeAbsent, _ := readRawRow(t, db, "pkgAbsent", "noarch")
	beforeNull, _ := readRawRow(t, db, "pkgNull", "noarch")
	beforeUnlisted, _ := readRawRow(t, db, "pkgUnlisted", "noarch")

	pkgNew := client.PackageState{
		PackageSpec: &goolib.PkgSpec{Name: "pkgNew", Arch: "noarch", Version: "1.0.0"},
		InstalledFiles: map[string]string{
			`C:\App\move1.dll`: "sum-new-1",
			`C:\App\move2.dll`: "sum-new-2",
		},
		InstallDate: 1700000100,
	}
	if err := db.AddPkgWithTransfers(pkgNew, []FileTransfer{
		{Name: "pkgDoesNotExist", Arch: "noarch", Files: []string{`C:\App\ghost.dll`}},
		{Name: "pkgAbsent", Arch: "noarch", Files: []string{`C:\App\ghost.dll`}},
		{Name: "pkgNull", Arch: "noarch", Files: []string{`C:\App\ghost.dll`}},
		{Name: "pkgUnlisted", Arch: "noarch", Files: []string{`C:\App\not-there.dll`}},
		{Name: "pkgEmptied", Arch: "noarch", Files: []string{`C:\App\move1.dll`}},
		{Name: "pkgEmptied", Arch: "noarch", Files: []string{`C:\App\move2.dll`, `C:\App\extra.dll`}},
	}); err != nil {
		t.Fatalf("AddPkgWithTransfers() unexpected error: %v", err)
	}

	// Rows with absent/null InstalledFiles or only unlisted paths must not even
	// be rewritten.
	afterAbsent, _ := readRawRow(t, db, "pkgAbsent", "noarch")
	afterNull, _ := readRawRow(t, db, "pkgNull", "noarch")
	afterUnlisted, _ := readRawRow(t, db, "pkgUnlisted", "noarch")
	if string(afterAbsent.pkgJSON) != string(beforeAbsent.pkgJSON) {
		t.Errorf("pkgAbsent rewritten: got %s, want %s", afterAbsent.pkgJSON, beforeAbsent.pkgJSON)
	}
	if string(afterNull.pkgJSON) != string(beforeNull.pkgJSON) {
		t.Errorf("pkgNull rewritten: got %s, want %s", afterNull.pkgJSON, beforeNull.pkgJSON)
	}
	if string(afterUnlisted.pkgJSON) != string(beforeUnlisted.pkgJSON) {
		t.Errorf("pkgUnlisted rewritten: got %s, want %s", afterUnlisted.pkgJSON, beforeUnlisted.pkgJSON)
	}

	gotEmptied, err := db.FetchPkg(goolib.PackageInfo{Name: "pkgEmptied", Arch: "noarch"})
	if err != nil {
		t.Fatalf("FetchPkg(pkgEmptied): %v", err)
	}
	if len(gotEmptied.InstalledFiles) != 0 {
		t.Errorf("pkgEmptied InstalledFiles = %v, want empty", gotEmptied.InstalledFiles)
	}

	emptiedRaw, ok := readRawRow(t, db, "pkgEmptied", "noarch")
	if !ok {
		t.Fatal("pkgEmptied row missing")
	}
	var frozenEmptied frozen07fd257PackageState
	if err := json.Unmarshal(emptiedRaw.pkgJSON, &frozenEmptied); err != nil {
		t.Fatalf("json.Unmarshal(pkgEmptied) into frozen 07fd257 PackageState failed: %v", err)
	}
	if len(frozenEmptied.InstalledFiles) != 0 {
		t.Errorf("frozen 07fd257 pkgEmptied InstalledFiles = %v, want empty", frozenEmptied.InstalledFiles)
	}
}

// TestAddPkgWithTransfersPreservesUnknownFieldsSpecialCharsAndRowColumns
// verifies semantic preservation of unknown JSON keys, paths containing '&',
// '<', '>', and non-ASCII characters, unchanged SQLite id and pkg_ver columns,
// and plain json.Unmarshal compatibility with the frozen 07fd257 PackageState.
func TestAddPkgWithTransfersPreservesUnknownFieldsSpecialCharsAndRowColumns(t *testing.T) {
	t.Parallel()
	db, _ := openTestDB(t)

	transferredSpecial := `C:\Program Files\R&D\<Tools>\Ümlaut\αβγ.dll`
	keptSpecial := `C:\Program Files\Keep<&>\δεζ\tool.exe`
	keptDir := `C:\Program Files\Keep<&>\δεζ`

	ownerState := client.PackageState{
		SourceRepo:  "https://repo.example/main",
		DownloadURL: "https://repo.example/main/pkgA.goo?x=1&y=<2>",
		Checksum:    "deadbeef",
		LocalPath:   `C:\GooGet\cache\pkgA.noarch.1.2.3@1.goo`,
		PackageSpec: &goolib.PkgSpec{
			Name:        "pkgA",
			Version:     "1.2.3@1",
			Arch:        "noarch",
			Description: "R&D <Tools> — Ümlaut",
			Provides:    []string{"tool-suite=1.2.0"},
			Conflicts:   []string{"legacy-tool"},
			Replaces:    []string{"old-tool"},
		},
		InstalledFiles: map[string]string{
			transferredSpecial: "sum-old",
			keptSpecial:        "sum-keep",
			keptDir:            "",
		},
		InstallDate: 1700000555,
	}
	if err := db.AddPkg(ownerState); err != nil {
		t.Fatalf("AddPkg(pkgA): %v", err)
	}

	// Inject unknown top-level fields onto pkgA's stored JSON without changing
	// its row id or pkg_ver.
	initialMeta, ok := readRawRow(t, db, "pkgA", "noarch")
	if !ok {
		t.Fatal("pkgA row missing")
	}
	var seedObj map[string]any
	if err := json.Unmarshal(initialMeta.pkgJSON, &seedObj); err != nil {
		t.Fatalf("json.Unmarshal(seed): %v", err)
	}
	seedObj["FutureSchemaField"] = map[string]any{
		"note":  "keep <all> & non-ASCII Ω",
		"items": []any{float64(7), true, "ok"},
	}
	seedObj["FutureNumericFlag"] = float64(42)
	seededBytes, err := json.Marshal(seedObj)
	if err != nil {
		t.Fatalf("json.Marshal(seeded): %v", err)
	}
	if _, err := db.db.Exec(
		`UPDATE InstalledPackages SET pkg_json = ? WHERE pkg_name = ? AND pkg_arch = ?`,
		seededBytes, "pkgA", "noarch",
	); err != nil {
		t.Fatalf("updating seeded pkgA json: %v", err)
	}
	beforeMeta, _ := readRawRow(t, db, "pkgA", "noarch")

	// Insert a dummy row so AUTOINCREMENT advances; if pkgA were replaced
	// instead of updated in place, pkgA's id would jump past dummy's id.
	if err := db.AddPkg(client.PackageState{
		PackageSpec: &goolib.PkgSpec{Name: "spacer", Arch: "noarch", Version: "1.0.0"},
		InstallDate: 1700000556,
	}); err != nil {
		t.Fatalf("AddPkg(spacer): %v", err)
	}

	pkgB := client.PackageState{
		SourceRepo:  "https://repo.example/main",
		DownloadURL: "https://repo.example/main/pkgB.goo",
		Checksum:    "cafebabe",
		LocalPath:   `C:\GooGet\cache\pkgB.x86_64.2.0.0@1.goo`,
		PackageSpec: &goolib.PkgSpec{
			Name:     "pkgB",
			Version:  "2.0.0@1",
			Arch:     "x86_64",
			Provides: []string{"new-suite"},
		},
		InstalledFiles: map[string]string{
			transferredSpecial: "sum-new",
		},
		InstallDate: 1700000999,
	}
	if err := db.AddPkgWithTransfers(pkgB, []FileTransfer{
		{Name: "pkgA", Arch: "noarch", Files: []string{transferredSpecial}},
		{Name: "pkgA", Arch: "noarch", Files: []string{transferredSpecial}},
	}); err != nil {
		t.Fatalf("AddPkgWithTransfers() unexpected error: %v", err)
	}

	afterMeta, ok := readRawRow(t, db, "pkgA", "noarch")
	if !ok {
		t.Fatal("pkgA row missing after transfer")
	}
	if afterMeta.id != beforeMeta.id {
		t.Errorf("pkgA SQLite id = %d, want unchanged %d", afterMeta.id, beforeMeta.id)
	}
	if afterMeta.pkgVer != beforeMeta.pkgVer {
		t.Errorf("pkgA SQLite pkg_ver = %q, want unchanged %q", afterMeta.pkgVer, beforeMeta.pkgVer)
	}

	var beforeDecoded, afterDecoded map[string]any
	if err := json.Unmarshal(beforeMeta.pkgJSON, &beforeDecoded); err != nil {
		t.Fatalf("json.Unmarshal(before): %v", err)
	}
	if err := json.Unmarshal(afterMeta.pkgJSON, &afterDecoded); err != nil {
		t.Fatalf("json.Unmarshal(after): %v", err)
	}
	wantFiles := map[string]any{
		keptSpecial: "sum-keep",
		keptDir:     "",
	}
	if diff := cmp.Diff(wantFiles, afterDecoded[installedFilesKey]); diff != "" {
		t.Errorf("pkgA InstalledFiles JSON mismatch (-want +got):\n%s", diff)
	}
	delete(beforeDecoded, installedFilesKey)
	delete(afterDecoded, installedFilesKey)
	if diff := cmp.Diff(beforeDecoded, afterDecoded); diff != "" {
		t.Errorf("pkgA non-InstalledFiles JSON fields mismatch (-want +got):\n%s", diff)
	}

	// Verify plain json.Unmarshal into the frozen 07fd257 3.4.2 PackageState
	// for both the transfer-edited owner row and the newly inserted row.
	var frozenA frozen07fd257PackageState
	if err := json.Unmarshal(afterMeta.pkgJSON, &frozenA); err != nil {
		t.Fatalf("json.Unmarshal(pkgA) into frozen 07fd257 PackageState failed: %v", err)
	}
	if diff := cmp.Diff(map[string]string{keptSpecial: "sum-keep", keptDir: ""}, frozenA.InstalledFiles); diff != "" {
		t.Errorf("frozen 07fd257 pkgA InstalledFiles (-want +got):\n%s", diff)
	}
	if frozenA.PackageSpec == nil || frozenA.PackageSpec.Name != "pkgA" || frozenA.PackageSpec.Version != "1.2.3@1" || frozenA.PackageSpec.Arch != "noarch" {
		t.Errorf("frozen 07fd257 pkgA PackageSpec = %+v, want pkgA.noarch.1.2.3@1", frozenA.PackageSpec)
	}

	metaB, ok := readRawRow(t, db, "pkgB", "x86_64")
	if !ok {
		t.Fatal("pkgB row missing")
	}
	var frozenB frozen07fd257PackageState
	if err := json.Unmarshal(metaB.pkgJSON, &frozenB); err != nil {
		t.Fatalf("json.Unmarshal(pkgB) into frozen 07fd257 PackageState failed: %v", err)
	}
	if diff := cmp.Diff(map[string]string{transferredSpecial: "sum-new"}, frozenB.InstalledFiles); diff != "" {
		t.Errorf("frozen 07fd257 pkgB InstalledFiles (-want +got):\n%s", diff)
	}
	if frozenB.PackageSpec == nil || frozenB.PackageSpec.Name != "pkgB" || frozenB.PackageSpec.Version != "2.0.0@1" || frozenB.PackageSpec.Arch != "x86_64" {
		t.Errorf("frozen 07fd257 pkgB PackageSpec = %+v, want pkgB.x86_64.2.0.0@1", frozenB.PackageSpec)
	}
}

// TestAddPkgDelegationAndRollbackReleasesConnection verifies AddPkg delegation
// to AddPkgWithTransfers and proves that post-Begin errors roll back the open
// transaction without blocking subsequent writes even on a single-connection
// pool.
func TestAddPkgDelegationAndRollbackReleasesConnection(t *testing.T) {
	db, _ := openTestDB(t)
	db.db.SetMaxOpenConns(1)

	if err := db.AddPkg(client.PackageState{}); err == nil {
		t.Fatal("AddPkg(nil PackageSpec) = nil, want error")
	}

	fixedUnix := int64(1765000000)
	prevNow := nowFunc
	nowFunc = func() time.Time { return time.Unix(fixedUnix, 0) }
	t.Cleanup(func() { nowFunc = prevNow })

	// Seed a corrupt owner row so AddPkgWithTransfers fails after Begin(), then
	// verify the deferred Rollback releases the sole pool connection for a
	// subsequent AddPkg call.
	if _, err := db.db.Exec(stateQuery, "corruptOwner", "1.0.0", "noarch", []byte("{bad-json")); err != nil {
		t.Fatalf("seeding corruptOwner: %v", err)
	}
	err := db.AddPkgWithTransfers(client.PackageState{
		PackageSpec:    &goolib.PkgSpec{Name: "failedPkg", Arch: "noarch", Version: "1.0.0"},
		InstalledFiles: map[string]string{`C:\f`: "sum"},
	}, []FileTransfer{{Name: "corruptOwner", Arch: "noarch", Files: []string{`C:\f`}}})
	if err == nil {
		t.Fatal("AddPkgWithTransfers(corrupt owner) = nil, want error")
	}
	if err := db.RemovePkg("corruptOwner", "noarch"); err != nil {
		t.Fatalf("RemovePkg(corruptOwner): %v", err)
	}

	// Also force a post-Begin failure at the INSERT statement via trigger, drop
	// the trigger, and verify subsequent AddPkg still succeeds immediately.
	if _, err := db.db.Exec(`
		CREATE TRIGGER fail_once_insert
		BEFORE INSERT ON InstalledPackages
		WHEN NEW.pkg_name = 'blockedPkg'
		BEGIN
			SELECT RAISE(ABORT, 'insert blocked');
		END;
	`); err != nil {
		t.Fatalf("creating trigger: %v", err)
	}
	if err := db.AddPkg(client.PackageState{
		PackageSpec: &goolib.PkgSpec{Name: "blockedPkg", Arch: "noarch", Version: "1.0.0"},
	}); err == nil {
		t.Fatal("AddPkg(blockedPkg) = nil, want trigger error")
	}
	if _, err := db.db.Exec(`DROP TRIGGER fail_once_insert`); err != nil {
		t.Fatalf("dropping trigger: %v", err)
	}

	good := client.PackageState{
		PackageSpec:    &goolib.PkgSpec{Name: "goodPkg", Arch: "noarch", Version: "1.0.0"},
		InstalledFiles: map[string]string{`C:\App\good.exe`: "sum-good"},
	}
	if err := db.AddPkg(good); err != nil {
		t.Fatalf("subsequent AddPkg(goodPkg) failed after rollback: %v", err)
	}
	got, err := db.FetchPkg(goolib.PackageInfo{Name: "goodPkg", Arch: "noarch"})
	if err != nil {
		t.Fatalf("FetchPkg(goodPkg): %v", err)
	}
	if got.InstallDate != fixedUnix {
		t.Errorf("goodPkg InstallDate = %d, want %d", got.InstallDate, fixedUnix)
	}
	if diff := cmp.Diff(good.InstalledFiles, got.InstalledFiles, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("goodPkg InstalledFiles (-want +got):\n%s", diff)
	}
}

// TestAddPkgWithTransfersTxConnectionAndConcurrentFetchPkgs verifies under
// -race that AddPkgWithTransfers performs all reads and writes on the
// transaction's single connection rather than opening a second pool connection,
// and that concurrent FetchPkgs readers see atomic snapshots without data races
// or SQLITE_BUSY.
func TestAddPkgWithTransfersTxConnectionAndConcurrentFetchPkgs(t *testing.T) {
	t.Parallel()

	t.Run("uses_single_tx_connection", func(t *testing.T) {
		t.Parallel()
		seedDB, dbFile := openTestDB(t)
		for _, name := range []string{"owner1", "owner2"} {
			if err := seedDB.AddPkg(client.PackageState{
				PackageSpec: &goolib.PkgSpec{Name: name, Arch: "noarch", Version: "1.0.0"},
				InstalledFiles: map[string]string{
					`C:\Shared\a.dll`: "sum-old",
					`C:\Shared\b.dll`: "sum-keep",
				},
				InstallDate: 1700000000,
			}); err != nil {
				t.Fatalf("seeding %s: %v", name, err)
			}
		}
		if err := seedDB.Close(); err != nil {
			t.Fatalf("closing seedDB: %v", err)
		}

		// Reopen GooDB so sql.Open starts with zero open connections.
		db, err := NewDB(dbFile)
		if err != nil {
			t.Fatalf("reopening NewDB: %v", err)
		}
		t.Cleanup(func() { _ = db.Close() })

		if err := db.AddPkgWithTransfers(client.PackageState{
			PackageSpec:    &goolib.PkgSpec{Name: "taker", Arch: "noarch", Version: "1.0.0"},
			InstalledFiles: map[string]string{`C:\Shared\a.dll`: "sum-taker"},
			InstallDate:    1700000100,
		}, []FileTransfer{
			{Name: "owner1", Arch: "noarch", Files: []string{`C:\Shared\a.dll`}},
			{Name: "owner2", Arch: "noarch", Files: []string{`C:\Shared\a.dll`}},
		}); err != nil {
			t.Fatalf("AddPkgWithTransfers() unexpected error: %v", err)
		}
		if gotConns := db.db.Stats().OpenConnections; gotConns != 1 {
			t.Errorf("db.Stats().OpenConnections after AddPkgWithTransfers = %d, want 1 (all reads must use tx)", gotConns)
		}
	})

	t.Run("concurrent_fetch_pkgs_reader_no_busy_no_race", func(t *testing.T) {
		t.Parallel()
		db, _ := openTestDB(t)
		// Serialize pool checkout to a single connection so concurrent
		// FetchPkgs readers queue cleanly in database/sql while also catching
		// any accidental g.db query inside an open transaction (which would
		// deadlock on the single pool slot).
		db.db.SetMaxOpenConns(1)

		sharedPath := `C:\Shared\concurrent.dll`
		if err := db.AddPkg(client.PackageState{
			PackageSpec: &goolib.PkgSpec{Name: "pkgA", Arch: "noarch", Version: "1.0.0"},
			InstalledFiles: map[string]string{
				sharedPath:        "sum-a",
				`C:\Shared\a.txt`: "sum-a-only",
			},
			InstallDate: 1700000000,
		}); err != nil {
			t.Fatalf("AddPkg(pkgA): %v", err)
		}

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		stop := make(chan struct{})
		var readerWG sync.WaitGroup
		for range 2 {
			readerWG.Add(1)
			go func() {
				defer readerWG.Done()
				for {
					select {
					case <-stop:
						return
					case <-ctx.Done():
						return
					default:
						pkgs, err := db.FetchPkgs("")
						if err != nil {
							t.Errorf("concurrent FetchPkgs(\"\") unexpected error: %v", err)
							return
						}
						owners := 0
						for _, p := range pkgs {
							if _, ok := p.InstalledFiles[sharedPath]; ok {
								owners++
							}
						}
						if owners != 1 {
							t.Errorf("snapshot listed %s in %d rows, want 1 atomic owner across transfer", sharedPath, owners)
							return
						}
					}
				}
			}()
		}

		writerDone := make(chan error, 1)
		go func() {
			for i := range 40 {
				var addPkg, fromPkg, sum string
				if i%2 == 0 {
					addPkg, fromPkg, sum = "pkgB", "pkgA", "sum-b"
				} else {
					addPkg, fromPkg, sum = "pkgA", "pkgB", "sum-a"
				}
				err := db.AddPkgWithTransfers(client.PackageState{
					PackageSpec: &goolib.PkgSpec{Name: addPkg, Arch: "noarch", Version: "1.0.0"},
					InstalledFiles: map[string]string{
						sharedPath: "sum-" + sum,
					},
					InstallDate: 1700000100,
				}, []FileTransfer{
					{Name: fromPkg, Arch: "noarch", Files: []string{sharedPath}},
				})
				if err != nil {
					writerDone <- err
					return
				}
			}
			writerDone <- nil
		}()

		select {
		case err := <-writerDone:
			close(stop)
			readerWG.Wait()
			if err != nil {
				t.Fatalf("AddPkgWithTransfers() under concurrent FetchPkgs failed: %v", err)
			}
		case <-ctx.Done():
			t.Fatal("timed out waiting for AddPkgWithTransfers (possible connection pool self-deadlock on g.db)")
		}
	})

	t.Run("default_pool_concurrent_reader_snapshot_consistency", func(t *testing.T) {
		t.Parallel()
		db, _ := openTestDB(t)
		sharedPath := `C:\Shared\pool_shared.dll`
		if err := db.AddPkg(client.PackageState{
			PackageSpec:    &goolib.PkgSpec{Name: "pkgA", Arch: "noarch", Version: "1.0.0"},
			InstalledFiles: map[string]string{sharedPath: "sum-a"},
			InstallDate:    1700000000,
		}); err != nil {
			t.Fatalf("AddPkg(pkgA): %v", err)
		}

		stop := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					pkgs, err := db.FetchPkgs("")
					if err != nil {
						if strings.Contains(err.Error(), "SQLITE_BUSY") {
							continue
						}
						t.Errorf("unexpected FetchPkgs error: %v", err)
						return
					}
					owners := 0
					for _, p := range pkgs {
						if _, ok := p.InstalledFiles[sharedPath]; ok {
							owners++
						}
					}
					if owners != 1 {
						t.Errorf("default-pool snapshot listed %s in %d rows, want 1", sharedPath, owners)
						return
					}
					time.Sleep(time.Millisecond)
				}
			}
		}()

		for i := range 25 {
			var addPkg, fromPkg string
			if i%2 == 0 {
				addPkg, fromPkg = "pkgB", "pkgA"
			} else {
				addPkg, fromPkg = "pkgA", "pkgB"
			}
			for {
				err := db.AddPkgWithTransfers(client.PackageState{
					PackageSpec:    &goolib.PkgSpec{Name: addPkg, Arch: "noarch", Version: "1.0.0"},
					InstalledFiles: map[string]string{sharedPath: "sum-" + addPkg},
					InstallDate:    1700000100,
				}, []FileTransfer{
					{Name: fromPkg, Arch: "noarch", Files: []string{sharedPath}},
				})
				if err != nil && strings.Contains(err.Error(), "SQLITE_BUSY") {
					time.Sleep(time.Millisecond)
					continue
				}
				if err != nil {
					close(stop)
					wg.Wait()
					t.Fatalf("AddPkgWithTransfers() unexpected error: %v", err)
				}
				break
			}
		}
		close(stop)
		wg.Wait()
	})
}
