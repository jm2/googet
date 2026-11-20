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
	"github.com/google/googet/v2/goolib"
)

// TestUpdateConsumersOperatorMatrix checks replaces, selectReplacer,
// multi-arch replacement, retired-provider fallback, cycle detection and
// coexistence handling across all five version operators.
func TestUpdateConsumersOperatorMatrix(t *testing.T) {
	ops := []struct {
		sym   string
		match func(ver string) bool
	}{
		{sym: "<", match: func(v string) bool { c, _ := goolib.Compare(v, "2.0.0"); return c < 0 }},
		{sym: "<=", match: func(v string) bool { c, _ := goolib.Compare(v, "2.0.0"); return c <= 0 }},
		{sym: "=", match: func(v string) bool { c, _ := goolib.Compare(v, "2.0.0"); return c == 0 }},
		{sym: ">=", match: func(v string) bool { c, _ := goolib.Compare(v, "2.0.0"); return c >= 0 }},
		{sym: ">", match: func(v string) bool { c, _ := goolib.Compare(v, "2.0.0"); return c > 0 }},
	}
	versions := []string{"1.0.0", "2.0.0-rc1", "2.0.0", "2.0.0+build", "2.0.0@1", "3.0.0"}

	for _, op := range ops {
		for _, ver := range versions {
			wantMatch := op.match(ver)
			entry := "P" + op.sym + "2.0.0"
			archEntry := "P.x86_64" + op.sym + "2.0.0"

			t.Run(op.sym+"/"+ver, func(t *testing.T) {
				holder := spec("R.x86_64.1.0.0", repl(entry), prov("P"))
				targetX64 := spec("P.x86_64." + ver)
				if got := replaces(holder, targetX64); got != wantMatch {
					t.Errorf("replaces(%q, %s) = %t, want %t", entry, targetX64, got, wantMatch)
				}
				// Replaces never matches through target Provides.
				providerOnly := spec("Q.x86_64.1.0.0", prov("P", "P="+ver))
				if replaces(holder, providerOnly) {
					t.Errorf("replaces(%q, provider %s) = true, want false", entry, providerOnly)
				}
				// Arch-qualified Replaces never matches another arch.
				archHolder := spec("R.noarch.1.0.0", repl(archEntry), prov("P"))
				if replaces(archHolder, spec("P.noarch."+ver)) {
					t.Errorf("replaces(%q, P.noarch.%s) = true, want false", archEntry, ver)
				}
				if got := replaces(archHolder, targetX64); got != wantMatch {
					t.Errorf("replaces(%q, %s) = %t, want %t", archEntry, targetX64, got, wantMatch)
				}

				// followReplaces schedules replacement only when installed P meets the operator constraint.
				plan := planUpdates(stateOf(targetX64), stable(targetX64, holder), followCfg)
				var wantSummary []string
				if wantMatch {
					wantSummary = []string{
						"Replace P.x86_64." + ver + " -> R.x86_64.1.0.0 from stable replaced=[P.x86_64." + ver + "] removed=[P.x86_64." + ver + "]",
					}
				}
				if diff := cmp.Diff(wantSummary, summarize(plan)); diff != "" {
					t.Errorf("planUpdates(followCfg, %s, %s) (-want +got):\n%s", entry, targetX64, diff)
				}
			})
		}
	}
}

// TestUpdateMultiArchOperatorReplaces checks that an operator Replaces entry
// replaces only the installed architecture rows whose versions satisfy the
// constraint.
func TestUpdateMultiArchOperatorReplaces(t *testing.T) {
	t.Run("archless_replaces_only_in_range_arch", func(t *testing.T) {
		followCase{
			name: "archless_in_range_noarch_only",
			state: stateOf(
				spec("P.noarch.1.5.0"),
				spec("P.x86_64.2.5.0"),
			),
			rm: stable(
				spec("P.x86_64.3.0.0"),
				spec("R.noarch.1.0.0", repl("P<2.0.0"), prov("P")),
			),
			want: []string{
				"Replace P.noarch.1.5.0 -> R.noarch.1.0.0 from stable replaced=[P.noarch.1.5.0] removed=[P.noarch.1.5.0]",
				"Upgrade P.x86_64.2.5.0 -> P.x86_64.3.0.0 from stable",
			},
		}.run(t)
	})

	t.Run("arch_qualified_replaces_targets_matching_arch_only", func(t *testing.T) {
		followCase{
			name: "arch_qualified_x86_64_only",
			state: stateOf(
				spec("P.noarch.1.0.0"),
				spec("P.x86_64.1.0.0"),
			),
			rm: stable(
				spec("P.noarch.1.5.0"),
				spec("R.x86_64.1.0.0", repl("P.x86_64<2.0.0"), prov("P")),
			),
			want: []string{
				"Replace P.x86_64.1.0.0 -> R.x86_64.1.0.0 from stable replaced=[P.x86_64.1.0.0] removed=[P.x86_64.1.0.0]",
				"Upgrade P.noarch.1.0.0 -> P.noarch.1.5.0 from stable",
			},
		}.run(t)
	})
}

