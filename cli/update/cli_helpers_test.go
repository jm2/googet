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

package update

import (
	"context"
	"encoding/json"
	"flag"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/googet/v2/goolib"
	"github.com/google/googet/v2/testutil"
	"github.com/google/subcommands"
)

const dryRunLine = "Dry run mode active, exiting without updating.\n"

// runUpdate runs googet update with args and returns its exit status and
// everything it wrote to standard output.
func runUpdate(t *testing.T, args ...string) (subcommands.ExitStatus, string) {
	t.Helper()
	cmd := &updateCmd{}
	fs := flag.NewFlagSet(cmd.Name(), flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	cmd.SetFlags(fs)
	if err := fs.Parse(args); err != nil {
		t.Fatalf("Parse(%q): %v", args, err)
	}
	var status subcommands.ExitStatus
	out := captureStdout(func() {
		status = cmd.Execute(context.Background(), fs)
	})
	return status, out
}

// serveRepos serves one repository for each entry of repos, keyed by name,
// from a single HTTP server, writing the packages of each into dst when
// installed. It returns the URL of each repository, keyed by name; a
// repository's URL sorts by its name.
func serveRepos(t *testing.T, dst string, repos map[string][]goolib.PkgSpec) map[string]string {
	t.Helper()
	gooDir := t.TempDir()
	srv := testutil.ServeGoo(t, gooDir)
	t.Cleanup(srv.Close)
	urls := make(map[string]string)
	for name, pkgs := range repos {
		dir := filepath.Join(gooDir, name)
		if err := os.MkdirAll(filepath.Join(dir, "repo"), 0755); err != nil {
			t.Fatal(err)
		}
		var specs []goolib.RepoSpec
		for _, ps := range pkgs {
			specs = append(specs, testutil.GenGoo(t, dir, dst, ps))
		}
		idx, err := json.Marshal(specs)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "repo", "index"), idx, 0644); err != nil {
			t.Fatal(err)
		}
		urls[name] = srv.URL + "/" + name + "/repo"
	}
	return urls
}
