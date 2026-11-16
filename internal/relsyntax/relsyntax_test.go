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
