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
	"go/ast"
	"go/doc"
	"go/parser"
	"go/token"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/googet/v2/goolib"
)

// TestPlanStringVersionChangesAndConflictModes checks how Plan.String formats
// fresh installs, upgrades, downgrades, same-version reinstalls, unparseable
// overwritten versions, and reverse conflicts under warn and ignore modes.
func TestPlanStringVersionChangesAndConflictModes(t *testing.T) {
	tests := []struct {
		name      string
		installed []goolib.PkgSpec
		available []goolib.PkgSpec
		target    goolib.PackageInfo
		opts      Options
		want      string
	}{
		{
			name:      "fresh_install",
			available: []goolib.PkgSpec{{Name: "A", Arch: "noarch", Version: "1"}},
			target:    goolib.PackageInfo{Name: "A"},
			want:      "The following packages will be installed:\n  A.noarch.1\n",
		},
		{
			name:      "upgrade",
			installed: []goolib.PkgSpec{{Name: "A", Arch: "noarch", Version: "1"}},
			available: []goolib.PkgSpec{{Name: "A", Arch: "noarch", Version: "2"}},
			target:    goolib.PackageInfo{Name: "A"},
			want:      "The following packages will be installed:\n  A.noarch.2 (upgrade from 1)\n",
		},
		{
			name:      "downgrade",
			installed: []goolib.PkgSpec{{Name: "A", Arch: "noarch", Version: "2"}},
			available: []goolib.PkgSpec{{Name: "A", Arch: "noarch", Version: "1"}},
			target:    goolib.PackageInfo{Name: "A", Arch: "noarch", Ver: "1"},
			want:      "The following packages will be installed:\n  A.noarch.1 (downgrade from 2)\n",
		},
		{
			name:      "reinstall_same_version",
			installed: []goolib.PkgSpec{{Name: "A", Arch: "noarch", Version: "1"}},
			available: []goolib.PkgSpec{{Name: "A", Arch: "noarch", Version: "1"}},
			target:    goolib.PackageInfo{Name: "A", Arch: "noarch", Ver: "1"},
			want:      "The following packages will be installed:\n  A.noarch.1 (reinstall)\n",
		},
		{
			name:      "unparseable_installed_version",
			installed: []goolib.PkgSpec{{Name: "A", Arch: "noarch", Version: "not-a-semver"}},
			available: []goolib.PkgSpec{{Name: "A", Arch: "noarch", Version: "1"}},
			target:    goolib.PackageInfo{Name: "A", Arch: "noarch", Ver: "1"},
			want:      "The following packages will be installed:\n  A.noarch.1 (replacing version not-a-semver)\n",
		},
		{
			name:      "reverse_conflict_ignored_omits_warning",
			installed: []goolib.PkgSpec{{Name: "X", Arch: "noarch", Version: "1", Conflicts: []string{"A"}}},
			available: []goolib.PkgSpec{{Name: "A", Arch: "noarch", Version: "1"}},
			target:    goolib.PackageInfo{Name: "A"},
			opts:      Options{PackageConflicts: ConflictIgnore},
			want:      "The following packages will be installed:\n  A.noarch.1\n",
		},
		{
			name: "multi_step_removals_preserve_plan_order",
			installed: []goolib.PkgSpec{
				{Name: "P", Arch: "noarch", Version: "1"},
				{Name: "Q", Arch: "noarch", Version: "1"},
			},
			available: []goolib.PkgSpec{
				{Name: "A", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"B": "1"}, Replaces: []string{"Q"}},
				{Name: "B", Arch: "noarch", Version: "1", Replaces: []string{"P"}},
			},
			target: goolib.PackageInfo{Name: "A"},
			want: strings.Join([]string{
				"The following packages will be installed:",
				"  B.noarch.1",
				"  A.noarch.1",
				"The following packages will be removed:",
				"  P.noarch.1 (replaced by B.noarch.1)",
				"  Q.noarch.1 (replaced by A.noarch.1)",
				"",
			}, "\n"),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newRelFixture(t, tc.installed, tc.available)
			var repo string
			if tc.target.Ver != "" {
				for u := range f.rm {
					repo = u
				}
			}
			p, err := New(f.cache, f.db, tc.opts).Plan(tc.target, repo, f.rm, corpusArchs)
			if err != nil {
				t.Fatalf("Plan(%+v) unexpected error: %v", tc.target, err)
			}
			if diff := cmp.Diff(tc.want, p.String()); diff != "" {
				t.Errorf("Plan.String() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestListDepsDeprecatedDocAndContract checks that ListDeps carries a standard
// Deprecated doc paragraph pointing to Installer.Plan while keeping its legacy
// behavior unchanged.
func TestListDepsDeprecatedDocAndContract(t *testing.T) {
	fset := token.NewFileSet()
	astFile, err := parser.ParseFile(fset, "install.go", nil, parser.ParseComments)
	if err != nil {
		t.Fatal(err)
	}
	pkg, err := doc.NewFromFiles(fset, []*ast.File{astFile}, "github.com/google/googet/v2/install", doc.PreserveAST)
	if err != nil {
		t.Fatal(err)
	}
	var listDepsDoc string
	for _, fn := range pkg.Funcs {
		if fn.Name == "ListDeps" {
			listDepsDoc = fn.Doc
			break
		}
	}
	if !strings.Contains(listDepsDoc, "\nDeprecated:") || !strings.Contains(listDepsDoc, "Installer.Plan") {
		t.Errorf("ListDeps doc = %q, want a Deprecated paragraph naming Installer.Plan", listDepsDoc)
	}

	// ListDeps continues to return already-installed dependencies and fails
	// when a dependency is absent from every repository.
	f := newRelFixture(t,
		[]goolib.PkgSpec{{Name: "B", Arch: "noarch", Version: "1"}},
		[]goolib.PkgSpec{{Name: "A", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"B": "1"}}})
	var repo string
	for u := range f.rm {
		repo = u
	}
	got, err := ListDeps(goolib.PackageInfo{Name: "A", Arch: "noarch", Ver: "1"}, f.rm, repo, corpusArchs, f.db)
	if err == nil || !strings.Contains(err.Error(), "cannot resolve dependency") {
		t.Errorf("ListDeps with repo-absent dependency = (%v, %v), want cannot-resolve-dependency error", got, err)
	}
}
