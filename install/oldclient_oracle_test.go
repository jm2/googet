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

// Provenance for the historical client relation oracles in this file:
//
//   - oracle34 reproduces GooGet 3.4.x at upstream commit
//     07fd25773e623f6fdd58ebc0474d6d5a9a3a4ff9:
//       * install/install.go: minInstalled, isSatisfied, resolveConflicts, and
//         the per-entry predicate of resolveReplacements.
//       * client/client.go: SatisfiesProvider and satisfiesVersion.
//       * goolib/goospec.go: Compare, ParseVersion, and fixVer (vendored here as
//         compare34, parseVersion34, and fixVer34 over github.com/blang/semver
//         from go.mod so short-version right-alignment stays pinned).
//
//   - oracle30 reproduces GooGet v3.0.0 at upstream tag v3.0.0
//     (commit f0d16dfe09ebad7e0fe63a1af2bea02d8cf9929d):
//       * install/install.go: minInstalled, resolveConflicts, and the per-entry
//         predicate of resolveReplacements (prior to Provides support).
//       * goolib/goospec.go: Compare, ParseVersion, and fixVer (identical to
//         07fd25773e623f6fdd58ebc0474d6d5a9a3a4ff9).
//
// Modifications from upstream:
//   1. Functions are bound to oracle34 and oracle30 receiver types so they do
//      not collide with the current-semantics helpers in oracle_test.go.
//   2. oracle34 accepts oldClientFetcher instead of *googetdb.GooDB so tests can
//      exercise both real SQLite databases and snapshot.Pick state slices.
//   3. Version comparisons call compare34 instead of goolib.Compare, and
//      satisfiesVersion omits the global logger call while returning the same
//      boolean result.
//
// Intermediate revision coverage: isSatisfied, resolveConflicts,
// resolveReplacements, SatisfiesProvider, satisfiesVersion, Compare, fixVer,
// and ParseVersion are byte-identical from commit
// c66961914d4adf570a78a1e1fe34c92de53b09b4 (which introduced Provides) through
// 07fd25773e623f6fdd58ebc0474d6d5a9a3a4ff9, and minInstalled only changed its
// lookup argument from FetchPkg(pi.Name) to FetchPkg(pi) while rechecking Name
// and Arch in every revision. Consequently, oracle34 and oracle30 together
// cover intermediate 3.x builds as well.

import (
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/blang/semver"
	"github.com/google/googet/v2/client"
	"github.com/google/googet/v2/goolib"
	"github.com/google/googet/v2/internal/snapshot"
)

// oldClientFetcher is the subset of *googetdb.GooDB read by GooGet 3.4.x
// relation evaluation.
type oldClientFetcher interface {
	FetchPkg(goolib.PackageInfo) (client.PackageState, error)
	FetchPkgs(string) (client.GooGetState, error)
}

// snapshotFetcher adapts an in-memory GooGetState to oldClientFetcher using
// snapshot.Pick, which mirrors googetdb.GooDB.FetchPkg.
type snapshotFetcher struct {
	state client.GooGetState
}

func (f snapshotFetcher) FetchPkg(pi goolib.PackageInfo) (client.PackageState, error) {
	p, _ := snapshot.Pick(pi, f.state)
	return p, nil
}

func (f snapshotFetcher) FetchPkgs(_ string) (client.GooGetState, error) {
	return f.state, nil
}

// oldClientOutcome describes what a historical GooGet client does with a single
// Conflicts or Replaces entry against an installed state.
type oldClientOutcome uint8

const (
	oldOutcomePass oldClientOutcome = iota
	oldOutcomeNoop
	oldOutcomeBlock
	oldOutcomeRemove
	oldOutcomeError
)

func (o oldClientOutcome) String() string {
	switch o {
	case oldOutcomePass:
		return "pass"
	case oldOutcomeNoop:
		return "noop"
	case oldOutcomeBlock:
		return "block"
	case oldOutcomeRemove:
		return "remove"
	case oldOutcomeError:
		return "error"
	default:
		return "unknown"
	}
}

// isFailOpen reports whether the old client allows installation without
// blocking, removing packages, or returning a version parse error.
func (o oldClientOutcome) isFailOpen() bool {
	return o == oldOutcomePass || o == oldOutcomeNoop
}

// version34 is goolib.Version at commit 07fd25773e623f6fdd58ebc0474d6d5a9a3a4ff9.
type version34 struct {
	Semver semver.Version
	GsVer  int64
}

