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
	"slices"
	"strings"
	"testing"

	"github.com/google/googet/v2/goolib"
	"github.com/google/googet/v2/internal/relsyntax"
)

// The old* functions are verbatim copies of the code this package replaced,
// kept so that the tests can check the two agree on every input.

// oldMinInstalled is the predicate from install.minInstalled.
func oldMinInstalled(pi goolib.PackageInfo, name, arch, ver string) (bool, error) {
	if name == pi.Name && (pi.Arch == "" || arch == pi.Arch) {
		c, err := goolib.Compare(pi.Ver, ver)
		if err != nil {
			return false, err
		}
		return c < 1, nil
	}
	return false, nil
}

// oldSatisfiesVersion is client.satisfiesVersion, which logged an error and
// returned false when a version couldn't be parsed.
func oldSatisfiesVersion(pkgVer, reqVer string) bool {
	if reqVer == "" || pkgVer == "" {
		return true
	}
	c, err := goolib.Compare(pkgVer, reqVer)
	if err != nil {
		return false
	}
	return c >= 0
}

// oldSatisfiesProvider is client.SatisfiesProvider.
func oldSatisfiesProvider(prov, reqName, reqVer string) bool {
	pName, pVer, _ := strings.Cut(prov, "=")
	return pName == reqName && oldSatisfiesVersion(pVer, reqVer)
}

// oldSameIdentity is the dependency match from remove.DepMap.build.
func oldSameIdentity(dep, name, arch string) bool {
	di := goolib.PkgNameSplit(dep)
	return di.Name == name && (di.Arch == arch || di.Arch == "")
}

var (
	relationCorpus = []string{
		"", " ", ".", "..", "foo", " foo ", "foo.", "foo..", "foo..1", "foo.x86_64", "foo.noarch",
		"foo.noarch.1.0.0", "foo.noarch.1.0.0@2", " foo.noarch.2 ", "foo.x86_64.1", "a.b.c.d",
	}
	// operatorRelations are well-formed operator entries. ParseRelation
	// gives them their operator, so they have no PkgNameSplit or
	// oldMinInstalled equivalent.
	operatorRelations = []struct {
		rel  string
		want Requirement
	}{
		{rel: "foo<2.0.0", want: Requirement{Name: "foo", Op: OpLT, Ver: "2.0.0"}},
		{rel: "foo.x86_64<2.0.0", want: Requirement{Name: "foo", Arch: "x86_64", Op: OpLT, Ver: "2.0.0"}},
		{rel: "foo >= 1", want: Requirement{Name: "foo", Op: OpGE, Ver: "1"}},
		{rel: "foo.noarch >= 1", want: Requirement{Name: "foo", Arch: "noarch", Op: OpGE, Ver: "1"}},
		{rel: "foo=1", want: Requirement{Name: "foo", Op: OpEQ, Ver: "1"}},
		{rel: "foo.noarch=1", want: Requirement{Name: "foo", Arch: "noarch", Op: OpEQ, Ver: "1"}},
	}
	provideCorpus = []string{
		"", "=", "foo", "foo=", "=1", "foo=1", "foo=1.0.0@1", "foo=1=2", " foo=1", "foo =1",
		"foo.noarch", "foo.noarch=1", "foo=abc", "foo=0.0.0-rc1",
	}
	versionCorpus = []string{
		"", "0", "0.0.0", "0.0.0-rc1", "1", "1.0", "1.0.0", "1.0.0@1", "1.0.0@x", "1.0.0-beta",
		"2.0.0", "3", "10", "abc", " 1",
	}
	nameCorpus = []string{"", "foo", "bar", "foo.noarch"}
	archCorpus = []string{"", "noarch", "x86_64"}
)

// allRelations returns relationCorpus followed by the operatorRelations
// entries.
func allRelations() []string {
	out := slices.Clone(relationCorpus)
	for _, tc := range operatorRelations {
		out = append(out, tc.rel)
	}
	return out
}

// TestParseRelationMatchesPkgNameSplit checks that ParseRelation splits an
// entry without a well-formed operator as PkgNameSplit does, and parses each
// well-formed operator entry with its operator.
func TestParseRelationMatchesPkgNameSplit(t *testing.T) {
	for _, s := range relationCorpus {
		if got, want := ParseRelation(s).PackageInfo(), goolib.PkgNameSplit(s); got != want {
			t.Errorf("ParseRelation(%q) = %+v, want %+v", s, got, want)
		}
	}
	for _, tc := range operatorRelations {
		if got := ParseRelation(tc.rel); got != tc.want {
			t.Errorf("ParseRelation(%q) = %+v, want %+v", tc.rel, got, tc.want)
		}
	}
}

