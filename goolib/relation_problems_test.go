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

package goolib

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// TestRelationProblemsClassificationAndOrder verifies fatal ErrRelationSyntax
// errors, non-fatal warnings, short-circuiting, deduplication, cross-arch
// replacement handling, and deterministic reporting order.
func TestRelationProblemsClassificationAndOrder(t *testing.T) {
	type expect struct {
		substr string
		fatal  bool
	}
	for _, tc := range []struct {
		name string
		ps   *PkgSpec
		want []expect
	}{
		{
			name: "nil spec",
			ps:   nil,
			want: nil,
		},
		{
			name: "fatal package name operators and whitespace",
			ps:   &PkgSpec{Name: "foo<2", Arch: "noarch", Version: "1"},
			want: []expect{{substr: `package name "foo<2"`, fatal: true}},
		},
		{
			name: "fatal package name with space",
			ps:   &PkgSpec{Name: "foo bar", Arch: "noarch", Version: "1"},
			want: []expect{{substr: `package name "foo bar"`, fatal: true}},
		},
		{
			name: "fatal package name with equals",
			ps:   &PkgSpec{Name: "foo=1", Arch: "noarch", Version: "1"},
			want: []expect{{substr: `package name "foo=1"`, fatal: true}},
		},
		{
			name: "fatal Conflicts operator short-circuits secondary arch and overlap warnings",
			ps: &PkgSpec{
				Name:      "pkg",
				Arch:      "noarch",
				Version:   "1",
				Conflicts: []string{"foo.x86_64>=1.0.0", "bar.noarch < 2"},
				Replaces:  []string{"foo"},
			},
			want: []expect{
				{substr: `Conflicts entry "foo.x86_64>=1.0.0"`, fatal: true},
				{substr: `Conflicts entry "bar.noarch < 2"`, fatal: true},
			},
		},
		{
			name: "fatal Replaces and PkgDependencies operators",
			ps: &PkgSpec{
				Name:            "pkg",
				Arch:            "noarch",
				Version:         "1",
				Replaces:        []string{"old=1.0"},
				PkgDependencies: map[string]string{"dep>1": "1.0.0"},
			},
			want: []expect{
				{substr: `Replaces entry "old=1.0"`, fatal: true},
				{substr: `PkgDependencies key "dep>1"`, fatal: true},
			},
		},
		{
			name: "fatal Provides untrimmed whitespace, operators, and extra equals",
			ps: &PkgSpec{
				Name:     "pkg",
				Arch:     "noarch",
				Version:  "1",
				Provides: []string{" foo=1.0", "bar =1.0", "baz=1=2", "qux<1", "quux=1>0"},
			},
			want: []expect{
				{substr: `Provides entry " foo=1.0"`, fatal: true},
				{substr: `Provides entry "bar =1.0"`, fatal: true},
				{substr: `Provides entry "baz=1=2"`, fatal: true},
				{substr: `Provides entry "qux<1"`, fatal: true},
				{substr: `Provides entry "quux=1>0"`, fatal: true},
			},
		},
		{
			name: "empty entries, trailing equals in Provides, and empty dot segments warn and short-circuit",
			ps: &PkgSpec{
				Name:            "pkg",
				Arch:            "noarch",
				Version:         "1",
				Conflicts:       []string{"", "   ", "foo.", "foo..1.0", "foo.noarch.", ".noarch"},
				Replaces:        []string{"bar..2.0", "bar.x86_64."},
				Provides:        []string{"", "=1.0", "=", "foo="},
				PkgDependencies: map[string]string{"": "1.0", "baz.": "1.0", "baz..1.0": "1.0", "baz.noarch.": "1.0"},
			},
			want: []expect{
				{substr: `Conflicts entry "" is empty`},
				{substr: `Conflicts entry "   " is empty`},
				{substr: `Conflicts entry "foo." has an empty architecture segment`},
				{substr: `Conflicts entry "foo..1.0" has an empty dot-delimited segment`},
				{substr: `Conflicts entry "foo.noarch." has an empty dot-delimited segment`},
				{substr: `Conflicts entry ".noarch" has an empty package name`},
				{substr: `Replaces entry "bar..2.0" has an empty dot-delimited segment`},
				{substr: `Replaces entry "bar.x86_64." has an empty dot-delimited segment`},
				{substr: `Provides entry "" has an empty name or version`},
				{substr: `Provides entry "=1.0" has an empty name or version`},
				{substr: `Provides entry "=" has an empty name or version`},
				{substr: `Provides entry "foo=" has an empty name or version`},
				{substr: `PkgDependencies key "" is empty`},
				{substr: `PkgDependencies key "baz." has an empty architecture segment`},
				{substr: `PkgDependencies key "baz..1.0" has an empty dot-delimited segment`},
				{substr: `PkgDependencies key "baz.noarch." has an empty dot-delimited segment`},
			},
		},
		{
			name: "unknown arch, invalid versions, deduplicated overlap, self-replace, and embedded dep version in order",
			ps: &PkgSpec{
				Name:      "selfpkg",
				Arch:      "x86_64",
				Version:   "2.0.0",
				Conflicts: []string{"dotted.name", "badver.noarch.notaver", "shared.noarch", "shared.x86_64"},
				Replaces: []string{
					"other.dotted",
					"badrep.noarch.notaver",
					"shared.noarch",
					"shared.x86_64",
					// Cross-arch replacement does not warn.
					"selfpkg.noarch",
					// Arch-less self-replacement warns.
					"selfpkg",
					// Same-arch self-replacement warns.
					"selfpkg.x86_64",
				},
				Provides: []string{"virt=notaver"},
				PkgDependencies: map[string]string{
					"zdep.noarch.1.0": "2.0",
					"adep.customarch": "1.0",
				},
			},
			want: []expect{
				{substr: `Conflicts entry "dotted.name" has unknown architecture "name"`},
				{substr: `Conflicts entry "badver.noarch.notaver" has invalid version "notaver"`},
				{substr: `Replaces entry "other.dotted" has unknown architecture "dotted"`},
				{substr: `Replaces entry "badrep.noarch.notaver" has invalid version "notaver"`},
				{substr: `Replaces and Conflicts both name "shared": googet before 3.5.0 refuses to install this package while shared is installed`},
				{substr: `Replaces entry "selfpkg" names this package itself: upgrading removes the installed version and its dependents first`},
				{substr: `Replaces entry "selfpkg.x86_64" names this package itself: upgrading removes the installed version and its dependents first`},
				{substr: `Provides entry "virt=notaver" has invalid version "notaver"`},
				{substr: `PkgDependencies key "adep.customarch" has unknown architecture "customarch"`},
				{substr: `PkgDependencies key "zdep.noarch.1.0" embeds version "1.0"`},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.ps.RelationProblems()
			if len(got) != len(tc.want) {
				var strs []string
				for _, e := range got {
					strs = append(strs, e.Error())
				}
				t.Fatalf("RelationProblems() returned %d problems %q, want %d", len(got), strs, len(tc.want))
			}
			for i, w := range tc.want {
				if !strings.Contains(got[i].Error(), w.substr) {
					t.Errorf("problem[%d] = %q, want substring %q", i, got[i].Error(), w.substr)
				}
				if isFatal := errors.Is(got[i], ErrRelationSyntax); isFatal != w.fatal {
					t.Errorf("problem[%d] (%q) errors.Is(ErrRelationSyntax) = %t, want %t", i, got[i].Error(), isFatal, w.fatal)
				}
			}
		})
	}
}

