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
	"bytes"
	"encoding/json"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/googet/v2/client"
	"github.com/google/googet/v2/goolib"
)

// TestFoldWindows verifies separator normalization and per-rune upper-casing
// without lexical path cleaning or multi-rune expansion.
func TestFoldWindows(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "empty",
			in:   "",
			want: "",
		},
		{
			name: "forward_slashes_and_ascii_case",
			in:   "C:/Program Files/GooGet/foo.dll",
			want: `C:\PROGRAM FILES\GOOGET\FOO.DLL`,
		},
		{
			name: "mixed_separators",
			in:   `c:\Program Files/App\Sub/bin.exe`,
			want: `C:\PROGRAM FILES\APP\SUB\BIN.EXE`,
		},
		{
			name: "umlaut_folded_per_rune",
			in:   "C:/Äpfel/äpfel/datei.txt",
			want: `C:\ÄPFEL\ÄPFEL\DATEI.TXT`,
		},
		{
			name: "sharp_s_not_expanded_to_double_s",
			in:   "C:/straße/groß.dll",
			want: `C:\STRAßE\GROß.DLL`,
		},
		{
			name: "kelvin_sign_kept_distinct_from_ascii_k",
			in:   "C:/\u212a/k",
			want: "C:\\\u212a\\K",
		},
		{
			name: "no_path_cleaning_performed",
			in:   "C:/a//b/../c/",
			want: `C:\A\\B\..\C\`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := FoldWindows(tc.in)
			if got != tc.want {
				t.Errorf("FoldWindows(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}

	if FoldWindows("C:/straße") == FoldWindows("C:/STRASSE") {
		t.Errorf("FoldWindows collapsed sharp s into ASCII SS: %q", FoldWindows("C:/straße"))
	}
	if FoldWindows("C:/\u212a") == FoldWindows("C:/K") {
		t.Errorf("FoldWindows collapsed Kelvin sign U+212A into ASCII K: %q", FoldWindows("C:/\u212a"))
	}
}

// TestKeyHostSemantics verifies that Key cleans paths on all hosts and preserves
// case sensitivity on non-Windows hosts.
func TestKeyHostSemantics(t *testing.T) {
	t.Parallel()
	in := filepath.Join("a", "b") + string(filepath.Separator) + ".." + string(filepath.Separator) + "C"
	got := Key(in)
	want := filepath.Join("a", "C")
	if runtime.GOOS == "windows" {
		want = FoldWindows(want)
	}
	if got != want {
		t.Errorf("Key(%q) = %q, want %q", in, got, want)
	}
	if runtime.GOOS != "windows" {
		upper := filepath.Join("dir", "Foo.txt")
		lower := filepath.Join("dir", "foo.txt")
		if Key(upper) == Key(lower) {
			t.Errorf("Key(%q) == Key(%q) = %q on %s, want case-sensitive keys", upper, lower, Key(upper), runtime.GOOS)
		}
	}
}

// TestIndex verifies row filtering, owner ordering, multi-entry folding,
// defensive cloning, and PathSet membership on Index.
func TestIndex(t *testing.T) {
	t.Parallel()
	state := client.GooGetState{
		{
			PackageSpec: nil,
			InstalledFiles: map[string]string{
				`C:\Shared\file.dll`: "ignored",
			},
		},
		{
			PackageSpec: &goolib.PkgSpec{Name: "zeta", Arch: "noarch", Version: "1"},
			InstalledFiles: map[string]string{
				`C:\Shared\FILE.dll`: "sum-zeta",
				`C:\Shared\Dir`:      "",
			},
		},
		{
			PackageSpec: &goolib.PkgSpec{Name: "alpha", Arch: "x86_64", Version: "1"},
			InstalledFiles: map[string]string{
				`c:/shared/file.dll`: "sum-alpha-x64",
			},
		},
		{
			PackageSpec: &goolib.PkgSpec{Name: "alpha", Arch: "noarch", Version: "1"},
			InstalledFiles: map[string]string{
				`C:\Shared\file.dll`: "sum-alpha-noarch",
				`C:\OnlyAlpha\a.txt`: "sum-only-alpha",
			},
		},
		{
			PackageSpec: &goolib.PkgSpec{Name: "dual", Arch: "noarch", Version: "1"},
			InstalledFiles: map[string]string{
				`C:\Dual\ item.txt`: "sum-upper",
				`c:/dual/ item.txt`: "sum-lower",
			},
		},
	}

	ix := NewIndexWithRule(state, RowID{Name: "alpha", Arch: "noarch"}, WindowsRule)

	if got := ix.Owners(`C:\OnlyAlpha\a.txt`); len(got) != 0 {
		t.Errorf("Owners(skipped row file) = %v, want empty", got)
	}

	wantSharedOwners := []Owner{
		{
			Row:      RowID{Name: "alpha", Arch: "x86_64"},
			Path:     `c:/shared/file.dll`,
			Checksum: "sum-alpha-x64",
		},
		{
			Row:      RowID{Name: "zeta", Arch: "noarch"},
			Path:     `C:\Shared\FILE.dll`,
			Checksum: "sum-zeta",
		},
	}
	gotShared := ix.Owners(`C:/SHARED/file.DLL`)
	if diff := cmp.Diff(wantSharedOwners, gotShared); diff != "" {
		t.Errorf("Owners(shared) mismatch (-want +got):\n%s", diff)
	}
	if len(ix.Owners(`c:\shared\file.dll`)) == 0 {
		t.Errorf("Owners(shared lowercase) = empty, want owners")
	}

	// Mutating the slice returned by Owners must not affect the Index.
	gotShared[0].Path = "mutated"
	if diff := cmp.Diff(wantSharedOwners, ix.Owners(`C:\Shared\file.dll`)); diff != "" {
		t.Errorf("Owners(shared) after caller mutation (-want +got):\n%s", diff)
	}

	// Empty-checksum directory entries still count as owned.
	if len(ix.Owners(`c:/shared/dir`)) == 0 {
		t.Errorf("Owners(empty-checksum directory) = empty, want owner")
	}

	// Two entries from the same row that fold to one key both appear, sorted by raw path.
	wantDual := []Owner{
		{
			Row:      RowID{Name: "dual", Arch: "noarch"},
			Path:     `C:\Dual\ item.txt`,
			Checksum: "sum-upper",
		},
		{
			Row:      RowID{Name: "dual", Arch: "noarch"},
			Path:     `c:/dual/ item.txt`,
			Checksum: "sum-lower",
		},
	}
	if diff := cmp.Diff(wantDual, ix.Owners(`C:\DUAL\ ITEM.TXT`)); diff != "" {
		t.Errorf("Owners(dual same-row collision) mismatch (-want +got):\n%s", diff)
	}

	set := ix.KeySet(map[string]string{
		`C:\Keep\One.dll`: "sum1",
		`C:\Keep\Dir`:     "",
	})
	if !set.Has(`c:/keep/one.dll`) {
		t.Errorf("PathSet.Has(case/separator variant) = false, want true")
	}
	if !set.Has(`C:\KEEP\DIR`) {
		t.Errorf("PathSet.Has(directory variant) = false, want true")
	}
	if set.Has(`C:\Keep\Other.dll`) {
		t.Errorf("PathSet.Has(unlisted path) = true, want false")
	}
	if set.Add(`c:/keep/ONE.dll`) {
		t.Errorf("PathSet.Add(existing case/separator variant) = true, want false")
	}
	if !set.Add(`C:\Keep\Added.dll`) {
		t.Errorf("PathSet.Add(new path) = false, want true")
	}
	if !set.Has(`c:/keep/added.dll`) {
		t.Errorf("PathSet.Has(newly added path variant) = false, want true")
	}
	if set.Add(`c:/keep/added.dll`) {
		t.Errorf("PathSet.Add(duplicate newly added variant) = true, want false")
	}

	// Zero Skip RowID indexes all non-nil rows.
	all := NewIndexWithRule(state, RowID{}, WindowsRule)
	if len(all.Owners(`c:/onlyalpha/a.txt`)) == 0 {
		t.Errorf("NewIndexWithRule with empty skip omitted alpha.noarch")
	}

	// Nil Index and zero PathSet are safe.
	var nilIx *Index
	if nilIx.Owners("x") != nil {
		t.Errorf("nil Index reported ownership")
	}
	nilEmptySet := nilIx.KeySet(nil)
	if nilEmptySet.Has("x") {
		t.Errorf("nil Index KeySet(nil).Has(x) = true, want false")
	}
	if !nilEmptySet.Add("/tmp/a/../b") || !nilEmptySet.Has("/tmp/b") || nilEmptySet.Add("/tmp/b") {
		t.Errorf("nil Index KeySet(nil).Add did not default to Key cleaning and deduplication")
	}
	nilSeededSet := nilIx.KeySet(map[string]string{"/tmp/x/../y": "sum"})
	if !nilSeededSet.Has("/tmp/y") || nilSeededSet.Add("/tmp/y") {
		t.Errorf("nil Index KeySet(map) did not normalize initial and added keys with Key")
	}

	var zeroSet PathSet
	if zeroSet.Has("x") {
		t.Errorf("zero PathSet.Has(x) = true, want false")
	}
	if !zeroSet.Add("/tmp/a/../b") {
		t.Errorf("zero PathSet.Add(first) = false, want true")
	}
	if !zeroSet.Has("/tmp/b") {
		t.Errorf("zero PathSet.Has(cleaned equivalent) = false, want true")
	}
	if zeroSet.Add("/tmp/b") {
		t.Errorf("zero PathSet.Add(cleaned duplicate) = true, want false")
	}
	if !zeroSet.Add("/tmp/c") {
		t.Errorf("zero PathSet.Add(distinct path) = false, want true")
	}
	if emptySet := ix.KeySet(nil); emptySet.Has("x") {
		t.Errorf("empty KeySet.Has(x) = true, want false")
	}
	emptyRuleSet := ix.KeySet(nil)
	if !emptyRuleSet.Add(`C:\OnlyInSet\File.dll`) || emptyRuleSet.Add(`c:/onlyinset/file.dll`) {
		t.Errorf("empty KeySet PathSet.Add did not preserve WindowsRule folding")
	}

	// Custom KeyRule on NewIndexWithRule propagates through KeySet and Add.
	customIx := NewIndexWithRule(nil, RowID{}, KeyRule{
		Fold: func(p string) string { return filepath.Base(p) },
		Sep:  '/',
	})
	customSet := customIx.KeySet(map[string]string{"/dir1/shared.bin": "sum"})
	if !customSet.Has("/dir2/shared.bin") || customSet.Add("/dir3/shared.bin") {
		t.Errorf("custom KeyRule PathSet did not fold matching basename")
	}
	if !customSet.Add("/dir3/unique.bin") || !customSet.Has("/other/unique.bin") {
		t.Errorf("custom KeyRule PathSet.Add(unique.bin) failed to insert under custom Fold")
	}
}

// TestPackageImports verifies that internal/ownership imports only the Go
// standard library and client, keeping it free of cycles and higher-level
// GooGet packages.
func TestPackageImports(t *testing.T) {
	t.Parallel()
	const (
		ownershipPkg = "github.com/google/googet/v2/internal/ownership"
		clientPkg    = "github.com/google/googet/v2/client"
	)
	forbidden := map[string]bool{
		"github.com/google/googet/v2/install":  true,
		"github.com/google/googet/v2/remove":   true,
		"github.com/google/googet/v2/googetdb": true,
		"github.com/google/googet/v2/system":   true,
		"github.com/google/googet/v2/oswrap":   true,
	}

	cmd := exec.Command("go", "list", "-deps", "-json", ownershipPkg, clientPkg)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps -json failed: %v\n%s", err, out)
	}

	type listedPkg struct {
		ImportPath string
		Standard   bool
		Imports    []string
		Deps       []string
	}
	byPath := make(map[string]listedPkg)
	dec := json.NewDecoder(bytes.NewReader(out))
	for dec.More() {
		var p listedPkg
		if err := dec.Decode(&p); err != nil {
			t.Fatalf("decoding go list output: %v", err)
		}
		byPath[p.ImportPath] = p
	}

	own, ok := byPath[ownershipPkg]
	if !ok {
		t.Fatalf("go list output missing %s", ownershipPkg)
	}
	cli, ok := byPath[clientPkg]
	if !ok {
		t.Fatalf("go list output missing %s", clientPkg)
	}

	for _, imp := range own.Imports {
		if forbidden[imp] {
			t.Errorf("%s directly imports forbidden package %s", ownershipPkg, imp)
		}
		if imp == clientPkg {
			continue
		}
		if p, exists := byPath[imp]; !exists || !p.Standard {
			t.Errorf("%s directly imports %s, want only standard library packages and %s", ownershipPkg, imp, clientPkg)
		}
	}

	allowedTransitive := map[string]bool{clientPkg: true}
	for _, d := range cli.Deps {
		allowedTransitive[d] = true
	}
	disallowedTransitive := map[string]bool{
		"github.com/google/googet/v2/install":  true,
		"github.com/google/googet/v2/remove":   true,
		"github.com/google/googet/v2/googetdb": true,
		"github.com/google/googet/v2/system":   true,
	}
	for _, dep := range own.Deps {
		if disallowedTransitive[dep] {
			t.Errorf("%s transitively depends on forbidden package %s", ownershipPkg, dep)
		}
		if p := byPath[dep]; !p.Standard && !allowedTransitive[dep] {
			t.Errorf("%s transitively depends on %s outside %s", ownershipPkg, dep, clientPkg)
		}
	}
}