func TestParseDependency(t *testing.T) {
	for _, key := range allRelations() {
		for _, ver := range versionCorpus {
			pi := goolib.PkgNameSplit(key)
			want := Requirement{Name: pi.Name, Arch: pi.Arch, Ver: ver}
			if got := ParseDependency(key, ver); got != want {
				t.Errorf("ParseDependency(%q, %q) = %+v, want %+v", key, ver, got, want)
			}
		}
	}
}

func TestParseProvideMatchesCut(t *testing.T) {
	for _, s := range provideCorpus {
		name, ver, _ := strings.Cut(s, "=")
		if got, want := ParseProvide(s), (Provide{Name: name, Ver: ver}); got != want {
			t.Errorf("ParseProvide(%q) = %+v, want %+v", s, got, want)
		}
	}
}

// fuzzRelationSeeds returns Legacy, spaced and unspaced Operator, and
// Malformed entries for relation fuzz targets.
func fuzzRelationSeeds() []string {
	return slices.Concat(allRelations(), malformedOperatorCorpus)
}

// oracleOperatorMatch evaluates op against the comparison sign of (ver, reqVer)
// independently of Requirement methods.
func oracleOperatorMatch(op Op, sign int) bool {
	switch op {
	case OpLT:
		return sign < 0
	case OpLE:
		return sign <= 0
	case OpEQ:
		return sign == 0
	case OpGE:
		return sign >= 0
	case OpGT:
		return sign > 0
	default:
		return false
	}
}

func checkMinInstalled(t *testing.T, rel, name, arch, ver string) {
	t.Helper()
	r := ParseRelation(rel)
	got, gotErr := r.MinInstalled(name, arch, ver)
	if p := relsyntax.Split(rel); p.Form == relsyntax.Operator {
		wantID := name == p.Name && (p.Arch == "" || arch == p.Arch)
		if !wantID {
			if got || gotErr != nil {
				t.Errorf("ParseRelation(%q).MinInstalled(%q, %q, %q) = (%t, %v), want (false, nil)", rel, name, arch, ver, got, gotErr)
			}
			return
		}
		sign, cmpErr := goolib.Compare(ver, p.Ver)
		if (gotErr == nil) != (cmpErr == nil) {
			t.Errorf("ParseRelation(%q).MinInstalled(%q, %q, %q) err = %v, Compare err = %v", rel, name, arch, ver, gotErr, cmpErr)
			return
		}
		if cmpErr != nil {
			if got {
				t.Errorf("ParseRelation(%q).MinInstalled(%q, %q, %q) = true with error %v", rel, name, arch, ver, gotErr)
			}
			return
		}
		if want := oracleOperatorMatch(p.Op, sign); got != want {
			t.Errorf("ParseRelation(%q).MinInstalled(%q, %q, %q) = %t, want %t", rel, name, arch, ver, got, want)
		}
		satOK, satErr := r.Satisfies(ver)
		wantSat := ver == "" || oracleOperatorMatch(p.Op, sign)
		if satErr != nil || satOK != wantSat {
			t.Errorf("ParseRelation(%q).Satisfies(%q) = (%t, %v), want (%t, nil)", rel, ver, satOK, satErr, wantSat)
		}
		return
	}
	want, wantErr := oldMinInstalled(r.PackageInfo(), name, arch, ver)
	if got != want || (gotErr == nil) != (wantErr == nil) {
		t.Errorf("ParseRelation(%q).MinInstalled(%q, %q, %q) = %t, %v; old code returned %t, %v", rel, name, arch, ver, got, gotErr, want, wantErr)
	}
}

func TestMinInstalledMatchesOld(t *testing.T) {
	for _, rel := range relationCorpus {
		for _, name := range nameCorpus {
			for _, arch := range archCorpus {
				for _, ver := range versionCorpus {
					checkMinInstalled(t, rel, name, arch, ver)
				}
			}
		}
	}
}

func checkSatisfies(t *testing.T, req, ver string) {
	t.Helper()
	got, err := Requirement{Ver: req}.Satisfies(ver)
	if want := oldSatisfiesVersion(ver, req); (got && err == nil) != want {
		t.Errorf("Requirement{Ver: %q}.Satisfies(%q) = %t, %v; old code returned %t", req, ver, got, err, want)
	}
	if err != nil && got {
		t.Errorf("Requirement{Ver: %q}.Satisfies(%q) = true with error %v", req, ver, err)
	}
}

func TestSatisfiesMatchesOld(t *testing.T) {
	for _, req := range versionCorpus {
		for _, ver := range versionCorpus {
			checkSatisfies(t, req, ver)
		}
	}
}

