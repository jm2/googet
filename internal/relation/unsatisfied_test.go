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

package relation

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/googet/v2/goolib"
)

func formatOrphans(orphans []Orphan) []string {
	var out []string
	for _, o := range orphans {
		var rem []string
		for _, r := range o.Removed {
			rem = append(rem, r.String())
		}
		kind := "provides"
		if o.ByName {
			kind = "name"
		}
		out = append(out, fmt.Sprintf("%s req=%s.%s.%s kind=%s removed=[%s]",
			o.Dependent, o.Requirement.Name, o.Requirement.Arch, o.Requirement.Ver, kind, strings.Join(rem, ",")))
	}
	return out
}

func TestUnsatisfied(t *testing.T) {
	spec := func(name, arch, ver string, deps map[string]string, provides ...string) *goolib.PkgSpec {
		return &goolib.PkgSpec{
			Name:            name,
			Arch:            arch,
			Version:         ver,
			PkgDependencies: deps,
			Provides:        provides,
		}
	}

	for _, tc := range []struct {
		name      string
		removing  []*goolib.PkgSpec
		remaining []*goolib.PkgSpec
		want      []string
		wantErr   bool
	}{
		{
			name:      "identity orphan sets ByName true",
			removing:  []*goolib.PkgSpec{spec("P", "noarch", "2", nil)},
			remaining: []*goolib.PkgSpec{spec("D", "noarch", "1", map[string]string{"P": "1"})},
			want:      []string{"D.noarch.1 req=P..1 kind=name removed=[P.noarch.2]"},
		},
		{
			name:      "provides-only orphan sets ByName false",
			removing:  []*goolib.PkgSpec{spec("P", "noarch", "1", nil, "virt=2")},
			remaining: []*goolib.PkgSpec{spec("Q", "noarch", "1", map[string]string{"virt": "2"})},
			want:      []string{"Q.noarch.1 req=virt..2 kind=provides removed=[P.noarch.1]"},
		},
		{
			name:     "versioned provides in remaining below minimum leaves dependent unsatisfied",
			removing: []*goolib.PkgSpec{spec("P", "noarch", "1", nil, "virt=2")},
			remaining: []*goolib.PkgSpec{
				spec("Q", "noarch", "1", map[string]string{"virt": "2"}),
				spec("R", "noarch", "1", nil, "virt=1"),
			},
			want: []string{"Q.noarch.1 req=virt..2 kind=provides removed=[P.noarch.1]"},
		},
		{
			name:      "versioned provides in removing below minimum was never satisfied",
			removing:  []*goolib.PkgSpec{spec("P", "noarch", "1", nil, "virt=1")},
			remaining: []*goolib.PkgSpec{spec("Q", "noarch", "1", map[string]string{"virt": "2"})},
			want:      nil,
		},
		{
			name:     "unversioned provides in remaining satisfies versioned requirement",
			removing: []*goolib.PkgSpec{spec("P", "noarch", "2", nil)},
			remaining: []*goolib.PkgSpec{
				spec("D", "noarch", "1", map[string]string{"P": "2"}),
				spec("R", "noarch", "1", nil, "P"),
			},
			want: nil,
		},
		{
			name:      "unversioned provides in removing orphans versioned requirement",
			removing:  []*goolib.PkgSpec{spec("P", "noarch", "1", nil, "virt")},
			remaining: []*goolib.PkgSpec{spec("Q", "noarch", "1", map[string]string{"virt": "5"})},
			want:      []string{"Q.noarch.1 req=virt..5 kind=provides removed=[P.noarch.1]"},
		},
		{
			name:      "arch-qualified requirement matched by provides ignores arch",
			removing:  []*goolib.PkgSpec{spec("P", "noarch", "1", nil, "virt=2")},
			remaining: []*goolib.PkgSpec{spec("Q", "x86_64", "1", map[string]string{"virt.x86_64": "2"})},
			want:      []string{"Q.x86_64.1 req=virt.x86_64.2 kind=provides removed=[P.noarch.1]"},
		},
		{
			name:     "arch-qualified requirement satisfied by remaining provider of another arch",
			removing: []*goolib.PkgSpec{spec("P", "x86_64", "2", nil)},
			remaining: []*goolib.PkgSpec{
				spec("D", "x86_64", "1", map[string]string{"P.x86_64": "2"}),
				spec("R", "noarch", "1", nil, "P=2"),
			},
			want: nil,
		},
		{
			name:     "second installed provider keeps dependent satisfied",
			removing: []*goolib.PkgSpec{spec("P1", "noarch", "1", nil, "virt=2")},
			remaining: []*goolib.PkgSpec{
				spec("P2", "noarch", "1", nil, "virt=2"),
				spec("Q", "noarch", "1", map[string]string{"virt": "2"}),
			},
			want: nil,
		},
		{
			name:     "remaining arch sibling keeps arch-less requirement satisfied",
			removing: []*goolib.PkgSpec{spec("P", "noarch", "2", nil)},
			remaining: []*goolib.PkgSpec{
				spec("P", "x86_64", "2", nil),
				spec("D", "noarch", "1", map[string]string{"P": "2"}),
			},
			want: nil,
		},
		{
			name:     "combined identity and provides in removing sets ByName true and lists both sorted",
			removing: []*goolib.PkgSpec{spec("Q", "noarch", "1", nil, "P=2"), spec("P", "noarch", "2", nil)},
			remaining: []*goolib.PkgSpec{
				spec("D", "noarch", "1", map[string]string{"P": "2"}),
			},
			want: []string{"D.noarch.1 req=P..2 kind=name removed=[P.noarch.2,Q.noarch.1]"},
		},
		{
			name: "nil and duplicate specs are skipped cleanly",
			removing: []*goolib.PkgSpec{
				nil,
				spec("P", "noarch", "1", nil, "virt=1"),
				spec("P", "noarch", "1", nil, "virt=1"),
			},
			remaining: []*goolib.PkgSpec{
				nil,
				spec("Q", "noarch", "1", map[string]string{"virt": "1", "virt..99": "1"}),
				spec("Q", "noarch", "1", map[string]string{"virt": "1"}),
			},
			want: []string{"Q.noarch.1 req=virt..1 kind=provides removed=[P.noarch.1]"},
		},
		{
			name:      "unparseable version in removing identity returns error",
			removing:  []*goolib.PkgSpec{spec("P", "noarch", "notaver", nil)},
			remaining: []*goolib.PkgSpec{spec("D", "noarch", "1", map[string]string{"P": "1"})},
			wantErr:   true,
		},
		{
			name:     "unparseable version in remaining identity returns error",
			removing: []*goolib.PkgSpec{spec("P", "noarch", "2", nil)},
			remaining: []*goolib.PkgSpec{
				spec("P", "x86_64", "notaver", nil),
				spec("D", "noarch", "1", map[string]string{"P": "1"}),
			},
			wantErr: true,
		},
		{
			name:      "unparseable dependency version against identity returns error",
			removing:  []*goolib.PkgSpec{spec("P", "noarch", "1", nil)},
			remaining: []*goolib.PkgSpec{spec("D", "noarch", "1", map[string]string{"P": "notaver"})},
			wantErr:   true,
		},
		{
			name:     "malformed provides version in remaining does not satisfy requirement",
			removing: []*goolib.PkgSpec{spec("P", "noarch", "1", nil, "virt=2")},
			remaining: []*goolib.PkgSpec{
				spec("R", "noarch", "1", nil, "virt=notaver"),
				spec("Q", "noarch", "1", map[string]string{"virt": "2"}),
			},
			want: []string{"Q.noarch.1 req=virt..2 kind=provides removed=[P.noarch.1]"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Unsatisfied(tc.removing, tc.remaining)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("Unsatisfied() = %v, nil; want error", formatOrphans(got))
				}
				return
			}
			if err != nil {
				t.Fatalf("Unsatisfied() unexpected error: %v", err)
			}
			if diff := cmp.Diff(tc.want, formatOrphans(got)); diff != "" {
				t.Errorf("Unsatisfied() mismatch (-want +got):\n%s", diff)
			}
		})
	}

	// Empty slices return nil without error.
	if got, err := Unsatisfied(nil, nil); err != nil || !slices.Equal(formatOrphans(got), nil) {
		t.Errorf("Unsatisfied(nil, nil) = %v, %v; want nil, nil", got, err)
	}
}

