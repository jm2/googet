/*
Copyright 2016 Google Inc. All Rights Reserved.
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

package main

import (
	"archive/tar"
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io/ioutil"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/google/googet/v2/goolib"
	"github.com/google/googet/v2/oswrap"
)

func TestPathMatch(t *testing.T) {
	tests := []struct {
		pattern, path string
		result        bool
	}{
		{"/path**.file", "/path/to.file", true},
		{"/path[a-z]", "/pathb", false},
		{"/path[a-z]", "/path[a-z]", true},
		{"path/*/file", "path/to/file", true},
		{"path/*/file", "path/to/the/file", false},
		{"path/**/file", "path/to/the/file", true},
		{"^$[a(-z])%{}}\\{{\\", "^$[a(-z])%{}}\\{{\\", true},
	}

	for _, test := range tests {
		res, err := pathMatch(test.pattern, test.path)
		if err != nil {
			t.Fatalf("match %q %q: %v", test.pattern, test.path, err)
		}
		if res != test.result {
			t.Fatalf("match %q %q: expected %v got %v", test.pattern, test.path, test.result, res)
		}
	}
}

func TestMergeWalks(t *testing.T) {
	before := []pathWalk{
		{[][]string{{"path", "to", "file"}}, -1},
		// Foo/bar/baz cases cover that the outer and inner loops of the walk
		// elimination need to be travel in opposite directions.
		{[][]string{{"foo", "bar", "*.txt"}}, 2},
		{[][]string{{"foo", "baz", "*"}}, 2},
		// Ensure coverage of element removal from both end and middle.
		{[][]string{{"path", "to", "other", "file"}}, -1},
		{[][]string{{"foo", "*"}}, 1},
	}
	expected := []pathWalk{
		{[][]string{{"path", "to", "file"}}, -1},
		{
			[][]string{
				{"foo", "bar", "*.txt"},
				{"foo", "baz", "*"},
				{"foo", "*"},
			}, 1,
		},
		{[][]string{{"path", "to", "other", "file"}}, -1},
	}
	after := mergeWalks(before)

	if !reflect.DeepEqual(after, expected) {
		t.Fatalf("mergeWalks: \nexp'd  %v \nactual %v", expected, after)
	}
}

func TestMapFiles(t *testing.T) {
	tempDir, err := ioutil.TempDir("", "")
	if err != nil {
		t.Fatalf("error creating temp directory: %v", err)
	}
	defer oswrap.RemoveAll(tempDir)
	wf1 := filepath.FromSlash(path.Join(tempDir, "globme.file"))
	f, err := oswrap.Create(wf1)
	if err != nil {
		t.Fatalf("error creating test file: %v", err)
	}
	f.Close()
	f, err = oswrap.Create(path.Join(tempDir, "notme.file"))
	if err != nil {
		t.Fatalf("error creating test file: %v", err)
	}
	f.Close()
	wd := path.Join(tempDir, "globdir")
	if err := oswrap.Mkdir(wd, 0755); err != nil {
		t.Fatalf("error creating test directory: %v", err)
	}
	wf2 := filepath.FromSlash(path.Join(wd, "globmetoo.file"))
	f, err = oswrap.Create(wf2)
	if err != nil {
		t.Fatalf("error creating test file: %v", err)
	}
	f.Close()
	f, err = oswrap.Create(path.Join(tempDir, "notmeeither.file"))
	if err != nil {
		t.Fatalf("error creating test file: %v", err)
	}
	f.Close()

	ps := []goolib.PkgSources{
		{
			Include: []string{"**"},
			Exclude: []string{"notme*"},
			Target:  "foo",
			Root:    tempDir,
		},
	}
	fm, err := mapFiles(ps)
	if err != nil {
		t.Fatalf("error getting file map: %v", err)
	}
	em := fileMap{"foo": []string{wf1}, strings.Join([]string{"foo", "globdir"}, "/"): []string{wf2}}
	if !reflect.DeepEqual(fm, em) {
		t.Errorf("did not get expected package map: got %v, want %v", fm, em)
	}
}

