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
	"bytes"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/googet/v2/client"
	"github.com/google/googet/v2/goolib"
	"github.com/google/googet/v2/priority"
	"github.com/google/googet/v2/testutil"
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

// TestUpdateRetiredProviderFallbackWithOperators checks the retired-provider
// fallback path when P is retired for its arch and R provides P with a bounded
// operator Replaces entry, including idempotence and noarch-only successors.
func TestUpdateRetiredProviderFallbackWithOperators(t *testing.T) {
	replacer := spec("R.x86_64.4.0.0", repl("P<2.0.0"), prov("P"))
	rm := stable(replacer)

	t.Run("nil_successor_in_range_replaces_and_second_run_is_noop", func(t *testing.T) {
		inRange := planUpdates(stateOf(spec("P.x86_64.1.0.0")), rm, gateCfg)
		wantInRange := []string{
			"Replace P.x86_64.1.0.0 -> R.x86_64.4.0.0 from stable replaced=[P.x86_64.1.0.0] removed=[P.x86_64.1.0.0]",
		}
		if diff := cmp.Diff(wantInRange, summarize(inRange)); diff != "" {
			t.Errorf("retired fallback in-range (-want +got):\n%s", diff)
		}

		secondRun := planUpdates(stateOf(replacer), rm, gateCfg)
		if diff := cmp.Diff([]string(nil), summarize(secondRun)); diff != "" {
			t.Errorf("retired fallback second run (-want +got):\n%s", diff)
		}
	})

	t.Run("nil_successor_out_of_range_never_fires_successor_skip", func(t *testing.T) {
		// Out-of-range installed P does not match Replaces, so pass 2 treats R
		// as a plain provider upgrade without removal and without a skip.
		for _, cfg := range []updateConfig{gateCfg, followCfg} {
			outOfRange := planUpdates(stateOf(spec("P.x86_64.3.0.0")), rm, cfg)
			wantOutOfRange := []string{
				"Upgrade P.x86_64.3.0.0 -> R.x86_64.4.0.0 from stable",
			}
			if diff := cmp.Diff(wantOutOfRange, summarize(outOfRange)); diff != "" {
				t.Errorf("retired fallback out-of-range (follow=%t) (-want +got):\n%s", cfg.followReplaces, diff)
			}
		}
	})

	t.Run("noarch_only_out_of_range_successor_skips_and_keeps_installed", func(t *testing.T) {
		// latestFor checks exact p.Arch (x86_64) so fallback triggers and marks
		// P handled, while selectReplacer sees P.noarch.2.0.0 via admissible
		// and skips with successor_out_of_range, leaving P installed.
		rmNoarchOut := stable(
			spec("P.noarch.2.0.0"),
			replacer,
		)
		for _, cfg := range []updateConfig{gateCfg, followCfg} {
			got := planUpdates(stateOf(spec("P.x86_64.1.0.0")), rmNoarchOut, cfg)
			want := []string{
				"skip P.x86_64.1.0.0 reason=successor_out_of_range replacer=R.x86_64.4.0.0",
			}
			if diff := cmp.Diff(want, summarize(got)); diff != "" {
				t.Errorf("noarch-only out-of-range successor (follow=%t) (-want +got):\n%s", cfg.followReplaces, diff)
			}
		}

		// When the noarch successor is still covered by R's Replaces bound, R
		// replaces P.
		rmNoarchIn := stable(
			spec("P.noarch.1.5.0"),
			replacer,
		)
		gotIn := planUpdates(stateOf(spec("P.x86_64.1.0.0")), rmNoarchIn, gateCfg)
		wantIn := []string{
			"Replace P.x86_64.1.0.0 -> R.x86_64.4.0.0 from stable replaced=[P.x86_64.1.0.0] removed=[P.x86_64.1.0.0]",
		}
		if diff := cmp.Diff(wantIn, summarize(gotIn)); diff != "" {
			t.Errorf("noarch-only in-range successor (-want +got):\n%s", diff)
		}
	})

	t.Run("below_default_priority_noarch_successor_does_not_block_replacement", func(t *testing.T) {
		versionedReplacer := spec("R.x86_64.2.0.0", repl("P<2.0.0"), prov("P=2.0.0"))
		for _, lowPrio := range []priority.Value{100, priority.Default - 1} {
			rmLowNoarch := client.RepoMap{
				"stable": repoOf(priority.Default, versionedReplacer),
				"low":    repoOf(lowPrio, spec("P.noarch.2.0.0")),
			}
			for _, cfg := range []updateConfig{gateCfg, followCfg} {
				got := planUpdates(stateOf(spec("P.x86_64.1.0.0")), rmLowNoarch, cfg)
				want := []string{
					"Replace P.x86_64.1.0.0 -> R.x86_64.2.0.0 from stable replaced=[P.x86_64.1.0.0] removed=[P.x86_64.1.0.0]",
				}
				if diff := cmp.Diff(want, summarize(got)); diff != "" {
					t.Errorf("below-default noarch successor (prio=%d, follow=%t) (-want +got):\n%s", lowPrio, cfg.followReplaces, diff)
				}
			}
		}
	})

	t.Run("arch_qualified_replaces_covers_in_range_noarch_successor", func(t *testing.T) {
		archReplacer := spec("R.x86_64.2.0.0", repl("P.x86_64<2.0.0"), prov("P=2.0.0"))
		rmArchInRangeNoarch := stable(
			spec("P.noarch.1.5.0"),
			archReplacer,
		)
		for _, cfg := range []updateConfig{gateCfg, followCfg} {
			got := planUpdates(stateOf(spec("P.x86_64.1.0.0")), rmArchInRangeNoarch, cfg)
			want := []string{
				"Replace P.x86_64.1.0.0 -> R.x86_64.2.0.0 from stable replaced=[P.x86_64.1.0.0] removed=[P.x86_64.1.0.0]",
			}
			if diff := cmp.Diff(want, summarize(got)); diff != "" {
				t.Errorf("arch-qualified Replaces with in-range noarch successor (follow=%t) (-want +got):\n%s", cfg.followReplaces, diff)
			}
		}
	})
}