func checkSatisfiedBy(t *testing.T, prov, name, ver string) {
	t.Helper()
	got, err := Requirement{Name: name, Ver: ver}.SatisfiedBy(ParseProvide(prov))
	if want := oldSatisfiesProvider(prov, name, ver); (got && err == nil) != want {
		t.Errorf("Requirement{%q, %q}.SatisfiedBy(%q) = %t, %v; old code returned %t", name, ver, prov, got, err, want)
	}
}

func TestSatisfiedByMatchesOld(t *testing.T) {
	for _, prov := range provideCorpus {
		for _, name := range nameCorpus {
			for _, ver := range versionCorpus {
				checkSatisfiedBy(t, prov, name, ver)
			}
		}
	}
}

func TestSameIdentityMatchesOld(t *testing.T) {
	for _, dep := range allRelations() {
		for _, name := range nameCorpus {
			for _, arch := range archCorpus {
				got := ParseDependency(dep, "").SameIdentity(name, arch)
				if want := oldSameIdentity(dep, name, arch); got != want {
					t.Errorf("ParseDependency(%q).SameIdentity(%q, %q) = %t, old code returned %t", dep, name, arch, got, want)
				}
			}
		}
	}
}

func FuzzParseRelation(f *testing.F) {
	for _, s := range fuzzRelationSeeds() {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		r := ParseRelation(s)
		if p := relsyntax.Split(s); p.Form == relsyntax.Operator {
			want := Requirement{Name: p.Name, Arch: p.Arch, Op: p.Op, Ver: p.Ver}
			if r != want {
				t.Errorf("ParseRelation(%q) = %+v, want %+v", s, r, want)
			}
			if rt := ParseRelation(r.Entry()); rt != r {
				t.Errorf("ParseRelation(%q.Entry()=%q) = %+v, want %+v", s, r.Entry(), rt, r)
			}
			return
		}
		pi := goolib.PkgNameSplit(s)
		want := Requirement{Name: pi.Name, Arch: pi.Arch, Op: OpMin, Ver: pi.Ver}
		if r != want {
			t.Errorf("ParseRelation(%q) = %+v, want %+v", s, r, want)
		}
	})
}

func FuzzParseProvide(f *testing.F) {
	for _, s := range provideCorpus {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		name, ver, _ := strings.Cut(s, "=")
		if got, want := ParseProvide(s), (Provide{Name: name, Ver: ver}); got != want {
			t.Errorf("ParseProvide(%q) = %+v, want %+v", s, got, want)
		}
	})
}

func FuzzMinInstalled(f *testing.F) {
	for _, rel := range fuzzRelationSeeds() {
		for _, ver := range versionCorpus {
			f.Add(rel, "foo", "noarch", ver)
		}
	}
	f.Fuzz(checkMinInstalled)
}

func FuzzSatisfies(f *testing.F) {
	for _, req := range versionCorpus {
		for _, ver := range versionCorpus {
			f.Add(req, ver)
		}
	}
	f.Fuzz(checkSatisfies)
}

func FuzzSatisfiedBy(f *testing.F) {
	for _, prov := range provideCorpus {
		for _, ver := range versionCorpus {
			f.Add(prov, "foo", ver)
		}
	}
	f.Fuzz(checkSatisfiedBy)
}

func TestResolve(t *testing.T) {
	spec := func(name, arch, ver string) *goolib.PkgSpec {
		return &goolib.PkgSpec{Name: name, Arch: arch, Version: ver}
	}
	installed := []*goolib.PkgSpec{
		spec("foo", "x86_64", "1"),
		nil,
		spec("bar", "noarch", "1"),
		spec("foo", "noarch", "3"),
		spec("foo", "noarch", "3"),
	}
	for _, tc := range []struct {
		rel     string
		want    []string
		wantErr bool
	}{
		{rel: "foo", want: []string{"foo.noarch.3", "foo.x86_64.1"}},
		{rel: "foo.x86_64", want: []string{"foo.x86_64.1"}},
		{rel: "foo.noarch.2", want: []string{"foo.noarch.3"}},
		{rel: "foo..2", want: []string{"foo.noarch.3"}},
		{rel: "foo.noarch.4"},
		{rel: "baz"},
		{rel: "foo.noarch.x", wantErr: true},
	} {
		got, err := Resolve(ParseRelation(tc.rel), installed)
		if (err != nil) != tc.wantErr {
			t.Errorf("Resolve(%q) error = %v, want error: %t", tc.rel, err, tc.wantErr)
			continue
		}
		var names []string
		for _, ps := range got {
			names = append(names, ps.String())
		}
		if !slices.Equal(names, tc.want) {
			t.Errorf("Resolve(%q) = %v, want %v", tc.rel, names, tc.want)
		}
	}
}