// fixVer34 is goolib.fixVer from commit 07fd25773e623f6fdd58ebc0474d6d5a9a3a4ff9,
// preserving right-aligned zero-fill for short version strings ("2" -> "0.0.2",
// "1.2" -> "0.1.2").
func fixVer34(ver string) string {
	suffix := ""
	// Patch number can contain prerelease or build metadata suffix.
	if i := strings.IndexAny(ver, "+-"); i != -1 {
		suffix = ver[i:]
		ver = ver[:i]
	}
	out := []string{"0", "0", "0"}
	nums := strings.SplitN(ver, ".", 3)
	offset := len(out) - len(nums)
	for i, str := range nums {
		trimmed := strings.TrimLeft(str, "0")
		if trimmed == "" {
			trimmed = "0"
		}
		out[i+offset] = trimmed
	}
	return strings.Join(out, ".") + suffix
}

// parseVersion34 is goolib.ParseVersion from commit
// 07fd25773e623f6fdd58ebc0474d6d5a9a3a4ff9.
func parseVersion34(ver string) (version34, error) {
	v := strings.SplitN(ver, "@", 2)
	v[0] = fixVer34(v[0])

	sv, err := semver.Parse(v[0])
	if err != nil {
		return version34{}, err
	}
	version := version34{Semver: sv}
	if len(v) == 2 {
		gv, err := strconv.ParseInt(v[1], 10, 64)
		if err != nil {
			return version, err
		}
		version = version34{
			Semver: sv,
			GsVer:  gv,
		}
	} else {
		version = version34{Semver: sv}
	}
	return version, nil
}

// compare34 is goolib.Compare from commit 07fd25773e623f6fdd58ebc0474d6d5a9a3a4ff9.
func compare34(v1, v2 string) (int, error) {
	pv1, err := parseVersion34(v1)
	if err != nil {
		return 0, err
	}
	pv2, err := parseVersion34(v2)
	if err != nil {
		return 0, err
	}
	var c int
	if c = pv1.Semver.Compare(pv2.Semver); c == 0 {
		if pv1.GsVer > pv2.GsVer {
			return 1, nil
		}
		if pv1.GsVer < pv2.GsVer {
			return -1, nil
		}
		return 0, nil
	}
	return c, nil
}

// oracle34 evaluates Conflicts and Replaces entries using GooGet 3.4.x rules.
type oracle34 struct{}

func (o oracle34) minInstalled(pi goolib.PackageInfo, db oldClientFetcher) (bool, error) {
	p, err := db.FetchPkg(pi)
	if err != nil {
		return false, err
	}
	if p.PackageSpec == nil {
		return false, nil
	}
	if p.PackageSpec.Name == pi.Name && (pi.Arch == "" || p.PackageSpec.Arch == pi.Arch) {
		c, err := compare34(pi.Ver, p.PackageSpec.Version)
		if err != nil {
			return false, err
		}
		return c < 1, nil
	}
	return false, nil
}

func (o oracle34) satisfiesVersion(pkgVer, reqVer string) bool {
	if reqVer == "" || pkgVer == "" {
		return true
	}
	c, err := compare34(pkgVer, reqVer)
	if err != nil {
		return false
	}
	return c >= 0
}

func (o oracle34) satisfiesProvider(prov, reqName, reqVer string) bool {
	pName, pVer, _ := strings.Cut(prov, "=")
	return pName == reqName && o.satisfiesVersion(pVer, reqVer)
}

func (o oracle34) isSatisfied(pi goolib.PackageInfo, db oldClientFetcher) (bool, error) {
	// First check if the package itself is installed.
	ok, err := o.minInstalled(pi, db)
	if err != nil {
		return false, err
	}
	if ok {
		return true, nil
	}

	// Check if any installed package provides this dependency.
	pkgs, err := db.FetchPkgs("")
	if err != nil {
		return false, err
	}
	for _, p := range pkgs {
		if p.PackageSpec == nil {
			continue
		}
		for _, prov := range p.PackageSpec.Provides {
			if o.satisfiesProvider(prov, pi.Name, pi.Ver) {
				return true, nil
			}
		}
	}
	return false, nil
}

func (o oracle34) resolveConflicts(ps *goolib.PkgSpec, db oldClientFetcher) error {
	for _, pkg := range ps.Conflicts {
		pi := goolib.PkgNameSplit(pkg)
		sat, err := o.isSatisfied(goolib.PackageInfo{Name: pi.Name, Arch: pi.Arch, Ver: pi.Ver}, db)
		if err != nil {
			return err
		}
		if sat {
			return fmt.Errorf("cannot install, conflict with installed package or provider: %s", pi)
		}
	}
	return nil
}

func (o oracle34) checkConflictsEntry(entry string, db oldClientFetcher) oldClientOutcome {
	pi := goolib.PkgNameSplit(entry)
	sat, err := o.isSatisfied(goolib.PackageInfo{Name: pi.Name, Arch: pi.Arch, Ver: pi.Ver}, db)
	if err != nil {
		return oldOutcomeError
	}
	if sat {
		return oldOutcomeBlock
	}
	return oldOutcomePass
}

