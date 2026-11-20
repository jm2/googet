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

package update

import (
	"testing"

	"github.com/google/googet/v2/goolib"
	"github.com/google/googet/v2/internal/repocmp"
	"github.com/google/googet/v2/priority"
)

// TestPlanInPlaceRecordsComparisonErrors checks that planInPlace records each
// version comparison that ranking couldn't make as an error note, worded as
// client.FindRepoLatest logs it, before the update it plans from the
// candidates it could rank.
func TestPlanInPlaceRecordsComparisonErrors(t *testing.T) {
	p := &goolib.PkgSpec{Name: "P", Arch: "x86_64", Version: "1.0.0"}
	repos := []repocmp.Repo{{URL: "stable", Priority: priority.Default, Specs: []*goolib.PkgSpec{
		{Name: "P", Arch: "x86_64", Version: "bad"},
		{Name: "P", Arch: "x86_64", Version: "2.0.0"},
	}}}
	var plan updatePlan
	planInPlace(&plan, p, nil, repos)
	if len(plan.entries) != 2 {
		t.Fatalf("planInPlace recorded %d entries, want an error note and an upgrade: %+v", len(plan.entries), plan.entries)
	}
	const wantNote = `Error comparing priority/version: Invalid character(s) found in major number "bad"`
	if n := plan.entries[0].note; n == nil || n.level != noteError || n.text != wantNote {
		t.Errorf("planInPlace entry 0 = %+v, want the error note %q", plan.entries[0].note, wantNote)
	}
	if a := plan.entries[1].action; a == nil || a.op != opUpgrade || a.to.Version != "2.0.0" || a.repo != "stable" {
		t.Errorf("planInPlace entry 1 = %+v, want the upgrade to P.x86_64.2.0.0 from stable", plan.entries[1].action)
	}
}