func TestSplitKey(t *testing.T) {
	for _, tc := range []struct {
		key  string
		want goolib.PackageInfo
	}{
		{key: "P.noarch", want: goolib.PackageInfo{Name: "P", Arch: "noarch"}},
		{key: "P.", want: goolib.PackageInfo{Name: "P"}},
		{key: "foo.bar.noarch", want: goolib.PackageInfo{Name: "foo.bar", Arch: "noarch"}},
		{key: "a.b.c.x86_64", want: goolib.PackageInfo{Name: "a.b.c", Arch: "x86_64"}},
		{key: "nodot", want: goolib.PackageInfo{Name: "nodot"}},
	} {
		if got := SplitKey(tc.key); got != tc.want {
			t.Errorf("SplitKey(%q) = %+v, want %+v", tc.key, got, tc.want)
		}
	}
}

func TestFormatHelpers(t *testing.T) {
	for _, tc := range []struct {
		req  Requirement
		want string
	}{
		{req: Requirement{Name: "P"}, want: "P"},
		{req: Requirement{Name: "P", Arch: "x86_64"}, want: "P.x86_64"},
		{req: Requirement{Name: "P", Ver: "2.0"}, want: "P >= 2.0"},
		{req: Requirement{Name: "foo.bar", Arch: "noarch", Ver: "1"}, want: "foo.bar.noarch >= 1"},
	} {
		if got := tc.req.String(); got != tc.want {
			t.Errorf("%+v.String() = %q, want %q", tc.req, got, tc.want)
		}
	}
	specs := []*goolib.PkgSpec{
		{Name: "A", Arch: "noarch", Version: "1"},
		nil,
		{Name: "B", Arch: "x86_64", Version: "2"},
	}
	if got, want := JoinSpecs(specs, ","), "A.noarch.1,B.x86_64.2"; got != want {
		t.Errorf("JoinSpecs(',') = %q, want %q", got, want)
	}
}

// TestReplacesInstalled checks that a list of Replaces entries matches an
// installed package by name, by arch when an entry names one, and at the
// entry's minimum version or later, and that an entry whose version can't be
// compared never matches but doesn't stop the later entries from matching.
func TestReplacesInstalled(t *testing.T) {
	for _, tc := range []struct {
		name    string
		entries []string
		pkg     goolib.PackageInfo
		want    bool
	}{
		{name: "no entries", pkg: goolib.PackageInfo{Name: "P", Arch: "x86_64", Ver: "1.0.0"}},
		{name: "bare name", entries: []string{"P"}, pkg: goolib.PackageInfo{Name: "P", Arch: "x86_64", Ver: "1.0.0"}, want: true},
		{name: "other name", entries: []string{"Q"}, pkg: goolib.PackageInfo{Name: "P", Arch: "x86_64", Ver: "1.0.0"}},
		{name: "name is not a prefix match", entries: []string{"P"}, pkg: goolib.PackageInfo{Name: "PP", Arch: "x86_64", Ver: "1.0.0"}},
		{name: "matching arch", entries: []string{"P.x86_64"}, pkg: goolib.PackageInfo{Name: "P", Arch: "x86_64", Ver: "1.0.0"}, want: true},
		{name: "other arch", entries: []string{"P.x86_32"}, pkg: goolib.PackageInfo{Name: "P", Arch: "x86_64", Ver: "1.0.0"}},
		{name: "minimum version below", entries: []string{"P.x86_64.0.5.0"}, pkg: goolib.PackageInfo{Name: "P", Arch: "x86_64", Ver: "1.0.0"}, want: true},
		{name: "minimum version equal", entries: []string{"P.x86_64.1.0.0"}, pkg: goolib.PackageInfo{Name: "P", Arch: "x86_64", Ver: "1.0.0"}, want: true},
		{name: "minimum version above", entries: []string{"P.x86_64.2.0.0"}, pkg: goolib.PackageInfo{Name: "P", Arch: "x86_64", Ver: "1.0.0"}},
		{name: "any entry matches", entries: []string{"Q", "P.x86_32", "P"}, pkg: goolib.PackageInfo{Name: "P", Arch: "x86_64", Ver: "1.0.0"}, want: true},
		{name: "unparseable entry version", entries: []string{"P.x86_64.bad"}, pkg: goolib.PackageInfo{Name: "P", Arch: "x86_64", Ver: "1.0.0"}},
		{name: "unparseable entry before a match", entries: []string{"P.x86_64.bad", "P"}, pkg: goolib.PackageInfo{Name: "P", Arch: "x86_64", Ver: "1.0.0"}, want: true},
		{name: "unparseable installed version", entries: []string{"P"}, pkg: goolib.PackageInfo{Name: "P", Arch: "x86_64", Ver: "bad"}},
	} {
		if got := ReplacesInstalled(tc.entries, tc.pkg.Name, tc.pkg.Arch, tc.pkg.Ver); got != tc.want {
			t.Errorf("%s: ReplacesInstalled(%q, %s) = %t, want %t", tc.name, tc.entries, tc.pkg, got, tc.want)
		}
	}
}

