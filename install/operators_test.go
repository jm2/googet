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
	"strings"
	"testing"

	"github.com/google/googet/v2/goolib"
	"github.com/google/googet/v2/internal/relation"
	"github.com/google/googet/v2/testutil"
)

// TestProvidesRequirementOperators checks that forward provider matching
// honors version operators through relation.ProvidedBy without emitting log
// lines on unparseable Provides versions, while preserving legacy
// client.SatisfiesProvider logging for OpMin requirements.
func TestProvidesRequirementOperators(t *testing.T) {
	for _, tc := range []struct {
		name     string
		req      relation.Requirement
		provides []string
		want     bool
		wantLog  bool
	}{
		{
			name:     "lt_unversioned_provider_matches",
			req:      relation.Requirement{Name: "foo", Op: relation.OpLT, Ver: "2.0.0"},
			provides: []string{"foo"},
			want:     true,
		},
		{
			name:     "lt_in_range_provider_matches",
			req:      relation.Requirement{Name: "foo", Op: relation.OpLT, Ver: "2.0.0"},
			provides: []string{"foo=1.0.0"},
			want:     true,
		},
		{
			name:     "lt_at_bound_provider_misses",
			req:      relation.Requirement{Name: "foo", Op: relation.OpLT, Ver: "2.0.0"},
			provides: []string{"foo=2.0.0"},
			want:     false,
		},
		{
			name:     "lt_above_bound_provider_misses",
			req:      relation.Requirement{Name: "foo", Op: relation.OpLT, Ver: "2.0.0"},
			provides: []string{"foo=3.0.0"},
			want:     false,
		},
		{
			name:     "lt_unparseable_provider_misses_silently",
			req:      relation.Requirement{Name: "foo", Op: relation.OpLT, Ver: "2.0.0"},
			provides: []string{"foo=notaver"},
			want:     false,
			wantLog:  false,
		},
		{
			name:     "lt_valid_provider_after_unparseable_matches_silently",
			req:      relation.Requirement{Name: "foo", Op: relation.OpLT, Ver: "2.0.0"},
			provides: []string{"foo=notaver", "foo=1.5.0"},
			want:     true,
			wantLog:  false,
		},
		{
			name:     "gt_at_bound_provider_misses",
			req:      relation.Requirement{Name: "foo", Op: relation.OpGT, Ver: "2.0.0"},
			provides: []string{"foo=2.0.0"},
			want:     false,
		},
		{
			name:     "gt_above_bound_provider_matches",
			req:      relation.Requirement{Name: "foo", Op: relation.OpGT, Ver: "2.0.0"},
			provides: []string{"foo=2.0.1"},
			want:     true,
		},
		{
			name:     "eq_exact_matches_and_release_suffix_misses",
			req:      relation.Requirement{Name: "foo", Op: relation.OpEQ, Ver: "2.0.0"},
			provides: []string{"foo=2.0.0@1"},
			want:     false,
		},
		{
			name:     "opmin_unparseable_provider_logs_via_client",
			req:      relation.Requirement{Name: "foo", Op: relation.OpMin, Ver: "2.0.0"},
			provides: []string{"foo=notaver"},
			want:     false,
			wantLog:  true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs := testutil.CaptureLogs(t)
			ps := &goolib.PkgSpec{Name: "provider", Arch: "noarch", Version: "1.0.0", Provides: tc.provides}
			got := providesRequirement(ps, tc.req)
			if got != tc.want {
				t.Errorf("providesRequirement(%v, %+v) = %t, want %t", tc.provides, tc.req, got, tc.want)
			}
			gotLog := logs.Len() > 0
			if gotLog != tc.wantLog {
				t.Errorf("providesRequirement(%v, %+v) logged %q (non-empty=%t), want non-empty=%t", tc.provides, tc.req, logs.String(), gotLog, tc.wantLog)
			}
		})
	}
}

// TestForwardConflictHandBuiltOperatorRequirement checks checkForwardConflict
// and recheckForwardEntries with hand-built operator requirements.
func TestForwardConflictHandBuiltOperatorRequirement(t *testing.T) {
	incoming := &goolib.PkgSpec{Name: "incoming", Arch: "noarch", Version: "1.0.0"}
	req := relation.Requirement{Name: "foo", Op: relation.OpLT, Ver: "2.0.0"}

	inRange := []*goolib.PkgSpec{{Name: "holder", Arch: "noarch", Version: "1.0.0", Provides: []string{"foo=1.0.0"}}}
	hit, _, err := checkForwardConflict(req, inRange, selfExclusion(incoming))
	if err != nil || !hit {
		t.Errorf("checkForwardConflict(foo=1.0.0) = (%t, %v), want (true, nil)", hit, err)
	}

	outOfRange := []*goolib.PkgSpec{{Name: "holder", Arch: "noarch", Version: "1.0.0", Provides: []string{"foo=3.0.0"}}}
	hit, _, err = checkForwardConflict(req, outOfRange, selfExclusion(incoming))
	if err != nil || hit {
		t.Errorf("checkForwardConflict(foo=3.0.0) = (%t, %v), want (false, nil)", hit, err)
	}

	recheckErr := recheckForwardEntries([]relation.Requirement{req}, incoming, inRange, " (still installed after replacing)")
	wantErr := "cannot install, conflict with installed package or provider: foo<2.0.0 (still installed after replacing)"
	if recheckErr == nil || recheckErr.Error() != wantErr {
		t.Errorf("recheckForwardEntries error = %v, want %q", recheckErr, wantErr)
	}
	if err := recheckForwardEntries([]relation.Requirement{req}, incoming, outOfRange, ""); err != nil {
		t.Errorf("recheckForwardEntries(outOfRange) = %v, want nil", err)
	}
	legacyA := relation.Requirement{Name: "A", Op: relation.OpMin, Ver: "3"}
	installedA := []*goolib.PkgSpec{{Name: "A", Arch: "noarch", Version: "4"}}
	legacyErr := recheckForwardEntries([]relation.Requirement{legacyA}, incoming, installedA, "")
	if legacyErr == nil || !strings.HasSuffix(legacyErr.Error(), ": A") {
		t.Errorf("recheckForwardEntries(OpMin A..3) = %v, want suffix \": A\"", legacyErr)
	}
}