// TestUpdateRetiredProviderFallbackWithOperators checks the default update
// configuration when P is retired for its arch and R provides P with a bounded
// operator Replaces entry.
func TestUpdateRetiredProviderFallbackWithOperators(t *testing.T) {
	rm := stable(spec("R.x86_64.4.0.0", repl("P<2.0.0"), prov("P")))

	// In-range installed P enters the replacement path and is removed.
	inRange := planUpdates(stateOf(spec("P.x86_64.1.0.0")), rm, gateCfg)
	wantInRange := []string{
		"Replace P.x86_64.1.0.0 -> R.x86_64.4.0.0 from stable replaced=[P.x86_64.1.0.0] removed=[P.x86_64.1.0.0]",
	}
	if diff := cmp.Diff(wantInRange, summarize(inRange)); diff != "" {
		t.Errorf("retired fallback in-range (-want +got):\n%s", diff)
	}

	// Out-of-range installed P does not match Replaces, so pass 2 treats R as a plain provider upgrade without removal.
	outOfRange := planUpdates(stateOf(spec("P.x86_64.3.0.0")), rm, gateCfg)
	wantOutOfRange := []string{
		"Upgrade P.x86_64.3.0.0 -> R.x86_64.4.0.0 from stable",
	}
	if diff := cmp.Diff(wantOutOfRange, summarize(outOfRange)); diff != "" {
		t.Errorf("retired fallback out-of-range (-want +got):\n%s", diff)
	}
}

// TestUpdateCycleCheckWithOperators checks that cyclic evaluates operator
// bounds on both P's best offered spec and installed third-party packages.
func TestUpdateCycleCheckWithOperators(t *testing.T) {
	t.Run("out_of_range_reverse_replaces_is_not_cyclic", func(t *testing.T) {
		followCase{
			name:  "not_cyclic",
			state: stateOf(spec("P.x86_64.1.0.0")),
			rm: stable(
				spec("P.x86_64.1.5.0", repl("R<1.0.0")),
				spec("R.x86_64.2.0.0", repl("P<2.0.0"), prov("P")),
			),
			want: []string{
				"Replace P.x86_64.1.0.0 -> R.x86_64.2.0.0 from stable replaced=[P.x86_64.1.0.0] removed=[P.x86_64.1.0.0]",
			},
		}.run(t)
	})

	t.Run("in_range_best_spec_reverse_replaces_is_cyclic", func(t *testing.T) {
		followCase{
			name:  "best_spec_cyclic",
			state: stateOf(spec("P.x86_64.1.0.0")),
			rm: stable(
				spec("P.x86_64.1.5.0", repl("R>=2.0.0")),
				spec("R.x86_64.2.0.0", repl("P<2.0.0"), prov("P")),
			),
			want: []string{
				"skip P.x86_64.1.0.0 reason=replaces_cycle replacer=R.x86_64.2.0.0",
				"Upgrade P.x86_64.1.0.0 -> P.x86_64.1.5.0 from stable",
			},
		}.run(t)
	})

	t.Run("installed_row_in_range_replaces_is_cyclic", func(t *testing.T) {
		followCase{
			name: "installed_row_cyclic",
			state: stateOf(
				spec("P.x86_64.1.0.0"),
				spec("Q.x86_64.1.0.0", repl("R=2.0.0")),
			),
			rm: stable(
				spec("Q.x86_64.1.0.0", repl("R=2.0.0")),
				spec("R.x86_64.2.0.0", repl("P<2.0.0"), prov("P")),
			),
			want: []string{
				"skip P.x86_64.1.0.0 reason=replaces_cycle replacer=R.x86_64.2.0.0",
			},
		}.run(t)
	})
}

// TestUpdateCoexistingCurrentWithOperators checks that an installed replacing
// package at its best version suppresses P only while P's installed version
// satisfies R's operator Replaces entry.
func TestUpdateCoexistingCurrentWithOperators(t *testing.T) {
	installedR := spec("R.noarch.2.0.0", repl("P<2.0.0"), prov("P"))

	t.Run("in_range_installed_is_suppressed", func(t *testing.T) {
		followCase{
			name: "in_range_coexisting",
			state: stateOf(
				spec("P.noarch.1.0.0"),
				installedR,
			),
			rm:        stable(installedR),
			wantDiags: []string{"update_replace_coexisting_current P.noarch.1.0.0 replacer=R.noarch.2.0.0"},
		}.run(t)
	})

	t.Run("out_of_range_installed_upgrades_in_place", func(t *testing.T) {
		followCase{
			name: "out_of_range_coexisting",
			state: stateOf(
				spec("P.noarch.2.5.0"),
				installedR,
			),
			rm: stable(
				spec("P.noarch.3.0.0"),
				installedR,
			),
			want: []string{"Upgrade P.noarch.2.5.0 -> P.noarch.3.0.0 from stable"},
		}.run(t)
	})
}