// TestOperatorZeroAndEquality checks that OpMin is the zero value, all six
// operator constants are distinct, and Requirement equality distinguishes Op.
func TestOperatorZeroAndEquality(t *testing.T) {
	var zero Requirement
	if zero.Op != OpMin || OpMin != 0 {
		t.Fatalf("zero Requirement.Op = %v (OpMin=%v), want 0", zero.Op, OpMin)
	}
	ops := []Op{OpMin, OpLT, OpLE, OpEQ, OpGE, OpGT}
	for i := range ops {
		for j := i + 1; j < len(ops); j++ {
			if ops[i] == ops[j] {
				t.Fatalf("ops[%d] == ops[%d] (%v)", i, j, ops[i])
			}
		}
	}
	rLT := Requirement{Name: "foo", Arch: "noarch", Op: OpLT, Ver: "2.0.0"}
	rLE := Requirement{Name: "foo", Arch: "noarch", Op: OpLE, Ver: "2.0.0"}
	rMin := Requirement{Name: "foo", Arch: "noarch", Op: OpMin, Ver: "2.0.0"}
	rGE := Requirement{Name: "foo", Arch: "noarch", Op: OpGE, Ver: "2.0.0"}
	if rLT == rLE || rMin == rGE || rLT != (Requirement{Name: "foo", Arch: "noarch", Op: OpLT, Ver: "2.0.0"}) {
		t.Fatal("Requirement equality does not distinguish Op values")
	}
}

