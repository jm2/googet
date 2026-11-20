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

// validRelationToken reports whether goolib accepts s as a relation token
// without forbidden syntax characters by probing PkgSpec.Name validation.
func validRelationToken(s string) bool {
	return len((&goolib.PkgSpec{Name: s}).RelationProblems()) == 0
}

// checkRelationParity asserts that PkgSpec.RelationProblems agrees with
// ParseRelation and Requirement.SameIdentity on a single Conflicts or Replaces
// entry.
func checkRelationParity(t *testing.T, entry string) {
	t.Helper()
	validArchs := goolib.ValidArchs()
	psConflicts := &goolib.PkgSpec{Name: "holder", Arch: "noarch", Version: "1.0.0", Conflicts: []string{entry}}
	psReplaces := &goolib.PkgSpec{Name: "holder", Arch: "noarch", Version: "1.0.0", Replaces: []string{entry}}

	cProbs := psConflicts.RelationProblems()
	rProbs := psReplaces.RelationProblems()

	r := ParseRelation(entry)
	trimmed := strings.TrimSpace(entry)
	_, verErr := goolib.ParseVersion(r.Ver)
	wantConflictsClean := r.Name != "" &&
		validRelationToken(r.Name) &&
		validRelationToken(r.Arch) &&
		validRelationToken(r.Ver) &&
		(r.Arch == "" || slices.Contains(validArchs, r.Arch)) &&
		(r.Ver == "" || verErr == nil) &&
		r.PackageInfo().String() == trimmed
	wantReplacesClean := wantConflictsClean && !r.SameIdentity(psReplaces.Name, psReplaces.Arch)

	if (len(cProbs) == 0) != wantConflictsClean {
		t.Errorf("Conflicts(%q) -> %+v: RelationProblems clean=%t (probs=%v), want %t", entry, r, len(cProbs) == 0, cProbs, wantConflictsClean)
	}
	if (len(rProbs) == 0) != wantReplacesClean {
		t.Errorf("Replaces(%q) -> %+v: RelationProblems clean=%t (probs=%v), want %t", entry, r, len(rProbs) == 0, rProbs, wantReplacesClean)
	}
}

// checkDependencyParity asserts that PkgSpec.RelationProblems agrees with
// ParseDependency on a single PkgDependencies key.
func checkDependencyParity(t *testing.T, key string) {
	t.Helper()
	validArchs := goolib.ValidArchs()
	ps := &goolib.PkgSpec{
		Name:            "holder",
		Arch:            "noarch",
		Version:         "1.0.0",
		PkgDependencies: map[string]string{key: "1.0.0"},
	}
	probs := ps.RelationProblems()
	r := ParseDependency(key, "1.0.0")
	trimmed := strings.TrimSpace(key)
	reconstructed := goolib.PackageInfo{Name: r.Name, Arch: r.Arch}.String()
	wantClean := r.Name != "" &&
		validRelationToken(r.Name) &&
		validRelationToken(r.Arch) &&
		(r.Arch == "" || slices.Contains(validArchs, r.Arch)) &&
		reconstructed == trimmed

	if (len(probs) == 0) != wantClean {
		t.Errorf("ParseDependency(%q) = %+v: RelationProblems clean=%t (probs=%v), want %t", key, r, len(probs) == 0, probs, wantClean)
	}
}

// checkProvideParity asserts that PkgSpec.RelationProblems agrees with
// ParseProvide on a single Provides entry.
func checkProvideParity(t *testing.T, entry string) {
	t.Helper()
	ps := &goolib.PkgSpec{
		Name:     "holder",
		Arch:     "noarch",
		Version:  "1.0.0",
		Provides: []string{entry},
	}
	probs := ps.RelationProblems()
	p := ParseProvide(entry)
	hasEq := strings.Contains(entry, "=")
	_, verErr := goolib.ParseVersion(p.Ver)
	reconstructed := p.Name
	if hasEq {
		reconstructed = p.Name + "=" + p.Ver
	}
	wantClean := p.Name != "" &&
		validRelationToken(p.Name) &&
		(!hasEq || (p.Ver != "" && validRelationToken(p.Ver) && verErr == nil)) &&
		reconstructed == entry

	if (len(probs) == 0) != wantClean {
		t.Errorf("ParseProvide(%q) = %+v: RelationProblems clean=%t (probs=%v), want %t", entry, p, len(probs) == 0, probs, wantClean)
	}
}

// TestRelationProblemsParity checks differential parity between
// PkgSpec.RelationProblems and ParseRelation, ParseDependency, and ParseProvide.
func TestRelationProblemsParity(t *testing.T) {
	extraRelations := []string{
		"foo.noarch.", "foo..1.0", "foo.x86_64.1.2.3@4", "gwindows-updater.noarch",
		"\tfoo.arm64.0.0.0-rc1\n", "foo\tbar", "foo.noarch=1.0", "holder", "holder.x86_64",
	}
	for _, s := range append(slices.Clone(relationCorpus), extraRelations...) {
		checkRelationParity(t, s)
		checkDependencyParity(t, s)
	}
	for _, s := range append(slices.Clone(provideCorpus), "virt=1.2.3@4", "virt=\t1.0", "virt\n=1.0") {
		checkProvideParity(t, s)
	}
}

func FuzzRelationProblemsParity(f *testing.F) {
	for _, s := range relationCorpus {
		f.Add(s)
	}
	for _, s := range provideCorpus {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		checkRelationParity(t, s)
		checkDependencyParity(t, s)
		checkProvideParity(t, s)
	})
}
