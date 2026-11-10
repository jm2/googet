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