// TestRequirementEvaluationSignTable checks MinInstalled, Satisfies and
// SatisfiedBy across every version operator, including prerelease, build
// metadata, release suffix, empty version and unparseable version inputs.
func TestRequirementEvaluationSignTable(t *testing.T) {
	ops := []struct {
		name  string
		op    Op
		signs map[int]bool
	}{
		{name: "min", op: OpMin, signs: map[int]bool{0: true, 1: true}},
		{name: "lt", op: OpLT, signs: map[int]bool{-1: true}},
		{name: "le", op: OpLE, signs: map[int]bool{-1: true, 0: true}},
		{name: "eq", op: OpEQ, signs: map[int]bool{0: true}},
		{name: "ge", op: OpGE, signs: map[int]bool{0: true, 1: true}},
		{name: "gt", op: OpGT, signs: map[int]bool{1: true}},
	}
	versions := []struct {
		label       string
		ver         string
		wantSign    int
		unparseable bool
	}{
		{label: "below", ver: "1.0.0", wantSign: -1},
		{label: "equal", ver: "2.0.0", wantSign: 0},
		{label: "release_suffix", ver: "2.0.0@1", wantSign: 1},
		{label: "prerelease", ver: "2.0.0-rc1", wantSign: -1},
		{label: "build_metadata", ver: "2.0.0+build", wantSign: 0},
		{label: "above", ver: "3.0.0", wantSign: 1},
		{label: "zero_prerelease", ver: "0.0.0-rc1", wantSign: -1},
		{label: "empty", ver: "", wantSign: -1},
		{label: "unparseable", ver: "notaver", unparseable: true},
	}

	for _, o := range ops {
		req := Requirement{Name: "foo", Op: o.op, Ver: "2.0.0"}
		for _, v := range versions {
			t.Run(o.name+"/"+v.label, func(t *testing.T) {
				gotMin, minErr := req.MinInstalled("foo", "noarch", v.ver)
				gotSat, satErr := req.Satisfies(v.ver)
				gotProv, provErr := req.SatisfiedBy(Provide{Name: "foo", Ver: v.ver})

				if v.unparseable {
					if minErr == nil || gotMin {
						t.Errorf("MinInstalled(%q) = (%t, %v), want (false, non-nil)", v.ver, gotMin, minErr)
					}
					_, legacyErr := oldMinInstalled(goolib.PackageInfo{Name: "foo", Ver: "2.0.0"}, "foo", "noarch", v.ver)
					if legacyErr == nil || minErr.Error() != legacyErr.Error() {
						t.Errorf("MinInstalled(%q) error = %v, want %v", v.ver, minErr, legacyErr)
					}
					if satErr == nil || gotSat {
						t.Errorf("Satisfies(%q) = (%t, %v), want (false, non-nil)", v.ver, gotSat, satErr)
					}
					if provErr == nil || gotProv {
						t.Errorf("SatisfiedBy(%q) = (%t, %v), want (false, non-nil)", v.ver, gotProv, provErr)
					}
					return
				}

				if cmpSign, err := goolib.Compare(v.ver, "2.0.0"); err != nil || cmpSign != v.wantSign {
					t.Fatalf("goolib.Compare(%q, \"2.0.0\") = (%d, %v), want (%d, nil)", v.ver, cmpSign, err, v.wantSign)
				}
				wantMin := o.signs[v.wantSign]
				if minErr != nil || gotMin != wantMin {
					t.Errorf("MinInstalled(%q) = (%t, %v), want (%t, nil)", v.ver, gotMin, minErr, wantMin)
				}

				wantSat := wantMin
				if v.ver == "" {
					// An empty provided version satisfies every operator without version comparison.
					wantSat = true
				}
				if satErr != nil || gotSat != wantSat {
					t.Errorf("Satisfies(%q) = (%t, %v), want (%t, nil)", v.ver, gotSat, satErr, wantSat)
				}
				if provErr != nil || gotProv != wantSat {
					t.Errorf("SatisfiedBy(%q) = (%t, %v), want (%t, nil)", v.ver, gotProv, provErr, wantSat)
				}
			})
		}
	}

	// Verify OpMin and OpGE produce identical evaluations over the entire version corpus.
	for _, reqVer := range versionCorpus {
		for _, ver := range versionCorpus {
			rMin := Requirement{Name: "foo", Arch: "noarch", Op: OpMin, Ver: reqVer}
			rGE := Requirement{Name: "foo", Arch: "noarch", Op: OpGE, Ver: reqVer}
			minOK, minErr := rMin.MinInstalled("foo", "noarch", ver)
			geOK, geErr := rGE.MinInstalled("foo", "noarch", ver)
			if minOK != geOK || (minErr == nil) != (geErr == nil) {
				t.Errorf("MinInstalled(%q vs %q): OpMin=(%t,%v), OpGE=(%t,%v)", reqVer, ver, minOK, minErr, geOK, geErr)
			}
			satMin, satMinErr := rMin.Satisfies(ver)
			satGE, satGEErr := rGE.Satisfies(ver)
			if satMin != satGE || (satMinErr == nil) != (satGEErr == nil) {
				t.Errorf("Satisfies(%q vs %q): OpMin=(%t,%v), OpGE=(%t,%v)", reqVer, ver, satMin, satMinErr, satGE, satGEErr)
			}
		}
	}

	// Verify empty requirement version short-circuits Satisfies for every operator, even on unparseable input.
	for _, o := range ops {
		r := Requirement{Name: "foo", Op: o.op}
		if ok, err := r.Satisfies("notaver"); !ok || err != nil {
			t.Errorf("Requirement{Op: %s}.Satisfies(\"notaver\") = (%t, %v), want (true, nil)", o.name, ok, err)
		}
		if ok, err := r.MinInstalled("bar", "noarch", "notaver"); ok || err != nil {
			t.Errorf("Requirement{Name: \"foo\", Op: %s}.MinInstalled(\"bar\") = (%t, %v), want (false, nil)", o.name, ok, err)
		}
	}

	// Verify argument order in MinInstalled preserves the legacy error when both versions fail to parse.
	_, wantOrderErr := oldMinInstalled(goolib.PackageInfo{Name: "foo", Ver: "badreq"}, "foo", "noarch", "badinst")
	for _, o := range ops {
		got, err := (Requirement{Name: "foo", Op: o.op, Ver: "badreq"}).MinInstalled("foo", "noarch", "badinst")
		if got || err == nil || err.Error() != wantOrderErr.Error() {
			t.Errorf("Op %s MinInstalled(badreq, badinst) = (%t, %v), want (false, %v)", o.name, got, err, wantOrderErr)
		}
	}

	// Verify an out-of-range operator value never matches non-empty versions.
	badOp := Requirement{Name: "foo", Op: Op(99), Ver: "2.0.0"}
	if ok, err := badOp.MinInstalled("foo", "noarch", "2.0.0"); ok || err != nil {
		t.Errorf("Op(99).MinInstalled = (%t, %v), want (false, nil)", ok, err)
	}
	if ok, err := badOp.Satisfies("2.0.0"); ok || err != nil {
		t.Errorf("Op(99).Satisfies = (%t, %v), want (false, nil)", ok, err)
	}
}