// TestSatisfied exercises Satisfied directly across identity, multi-arch,
// Provides, nil-entry, and invalid-version cases.
func TestSatisfied(t *testing.T) {
	spec := func(name, arch, ver string, provides ...string) *goolib.PkgSpec {
		return &goolib.PkgSpec{
			Name:     name,
			Arch:     arch,
			Version:  ver,
			Provides: provides,
		}
	}

	for _, tc := range []struct {
		name    string
		req     Requirement
		specs   []*goolib.PkgSpec
		want    bool
		wantErr bool
	}{
		{
			name:  "empty_specs",
			req:   ParseDependency("P", "1"),
			specs: nil,
			want:  false,
		},
		{
			name:  "nil_only_specs",
			req:   ParseDependency("P", "1"),
			specs: []*goolib.PkgSpec{nil, nil},
			want:  false,
		},
		{
			name: "multi_arch_second_arch_satisfies_archless_requirement",
			req:  ParseDependency("P", "2"),
			specs: []*goolib.PkgSpec{
				spec("P", "noarch", "1"),
				spec("P", "x86_64", "3"),
			},
			want: true,
		},
		{
			name: "multi_arch_wrong_arch_does_not_satisfy_arch_qualified_requirement",
			req:  ParseDependency("P.noarch", "2"),
			specs: []*goolib.PkgSpec{
				spec("P", "noarch", "1"),
				spec("P", "x86_64", "3"),
			},
			want: false,
		},
		{
			name: "arch_qualified_requirement_satisfied_by_provider_of_other_arch",
			req:  ParseDependency("virt.x86_64", "2"),
			specs: []*goolib.PkgSpec{
				spec("P", "noarch", "1", "virt=2"),
			},
			want: true,
		},
		{
			name: "unversioned_provide_satisfies_versioned_requirement",
			req:  ParseDependency("virt", "5"),
			specs: []*goolib.PkgSpec{
				spec("P", "noarch", "1", "virt"),
			},
			want: true,
		},
		{
			name: "malformed_provide_version_ignored_without_error",
			req:  ParseDependency("virt", "2"),
			specs: []*goolib.PkgSpec{
				spec("P", "noarch", "1", "virt=notaver"),
			},
			want: false,
		},
		{
			name: "malformed_provide_followed_by_valid_provider_succeeds",
			req:  ParseDependency("virt", "2"),
			specs: []*goolib.PkgSpec{
				spec("P1", "noarch", "1", "virt=notaver"),
				spec("P2", "noarch", "1", "virt=2"),
			},
			want: true,
		},
		{
			name: "unparseable_identity_version_returns_error",
			req:  ParseDependency("P", "1"),
			specs: []*goolib.PkgSpec{
				spec("P", "noarch", "notaver"),
			},
			wantErr: true,
		},
		{
			name: "unparseable_requirement_version_against_matching_identity_returns_error",
			req:  ParseDependency("P", "notaver"),
			specs: []*goolib.PkgSpec{
				spec("P", "noarch", "1"),
			},
			wantErr: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Satisfied(tc.req, tc.specs)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("Satisfied(%+v) = %t, nil; want error", tc.req, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("Satisfied(%+v) unexpected error: %v", tc.req, err)
			}
			if got != tc.want {
				t.Errorf("Satisfied(%+v) = %t, want %t", tc.req, got, tc.want)
			}
		})
	}
}

