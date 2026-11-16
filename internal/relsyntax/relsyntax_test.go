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

package relsyntax

import (
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"unicode"

	"github.com/google/go-cmp/cmp"
)

// TestOpZeroAndStrings checks that OpNone is the zero value of Op and that
// String formats valid operators and returns an empty string otherwise.
func TestOpZeroAndStrings(t *testing.T) {
	var zero Op
	if zero != OpNone {
		t.Fatalf("zero Op = %v, want OpNone (%v)", zero, OpNone)
	}
	for _, tc := range []struct {
		name string
		op   Op
		want string
	}{
		{name: "none", op: OpNone, want: ""},
		{name: "lt", op: OpLT, want: "<"},
		{name: "le", op: OpLE, want: "<="},
		{name: "eq", op: OpEQ, want: "="},
		{name: "ge", op: OpGE, want: ">="},
		{name: "gt", op: OpGT, want: ">"},
		{name: "unknown", op: Op(99), want: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.op.String(); got != tc.want {
				t.Errorf("Op(%d).String() = %q, want %q", tc.op, got, tc.want)
			}
		})
	}
}

// TestIsOpRuneExhaustive checks over the entire Unicode code point range that
// IsOpRune reports true only for '<', '>' and '='.
func TestIsOpRuneExhaustive(t *testing.T) {
	for r := rune(0); r <= unicode.MaxRune; r++ {
		got := IsOpRune(r)
		want := r == '<' || r == '>' || r == '='
		if got != want {
			t.Fatalf("IsOpRune(%U) = %t, want %t", r, got, want)
		}
	}
}

// TestStdlibOnlyImports checks that non-test files in package relsyntax import
// only the allowed standard library lexical packages.
func TestStdlibOnlyImports(t *testing.T) {
	allowed := []string{"strings", "unicode", "unicode/utf8"}
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("os.ReadDir(.) failed: %v", err)
	}
	fset := token.NewFileSet()
	foundGoFile := false
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		foundGoFile = true
		f, err := parser.ParseFile(fset, name, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parser.ParseFile(%q) failed: %v", name, err)
		}
		for _, imp := range f.Imports {
			pkg, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				t.Fatalf("strconv.Unquote(%s) failed: %v", imp.Path.Value, err)
			}
			if strings.Contains(pkg, ".") || !slices.Contains(allowed, pkg) {
				t.Errorf("%s imports %q, want subset of %v", name, pkg, allowed)
			}
		}
	}
	if !foundGoFile {
		t.Fatal("no production Go files found in internal/relsyntax")
	}
}

// TestImportersRestricted checks across the module tree that only goolib and
// internal/relation import internal/relsyntax.
func TestImportersRestricted(t *testing.T) {
	const targetImport = "github.com/google/googet/v2/internal/relsyntax"
	root := filepath.Clean("../..")
	allowedDirs := map[string]bool{
		"goolib":             true,
		"internal/relation":  true,
		"internal/relsyntax": true,
	}
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			if d.Name() == ".git" || d.Name() == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".go") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		relDir, err := filepath.Rel(root, filepath.Dir(path))
		if err != nil {
			return err
		}
		relDir = filepath.ToSlash(relDir)
		for _, imp := range f.Imports {
			pkg, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				return err
			}
			if pkg == targetImport && !allowedDirs[relDir] {
				t.Errorf("%s (in %s) imports %q; only goolib and internal/relation may import it", path, relDir, targetImport)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking module root failed: %v", err)
	}
}

// TestFormStrings checks Form.String across all known forms and an invalid value.
func TestFormStrings(t *testing.T) {
	for _, tc := range []struct {
		name string
		form Form
		want string
	}{
		{name: "legacy", form: Legacy, want: "Legacy"},
		{name: "operator", form: Operator, want: "Operator"},
		{name: "malformed", form: Malformed, want: "Malformed"},
		{name: "invalid", form: Form(99), want: "Form(invalid)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.form.String(); got != tc.want {
				t.Errorf("Form(%d).String() = %q, want %q", tc.form, got, tc.want)
			}
		})
	}
}