// TestEntryAndStringFormatting checks that OpMin formatting stays
// byte-identical to PackageInfo.String and FormatPackageRequirement, while
// non-OpMin operators format compactly in Entry and with spaces in String.
func TestEntryAndStringFormatting(t *testing.T) {
	legacyInputs := append(slices.Clone(relationCorpus), "A..3", "A.noarch.3", "foo.bar.x86_64.1.2.3@4")
	for _, raw := range legacyInputs {
		pi := goolib.PkgNameSplit(raw)
		r := Requirement{Name: pi.Name, Arch: pi.Arch, Op: OpMin, Ver: pi.Ver}
		if got, want := r.Entry(), r.PackageInfo().String(); got != want {
			t.Errorf("OpMin Entry(%+v) = %q, want %q", r, got, want)
		}
		if got, want := r.String(), FormatPackageRequirement(r.PackageInfo()); got != want {
			t.Errorf("OpMin String(%+v) = %q, want %q", r, got, want)
		}
	}
	if got := (Requirement{Name: "A", Ver: "3"}).Entry(); got != "A" {
		t.Errorf("Requirement{Name: \"A\", Ver: \"3\"}.Entry() = %q, want \"A\"", got)
	}

	for _, tc := range []struct {
		name       string
		req        Requirement
		wantEntry  string
		wantString string
	}{
		{
			name:       "lt_archless",
			req:        Requirement{Name: "foo", Op: OpLT, Ver: "2.0.0"},
			wantEntry:  "foo<2.0.0",
			wantString: "foo < 2.0.0",
		},
		{
			name:       "le_arch",
			req:        Requirement{Name: "foo", Arch: "x86_64", Op: OpLE, Ver: "2.0.0"},
			wantEntry:  "foo.x86_64<=2.0.0",
			wantString: "foo.x86_64 <= 2.0.0",
		},
		{
			name:       "eq_release",
			req:        Requirement{Name: "foo", Op: OpEQ, Ver: "1.0.0@2"},
			wantEntry:  "foo=1.0.0@2",
			wantString: "foo = 1.0.0@2",
		},
		{
			name:       "ge_noarch",
			req:        Requirement{Name: "foo", Arch: "noarch", Op: OpGE, Ver: "2.0.0-rc1"},
			wantEntry:  "foo.noarch>=2.0.0-rc1",
			wantString: "foo.noarch >= 2.0.0-rc1",
		},
		{
			name:       "gt_archless",
			req:        Requirement{Name: "foo", Op: OpGT, Ver: "3.0.0+build"},
			wantEntry:  "foo>3.0.0+build",
			wantString: "foo > 3.0.0+build",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.req.Entry(); got != tc.wantEntry {
				t.Errorf("%+v.Entry() = %q, want %q", tc.req, got, tc.wantEntry)
			}
			if got := tc.req.String(); got != tc.wantString {
				t.Errorf("%+v.String() = %q, want %q", tc.req, got, tc.wantString)
			}
		})
	}

	// Verify Resolve uses Entry() in its version comparison error message.
	installed := []*goolib.PkgSpec{{Name: "foo", Arch: "noarch", Version: "notaver"}}
	_, err := Resolve(Requirement{Name: "foo", Op: OpLT, Ver: "2.0.0"}, installed)
	if err == nil || !strings.Contains(err.Error(), "against foo<2.0.0:") {
		t.Errorf("Resolve operator error = %v, want substring \"against foo<2.0.0:\"", err)
	}
	_, legacyErr := Resolve(Requirement{Name: "foo", Op: OpMin, Ver: "3"}, installed)
	if legacyErr == nil || !strings.Contains(legacyErr.Error(), "against foo:") {
		t.Errorf("Resolve OpMin error = %v, want substring \"against foo:\"", legacyErr)
	}
}

// TestProvidedBy checks nil handling, match precedence over unparseable
// sibling Provides entries, and first-error reporting when nothing matches.
func TestProvidedBy(t *testing.T) {
	req := Requirement{Name: "foo", Op: OpLT, Ver: "2.0.0"}
	if ok, err := ProvidedBy(nil, req); ok || err != nil {
		t.Fatalf("ProvidedBy(nil) = (%t, %v), want (false, nil)", ok, err)
	}
	if ok, err := ProvidedBy(&goolib.PkgSpec{Name: "p"}, req); ok || err != nil {
		t.Fatalf("ProvidedBy(empty) = (%t, %v), want (false, nil)", ok, err)
	}
	matchAfterErr := &goolib.PkgSpec{
		Name:     "p",
		Provides: []string{"other=1.0.0", "foo=badver", "foo=1.5.0"},
	}
	if ok, err := ProvidedBy(matchAfterErr, req); !ok || err != nil {
		t.Errorf("ProvidedBy(match after badver) = (%t, %v), want (true, nil)", ok, err)
	}
	unversioned := &goolib.PkgSpec{
		Name:     "p",
		Provides: []string{"foo"},
	}
	if ok, err := ProvidedBy(unversioned, req); !ok || err != nil {
		t.Errorf("ProvidedBy(unversioned) = (%t, %v), want (true, nil)", ok, err)
	}
	twoErrors := &goolib.PkgSpec{
		Name:     "p",
		Provides: []string{"foo=badfirst", "foo=3.0.0", "foo=badsecond"},
	}
	ok, err := ProvidedBy(twoErrors, req)
	if ok || err == nil || !strings.Contains(err.Error(), "badfirst") {
		t.Errorf("ProvidedBy(two errors) = (%t, %v), want (false, error mentioning badfirst)", ok, err)
	}
}

