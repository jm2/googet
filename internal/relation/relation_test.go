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
		"foo<2.0.0", "foo.x86_64<2.0.0", "foo >= 1", "foo.noarch >= 1", "foo=1", "foo.noarch=1",
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

func TestParseRelationMatchesPkgNameSplit(t *testing.T) {
	for _, s := range relationCorpus {
		if got, want := ParseRelation(s).PackageInfo(), goolib.PkgNameSplit(s); got != want {
			t.Errorf("ParseRelation(%q) = %+v, want %+v", s, got, want)
		}
	}
}

func TestParseDependency(t *testing.T) {
	for _, key := range relationCorpus {
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

func checkMinInstalled(t *testing.T, rel, name, arch, ver string) {
	t.Helper()
	r := ParseRelation(rel)
	got, gotErr := r.MinInstalled(name, arch, ver)
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
	for _, dep := range relationCorpus {
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
	for _, s := range relationCorpus {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		if got, want := ParseRelation(s).PackageInfo(), goolib.PkgNameSplit(s); got != want {
			t.Errorf("ParseRelation(%q) = %+v, want %+v", s, got, want)
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
	for _, rel := range relationCorpus {
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
