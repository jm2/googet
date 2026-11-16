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

package ownership

import (
	"errors"
	"fmt"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// TestDropped verifies that Dropped compares old and keep paths under the
// index's KeyRule and preserves each dropped entry's original path and checksum.
func TestDropped(t *testing.T) {
	t.Parallel()
	ix := NewIndexWithRule(nil, RowID{}, WindowsRule)

	if got := Dropped(nil, map[string]string{`C:\a`: "1"}, ix); got != nil {
		t.Errorf("Dropped(nil, keep) = %v, want nil", got)
	}

	old := map[string]string{
		`C:\App\Foo.dll`:     "sum-foo",
		`C:\App\Sub/Bar.dll`: "sum-bar",
		`C:\App\OldOnly.dll`: "sum-old",
		`C:\App\OldDir`:      "",
	}
	keep := map[string]string{
		`c:\app\foo.dll`:     "sum-foo-v2",
		`c:/app/sub\bar.dll`: "sum-bar-v2",
	}

	got := Dropped(old, keep, ix)
	want := map[string]string{
		`C:\App\OldOnly.dll`: "sum-old",
		`C:\App\OldDir`:      "",
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("Dropped() mismatch (-want +got):\n%s", diff)
	}

	// When all entries match by folded key, Dropped returns nil.
	if gotNone := Dropped(map[string]string{`C:\App\Foo.dll`: "sum1"}, map[string]string{`C:\App\foo.dll`: "sum2"}, ix); gotNone != nil {
		t.Errorf("Dropped(case-only rename) = %v, want nil", gotNone)
	}
}

// TestExecuteUpgradeLogging verifies that Execute with op "upgrade" logs old
// file cleanups at Info, file removal failures at Error, and directory removal
// failures at Info without "Removing" lines for directories.
func TestExecuteUpgradeLogging(t *testing.T) {
	t.Parallel()
	plan := DeletePlan{
		Files: []string{`C:\App\old.dll`},
		Dirs:  []string{`C:\App\olddir`},
		Kept: []Kept{
			{
				Path:   `C:\App\shared.dll`,
				Owners: []RowID{{Name: "other", Arch: "noarch"}},
			},
		},
	}

	var infoLines, errLines []string
	rm := func(p string) (string, error) {
		if p == `C:\App\old.dll` {
			return "", errors.New("access denied")
		}
		return "", errors.New("dir not empty")
	}
	lg := Logger{
		Infof: func(format string, args ...any) {
			infoLines = append(infoLines, fmt.Sprintf(format, args...))
		},
		Errorf: func(format string, args ...any) {
			errLines = append(errLines, fmt.Sprintf(format, args...))
		},
	}

	Execute(plan, "upgrade", "app.noarch.1", rm, lg)

	wantInfo := []string{
		`file_kept op=upgrade pkg=app.noarch.1 path="C:\\App\\shared.dll" owners=other.noarch`,
		`Cleaning up old file "C:\\App\\old.dll"`,
		"dir not empty",
	}
	if diff := cmp.Diff(wantInfo, infoLines); diff != "" {
		t.Errorf("Execute(upgrade) info logs (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff([]string{"access denied"}, errLines); diff != "" {
		t.Errorf("Execute(upgrade) error logs (-want +got):\n%s", diff)
	}
}
