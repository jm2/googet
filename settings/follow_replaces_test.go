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

package settings_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/googet/v2/settings"
	"github.com/google/googet/v2/testutil"
)

// TestFollowReplacesParsingAndReset checks that followreplaces in
// googet.conf sets settings.FollowReplaces, that an absent, false or invalid
// value leaves it false, and that every read starts from false again.
func TestFollowReplacesParsingAndReset(t *testing.T) {
	t.Cleanup(settings.Snapshot())
	for _, tc := range []struct {
		name    string
		conf    string
		want    bool
		wantErr bool
	}{
		{name: "true", conf: "followreplaces: true\n", want: true},
		{name: "false", conf: "followreplaces: false\n"},
		{name: "absent", conf: ""},
		{name: "invalid", conf: "followreplaces: maybe\n", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs := testutil.CaptureLogs(t)
			// A previous read that turned it on must not leak into this one.
			settings.FollowReplaces = true
			rootDir := t.TempDir()
			if err := os.WriteFile(filepath.Join(rootDir, "googet.conf"), []byte("archs: [noarch]\n"+tc.conf), 0644); err != nil {
				t.Fatal(err)
			}
			settings.Initialize(rootDir, false)
			if settings.FollowReplaces != tc.want {
				t.Errorf("settings.FollowReplaces = %t, want %t", settings.FollowReplaces, tc.want)
			}
			if got := strings.Contains(logs.String(), "ERROR"); got != tc.wantErr {
				t.Errorf("error logged = %t, want %t; logs:\n%s", got, tc.wantErr, logs.String())
			}
		})
	}

	t.Run("missing conf resets to false", func(t *testing.T) {
		settings.FollowReplaces = true
		settings.Initialize(t.TempDir(), false)
		if settings.FollowReplaces {
			t.Error("after missing conf, settings.FollowReplaces = true, want false")
		}
	})
}

// TestSnapshotRestoresFollowReplaces checks that the function Snapshot
// returns restores settings.FollowReplaces in both directions.
func TestSnapshotRestoresFollowReplaces(t *testing.T) {
	t.Cleanup(settings.Snapshot())
	for _, orig := range []bool{false, true} {
		settings.FollowReplaces = orig
		restore := settings.Snapshot()
		settings.FollowReplaces = !orig
		restore()
		if settings.FollowReplaces != orig {
			t.Errorf("after restore, settings.FollowReplaces = %t, want %t", settings.FollowReplaces, orig)
		}
	}
}
