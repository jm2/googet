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
	"flag"
	"io"
	"strings"
	"testing"

	"github.com/google/googet/v2/testutil"
)

// TestUpdateCmdFollowReplacesFlag checks that googet update registers
// -follow_replaces as a boolean flag defaulting to false, documents it in its
// usage line, and rejects values that aren't booleans.
func TestUpdateCmdFollowReplacesFlag(t *testing.T) {
	var cmd updateCmd
	if !strings.Contains(cmd.Usage(), "[-follow_replaces]") {
		t.Errorf("Usage() = %q, want [-follow_replaces]", cmd.Usage())
	}
	fs := flag.NewFlagSet("update", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	cmd.SetFlags(fs)
	f := fs.Lookup("follow_replaces")
	if f == nil {
		t.Fatal("updateCmd did not register -follow_replaces")
	}
	if f.DefValue != "false" {
		t.Errorf("-follow_replaces default = %q, want false", f.DefValue)
	}
	for _, bad := range []string{"-follow_replaces=", "-follow_replaces=maybe", "-follow_replaces= true"} {
		if _, err := testutil.RunCmd(t, &updateCmd{}, bad); err == nil {
			t.Errorf("RunCmd(%q) succeeded, want parse error", bad)
		}
	}
}