// TestUnsatisfiedEdgeCases verifies multi-arch identity filtering, ByName
// classification when a non-matching arch shares a requirement's name,
// in-place version/provider changes across removing and remaining, sorting
// order, and unrelated corrupt versions in remaining.
func TestUnsatisfiedEdgeCases(t *testing.T) {
	spec := func(name, arch, ver string, deps map[string]string, provides ...string) *goolib.PkgSpec {
		return &goolib.PkgSpec{
			Name:            name,
			Arch:            arch,
			Version:         ver,
			PkgDependencies: deps,
			Provides:        provides,
		}
	}

	for _, tc := range []struct {
		name      string
		removing  []*goolib.PkgSpec
		remaining []*goolib.PkgSpec
		want      []string
	}{
		{
			name: "multi_arch_removing_excludes_arch_below_minimum_from_removed_list",
			removing: []*goolib.PkgSpec{
				spec("P", "noarch", "1", nil),
				spec("P", "x86_64", "3", nil),
			},
			remaining: []*goolib.PkgSpec{
				spec("D", "noarch", "1", map[string]string{"P": "2"}),
			},
			want: []string{"D.noarch.1 req=P..2 kind=name removed=[P.x86_64.3]"},
		},
		{
			name: "wrong_arch_identity_plus_provider_in_removing_sets_ByName_false",
			removing: []*goolib.PkgSpec{
				spec("P", "noarch", "2", nil),
				spec("V", "noarch", "1", nil, "P=2"),
			},
			remaining: []*goolib.PkgSpec{
				spec("D", "x86_64", "1", map[string]string{"P.x86_64": "2"}),
			},
			want: []string{"D.x86_64.1 req=P.x86_64.2 kind=provides removed=[V.noarch.1]"},
		},
		{
			name: "in_place_upgrade_dropping_provides_reports_orphan",
			removing: []*goolib.PkgSpec{
				spec("P", "noarch", "1", nil, "virt=2"),
			},
			remaining: []*goolib.PkgSpec{
				spec("P", "noarch", "2", nil),
				spec("Q", "noarch", "1", map[string]string{"virt": "2"}),
			},
			want: []string{"Q.noarch.1 req=virt..2 kind=provides removed=[P.noarch.1]"},
		},
		{
			name: "in_place_downgrade_below_minimum_reports_ByName_orphan",
			removing: []*goolib.PkgSpec{
				spec("P", "noarch", "3", nil),
			},
			remaining: []*goolib.PkgSpec{
				spec("P", "noarch", "1", nil),
				spec("D", "noarch", "1", map[string]string{"P": "2"}),
			},
			want: []string{"D.noarch.1 req=P..2 kind=name removed=[P.noarch.3]"},
		},
		{
			name: "results_sorted_by_dependent_name_arch_then_requirement_name_arch_ver",
			removing: []*goolib.PkgSpec{
				spec("P", "x86_64", "2", nil, "bvirt=2", "avirt=2"),
				spec("P", "noarch", "2", nil),
			},
			remaining: []*goolib.PkgSpec{
				spec("B", "noarch", "1", map[string]string{"avirt": "1"}),
				spec("A", "x86_64", "1", map[string]string{"avirt": "1"}),
				spec("A", "noarch", "1", map[string]string{
					"bvirt":        "1",
					"avirt.x86_64": "1",
					"avirt":        "2",
					"P":            "2",
				}),
			},
			want: []string{
				"A.noarch.1 req=P..2 kind=name removed=[P.noarch.2,P.x86_64.2]",
				"A.noarch.1 req=avirt..2 kind=provides removed=[P.x86_64.2]",
				"A.noarch.1 req=avirt.x86_64.1 kind=provides removed=[P.x86_64.2]",
				"A.noarch.1 req=bvirt..1 kind=provides removed=[P.x86_64.2]",
				"A.x86_64.1 req=avirt..1 kind=provides removed=[P.x86_64.2]",
				"B.noarch.1 req=avirt..1 kind=provides removed=[P.x86_64.2]",
			},
		},
		{
			name: "unrelated_unparseable_version_in_remaining_is_ignored",
			removing: []*goolib.PkgSpec{
				spec("V", "noarch", "1", nil),
			},
			remaining: []*goolib.PkgSpec{
				spec("P", "noarch", "notaver", nil),
				spec("D", "noarch", "1", map[string]string{"P": "1"}),
			},
			want: nil,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Unsatisfied(tc.removing, tc.remaining)
			if err != nil {
				t.Fatalf("Unsatisfied() unexpected error: %v", err)
			}
			if diff := cmp.Diff(tc.want, formatOrphans(got)); diff != "" {
				t.Errorf("Unsatisfied() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}