// TestSplit checks Legacy, Operator and Malformed classification, longest
// operator matching, ASCII and Unicode whitespace detection, version suffixes,
// and rejection of malformed forms.
func TestSplit(t *testing.T) {
	for _, tc := range []struct {
		name  string
		input string
		want  Parts
	}{
		// Legacy entries without operator runes.
		{name: "empty", input: "", want: Parts{Form: Legacy}},
		{name: "spaces_only", input: "   ", want: Parts{Form: Legacy}},
		{name: "bare_name", input: "foo", want: Parts{Form: Legacy}},
		{name: "trimmed_bare_name", input: " foo ", want: Parts{Form: Legacy}},
		{name: "name_arch", input: "foo.noarch", want: Parts{Form: Legacy}},
		{name: "name_arch_ver", input: "foo.x86_64.1.0.0@2", want: Parts{Form: Legacy}},
		{name: "multi_dot_legacy", input: "a.b.c.d", want: Parts{Form: Legacy}},
		{name: "empty_arch_legacy", input: "foo..1", want: Parts{Form: Legacy}},

		// Well-formed unspaced operators, arch-less and arch-qualified.
		{
			name:  "lt_archless",
			input: "foo<2.0.0",
			want:  Parts{Form: Operator, Name: "foo", Op: OpLT, Ver: "2.0.0"},
		},
		{
			name:  "le_longest_match",
			input: "foo<=2.0.0",
			want:  Parts{Form: Operator, Name: "foo", Op: OpLE, Ver: "2.0.0"},
		},
		{
			name:  "eq_archless",
			input: "foo=2.0.0",
			want:  Parts{Form: Operator, Name: "foo", Op: OpEQ, Ver: "2.0.0"},
		},
		{
			name:  "ge_longest_match",
			input: "foo>=2.0.0",
			want:  Parts{Form: Operator, Name: "foo", Op: OpGE, Ver: "2.0.0"},
		},
		{
			name:  "gt_archless",
			input: "foo>2.0.0",
			want:  Parts{Form: Operator, Name: "foo", Op: OpGT, Ver: "2.0.0"},
		},
		{
			name:  "arch_qualified_x86_64",
			input: "foo.x86_64<2.0.0",
			want:  Parts{Form: Operator, Name: "foo", Arch: "x86_64", Op: OpLT, Ver: "2.0.0"},
		},
		{
			name:  "arch_qualified_noarch",
			input: "foo.noarch>=1.5.0",
			want:  Parts{Form: Operator, Name: "foo", Arch: "noarch", Op: OpGE, Ver: "1.5.0"},
		},
		{
			name:  "dotted_name_splits_at_first_dot",
			input: "dotted.pkg<2.0.0",
			want:  Parts{Form: Operator, Name: "dotted", Arch: "pkg", Op: OpLT, Ver: "2.0.0"},
		},

		// Versions with dots, release suffixes, prereleases and build metadata.
		{
			name:  "version_with_release_suffix",
			input: "foo<2.5.1@3",
			want:  Parts{Form: Operator, Name: "foo", Op: OpLT, Ver: "2.5.1@3"},
		},
		{
			name:  "version_with_prerelease_and_dots",
			input: "foo<=2.0.0-rc.1@2",
			want:  Parts{Form: Operator, Name: "foo", Op: OpLE, Ver: "2.0.0-rc.1@2"},
		},
		{
			name:  "version_with_build_metadata",
			input: "foo=1.0.0+build.5",
			want:  Parts{Form: Operator, Name: "foo", Op: OpEQ, Ver: "1.0.0+build.5"},
		},
		{
			name:  "version_with_prerelease_build_and_release",
			input: "foo.x86_64>=1.2.3-alpha.1+20260101@4",
			want:  Parts{Form: Operator, Name: "foo", Arch: "x86_64", Op: OpGE, Ver: "1.2.3-alpha.1+20260101@4"},
		},

		// Whitespace handling: outer whitespace keeps Spaced false; inner whitespace around op sets Spaced true.
		{
			name:  "outer_whitespace_not_spaced",
			input: "  foo<2.0.0\t\n",
			want:  Parts{Form: Operator, Name: "foo", Op: OpLT, Ver: "2.0.0", Spaced: false},
		},
		{
			name:  "left_ascii_space",
			input: "foo <2.0.0",
			want:  Parts{Form: Operator, Name: "foo", Op: OpLT, Ver: "2.0.0", Spaced: true},
		},
		{
			name:  "right_ascii_space",
			input: "foo< 2.0.0",
			want:  Parts{Form: Operator, Name: "foo", Op: OpLT, Ver: "2.0.0", Spaced: true},
		},
		{
			name:  "both_ascii_spaces",
			input: "foo.noarch >= 2.0.0",
			want:  Parts{Form: Operator, Name: "foo", Arch: "noarch", Op: OpGE, Ver: "2.0.0", Spaced: true},
		},
		{
			name:  "unicode_nbsp_u00a0_left",
			input: "foo\u00a0<=2.0.0",
			want:  Parts{Form: Operator, Name: "foo", Op: OpLE, Ver: "2.0.0", Spaced: true},
		},
		{
			name:  "unicode_nbsp_u00a0_right",
			input: "foo<=\u00a02.0.0",
			want:  Parts{Form: Operator, Name: "foo", Op: OpLE, Ver: "2.0.0", Spaced: true},
		},
		{
			name:  "unicode_nel_u0085_left",
			input: "foo\u0085>2.0.0",
			want:  Parts{Form: Operator, Name: "foo", Op: OpGT, Ver: "2.0.0", Spaced: true},
		},
		{
			name:  "unicode_nel_u0085_right",
			input: "foo>\u00852.0.0",
			want:  Parts{Form: Operator, Name: "foo", Op: OpGT, Ver: "2.0.0", Spaced: true},
		},
		{
			name:  "unicode_em_space_both",
			input: "foo\u2003=\u20032.0.0",
			want:  Parts{Form: Operator, Name: "foo", Op: OpEQ, Ver: "2.0.0", Spaced: true},
		},

		// Malformed operator inputs.
		{name: "bare_eq", input: "=", want: Parts{Form: Malformed}},
		{name: "bare_lt", input: "<", want: Parts{Form: Malformed}},
		{name: "bare_gt", input: ">", want: Parts{Form: Malformed}},
		{name: "bare_le", input: "<=", want: Parts{Form: Malformed}},
		{name: "bare_ge", input: ">=", want: Parts{Form: Malformed}},
		{name: "missing_version_lt", input: "foo<", want: Parts{Form: Malformed}},
		{name: "missing_version_le", input: "foo<=", want: Parts{Form: Malformed}},
		{name: "missing_version_eq", input: "foo=", want: Parts{Form: Malformed}},
		{name: "missing_version_space", input: "foo<   ", want: Parts{Form: Malformed}},
		{name: "missing_lhs_lt", input: "<2", want: Parts{Form: Malformed}},
		{name: "missing_lhs_eq", input: "=2", want: Parts{Form: Malformed}},
		{name: "missing_lhs_space", input: "   <2", want: Parts{Form: Malformed}},
		{name: "double_eq", input: "foo==2", want: Parts{Form: Malformed}},
		{name: "eq_lt", input: "foo=<2", want: Parts{Form: Malformed}},
		{name: "eq_gt", input: "foo=>2", want: Parts{Form: Malformed}},
		{name: "lt_gt", input: "foo<>2", want: Parts{Form: Malformed}},
		{name: "gt_lt", input: "foo><2", want: Parts{Form: Malformed}},
		{name: "spaced_compound_le", input: "foo< =2", want: Parts{Form: Malformed}},
		{name: "spaced_compound_ge", input: "foo> =2", want: Parts{Form: Malformed}},
		{name: "two_operators_lt", input: "foo<2<3", want: Parts{Form: Malformed}},
		{name: "two_operators_eq", input: "foo=1=2", want: Parts{Form: Malformed}},
		{name: "two_dots_on_lhs", input: "a.b.c<2", want: Parts{Form: Malformed}},
		{name: "three_dots_on_lhs", input: "a.b.c.d<=2.0.0", want: Parts{Form: Malformed}},
		{name: "trailing_dot_on_lhs", input: "foo.<2", want: Parts{Form: Malformed}},
		{name: "leading_dot_on_lhs", input: ".noarch<2", want: Parts{Form: Malformed}},
		{name: "double_dot_on_lhs", input: "foo..noarch<2", want: Parts{Form: Malformed}},
		{name: "space_inside_name", input: "foo bar<2", want: Parts{Form: Malformed}},
		{name: "space_before_dot", input: "foo .noarch<2", want: Parts{Form: Malformed}},
		{name: "space_after_dot", input: "foo. noarch<2", want: Parts{Form: Malformed}},
		{name: "space_inside_version", input: "foo<2 3", want: Parts{Form: Malformed}},
		{name: "nbsp_inside_name", input: "foo\u00a0bar<2", want: Parts{Form: Malformed}},
		{name: "nel_inside_version", input: "foo<2\u00853", want: Parts{Form: Malformed}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Split(tc.input)
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("Split(%q) mismatch (-want +got):\n%s", tc.input, diff)
			}
			if got.Form == Operator {
				for _, tok := range []string{got.Name, got.Arch, got.Ver} {
					if strings.IndexFunc(tok, func(r rune) bool { return IsOpRune(r) || unicode.IsSpace(r) }) >= 0 {
						t.Errorf("Split(%q) produced forbidden rune in token %q", tc.input, tok)
					}
				}
				if strings.Contains(got.Name, ".") || strings.Contains(got.Arch, ".") {
					t.Errorf("Split(%q) left a dot in Name=%q or Arch=%q", tc.input, got.Name, got.Arch)
				}
			}
		})
	}
}