func TestWriteFiles(t *testing.T) {
	tempDir, err := ioutil.TempDir("", "")
	if err != nil {
		t.Errorf("error creating temp directory: %v", err)
	}
	defer oswrap.RemoveAll(tempDir)
	wf := path.Join(tempDir, "test.pkg")
	f, err := oswrap.Create(wf)
	if err != nil {
		t.Errorf("error creating test package: %v", err)
	}
	f.Close()
	fm := fileMap{"foo": []string{wf}}
	ef := path.Join("foo", path.Base(wf))

	buf := new(bytes.Buffer)
	tw := tar.NewWriter(buf)
	if err := writeFiles(tw, fm); err != nil {
		t.Errorf("error writing files to zip: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Errorf("error closing zip writer: %v", err)
	}
	tr := tar.NewReader(buf)
	hdr, err := tr.Next()
	if err != nil {
		t.Error(err)
	}
	if hdr.Name != ef {
		t.Errorf("zip contains unexpected file: expect %q got %q", ef, f.Name())
	}
}

func TestPopulateVars(t *testing.T) {
	flag.String("var:TestPopulateVars1", "", "")
	flag.String("var:TestPopulateVars2", "", "")
	flag.CommandLine.Parse([]string{"-var:TestPopulateVars1", "value", "-var:TestPopulateVars2=value"})
	want := map[string]string{"TestPopulateVars1": "value", "TestPopulateVars2": "value"}

	got := populateVars()
	if !reflect.DeepEqual(want, got) {
		t.Errorf("want: %q, got: %q", want, got)
	}
}

func TestAddFlags(t *testing.T) {
	firstFlag := "var:first_var"
	secondFlag := "var:second_var"
	value := "value"

	flag.Bool("var:test2", false, "")
	flag.CommandLine.Parse([]string{"-var:test2"})

	args := []string{"-var:test2", "-" + firstFlag, value, fmt.Sprintf("--%s=%s", secondFlag, value), "var:not_a_flag", "also_not_a_flag"}
	before := flag.NFlag()
	addFlags(args)
	flag.CommandLine.Parse(args)
	after := flag.NFlag()

	want := before + 2
	if after != want {
		t.Errorf("number of flags after does not match expectation, want %d, got %d", want, after)
	}

	for _, fn := range []string{firstFlag, secondFlag} {
		got := flag.Lookup(fn).Value.String()
		if got != value {
			t.Errorf("flag %q value %q!=%q", fn, got, value)
		}
	}
}

// TestCheckRelations verifies that checkRelations writes warnings without
// failing when no fatal syntax problem is present, and returns an error
// wrapping goolib.ErrRelationSyntax when fatal problems exist.
func TestCheckRelations(t *testing.T) {
	t.Run("clean spec", func(t *testing.T) {
		var buf bytes.Buffer
		ps := &goolib.PkgSpec{
			Name:      "foo",
			Arch:      "noarch",
			Version:   "1.0.0",
			Conflicts: []string{"bar.noarch"},
			Provides:  []string{"virt=1.0"},
		}
		if err := checkRelations(ps, &buf); err != nil {
			t.Fatalf("checkRelations = %v, want nil", err)
		}
		if buf.Len() != 0 {
			t.Errorf("unexpected warning output: %q", buf.String())
		}
	})

	t.Run("warning-only spec succeeds and prints warnings", func(t *testing.T) {
		var buf bytes.Buffer
		ps := &goolib.PkgSpec{
			Name:      "foo",
			Arch:      "noarch",
			Version:   "1.0.0",
			Conflicts: []string{"bar", "bad.noarch.notaver"},
			Replaces:  []string{"bar"},
		}
		if err := checkRelations(ps, &buf); err != nil {
			t.Fatalf("checkRelations on warning-only spec = %v, want nil", err)
		}
		for _, want := range []string{
			`Warning: Conflicts entry "bad.noarch.notaver" has invalid version`,
			`Warning: Replaces and Conflicts both name "bar"`,
		} {
			if !strings.Contains(buf.String(), want) {
				t.Errorf("warnings missing %q; got:\n%s", want, buf.String())
			}
		}
	})

	t.Run("fatal spec prints warnings and returns ErrRelationSyntax", func(t *testing.T) {
		var buf bytes.Buffer
		ps := &goolib.PkgSpec{
			Name:      "foo",
			Arch:      "noarch",
			Version:   "1.0.0",
			Conflicts: []string{"bar>=1.0", "dotted.pkg"},
		}
		err := checkRelations(ps, &buf)
		if !errors.Is(err, goolib.ErrRelationSyntax) {
			t.Fatalf("checkRelations error = %v, want errors.Is(ErrRelationSyntax)", err)
		}
		if !strings.Contains(buf.String(), `Warning: Conflicts entry "dotted.pkg"`) {
			t.Errorf("warnings missing dotted.pkg warning; got:\n%s", buf.String())
		}
	})
}

// TestBuildFromSpecEnforcesRelationsBeforeBuild checks that buildFromSpec
// rejects fatal relation syntax before running build scripts, while allowing
// warning-only specs to build a .goo archive.
func TestBuildFromSpecEnforcesRelationsBeforeBuild(t *testing.T) {
	t.Run("fatal relation rejected before build script runs", func(t *testing.T) {
		dir := t.TempDir()
		specPath := filepath.Join(dir, "bad.goospec")
		// Build points to a nonexistent script; if createPackage ran, it would
		// fail executing the missing script instead of returning ErrRelationSyntax.
		specJSON := `{
  "name": "badpkg",
  "version": "1.0.0",
  "arch": "noarch",
  "conflicts": ["dep >= 2.0"],
  "build": {
    "linux": "missing-build-script.sh",
    "windows": "missing-build-script.cmd"
  }
}`
		if err := os.WriteFile(specPath, []byte(specJSON), 0644); err != nil {
			t.Fatal(err)
		}
		var errBuf bytes.Buffer
		err := buildFromSpec(specPath, dir, nil, &errBuf)
		if !errors.Is(err, goolib.ErrRelationSyntax) {
			t.Fatalf("buildFromSpec = %v, want errors.Is(ErrRelationSyntax)", err)
		}
		if _, statErr := os.Stat(filepath.Join(dir, "badpkg.noarch.1.0.0.goo")); !os.IsNotExist(statErr) {
			t.Errorf("expected no .goo archive to be written, stat err = %v", statErr)
		}
	})

	t.Run("warning-only spec builds archive and writes warning", func(t *testing.T) {
		dir := t.TempDir()
		specPath := filepath.Join(dir, "warn.goospec")
		specJSON := `{
  "name": "warnpkg",
  "version": "1.0.0",
  "arch": "noarch",
  "conflicts": ["oldpkg"],
  "replaces": ["oldpkg"]
}`
		if err := os.WriteFile(specPath, []byte(specJSON), 0644); err != nil {
			t.Fatal(err)
		}
		var errBuf bytes.Buffer
		if err := buildFromSpec(specPath, dir, nil, &errBuf); err != nil {
			t.Fatalf("buildFromSpec = %v, want nil", err)
		}
		if !strings.Contains(errBuf.String(), `Warning: Replaces and Conflicts both name "oldpkg"`) {
			t.Errorf("stderr warnings = %q, want overlap warning", errBuf.String())
		}
		if _, err := os.Stat(filepath.Join(dir, "warnpkg.noarch.1.0.0.goo")); err != nil {
			t.Errorf("built .goo archive missing: %v", err)
		}
	})
}
