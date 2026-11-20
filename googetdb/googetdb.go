/*
Copyright 2025 Google Inc. All Rights Reserved.
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

// Package googetdb manages the googet state sqlite database.
package googetdb

import (
	"bytes"
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/google/googet/v2/client"
	"github.com/google/googet/v2/goolib"
	"github.com/google/googet/v2/settings"
	"github.com/google/googet/v2/system"
	"github.com/google/logger"

	_ "modernc.org/sqlite" // Import the SQLite driver (unnamed)
)

const (
	stateQuery = `INSERT or REPLACE INTO InstalledPackages (pkg_name, pkg_ver, pkg_arch, pkg_json) VALUES (
		?, ?, ?, ?)`

	// installedFilesKey is the JSON object key for
	// client.PackageState.InstalledFiles. The field carries no json struct tag,
	// so encoding/json uses the Go field name directly; TestInstalledFilesKey
	// pins this coupling.
	installedFilesKey = "InstalledFiles"
)

// nowFunc returns the current time; can be overridden in tests.
var nowFunc = time.Now

// GooDB is the googet database.
type GooDB struct {
	db *sql.DB
}

// NewDB returns the googet DB object
func NewDB(dbFile string) (*GooDB, error) {
	var gdb GooDB
	var err error
	if _, err := os.Stat(dbFile); errors.Is(err, os.ErrNotExist) {
		gdb.db, err = createDB(dbFile)
		if err != nil {
			return nil, err
		}
		return &gdb, nil
	}
	gdb.db, err = sql.Open("sqlite", dbFile)
	if err != nil {
		return nil, err
	}
	return &gdb, nil
}

// Close will close the db connection
func (g *GooDB) Close() error {
	return g.db.Close()
}

// Create db creates the initial googet database
func createDB(dbFile string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", dbFile)
	if err != nil {
		return nil, err
	}

	createDBQuery := `BEGIN;
	CREATE TABLE IF NOT EXISTS InstalledPackages (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			pkg_name TEXT NOT NULL,
			pkg_arch TEXT NOT NULL,
			pkg_ver TEXT NOT NULL,
			pkg_json BLOB NOT NULL,
			UNIQUE(pkg_name, pkg_arch) ON CONFLICT REPLACE
		) STRICT;
	COMMIT;
		`

	_, err = db.ExecContext(context.Background(), createDBQuery)
	if err != nil {
		return nil, err
	}

	return db, nil
}

// WriteStateToDB writes new or partial state to the db.
func (g *GooDB) WriteStateToDB(gooState client.GooGetState) error {
	for _, pkgState := range gooState {
		if pkgState.PackageSpec == nil {
			continue
		}
		err := g.AddPkg(pkgState)
		if err != nil {
			return err
		}
	}
	return nil
}

// FileTransfer names InstalledFiles entries of the installed row Name.Arch,
// using the exact path strings that row stores, whose ownership transfers to
// the package being added.
type FileTransfer struct {
	Name  string
	Arch  string
	Files []string
}

// AddPkgWithTransfers adds pkgState to the GooGet database like AddPkg and, in
// the same SQLite transaction, removes each transfer's Files from the named
// owner row's InstalledFiles map.
//
// The call is all-or-nothing: nothing is written unless the single transaction
// commits, and all owner updates run before the new row is inserted. It returns
// an error before starting a transaction if any transfer targets pkgState's own
// name and architecture. Multiple transfers targeting the same (Name, Arch) row
// are merged. Owner rows that do not exist, rows whose InstalledFiles field is
// absent or null, and paths an owner row does not list are skipped without
// error. Every other JSON field on an edited owner row (including fields
// unknown to this binary) as well as its SQLite id and pkg_ver columns are
// preserved. Files must use the owner row's exact map keys.
func (g *GooDB) AddPkgWithTransfers(pkgState client.PackageState, transfers []FileTransfer) error {
	spec := pkgState.PackageSpec
	if spec == nil {
		return errors.New("cannot add package with nil PackageSpec")
	}

	merged, err := mergeTransfers(spec.Name, spec.Arch, transfers)
	if err != nil {
		return err
	}

	pkgState.InstalledApp.Name, pkgState.InstalledApp.Reg = system.AppAssociation(spec, pkgState.LocalPath)
	if pkgState.InstallDate == 0 {
		pkgState.InstallDate = nowFunc().Unix()
	}

	tx, err := g.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	for _, tr := range merged {
		if err := editInstalledFiles(tx, tr.Name, tr.Arch, tr.Files); err != nil {
			return err
		}
	}

	jsonState, err := json.Marshal(pkgState)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(context.Background(), stateQuery, spec.Name, spec.Version, spec.Arch, jsonState); err != nil {
		return err
	}
	return tx.Commit()
}

// AddPkg adds a single package to the GooGet database.
func (g *GooDB) AddPkg(pkgState client.PackageState) error {
	return g.AddPkgWithTransfers(pkgState, nil)
}

func mergeTransfers(selfName, selfArch string, transfers []FileTransfer) ([]FileTransfer, error) {
	if len(transfers) == 0 {
		return nil, nil
	}
	type rowKey struct {
		name string
		arch string
	}
	byRow := make(map[rowKey]map[string]struct{})
	for _, tr := range transfers {
		if tr.Name == selfName && tr.Arch == selfArch {
			return nil, fmt.Errorf("cannot transfer files from %s.%s to itself", selfName, selfArch)
		}
		if len(tr.Files) == 0 {
			continue
		}
		k := rowKey{name: tr.Name, arch: tr.Arch}
		set := byRow[k]
		if set == nil {
			set = make(map[string]struct{}, len(tr.Files))
			byRow[k] = set
		}
		for _, f := range tr.Files {
			set[f] = struct{}{}
		}
	}
	if len(byRow) == 0 {
		return nil, nil
	}
	keys := make([]rowKey, 0, len(byRow))
	for k := range byRow {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, func(a, b rowKey) int {
		return cmp.Or(strings.Compare(a.name, b.name), strings.Compare(a.arch, b.arch))
	})
	out := make([]FileTransfer, 0, len(keys))
	for _, k := range keys {
		files := make([]string, 0, len(byRow[k]))
		for f := range byRow[k] {
			files = append(files, f)
		}
		slices.Sort(files)
		out = append(out, FileTransfer{Name: k.name, Arch: k.arch, Files: files})
	}
	return out, nil
}

func editInstalledFiles(tx *sql.Tx, name, arch string, files []string) error {
	ctx := context.Background()
	var raw []byte
	err := tx.QueryRowContext(ctx, `SELECT pkg_json FROM InstalledPackages WHERE pkg_name = ? AND pkg_arch = ?`, name, arch).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}

	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return err
	}
	if obj == nil {
		return fmt.Errorf("row %s.%s pkg_json is not a JSON object", name, arch)
	}

	rawFiles, ok := obj[installedFilesKey]
	if !ok || bytes.Equal(bytes.TrimSpace(rawFiles), []byte("null")) {
		return nil
	}

	var installed map[string]string
	if err := json.Unmarshal(rawFiles, &installed); err != nil {
		return err
	}
	if installed == nil {
		return nil
	}

	changed := false
	for _, f := range files {
		if _, exists := installed[f]; exists {
			delete(installed, f)
			changed = true
		}
	}
	if !changed {
		return nil
	}

	updatedFiles, err := json.Marshal(installed)
	if err != nil {
		return err
	}
	obj[installedFilesKey] = updatedFiles
	updatedRow, err := json.Marshal(obj)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE InstalledPackages SET pkg_json = ? WHERE pkg_name = ? AND pkg_arch = ?`, updatedRow, name, arch)
	return err
}

// RemovePkg removes a single package from the googet database
func (g *GooDB) RemovePkg(pkgName, arch string) error {
	tx, err := g.db.Begin()
	if err != nil {
		return err
	}
	query := `DELETE FROM InstalledPackages where pkg_name = ? and pkg_arch = ?`
	if _, err := tx.ExecContext(context.Background(), query, pkgName, arch); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

// FetchPkg exports a single package from the googet database.
//
// Keep row filtering, (pkg_arch, pkg_ver) ordering, and the last-row fallback
// in sync with snapshot.Pick in internal/snapshot (covered by
// TestPickMatchesFetchPkg).
func (g *GooDB) FetchPkg(pi goolib.PackageInfo) (client.PackageState, error) {
	selectSpecQuery :=
		`SELECT 
			pkg_json
		FROM
			InstalledPackages
		WHERE pkg_name = ?
		ORDER BY pkg_arch, pkg_ver
		`
	spec, err := g.db.Query(selectSpecQuery, pi.Name)
	if err != nil {
		return client.PackageState{}, err
	}
	defer spec.Close()

	var lastPkgState client.PackageState
	for spec.Next() {
		var jsonState string
		if err := spec.Scan(&jsonState); err != nil {
			return client.PackageState{}, err
		}
		var pkgState client.PackageState
		if err := json.Unmarshal([]byte(jsonState), &pkgState); err != nil {
			return client.PackageState{}, err
		}
		if pkgState.Match(pi) {
			return pkgState, nil
		}
		lastPkgState = pkgState
	}
	return lastPkgState, nil
}

// FetchPkgs exports all of the current packages in the googet database
func (g *GooDB) FetchPkgs(pkgName string) (client.GooGetState, error) {
	var state client.GooGetState
	query := `SELECT pkg_json FROM InstalledPackages ORDER BY pkg_name, pkg_arch`
	var args []any
	if pkgName != "" {
		query = `SELECT pkg_json FROM InstalledPackages WHERE pkg_name LIKE ? ORDER BY pkg_name, pkg_arch`
		args = []any{pkgName}
	}
	rows, err := g.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var jsonState string
		if err := rows.Scan(&jsonState); err != nil {
			return nil, err
		}
		var pkgState client.PackageState
		if err := json.Unmarshal([]byte(jsonState), &pkgState); err != nil {
			return nil, err
		}
		state = append(state, pkgState)
	}
	return state, nil
}

// InstalledLockState returns the architecture, lock status, and version of the installed package.
// If the package is not installed, it returns empty string and false.
//
// pkgName is matched with SQL LIKE, so "_" and "%" act as wildcards and ASCII
// letters match either case, and an empty pkgName matches every package. Keep
// this in sync with snapshot.LockState in internal/snapshot (covered by
// TestLockStateMatchesInstalledLockState).
func (g *GooDB) InstalledLockState(pkgName string) (installedArch string, isLocked bool, ver string, pkgFound bool, err error) {
	pkgs, err := g.FetchPkgs(pkgName)
	if err != nil {
		return "", false, "", false, err
	}
	if len(pkgs) == 0 {
		return "", false, "", false, nil
	}
	// Assume only one arch is installed, or pick the first one.
	return pkgs[0].PackageSpec.Arch, pkgs[0].PackageSpec.LockArch, pkgs[0].PackageSpec.Version, true, nil
}

// readState reads the JSON installed package state from the given path,
// retrying with a .bak extension if the first read fails.
//
// Deprecated: Use the googet.db sqlite database instead.
func readState(sf string) (client.GooGetState, error) {
	state, err := readStateFromPath(sf)
	if err != nil {
		sfNotExist := os.IsNotExist(err)
		state, err = readStateFromPath(sf + ".bak")
		if sfNotExist && os.IsNotExist(err) {
			logger.Info("No state file found, assuming no packages installed.")
			return client.GooGetState{}, nil
		}
	}
	return state, err
}

// readStateFromPath is a helper function for readState.
func readStateFromPath(sf string) (client.GooGetState, error) {
	var s client.GooGetState
	b, err := os.ReadFile(sf)
	if err != nil {
		return s, err
	}
	return s, json.Unmarshal(b, &s)
}

// Exists returns true if the database file already exists.
func Exists(dbFile string) bool {
	_, err := os.Stat(dbFile)
	return !errors.Is(err, os.ErrNotExist)
}

// CreateIfMissing creates a new database file at the given path, seeding it
// with the contents of the old JSON state file if it exists.
func CreateIfMissing(dbFile string) error {
	if Exists(dbFile) {
		return nil
	}
	fmt.Println("Creating Googet DB and converting state file...")
	db, err := NewDB(dbFile)
	if err != nil {
		return fmt.Errorf("unable to create initial db file: %v", err)
	}
	defer db.Close()
	// If state file still exists, then convert.
	state, err := readState(settings.StateFile())
	if err != nil {
		return fmt.Errorf("could not read state file: %v", err)
	}
	return db.WriteStateToDB(state)
}