func (o oracle34) checkReplacesEntry(entry string, db oldClientFetcher) oldClientOutcome {
	pi := goolib.PkgNameSplit(entry)
	ins, err := o.minInstalled(goolib.PackageInfo{Name: pi.Name, Arch: pi.Arch, Ver: pi.Ver}, db)
	if err != nil {
		return oldOutcomeError
	}
	if ins {
		return oldOutcomeRemove
	}
	return oldOutcomeNoop
}

// oracle30 evaluates Conflicts and Replaces entries using GooGet v3.0.0 rules.
type oracle30 struct{}

func (o oracle30) minInstalled(pi goolib.PackageInfo, state client.GooGetState) (bool, error) {
	for _, p := range state {
		if p.PackageSpec == nil {
			continue
		}
		if p.PackageSpec.Name == pi.Name && (pi.Arch == "" || p.PackageSpec.Arch == pi.Arch) {
			c, err := compare34(pi.Ver, p.PackageSpec.Version)
			if err != nil {
				return false, err
			}
			return c < 1, nil
		}
	}
	return false, nil
}

func (o oracle30) resolveConflicts(ps *goolib.PkgSpec, state client.GooGetState) error {
	for _, pkg := range ps.Conflicts {
		pi := goolib.PkgNameSplit(pkg)
		ins, err := o.minInstalled(goolib.PackageInfo{Name: pi.Name, Arch: pi.Arch, Ver: pi.Ver}, state)
		if err != nil {
			return err
		}
		if ins {
			return fmt.Errorf("cannot install, conflict with installed package: %s", pi)
		}
	}
	return nil
}

func (o oracle30) checkConflictsEntry(entry string, state client.GooGetState) oldClientOutcome {
	pi := goolib.PkgNameSplit(entry)
	ins, err := o.minInstalled(goolib.PackageInfo{Name: pi.Name, Arch: pi.Arch, Ver: pi.Ver}, state)
	if err != nil {
		return oldOutcomeError
	}
	if ins {
		return oldOutcomeBlock
	}
	return oldOutcomePass
}

func (o oracle30) checkReplacesEntry(entry string, state client.GooGetState) oldClientOutcome {
	pi := goolib.PkgNameSplit(entry)
	ins, err := o.minInstalled(goolib.PackageInfo{Name: pi.Name, Arch: pi.Arch, Ver: pi.Ver}, state)
	if err != nil {
		return oldOutcomeError
	}
	if ins {
		return oldOutcomeRemove
	}
	return oldOutcomeNoop
}

// TestOldClientOracleVersionFidelity checks that the vendored 3.4.x fixVer34,
// parseVersion34, and compare34 helpers preserve right-aligned short versions
// ("2" -> 0.0.2, "1.2" -> 0.1.2) and release/metadata comparison semantics.
func TestOldClientOracleVersionFidelity(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want string
	}{
		{in: "2", want: "0.0.2"},
		{in: "1.2", want: "0.1.2"},
		{in: "01.02.03", want: "1.2.3"},
		{in: "0", want: "0.0.0"},
		{in: "", want: "0.0.0"},
		{in: "2.0.0-rc1", want: "2.0.0-rc1"},
		{in: "1.2+build5", want: "0.1.2+build5"},
		{in: "5.1", want: "0.5.1"},
	} {
		if got := fixVer34(tc.in); got != tc.want {
			t.Errorf("fixVer34(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}

	for _, tc := range []struct {
		v1, v2 string
		want   int
	}{
		{v1: "2", v2: "0.0.2", want: 0},
		{v1: "2", v2: "2.0.0", want: -1},
		{v1: "1.2", v2: "0.1.2", want: 0},
		{v1: "1.2", v2: "1.2.0", want: -1},
		{v1: "5.1@3", v2: "0.5.1@3", want: 0},
		{v1: "5.1@3", v2: "1.0.0", want: -1},
		{v1: "5.0-rc1", v2: "0.5.0-rc1", want: 0},
		{v1: "2.0.0-rc1", v2: "2.0.0", want: -1},
		{v1: "2.0.0+build5", v2: "2.0.0", want: 0},
		{v1: "2.0.0@1", v2: "2.0.0", want: 1},
	} {
		got, err := compare34(tc.v1, tc.v2)
		if err != nil {
			t.Fatalf("compare34(%q, %q) unexpected error: %v", tc.v1, tc.v2, err)
		}
		if got != tc.want {
			t.Errorf("compare34(%q, %q) = %d, want %d", tc.v1, tc.v2, got, tc.want)
		}
	}
}
