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

// PackageInfo returns r's fields as a PackageInfo.
func (r Requirement) PackageInfo() goolib.PackageInfo {
	return goolib.PackageInfo{Name: r.Name, Arch: r.Arch, Ver: r.Ver}
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