// malformedOperatorCorpus holds inputs containing operator runes that fail the
// operator grammar and must fall back to goolib.PkgNameSplit.
var malformedOperatorCorpus = []string{
	"=", "<", ">", "<=", ">=",
	"foo<", "foo<=", "foo=", "foo>=", "foo>",
	"<2", "<=2", "=2", ">=2", ">2",
	"foo==2", "foo=<2", "foo=>2", "foo<>2", "foo><2", "foo< =2", "foo> =2",
	"foo<2<3", "foo=1=2", "a.b.c<2", "foo.<2", ".noarch<2",
	"foo bar<2", "foo .noarch<2", "foo. noarch<2", "foo<2 3",
	"foo\u00a0bar<2", "foo<2\u00853", "   <2", "foo<   ",
}

// TestEntryOperatorRoundTrip checks that Entry round-trips through
// ParseRelation for every operator requirement.
func TestEntryOperatorRoundTrip(t *testing.T) {
	for _, op := range []Op{OpLT, OpLE, OpEQ, OpGE, OpGT} {
		for _, arch := range []string{"", "noarch", "x86_64"} {
			for _, ver := range []string{"2.0.0", "2.0.0-rc1", "2.0.0+build", "2.5.1@3"} {
				want := Requirement{Name: "foo", Arch: arch, Op: op, Ver: ver}
				if got := ParseRelation(want.Entry()); got != want {
					t.Errorf("ParseRelation(%q) = %+v, want %+v", want.Entry(), got, want)
				}
			}
		}
	}
}

// TestLegacyAndMalformedDifferential checks that every operator-free and
// malformed input parses identically to PkgNameSplit with OpMin and evaluates
// MinInstalled identically to oldMinInstalled.
func TestLegacyAndMalformedDifferential(t *testing.T) {
	inputs := slices.Concat(relationCorpus, malformedOperatorCorpus)
	for _, s := range inputs {
		form := relsyntax.Split(s).Form
		if form == relsyntax.Operator {
			t.Fatalf("Split(%q).Form = Operator, want Legacy or Malformed", s)
		}
		pi := goolib.PkgNameSplit(s)
		want := Requirement{Name: pi.Name, Arch: pi.Arch, Op: OpMin, Ver: pi.Ver}
		if got := ParseRelation(s); got != want {
			t.Errorf("ParseRelation(%q) = %+v, want %+v", s, got, want)
		}
		for _, name := range nameCorpus {
			for _, arch := range archCorpus {
				for _, ver := range versionCorpus {
					checkMinInstalled(t, s, name, arch, ver)
				}
			}
		}
	}
}

// TestFuzzRelationSeedCoverage checks that fuzzRelationSeeds exercises Legacy,
// unspaced Operator, spaced Operator and Malformed inputs.
func TestFuzzRelationSeedCoverage(t *testing.T) {
	var sawLegacy, sawUnspaced, sawSpaced, sawMalformed bool
	for _, s := range fuzzRelationSeeds() {
		p := relsyntax.Split(s)
		switch p.Form {
		case relsyntax.Legacy:
			sawLegacy = true
		case relsyntax.Operator:
			if p.Spaced {
				sawSpaced = true
			} else {
				sawUnspaced = true
			}
		case relsyntax.Malformed:
			sawMalformed = true
		}
	}
	if !sawLegacy || !sawUnspaced || !sawSpaced || !sawMalformed {
		t.Fatalf("relation fuzz seed coverage incomplete: legacy=%t unspaced=%t spaced=%t malformed=%t",
			sawLegacy, sawUnspaced, sawSpaced, sawMalformed)
	}
}

// TestOperatorConstantsMatchRelsyntax checks that the re-exported operator
// constants in relation match internal/relsyntax.
func TestOperatorConstantsMatchRelsyntax(t *testing.T) {
	if OpMin != relsyntax.OpNone ||
		OpLT != relsyntax.OpLT ||
		OpLE != relsyntax.OpLE ||
		OpEQ != relsyntax.OpEQ ||
		OpGE != relsyntax.OpGE ||
		OpGT != relsyntax.OpGT {
		t.Fatal("relation Op constants do not match relsyntax Op constants")
	}
}
