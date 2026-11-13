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
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/googet/v2/goolib"
	"github.com/google/googet/v2/testutil"
	"github.com/google/subcommands"
)

// TestUpdateDryRunExitCode checks that an update dry run lists updates and
// exits successfully without planning them, even when one of them would
// fail, and changes nothing.
func TestUpdateDryRunExitCode(t *testing.T) {
	_, repoURL := testutil.SetupCLIRepo(t, "packageconflicts: error\n",
		[]goolib.PkgSpec{
			{Name: "A", Arch: "noarch", Version: "1"},
			{Name: "C", Arch: "noarch", Version: "1", Conflicts: []string{"A"}},
		},
		[]goolib.PkgSpec{{Name: "A", Arch: "noarch", Version: "2"}})
	status, err := testutil.RunCmd(t, &updateCmd{}, "-sources="+repoURL, "-dry_run")
	if err != nil || status != subcommands.ExitSuccess {
		t.Errorf("Execute = (%v, %v), want (ExitSuccess, nil)", status, err)
	}
	if diff := cmp.Diff([]string{"A.noarch.1", "C.noarch.1"}, testutil.InstalledNames(t)); diff != "" {
		t.Errorf("installed (-want +got):\n%s", diff)
	}
}
