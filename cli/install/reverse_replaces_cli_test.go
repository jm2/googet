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
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/googet/v2/goolib"
	"github.com/google/googet/v2/testutil"
	"github.com/google/subcommands"
)

// TestInstallCmdReverseReplaces checks googet install of a package that an
// installed package replaces, from a repository and from a local archive,
// under each -package_conflicts mode: warn installs it with a structured
// warning, error fails with the CLI hint and leaves the database unchanged,
// and ignore installs it with an informational log line only.
func TestInstallCmdReverseReplaces(t *testing.T) {
	installed := []goolib.PkgSpec{{Name: "R", Arch: "noarch", Version: "2.0.0", Replaces: []string{"P"}, Provides: []string{"P"}}}
	incoming := goolib.PkgSpec{Name: "P", Arch: "noarch", Version: "1.5.0"}
	for _, tc := range []struct {
		mode       string
		wantStatus subcommands.ExitStatus
		wantTag    string
		wantResult string
		wantState  []string
	}{
		{mode: "warn", wantStatus: subcommands.ExitSuccess, wantTag: "WARN : ", wantResult: "warned", wantState: []string{"P.noarch.1.5.0", "R.noarch.2.0.0"}},
		{mode: "error", wantStatus: subcommands.ExitFailure, wantTag: "ERROR: ", wantResult: "failed", wantState: []string{"R.noarch.2.0.0"}},
		{mode: "ignore", wantStatus: subcommands.ExitSuccess, wantTag: "INFO : ", wantResult: "ignored", wantState: []string{"P.noarch.1.5.0", "R.noarch.2.0.0"}},
	} {
		for _, via := range []string{"repo", "file"} {
			t.Run(tc.mode+"/"+via, func(t *testing.T) {
				logs := testutil.CaptureLogs(t)
				dst, repoURL := testutil.SetupCLIRepo(t, "", installed, []goolib.PkgSpec{incoming})
				args := []string{"-sources=" + repoURL, "-package_conflicts=" + tc.mode}
				if via == "repo" {
					args = append(args, "P")
				} else {
					dir := t.TempDir()
					rs := testutil.GenGoo(t, dir, dst, incoming)
					args = append(args, filepath.Join(dir, rs.Source))
				}
				status, err := testutil.RunCmd(t, &installCmd{}, args...)
				if err != nil || status != tc.wantStatus {
					t.Fatalf("Execute(%v) = (%v, %v), want (%v, nil); logs:\n%s", args, status, err, tc.wantStatus, logs.String())
				}
				if diff := cmp.Diff(tc.wantState, testutil.InstalledNames(t)); diff != "" {
					t.Errorf("installed (-want +got):\n%s", diff)
				}
				wantLine := `package_conflict direction=reverse_replaces mode=` + tc.mode + ` result=` + tc.wantResult + ` installed=R.noarch.2.0.0 entry="P" incoming=P.noarch.1.5.0`
				var found bool
				for _, line := range strings.Split(logs.String(), "\n") {
					if strings.Contains(line, wantLine) && strings.HasPrefix(line, tc.wantTag) {
						found = true
					}
				}
				if !found {
					t.Errorf("logs missing %q at %q; logs:\n%s", wantLine, tc.wantTag, logs.String())
				}
				const hint = "installed package R.noarch.2.0.0 replaces it (entry \"P\"; package conflict mode is error) (pass -package_conflicts=warn to install anyway)"
				if got := strings.Contains(logs.String(), hint); got != (tc.mode == "error") {
					t.Errorf("logs contain %q = %t, want %t; logs:\n%s", hint, got, tc.mode == "error", logs.String())
				}
			})
		}
	}
}
