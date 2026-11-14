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

// Package relation parses and evaluates the relations between packages that
// a package spec declares: Conflicts, Replaces, PkgDependencies and Provides.
//
// Every relation that names a package is a Requirement: a name, an optional
// arch and an optional minimum version. A Provides entry is a Provide: a name
// the package also answers to, with an optional version.
package relation

import (
	"cmp"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/google/googet/v2/goolib"
)

// Requirement names a package, and optionally an arch and a minimum version.
// An empty Arch matches any arch.
type Requirement struct {
	Name, Arch, Ver string
}

// Provide is a name, and optionally a version, that a package provides in
// addition to its own name.
type Provide struct {
	Name, Ver string
}

// ParseRelation parses a Conflicts or Replaces entry of the form
// name[.arch[.version]]. A version is a minimum.
func ParseRelation(s string) Requirement {
	pi := goolib.PkgNameSplit(s)
	return Requirement{Name: pi.Name, Arch: pi.Arch, Ver: pi.Ver}
}

// ParseDependency parses a PkgDependencies entry, whose key has the form
// name[.arch] and whose value is the minimum version. A version in the key is
// ignored in favour of the value.
func ParseDependency(key, minVer string) Requirement {
	pi := goolib.PkgNameSplit(key)
	return Requirement{Name: pi.Name, Arch: pi.Arch, Ver: minVer}
}

// ParseProvide parses a Provides entry of the form name[=version].
func ParseProvide(s string) Provide {
	name, ver, _ := strings.Cut(s, "=")
	return Provide{Name: name, Ver: ver}
}

// SplitKey splits a name.arch package key, as remove.DepMap uses, into its
// name and arch at the last dot. Architectures never contain a dot, so a
// package name that does is kept whole. A key without a dot is a name with no
// arch.
func SplitKey(key string) goolib.PackageInfo {
	i := strings.LastIndex(key, ".")
	if i < 0 {
		return goolib.PackageInfo{Name: key}
	}
	return goolib.PackageInfo{Name: key[:i], Arch: key[i+1:]}
}

// PackageInfo returns r's fields as a PackageInfo.
func (r Requirement) PackageInfo() goolib.PackageInfo {
	return goolib.PackageInfo{Name: r.Name, Arch: r.Arch, Ver: r.Ver}
}

// String formats r as a human-readable requirement clause ("name[.arch][ >= ver]").
func (r Requirement) String() string {
	return FormatPackageRequirement(r.PackageInfo())
}

// FormatPackageRequirement formats pi as a requirement clause ("name[.arch][ >= ver]").
func FormatPackageRequirement(pi goolib.PackageInfo) string {
	name := pi.Name
	if pi.Arch != "" {
		name += "." + pi.Arch
	}
	if pi.Ver != "" {
		return name + " >= " + pi.Ver
	}
	return name
}

// JoinSpecs formats non-nil specs with sep between name.arch.version entries.
func JoinSpecs(specs []*goolib.PkgSpec, sep string) string {
	parts := make([]string, 0, len(specs))
	for _, ps := range specs {
		if ps != nil {
			parts = append(parts, ps.String())
		}
	}
	return strings.Join(parts, sep)
}

// SameIdentity reports whether a package with the given name and arch is the
// package r names, ignoring versions.
func (r Requirement) SameIdentity(name, arch string) bool {
	return name == r.Name && (r.Arch == "" || arch == r.Arch)
}

// MinInstalled reports whether an installed package with the given name, arch
// and version is the package r names at r's minimum version or later. The
// versions are always compared, with an empty version reading as 0.0.0, so it
// returns an error if either version can't be parsed.
func (r Requirement) MinInstalled(name, arch, ver string) (bool, error) {
	if !r.SameIdentity(name, arch) {
		return false, nil
	}
	c, err := goolib.Compare(r.Ver, ver)
	if err != nil {
		return false, err
	}
	return c < 1, nil
}

// ReplacesInstalled reports whether any of the Replaces entries names an
// installed package with the given name, arch and version, at the entry's
// minimum version or later. An entry whose version can't be compared with
// ver doesn't match.
func ReplacesInstalled(entries []string, name, arch, ver string) bool {
	for _, raw := range entries {
		if ok, err := ParseRelation(raw).MinInstalled(name, arch, ver); err == nil && ok {
			return true
		}
	}
	return false
}

// Satisfies reports whether version ver meets r's minimum version. An empty
// minimum or an empty ver always does, without parsing either version.
func (r Requirement) Satisfies(ver string) (bool, error) {
	if r.Ver == "" || ver == "" {
		return true, nil
	}
	c, err := goolib.Compare(ver, r.Ver)
	if err != nil {
		return false, err
	}
	return c >= 0, nil
}

// SatisfiedBy reports whether provide p satisfies r: p has r's name and its
// version satisfies r. r's arch is ignored, and a provide without a version
// satisfies any minimum version.
func (r Requirement) SatisfiedBy(p Provide) (bool, error) {
	if p.Name != r.Name {
		return false, nil
	}
	return r.Satisfies(p.Ver)
}