// fuzzSplitSeeds covers Legacy, unspaced Operator, spaced Operator and
// Malformed entries for seed-mode and continuous fuzzing.
var fuzzSplitSeeds = []string{
	"", " ", ".", "..", "foo", " foo ", "foo.noarch", "foo.x86_64.1.0.0@2", "a.b.c.d",
	"foo<2.0.0", "foo.noarch<=2.0.0", "foo=1.0.0+build", "foo.x86_64>=2.5.1@3", "dotted.pkg>2.0.0-rc1",
	"foo <2.0.0", "foo<= 2.0.0", "foo >= 2.0.0", "foo.noarch = 1.0.0", "foo\u00a0<\u00852.0.0",
	"=", "<", ">", "<=", ">=",
	"foo<", "foo<=", "foo=", "foo>=", "foo>",
	"<2", "<=2", "=2", ">=2", ">2",
	"foo==2", "foo=<2", "foo=>2", "foo<>2", "foo><2", "foo< =2", "foo> =2",
	"foo<2<3", "foo=1=2", "a.b.c<2", "foo.<2", ".noarch<2",
	"foo bar<2", "foo .noarch<2", "foo. noarch<2", "foo<2 3",
	"foo\u00a0bar<2", "foo<2\u00853", "   <2", "foo<   ",
}

