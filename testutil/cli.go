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

package testutil

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"io"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/google/googet/v2/client"
	"github.com/google/googet/v2/googetdb"
	"github.com/google/googet/v2/goolib"
	"github.com/google/googet/v2/settings"
	"github.com/google/subcommands"
)

// SetupCLIRepo initializes a temporary GooGet root with googet.conf, seeds
// initialState in SQLite, and serves repoPkgs from an HTTP test server with a
// /repo/index endpoint.
func SetupCLIRepo(t *testing.T, confYAML string, initialState, repoPkgs []goolib.PkgSpec) (dstDir, repoURL string) {
	t.Helper()
	rootDir := t.TempDir()
	dstDir = t.TempDir()
	gooDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(rootDir, "cache"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(rootDir, "repos"), 0755); err != nil {
		t.Fatal(err)
	}

	t.Cleanup(settings.Snapshot())

	fullConf := "allowunsafeurl: true\narchs: [noarch, x86_64]\n" + confYAML
	if err := os.WriteFile(filepath.Join(rootDir, "googet.conf"), []byte(fullConf), 0644); err != nil {
		t.Fatal(err)
	}
	settings.Initialize(rootDir, false)

	srv := ServeGoo(t, gooDir)
	t.Cleanup(srv.Close)
	repoURL = srv.URL + "/repo"

	db, err := googetdb.NewDB(settings.DBFile())
	if err != nil {
		t.Fatal(err)
	}
	var state client.GooGetState
	for _, ps := range initialState {
		rs := GenGoo(t, gooDir, dstDir, ps)
		local := filepath.Join(rootDir, "cache", rs.Source)
		b, err := os.ReadFile(filepath.Join(gooDir, rs.Source))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(local, b, 0644); err != nil {
			t.Fatal(err)
		}
		file := filepath.Join(dstDir, ps.Name)
		if err := os.WriteFile(file, []byte(rs.PackageSpec.String()), 0644); err != nil {
			t.Fatal(err)
		}
		f, err := os.Open(file)
		if err != nil {
			t.Fatal(err)
		}
		sum := goolib.Checksum(f)
		f.Close()
		state = append(state, client.PackageState{
			SourceRepo:     repoURL,
			DownloadURL:    srv.URL + "/" + rs.Source,
			Checksum:       rs.Checksum,
			LocalPath:      local,
			PackageSpec:    rs.PackageSpec,
			InstalledFiles: map[string]string{file: sum},
		})
	}
	if err := db.WriteStateToDB(state); err != nil {
		t.Fatal(err)
	}
	db.Close()

	var specs []goolib.RepoSpec
	for _, ps := range repoPkgs {
		specs = append(specs, GenGoo(t, gooDir, dstDir, ps))
	}
	if err := os.MkdirAll(filepath.Join(gooDir, "repo"), 0755); err != nil {
		t.Fatal(err)
	}
	idx, err := json.Marshal(specs)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gooDir, "repo", "index"), idx, 0644); err != nil {
		t.Fatal(err)
	}
	return dstDir, repoURL
}

// RunCmd parses args onto cmd's FlagSet and executes cmd while discarding
// console stdout.
func RunCmd(t *testing.T, cmd subcommands.Command, args ...string) (subcommands.ExitStatus, error) {
	t.Helper()
	fs := flag.NewFlagSet(cmd.Name(), flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	cmd.SetFlags(fs)
	if err := fs.Parse(args); err != nil {
		return subcommands.ExitUsageError, err
	}
	oldStdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	status := cmd.Execute(context.Background(), fs)
	w.Close()
	os.Stdout = oldStdout
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, r); err != nil {
		t.Fatal(err)
	}
	r.Close()
	return status, nil
}

// InstalledNames returns the sorted name.arch.version identifiers of all
// packages currently recorded in settings.DBFile.
func InstalledNames(t *testing.T) []string {
	t.Helper()
	db, err := googetdb.NewDB(settings.DBFile())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	state, err := db.FetchPkgs("")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, ps := range state {
		names = append(names, ps.PackageSpec.String())
	}
	slices.Sort(names)
	return names
}