// Resolve returns every spec in installed that r names at r's minimum version
// or later, using MinInstalled, ordered by name and then arch. Nil specs are
// skipped, and a spec listed more than once is returned once. It returns an
// error if a version can't be compared.
func Resolve(r Requirement, installed []*goolib.PkgSpec) ([]*goolib.PkgSpec, error) {
	var out []*goolib.PkgSpec
	for _, ps := range installed {
		if ps == nil {
			continue
		}
		ok, err := r.MinInstalled(ps.Name, ps.Arch, ps.Version)
		if err != nil {
			return nil, fmt.Errorf("checking %s against %s: %v", ps, r.PackageInfo(), err)
		}
		if ok && !slices.ContainsFunc(out, func(o *goolib.PkgSpec) bool { return o.Name == ps.Name && o.Arch == ps.Arch }) {
			out = append(out, ps)
		}
	}
	slices.SortFunc(out, compareSpecs)
	return out, nil
}

// Satisfied reports whether any non-nil spec in specs satisfies the dependency
// requirement r, either by package identity at r's minimum version or later
// (using Resolve) or through a Provides entry. A version comparison error on a
// matching package identity is returned; a malformed version in a Provides
// entry does not satisfy r and returns no error.
func Satisfied(r Requirement, specs []*goolib.PkgSpec) (bool, error) {
	rows, err := Resolve(r, specs)
	if err != nil {
		return false, err
	}
	if len(rows) > 0 {
		return true, nil
	}
	for _, ps := range specs {
		if ps == nil {
			continue
		}
		if providesRequirement(ps, r) {
			return true, nil
		}
	}
	return false, nil
}

// Orphan describes one dependency requirement of a remaining package that
// removing a set of packages leaves unsatisfied.
type Orphan struct {
	// Dependent is the remaining package whose dependency becomes unsatisfied.
	Dependent *goolib.PkgSpec
	// Requirement is the unsatisfied dependency requirement.
	Requirement Requirement
	// Removed lists the unique packages in removing, ordered by name and arch,
	// that satisfied Requirement before removal.
	Removed []*goolib.PkgSpec
	// ByName reports whether any package in removing satisfied Requirement by
	// package identity rather than solely through Provides.
	ByName bool
}

// Unsatisfied returns every dependency requirement of a package in remaining
// that at least one package in removing satisfied and that no package in
// remaining still satisfies. Nil specs are skipped, duplicate (name, arch)
// specs are counted once, and results are ordered by dependent (name, arch)
// and then requirement (name, arch, version). It returns an error if an
// identity version comparison fails.
func Unsatisfied(removing, remaining []*goolib.PkgSpec) ([]Orphan, error) {
	remUnique := uniqueSpecs(removing)
	remLeft := uniqueSpecs(remaining)
	if len(remUnique) == 0 || len(remLeft) == 0 {
		return nil, nil
	}
	var out []Orphan
	for _, dep := range remLeft {
		var seen []Requirement
		for _, key := range slices.Sorted(maps.Keys(dep.PkgDependencies)) {
			req := ParseDependency(key, dep.PkgDependencies[key])
			if slices.Contains(seen, req) {
				continue
			}
			seen = append(seen, req)

			byIdentity, err := Resolve(req, remUnique)
			if err != nil {
				return nil, err
			}
			removed := slices.Clone(byIdentity)
			for _, ps := range remUnique {
				if providesRequirement(ps, req) && !containsSpec(removed, ps) {
					removed = append(removed, ps)
				}
			}
			if len(removed) == 0 {
				continue
			}
			ok, err := Satisfied(req, remLeft)
			if err != nil {
				return nil, err
			}
			if ok {
				continue
			}
			slices.SortFunc(removed, compareSpecs)
			out = append(out, Orphan{
				Dependent:   dep,
				Requirement: req,
				Removed:     removed,
				ByName:      len(byIdentity) > 0,
			})
		}
	}
	slices.SortFunc(out, func(a, b Orphan) int {
		return cmp.Or(
			compareSpecs(a.Dependent, b.Dependent),
			strings.Compare(a.Requirement.Name, b.Requirement.Name),
			strings.Compare(a.Requirement.Arch, b.Requirement.Arch),
			strings.Compare(a.Requirement.Ver, b.Requirement.Ver),
		)
	})
	return out, nil
}

// providesRequirement reports whether ps declares a Provides entry that
// satisfies r without a version parse error.
func providesRequirement(ps *goolib.PkgSpec, r Requirement) bool {
	for _, raw := range ps.Provides {
		ok, err := r.SatisfiedBy(ParseProvide(raw))
		if err == nil && ok {
			return true
		}
	}
	return false
}

// uniqueSpecs returns the non-nil specs in in, keeping the first occurrence of
// each (name, arch) pair and sorting by name and then arch.
func uniqueSpecs(in []*goolib.PkgSpec) []*goolib.PkgSpec {
	var out []*goolib.PkgSpec
	for _, ps := range in {
		if ps != nil && !containsSpec(out, ps) {
			out = append(out, ps)
		}
	}
	slices.SortFunc(out, compareSpecs)
	return out
}

// containsSpec reports whether list already holds a spec with ps's name and arch.
func containsSpec(list []*goolib.PkgSpec, ps *goolib.PkgSpec) bool {
	return slices.ContainsFunc(list, func(o *goolib.PkgSpec) bool {
		return o.Name == ps.Name && o.Arch == ps.Arch
	})
}

// compareSpecs orders specs lexicographically by name and then arch.
func compareSpecs(a, b *goolib.PkgSpec) int {
	return cmp.Or(strings.Compare(a.Name, b.Name), strings.Compare(a.Arch, b.Arch))
}
