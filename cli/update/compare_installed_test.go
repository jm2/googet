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

// TestCompareInstalled checks how compareInstalled classifies the best
// offered package against an installed package of version 1.0.0: an upgrade,
// a downgrade from a higher priority, or no update with an informational or
// error note.
func TestCompareInstalled(t *testing.T) {
	installed := &goolib.PkgSpec{Name: "P", Arch: "x86_64", Version: "1.0.0"}
	for _, tc := range []struct {
		name     string
		prio     priority.Value
		ver      string
		wantOp   updateOp
		wantOK   bool
		wantNote planNote
	}{
		{name: "later version", prio: priority.Default, ver: "2.0.0", wantOp: opUpgrade, wantOK: true},
		{name: "later version at a lower priority", prio: 400, ver: "2.0.0", wantNote: planNote{level: noteInfo, text: "P.x86_64 - highest priority version already installed"}},
		{name: "earlier version at a higher priority", prio: priority.Canary, ver: "0.5.0", wantOp: opDowngrade, wantOK: true},
		{name: "earlier version at the default priority", prio: priority.Default, ver: "0.5.0", wantNote: planNote{level: noteInfo, text: "P.x86_64 - highest priority version already installed"}},
		{name: "same version at the default priority", prio: priority.Default, ver: "1.0.0", wantNote: planNote{level: noteInfo, text: "P.x86_64 - highest priority version already installed"}},
		{name: "same version at a higher priority", prio: priority.Canary, ver: "1.0.0", wantNote: planNote{level: noteInfo, text: "P.x86_64 - same version installed"}},
	} {
		best := repocmp.Candidate{Spec: &goolib.PkgSpec{Name: "P", Arch: "x86_64", Version: tc.ver}, Repo: "stable", Priority: tc.prio}
		op, ok, note := compareInstalled(best, installed)
		if op != tc.wantOp || ok != tc.wantOK || note != tc.wantNote {
			t.Errorf("%s: compareInstalled = (%v, %t, %+v), want (%v, %t, %+v)", tc.name, op, ok, note, tc.wantOp, tc.wantOK, tc.wantNote)
		}
	}
	for _, tc := range []struct {
		name string
		prio priority.Value
	}{
		{name: "unparseable version at the default priority", prio: priority.Default},
		{name: "unparseable version at a higher priority", prio: priority.Canary},
	} {
		best := repocmp.Candidate{Spec: &goolib.PkgSpec{Name: "P", Arch: "x86_64", Version: "bad"}, Repo: "stable", Priority: tc.prio}
		if _, ok, note := compareInstalled(best, installed); ok || note.level != noteError || note.text == "" {
			t.Errorf("%s: compareInstalled = (%t, %+v), want no update and an error note", tc.name, ok, note)
		}
	}
}
