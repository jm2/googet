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

	"github.com/google/go-cmp/cmp"
	"github.com/google/googet/v2/client"
	"github.com/google/googet/v2/goolib"
)

// entryKinds describes every entry of plan, in order, with summarize's format
// for actions and skips, "diag <kind>" for diagnostics and "note <text>" for
// notes.
func entryKinds(plan updatePlan) []string {
	var out []string
	for _, e := range plan.entries {
		switch {
		case e.diag != nil:
			out = append(out, "diag "+string(e.diag.kind))
		case e.note != nil:
			out = append(out, "note "+e.note.text)
		default:
			out = append(out, summarize(updatePlan{entries: []planEntry{e}})...)
		}
	}
	return out
}

// TestSelectReplacerRecordsWhyNothingWasPicked checks the contract of
// selectReplacer when it picks nothing: it records exactly one skip that says
// why when the Provides fallback asked, the missing_provides skip instead of
// the fallback's when a name replaces p without providing it under
// followreplaces, and nothing when it was asked only because of
// followreplaces and no package replaces p at all.
func TestSelectReplacerRecordsWhyNothingWasPicked(t *testing.T) {
	p := spec("P.x86_64.1.0.0")
	provider := spec("L.x86_64.2.0.0", repl("P"), prov("P"))
	dropped := spec("L.x86_64.3.0.0", repl("P"))
	for _, tc := range []struct {
		name     string
		rm       client.RepoMap
		cfg      updateConfig
		fallback *goolib.PkgSpec
		want     []string
	}{
		{
			name:     "fallback provider whose best version dropped Provides",
			rm:       stable(provider, dropped),
			cfg:      gateCfg,
			fallback: provider,
			want:     []string{"skip P.x86_64.1.0.0 reason=plan_rejected replacer=L.x86_64.2.0.0"},
		},
		{
			name:     "fallback provider whose best version dropped Provides, following Replaces",
			rm:       stable(provider, dropped),
			cfg:      followCfg,
			fallback: provider,
			want:     []string{"skip P.x86_64.1.0.0 reason=missing_provides candidates=[L.x86_64.3.0.0]"},
		},
		{
			name: "name that replaces without providing, following Replaces",
			rm:   stable(spec("P.x86_64.2.0.0"), dropped),
			cfg:  followCfg,
			want: []string{"skip P.x86_64.1.0.0 reason=missing_provides candidates=[L.x86_64.3.0.0]"},
		},
		{
			name: "no replacing package, following Replaces",
			rm:   stable(spec("P.x86_64.2.0.0"), spec("Q.x86_64.1.0.0", prov("P"))),
			cfg:  followCfg,
		},
		{
			name: "nothing offered, following Replaces",
			rm:   stable(),
			cfg:  followCfg,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pl := newPlanner(stateOf(p), tc.rm, tc.cfg)
			if got, ok := pl.selectReplacer(p, tc.fallback); ok {
				t.Fatalf("selectReplacer = %v, want no pick", got.Spec)
			}
			if diff := cmp.Diff(tc.want, entryKinds(pl.plan)); diff != "" {
				t.Errorf("selectReplacer entries (-want +got):\n%s", diff)
			}
		})
	}
}

// TestPlanUpdatesRecordsFallbackSkipOnce checks that planUpdates reports a
// replacement the Provides fallback asked for but the rules reject exactly
// once, with followreplaces off or on, and that following Replaces records
// nothing for a package that nothing replaces before updating it in place.
func TestPlanUpdatesRecordsFallbackSkipOnce(t *testing.T) {
	p := spec("P.x86_64.1.0.0")
	provider := spec("L.x86_64.2.0.0", repl("P"), prov("P"))
	dropped := spec("L.x86_64.3.0.0", repl("P"))
	for _, tc := range []struct {
		name string
		rm   client.RepoMap
		cfg  updateConfig
		want []string
	}{
		{
			name: "fallback rejected",
			rm:   stable(provider, dropped),
			cfg:  gateCfg,
			want: []string{"skip P.x86_64.1.0.0 reason=plan_rejected replacer=L.x86_64.2.0.0"},
		},
		{
			name: "fallback rejected, following Replaces",
			rm:   stable(provider, dropped),
			cfg:  followCfg,
			want: []string{"skip P.x86_64.1.0.0 reason=missing_provides candidates=[L.x86_64.3.0.0]"},
		},
		{
			name: "nothing replaces P, following Replaces",
			rm:   stable(spec("P.x86_64.2.0.0")),
			cfg:  followCfg,
			want: []string{"Upgrade P.x86_64.1.0.0 -> P.x86_64.2.0.0 from stable"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if diff := cmp.Diff(tc.want, entryKinds(planUpdates(stateOf(p), tc.rm, tc.cfg))); diff != "" {
				t.Errorf("planUpdates entries (-want +got):\n%s", diff)
			}
		})
	}
}
