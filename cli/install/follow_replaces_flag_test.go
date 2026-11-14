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
	"flag"
	"io"
	"strings"
	"testing"
)

// TestInstallCmdHasNoFollowReplacesFlag checks that -follow_replaces stays
// specific to googet update: googet install neither registers nor documents
// it.
func TestInstallCmdHasNoFollowReplacesFlag(t *testing.T) {
	var cmd installCmd
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	cmd.SetFlags(fs)
	if fs.Lookup("follow_replaces") != nil {
		t.Error("installCmd registered -follow_replaces, want it only on googet update")
	}
	if strings.Contains(cmd.Usage(), "follow_replaces") {
		t.Errorf("Usage() = %q, want no -follow_replaces", cmd.Usage())
	}
}