// TestFuzzSplitSeedCoverage checks that fuzzSplitSeeds exercises Legacy,
// unspaced Operator, spaced Operator and Malformed branches.
func TestFuzzSplitSeedCoverage(t *testing.T) {
	var sawLegacy, sawUnspaced, sawSpaced, sawMalformed bool
	for _, s := range fuzzSplitSeeds {
		p := Split(s)
		switch p.Form {
		case Legacy:
			sawLegacy = true
		case Operator:
			if p.Spaced {
				sawSpaced = true
			} else {
				sawUnspaced = true
			}
		case Malformed:
			sawMalformed = true
		}
	}
	if !sawLegacy || !sawUnspaced || !sawSpaced || !sawMalformed {
		t.Fatalf("seed coverage incomplete: legacy=%t unspaced=%t spaced=%t malformed=%t",
			sawLegacy, sawUnspaced, sawSpaced, sawMalformed)
	}
}

// FuzzSplit checks that Split never panics, preserves Legacy/Malformed zero
// fields, forbids operator/whitespace/dot leakage into tokens, and round-trips
// every Operator result in both compact and spaced forms.
func FuzzSplit(f *testing.F) {
	for _, s := range fuzzSplitSeeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		p := Split(s)
		hasOp := strings.IndexFunc(s, IsOpRune) >= 0
		if !hasOp {
			if p != (Parts{Form: Legacy}) {
				t.Fatalf("Split(%q) without op rune = %+v, want Legacy", s, p)
			}
			return
		}
		switch p.Form {
		case Malformed:
			if p != (Parts{Form: Malformed}) {
				t.Fatalf("Split(%q) Malformed carried non-zero fields: %+v", s, p)
			}
		case Operator:
			if p.Op == OpNone || p.Op.String() == "" {
				t.Fatalf("Split(%q) Operator has invalid Op %v", s, p.Op)
			}
			if p.Name == "" || p.Ver == "" {
				t.Fatalf("Split(%q) Operator has empty Name=%q or Ver=%q", s, p.Name, p.Ver)
			}
			for _, tok := range []string{p.Name, p.Arch, p.Ver} {
				if strings.IndexFunc(tok, func(r rune) bool { return IsOpRune(r) || unicode.IsSpace(r) }) >= 0 {
					t.Fatalf("Split(%q) token %q contains operator or whitespace rune", s, tok)
				}
			}
			if strings.Contains(p.Name, ".") || strings.Contains(p.Arch, ".") {
				t.Fatalf("Split(%q) dot in Name=%q or Arch=%q", s, p.Name, p.Arch)
			}
			lhs := p.Name
			if p.Arch != "" {
				lhs += "." + p.Arch
			}
			compact := lhs + p.Op.String() + p.Ver
			wantCompact := p
			wantCompact.Spaced = false
			if got := Split(compact); got != wantCompact {
				t.Fatalf("Split(compact %q) = %+v, want %+v", compact, got, wantCompact)
			}
			spaced := lhs + " " + p.Op.String() + " " + p.Ver
			wantSpaced := p
			wantSpaced.Spaced = true
			if got := Split(spaced); got != wantSpaced {
				t.Fatalf("Split(spaced %q) = %+v, want %+v", spaced, got, wantSpaced)
			}
		default:
			t.Fatalf("Split(%q) with op rune returned unexpected Form %v", s, p.Form)
		}
	})
}