// TestUpdateSuccessorOutOfRange checks that selectReplacer skips an
// upper-bounded replacement with reason successor_out_of_range only when a
// newer version of P outside the Replaces range is offered at default priority
// or higher, allowing P to upgrade in place under followReplaces while keeping
// upward-closed entries, below-default-priority successors, and in-range
// noarch successors unaffected.
func TestUpdateSuccessorOutOfRange(t *testing.T) {
	installedP := spec("P.x86_64.1.0.0")
	successorP := spec("P.x86_64.2.0.0")
	replacer := spec("R.x86_64.2.0.0", repl("P<2.0.0"), prov("P"))
	rm := stable(successorP, replacer)

	t.Run("default_config_upgrades_in_place_without_skip", func(t *testing.T) {
		plan := planUpdates(stateOf(installedP), rm, gateCfg)
		want := []string{
			"Upgrade P.x86_64.1.0.0 -> P.x86_64.2.0.0 from stable",
		}
		if diff := cmp.Diff(want, summarize(plan)); diff != "" {
			t.Errorf("default config (-want +got):\n%s", diff)
		}
	})

	t.Run("follow_replaces_skips_and_upgrades_in_place_and_renders_reason", func(t *testing.T) {
		plan := planUpdates(stateOf(installedP), rm, followCfg)
		want := []string{
			"skip P.x86_64.1.0.0 reason=successor_out_of_range replacer=R.x86_64.2.0.0",
			"Upgrade P.x86_64.1.0.0 -> P.x86_64.2.0.0 from stable",
		}
		if diff := cmp.Diff(want, summarize(plan)); diff != "" {
			t.Errorf("followReplaces (-want +got):\n%s", diff)
		}

		logs := testutil.CaptureLogs(t)
		var out bytes.Buffer
		renderUpdates(&out, plan)
		if !strings.Contains(out.String(), "successor_out_of_range") {
			t.Errorf("rendered output missing successor_out_of_range:\n%s", out.String())
		}
		if !strings.Contains(logs.String(), "update_replace_skipped") || !strings.Contains(logs.String(), "successor_out_of_range") {
			t.Errorf("log output missing update_replace_skipped / successor_out_of_range:\n%s", logs.String())
		}
	})

	t.Run("upper_bounded_operators_skip_while_in_range_and_upward_closed_replace", func(t *testing.T) {
		for _, tc := range []struct {
			name    string
			entry   string
			nextVer string
			want    []string
		}{
			{
				name:    "lt_excludes_bound",
				entry:   "P<2.0.0",
				nextVer: "2.0.0",
				want: []string{
					"skip P.x86_64.1.0.0 reason=successor_out_of_range replacer=R.x86_64.2.0.0",
					"Upgrade P.x86_64.1.0.0 -> P.x86_64.2.0.0 from stable",
				},
			},
			{
				name:    "le_excludes_above_bound",
				entry:   "P<=1.5.0",
				nextVer: "2.0.0",
				want: []string{
					"skip P.x86_64.1.0.0 reason=successor_out_of_range replacer=R.x86_64.2.0.0",
					"Upgrade P.x86_64.1.0.0 -> P.x86_64.2.0.0 from stable",
				},
			},
			{
				name:    "eq_excludes_newer_version",
				entry:   "P=1.0.0",
				nextVer: "1.0.1",
				want: []string{
					"skip P.x86_64.1.0.0 reason=successor_out_of_range replacer=R.x86_64.2.0.0",
					"Upgrade P.x86_64.1.0.0 -> P.x86_64.1.0.1 from stable",
				},
			},
			{
				name:    "arch_qualified_lt_excludes_bound",
				entry:   "P.x86_64<2.0.0",
				nextVer: "2.0.0",
				want: []string{
					"skip P.x86_64.1.0.0 reason=successor_out_of_range replacer=R.x86_64.2.0.0",
					"Upgrade P.x86_64.1.0.0 -> P.x86_64.2.0.0 from stable",
				},
			},
			{
				name:    "lt_includes_newer_in_range_version",
				entry:   "P<2.0.0",
				nextVer: "1.5.0",
				want: []string{
					"Replace P.x86_64.1.0.0 -> R.x86_64.2.0.0 from stable replaced=[P.x86_64.1.0.0] removed=[P.x86_64.1.0.0]",
				},
			},
			{
				name:    "le_includes_exact_bound",
				entry:   "P<=2.0.0",
				nextVer: "2.0.0",
				want: []string{
					"Replace P.x86_64.1.0.0 -> R.x86_64.2.0.0 from stable replaced=[P.x86_64.1.0.0] removed=[P.x86_64.1.0.0]",
				},
			},
			{
				name:    "ge_upward_closed_replaces",
				entry:   "P>=1.0.0",
				nextVer: "2.0.0",
				want: []string{
					"Replace P.x86_64.1.0.0 -> R.x86_64.2.0.0 from stable replaced=[P.x86_64.1.0.0] removed=[P.x86_64.1.0.0]",
				},
			},
			{
				name:    "gt_upward_closed_replaces",
				entry:   "P>0.5.0",
				nextVer: "2.0.0",
				want: []string{
					"Replace P.x86_64.1.0.0 -> R.x86_64.2.0.0 from stable replaced=[P.x86_64.1.0.0] removed=[P.x86_64.1.0.0]",
				},
			},
			{
				name:    "legacy_minimum_upward_closed_replaces",
				entry:   "P.x86_64.1.0.0",
				nextVer: "2.0.0",
				want: []string{
					"Replace P.x86_64.1.0.0 -> R.x86_64.2.0.0 from stable replaced=[P.x86_64.1.0.0] removed=[P.x86_64.1.0.0]",
				},
			},
		} {
			t.Run(tc.name, func(t *testing.T) {
				followCase{
					name:  tc.name,
					state: stateOf(installedP),
					rm: stable(
						spec("P.x86_64."+tc.nextVer),
						spec("R.x86_64.2.0.0", repl(tc.entry), prov("P=2.0.0")),
					),
					want: tc.want,
				}.run(t)
			})
		}
	})

	t.Run("skip_precedence_with_pinned_ambiguous_and_cycle", func(t *testing.T) {
		// Pinned priority takes precedence over successor_out_of_range.
		pinnedRM := client.RepoMap{
			"high":   repoOf(priority.Default+100, successorP),
			"stable": repoOf(priority.Default, replacer),
		}
		followCase{
			name:  "pinned_precedes_out_of_range",
			state: stateOf(installedP),
			rm:    pinnedRM,
			want: []string{
				"skip P.x86_64.1.0.0 reason=pinned_priority replacer=R.x86_64.2.0.0",
				"Upgrade P.x86_64.1.0.0 -> P.x86_64.2.0.0 from high",
			},
		}.run(t)

		// Ambiguous replacers take precedence over successor_out_of_range.
		followCase{
			name:  "ambiguous_precedes_out_of_range",
			state: stateOf(installedP),
			rm: stable(
				successorP,
				replacer,
				spec("R2.x86_64.2.0.0", repl("P<2.0.0"), prov("P")),
			),
			want: []string{
				"skip P.x86_64.1.0.0 reason=ambiguous_replacers candidates=[R.x86_64.2.0.0,R2.x86_64.2.0.0]",
				"Upgrade P.x86_64.1.0.0 -> P.x86_64.2.0.0 from stable",
			},
		}.run(t)

		// successor_out_of_range precedes cyclic check.
		followCase{
			name:  "out_of_range_precedes_cycle",
			state: stateOf(installedP),
			rm: stable(
				spec("P.x86_64.2.0.0", repl("R>=2.0.0")),
				replacer,
			),
			want: []string{
				"skip P.x86_64.1.0.0 reason=successor_out_of_range replacer=R.x86_64.2.0.0",
				"Upgrade P.x86_64.1.0.0 -> P.x86_64.2.0.0 from stable",
			},
		}.run(t)
	})

	t.Run("below_default_priority_successor_does_not_skip_replacement", func(t *testing.T) {
		versionedReplacer := spec("R.x86_64.2.0.0", repl("P<2.0.0"), prov("P=2.0.0"))
		for _, lowPrio := range []priority.Value{100, priority.Default - 1} {
			followCase{
				name:  "below_default_priority_successor",
				state: stateOf(installedP),
				rm: client.RepoMap{
					"stable": repoOf(priority.Default, versionedReplacer),
					"low":    repoOf(lowPrio, successorP),
				},
				want: []string{
					"Replace P.x86_64.1.0.0 -> R.x86_64.2.0.0 from stable replaced=[P.x86_64.1.0.0] removed=[P.x86_64.1.0.0]",
				},
			}.run(t)
		}
	})

	t.Run("arch_qualified_replaces_covers_in_range_noarch_successor", func(t *testing.T) {
		followCase{
			name:  "same_arch_replacer_with_noarch_successor",
			state: stateOf(installedP),
			rm: stable(
				spec("P.noarch.1.5.0"),
				spec("R.x86_64.2.0.0", repl("P.x86_64<2.0.0"), prov("P=2.0.0")),
			),
			want: []string{
				"Replace P.x86_64.1.0.0 -> R.x86_64.2.0.0 from stable replaced=[P.x86_64.1.0.0] removed=[P.x86_64.1.0.0]",
			},
		}.run(t)

		followCase{
			name:  "noarch_replacer_with_noarch_successor",
			state: stateOf(installedP),
			rm: stable(
				spec("P.noarch.1.5.0"),
				spec("R.noarch.2.0.0", repl("P.x86_64<2.0.0"), prov("P=2.0.0")),
			),
			want: []string{
				"Replace P.x86_64.1.0.0 -> R.noarch.2.0.0 from stable replaced=[P.x86_64.1.0.0] removed=[P.x86_64.1.0.0]",
			},
		}.run(t)
	})
}

