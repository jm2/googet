//go:build windows

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
	"path/filepath"
	"strings"
	"testing"
)

// TestKeyWindowsParity verifies Key against FoldWindows(filepath.Clean(p)) on
// Windows across UNC shares, extended-length prefixes, drive-relative paths,
// trailing separators, and parent segments above a volume root.
func TestKeyWindowsParity(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "unc_backslashes_and_dotdot",
			in:   `\\srv\share\dir\..\file.txt`,
			want: `\\SRV\SHARE\FILE.TXT`,
		},
		{
			name: "unc_forward_slashes",
			in:   `//SRV/Share/file.txt`,
			want: `\\SRV\SHARE\FILE.TXT`,
		},
		{
			name: "extended_length_prefix",
			in:   `\\?\C:\Program Files\App\..\App\foo.dll`,
			want: `\\?\C:\PROGRAM FILES\APP\FOO.DLL`,
		},
		{
			name: "drive_relative_path",
			in:   `C:foo\bar\..\baz.txt`,
			want: `C:FOO\BAZ.TXT`,
		},
		{
			name: "root_relative_path",
			in:   `\foo\bar\`,
			want: `\FOO\BAR`,
		},
		{
			name: "trailing_separators",
			in:   `C:/Program Files/App///`,
			want: `C:\PROGRAM FILES\APP`,
		},
		{
			name: "dotdot_above_volume_root",
			in:   `C:\..\..\Windows\System32`,
			want: `C:\WINDOWS\SYSTEM32`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := Key(tc.in)
			if wantClean := FoldWindows(filepath.Clean(tc.in)); got != wantClean {
				t.Errorf("Key(%q) = %q, want FoldWindows(filepath.Clean) = %q", tc.in, got, wantClean)
			}
			if got != tc.want {
				t.Errorf("Key(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}

	if !strings.HasPrefix(Key(`\\srv\share\x`), `\\SRV\SHARE\`) {
		t.Errorf("Key(UNC) = %q, want leading double backslash preserved", Key(`\\srv\share\x`))
	}
	if Key(`C:foo`) == Key(`C:\foo`) {
		t.Errorf("Key(C:foo) == Key(C:\\foo) = %q, want drive-relative path distinct from absolute path", Key(`C:foo`))
	}
	if Key(`\foo`) == Key(`C:\foo`) {
		t.Errorf("Key(\\foo) == Key(C:\\foo) = %q, want root-relative path distinct from drive-qualified path", Key(`\foo`))
	}
}
