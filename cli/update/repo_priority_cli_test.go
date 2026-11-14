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
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/googet/v2/goolib"
	"github.com/google/googet/v2/settings"
	"github.com/google/googet/v2/testutil"
	"github.com/google/subcommands"
)

// fromRepo matches the repository an action line of the update output names.
var fromRepo = regexp.MustCompile(` from (\S+)`)

// TestUpdateCmdInstallsFromHigherPriorityRepo checks that, when two
// repositories at different priorities offer the same name, arch and
// version, update prints the repository with the higher priority, even
// though the other one sorts first, and installs the package from exactly
// the repository it printed.
func TestUpdateCmdInstallsFromHigherPriorityRepo(t *testing.T) {
	for _, tc := range []struct {
		name      string
		installed goolib.PkgSpec
		offered   goolib.PkgSpec
		wantLine  string
	}{
		{
			name:      "upgrade in place",
			installed: goolib.PkgSpec{Name: "P", Arch: "x86_64", Version: "1.0.0"},
			offered:   goolib.PkgSpec{Name: "P", Arch: "x86_64", Version: "2.0.0"},
			wantLine:  "  P.x86_64, 1.0.0 --> P.x86_64, 2.0.0 from %s\n",
		},
		{
			name:      "replacement of a retired package",
			installed: goolib.PkgSpec{Name: "P", Arch: "x86_64", Version: "1.0.0"},
			offered:   goolib.PkgSpec{Name: "R", Arch: "x86_64", Version: "2.0.0", Replaces: []string{"P"}, Provides: []string{"P"}},
			wantLine:  "  P.x86_64, 1.0.0 --> R.x86_64, 2.0.0 from %s (replaces P.x86_64.1.0.0)\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dst, _ := testutil.SetupCLIRepo(t, "", []goolib.PkgSpec{tc.installed}, nil)
			urls := serveRepos(t, dst, map[string][]goolib.PkgSpec{
				"a": {tc.offered},
				"b": {tc.offered},
			})
			repoFile := fmt.Sprintf("- name: a\n  url: %s\n- name: b\n  url: %s\n  priority: canary\n", urls["a"], urls["b"])
			if err := os.WriteFile(filepath.Join(settings.RepoDir(), "test.repo"), []byte(repoFile), 0644); err != nil {
				t.Fatal(err)
			}
			status, out := runUpdate(t)
			if status != subcommands.ExitSuccess {
				t.Fatalf("update = %v; output:\n%s", status, out)
			}
			if want := strings.Replace(tc.wantLine, "%s", urls["b"], 1); !strings.Contains(out, want) {
				t.Errorf("output missing %q:\n%s", want, out)
			}
			m := fromRepo.FindAllStringSubmatch(out, -1)
			if len(m) != 1 {
				t.Fatalf("output names %d repositories, want 1:\n%s", len(m), out)
			}
			if diff := cmp.Diff(map[string]string{tc.offered.String(): m[0][1]}, sourceRepos(t)); diff != "" {
				t.Errorf("installed packages and their repositories (-want +got):\n%s", diff)
			}
		})
	}
}