// TestRelationProblemsLegalForms checks that all valid package names and
// relation entries report zero problems.
func TestRelationProblemsLegalForms(t *testing.T) {
	ps := &PkgSpec{
		Name:    "foo",
		Arch:    "x86_64",
		Version: "1.2.3@4",
		Conflicts: []string{
			"bar",
			" bar ",
			"baz.noarch",
			" baz.x86_64.1.2.3@4 ",
			"qux.arm64.0.0.0-rc1",
			"gwindows-updater.noarch",
			"gwindowsinformation.noarch",
			"winhelm.noarch",
		},
		Replaces: []string{
			"legacy",
			"legacy2.x86_32.1.0",
			// Cross-arch replacement from noarch to x86_64 is valid.
			"foo.noarch",
		},
		Provides: []string{
			"virt",
			"virt-ver=1.0",
			"virt-rc=0.0.0-rc1",
			"virt-gs=1.2.3@4",
			"dotted.provide=2.0",
		},
		PkgDependencies: map[string]string{
			"dep1":        "1.0.0",
			" dep2 ":      "0.0.0-rc1",
			"dep3.noarch": "1.2.3@4",
			"dep4.x86_64": "2",
			"dep5.arm":    "0",
		},
	}
	if probs := ps.RelationProblems(); len(probs) != 0 {
		t.Errorf("RelationProblems() on legal spec = %v, want empty", probs)
	}
}

// TestVerifyUnchangedForRelationSyntax pins that MarshalPackageSpec,
// UnmarshalPackageSpec, and ExtractPkgSpec continue to accept historical specs
// containing relation operators or malformed relation entries without invoking
// RelationProblems.
func TestVerifyUnchangedForRelationSyntax(t *testing.T) {
	ps := &PkgSpec{
		Name:      "foo<2",
		Arch:      "noarch",
		Version:   "1.0.0@1",
		Conflicts: []string{"x.noarch>=1", "bad.noarch.notaver"},
		Replaces:  []string{"y..1"},
		Provides:  []string{"z=", "w = 1"},
	}
	if probs := ps.RelationProblems(); len(probs) == 0 {
		t.Fatal("expected RelationProblems() to report problems on test spec")
	}

	data, err := MarshalPackageSpec(ps)
	if err != nil {
		t.Fatalf("MarshalPackageSpec failed: %v", err)
	}
	got, err := UnmarshalPackageSpec(data)
	if err != nil {
		t.Fatalf("UnmarshalPackageSpec failed: %v", err)
	}
	if diff := cmp.Diff(ps, got); diff != "" {
		t.Errorf("round-trip mismatch (-want +got):\n%s", diff)
	}

	var archive bytes.Buffer
	gw := gzip.NewWriter(&archive)
	tw := tar.NewWriter(gw)
	if err := WritePackageSpec(tw, ps); err != nil {
		t.Fatalf("WritePackageSpec failed: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	extracted, err := ExtractPkgSpec(&archive)
	if err != nil {
		t.Fatalf("ExtractPkgSpec failed: %v", err)
	}
	if diff := cmp.Diff(ps, extracted); diff != "" {
		t.Errorf("ExtractPkgSpec mismatch (-want +got):\n%s", diff)
	}
}