// TestUpdateCycleCheckWithOperators checks that cyclic evaluates operator
// bounds on P's best offered spec, installed third-party packages, and
// transitive replacement chains.
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

	t.Run("installed_row_in_range_ge_or_eq_replaces_is_cyclic", func(t *testing.T) {
		for _, opEntry := range []string{"R>=2.0.0", "R=2.0.0"} {
			followCase{
				name: "installed_row_" + opEntry,
				state: stateOf(
					spec("P.x86_64.1.0.0"),
					spec("Q.x86_64.1.0.0", repl(opEntry)),
				),
				rm: stable(
					spec("Q.x86_64.1.0.0", repl(opEntry)),
					spec("R.x86_64.2.0.0", repl("P<2.0.0"), prov("P")),
				),
				want: []string{
					"skip P.x86_64.1.0.0 reason=replaces_cycle replacer=R.x86_64.2.0.0",
				},
			}.run(t)
		}
	})

	t.Run("installed_row_out_of_range_replaces_is_not_cyclic", func(t *testing.T) {
		followCase{
			name: "installed_row_out_of_range",
			state: stateOf(
				spec("P.x86_64.1.0.0"),
				spec("Q.x86_64.1.0.0", repl("R<2.0.0")),
			),
			rm: stable(
				spec("Q.x86_64.1.0.0", repl("R<2.0.0")),
				spec("R.x86_64.2.0.0", repl("P<2.0.0"), prov("P")),
			),
			want: []string{
				"Replace P.x86_64.1.0.0 -> R.x86_64.2.0.0 from stable replaced=[P.x86_64.1.0.0] removed=[P.x86_64.1.0.0]",
			},
		}.run(t)
	})
}

// TestUpdateCoexistingCurrentWithOperators checks how an installed replacing
// package at its best version interacts with followReplaces across in-range
// installed versions (with and without an out-of-range successor offered) and
// out-of-range installed versions.
func TestUpdateCoexistingCurrentWithOperators(t *testing.T) {
	installedR := spec("R.noarch.2.0.0", repl("P<2.0.0"), prov("P"))

	t.Run("in_range_installed_without_out_of_range_successor_is_suppressed", func(t *testing.T) {
		followCase{
			name: "in_range_coexisting_no_successor",
			state: stateOf(
				spec("P.noarch.1.0.0"),
				installedR,
			),
			rm:        stable(installedR),
			wantDiags: []string{"update_replace_coexisting_current P.noarch.1.0.0 replacer=R.noarch.2.0.0"},
		}.run(t)

		// Even when an in-range update (1.5.0 < 2.0.0) is offered, R still
		// covers it, so coexisting suppression prevents upgrading into R's range.
		followCase{
			name: "in_range_coexisting_in_range_successor",
			state: stateOf(
				spec("P.noarch.1.0.0"),
				installedR,
			),
			rm: stable(
				spec("P.noarch.1.5.0"),
				installedR,
			),
			wantDiags: []string{"update_replace_coexisting_current P.noarch.1.0.0 replacer=R.noarch.2.0.0"},
		}.run(t)
	})

	t.Run("in_range_installed_with_out_of_range_successor_skips_before_suppression_and_upgrades", func(t *testing.T) {
		followCase{
			name: "coexisting_out_of_range_successor_upgrades",
			state: stateOf(
				spec("P.noarch.1.0.0"),
				installedR,
			),
			rm: stable(
				spec("P.noarch.2.0.0"),
				installedR,
			),
			want: []string{
				"skip P.noarch.1.0.0 reason=successor_out_of_range replacer=R.noarch.2.0.0",
				"Upgrade P.noarch.1.0.0 -> P.noarch.2.0.0 from stable",
			},
			wantDiags: nil,
		}.run(t)
	})

	t.Run("out_of_range_installed_is_not_suppressed_and_upgrades_in_place", func(t *testing.T) {
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
