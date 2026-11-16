/*
Copyright 2016 Google Inc. All Rights Reserved.
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

package goolib

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"maps"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"text/template"
	"time"
	"unicode"

	"github.com/blang/semver"
	"github.com/google/googet/v2/internal/relsyntax"
	"github.com/google/googet/v2/priority"
	"github.com/olekukonko/tablewriter/pkg/twwarp"
)

type build struct {
	Windows, Linux         string
	WindowsArgs, LinuxArgs []string
}

// PkgSources is a list of includes, excludes and their target in the package.
type PkgSources struct {
	Include, Exclude []string
	Target, Root     string
}

// GooSpec is the build specification for a package.
type GooSpec struct {
	Build       build
	Sources     []PkgSources
	PackageSpec *PkgSpec
}

// RepoSpec is the repository specification of a package.
type RepoSpec struct {
	Checksum, Source string
	PackageSpec      *PkgSpec
}

// Marshal returns the formatted RepoSpec.
func (rs *RepoSpec) Marshal() ([]byte, error) {
	return json.MarshalIndent(rs, "", "  ")
}

const (
	pkgSpecSuffix   = ".pkgspec"
	maxTagKeyLen    = 127
	maxTagValueSize = 1024 * 10 // 10k
)

var validArch = []string{"noarch", "x86_64", "x86_32", "arm", "arm64"}

// ValidArchs returns a copy of the architecture names recognized by GooGet
// package specifications.
func ValidArchs() []string {
	return slices.Clone(validArch)
}

// PkgSpec is an individual package specification.
type PkgSpec struct {
	Name    string
	Version string
	Arch    string
	// LockArch, if true, prevents upgrading an installed package to a different architecture
	// unless the newer candidate version sets LockArch to false (or leaves it default).
	LockArch        bool              `json:",omitempty"`
	ReleaseNotes    []string          `json:",omitempty"`
	Description     string            `json:",omitempty"`
	License         string            `json:",omitempty"`
	Authors         string            `json:",omitempty"`
	Owners          string            `json:",omitempty"`
	Source          string            `json:",omitempty"`
	Tags            map[string][]byte `json:",omitempty"`
	PkgDependencies map[string]string `json:",omitempty"`
	Replaces        []string
	Conflicts       []string
	Provides        []string
	Install         ExecFile
	Uninstall       ExecFile
	Verify          ExecFile
	Files           map[string]string `json:",omitempty"`
	// The Display Name of the entry in "Add/Remove programs" for matching
	ExternalProgramName string
	// ExecutableNames contains binary executable filenames (e.g. "excel.exe" or "chrome.exe").
	ExecutableNames []string `json:",omitempty"`
	// InstallPath contains default target installation directory path (e.g. "C:\Program Files\Vendor").
	InstallPath string `json:",omitempty"`
}

func (ps PkgSpec) String() string {
	return fmt.Sprintf("%s.%s.%s", ps.Name, ps.Arch, ps.Version)
}

// ExecFile contains info involved in running a script or binary file.
type ExecFile struct {
	Path      string   `json:",omitempty"`
	Args      []string `json:",omitempty"`
	ExitCodes []int    `json:",omitempty"`
}

// Version contains the semver version as well as the GsVer.
// Semver is semantic versioning version.
// GsVer is a GooSpec version number (usually version of installer).
type Version struct {
	Semver semver.Version
	GsVer  int64
}

// Ver returns the goospec version.
func (gs GooSpec) Ver() (Version, error) {
	return ParseVersion(gs.PackageSpec.Version)
}

func (gs GooSpec) verify() error {
	return gs.PackageSpec.verify()
}

func (gs GooSpec) normalize() {
	gs.PackageSpec.normalize()
}

// Compare compares string versions of packages v1 to v2:
// -1 == v1 is less than v2
// 0 == v1 is equal to v2
// 1 == v1 is greater than v2
func Compare(v1, v2 string) (int, error) {
	pv1, err := ParseVersion(v1)
	if err != nil {
		return 0, err
	}
	pv2, err := ParseVersion(v2)
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

func fixVer(ver string) string {
	suffix := ""
	// Patch number can contain PreRelease/Build meta data suffix.
	if i := strings.IndexAny(ver, "+-"); i != -1 {
		suffix = ver[i:]
		ver = ver[:i]
	}
	out := []string{"0", "0", "0"}
	nums := strings.SplitN(ver, ".", 3)
	for i, str := range nums {
		trimmed := strings.TrimLeft(str, "0")
		if trimmed == "" {
			trimmed = "0"
		}
		out[i] = trimmed
	}
	return strings.Join(out, ".") + suffix
}

// ComparePriorityVersion compares (p1, v1) to (p2, v2) as priority-version tuples.
func ComparePriorityVersion(p1 priority.Value, v1 string, p2 priority.Value, v2 string) (int, error) {
	if p1 < p2 {
		return -1, nil
	}
	if p1 > p2 {
		return 1, nil
	}
	return Compare(v1, v2)
}

// ParseVersion parses the string version into goospec.Version. ParseVersion
// attempts to fix non-compliant Semver strings by removing leading zeros from
// components, and replacing any missing components with zero values after
// using existing components for the least significant components first (i.e.
// "1" will become "0.0.1", not "1.0.0").
func ParseVersion(ver string) (Version, error) {
	v := strings.SplitN(ver, "@", 2)
	v[0] = fixVer(v[0])
	sv, err := semver.ParseTolerant(v[0])
	if err != nil {
		return Version{}, err
	}
	version := Version{Semver: sv}
	if len(v) == 2 {
		gv, err := strconv.ParseInt(v[1], 10, 64)
		if err != nil {
			return version, err
		}
		version = Version{
			Semver: sv,
			GsVer:  gv,
		}
	} else {
		version = Version{Semver: sv}
	}
	return version, nil
}

// Versions contains a list of goospec string versions.
type Versions []string

func (s Versions) Len() int {
	return len(s)
}

func (s Versions) Swap(i, j int) {
	s[i], s[j] = s[j], s[i]
}

func (s Versions) Less(i, j int) bool {
	c, err := Compare(s[i], s[j])
	if err != nil {
		fmt.Fprintf(os.Stderr, "Compare failed, %q or %s are not a proper version", s[i], s[j])
	}
	return c == -1
}

// SortVersions sorts a list of goospec string versions.
func SortVersions(versions []string) []string {
	var vl []string
	for i, v := range versions {
		if _, err := ParseVersion(v); err != nil {
			fmt.Fprintf(os.Stderr, "Removing %q from list: %v", v, err)
			continue
		}
		vl = append(vl, versions[i])
	}

	sort.Sort(Versions(vl))
	return vl
}

func jsonError(data []byte, err error) error {
	// If this is a syntax error return a useful error.
	sErr, ok := err.(*json.SyntaxError)
	if !ok {
		return err
	}

	// Byte number where the error line starts.
	start := bytes.LastIndex(data[:sErr.Offset], []byte("\n")) + 1

	// Line number of error.
	line := bytes.Count(data[:start], []byte("\n")) + 1
	// Position of error in line (where to place the '^').
	pos := int(sErr.Offset) - start
	if pos != 0 {
		pos = pos - 1
	}

	var buf bytes.Buffer
	for i, l := range bytes.SplitAfter(data, []byte("\n")) {
		if i >= line-3 && i <= line+2 {
			buf.Write(l)
		}
		if i+1 == line {
			buf.Write(bytes.Repeat([]byte(" "), pos))
			buf.Write([]byte("^\n"))
		}
	}
	return fmt.Errorf("JSON syntax error in line %d: %s:\n%s", line, err, buf.String())
}

func unmarshalGooSpec(c []byte, varMap map[string]string) (*GooSpec, error) {
	goospecTemplate := template.New("goospecTemplate").Option("missingkey=zero")
	tmpl, err := goospecTemplate.Parse(string(c))
	if err != nil {
		return nil, err
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, varMap); err != nil {
		return nil, err
	}

	var gs GooSpec
	data := buf.Bytes()
	if err := json.Unmarshal(data, &gs.PackageSpec); err != nil {
		return nil, jsonError(data, err)
	}
	if err := json.Unmarshal(data, &gs); err != nil {
		return nil, jsonError(data, err)
	}
	return &gs, nil
}

// ReadGooSpec unmarshalls and verifies a goospec file into the GooSpec struct.
func ReadGooSpec(cf string, varMap map[string]string) (*GooSpec, error) {
	c, err := ioutil.ReadFile(cf)
	if err != nil {
		return nil, err
	}
	gs, err := unmarshalGooSpec(c, varMap)
	if err != nil {
		return nil, err
	}
	gs.normalize()
	if err = gs.verify(); err != nil {
		return nil, err
	}
	return gs, nil
}

// WritePackageSpec takes a PkgSpec and writes it as a JSON file using
// the provided tar writer.
func WritePackageSpec(tw *tar.Writer, spec *PkgSpec) error {
	buf := &bytes.Buffer{}

	c, err := MarshalPackageSpec(spec)
	if err != nil {
		return err
	}
	buf.Write(c)

	fh := &tar.Header{
		Name:    spec.Name + pkgSpecSuffix,
		Size:    int64(buf.Len()),
		ModTime: time.Now(),
		Mode:    0644,
	}

	if err := tw.WriteHeader(fh); err != nil {
		return err
	}
	if _, err := tw.Write(buf.Bytes()); err != nil {
		return err
	}
	return nil
}

// ReadPackageSpec reads a PkgSpec from the given reader, which is
// expected to contain an uncompressed tar archive.
func ReadPackageSpec(r io.Reader) (*PkgSpec, error) {
	tr := tar.NewReader(r)
	for {
		header, err := tr.Next()
		if err == io.EOF {
			return nil, fmt.Errorf("no file with suffix %q found in package", pkgSpecSuffix)
		}
		if err != nil {
			return nil, err
		}
		if filepath.Ext(header.Name) != pkgSpecSuffix {
			continue
		}

		data, err := ioutil.ReadAll(tr)
		if err != nil {
			return nil, err
		}
		return UnmarshalPackageSpec(data)
	}
}

func (ps *PkgSpec) verify() error {
	if ps.Name == "" {
		return errors.New("no name defined in package spec")
	}
	if !slices.Contains(validArch, ps.Arch) {
		return fmt.Errorf("invalid architecture: %q", ps.Arch)
	}
	if ps.Version == "" {
		return errors.New("version string empty")
	}
	if _, err := ParseVersion(ps.Version); err != nil {
		return fmt.Errorf("can't parse %q: %v", ps.Version, err)
	}
	if len(ps.Tags) > 10 {
		return errors.New("too many tags")
	}
	for k, v := range ps.Tags {
		if len(k) > maxTagKeyLen {
			return errors.New("tag key too large")
		}
		if len(v) > maxTagValueSize {
			return fmt.Errorf("tag %q too large", k)
		}
	}
	for k, v := range ps.PkgDependencies {
		if _, err := ParseVersion(v); err != nil {
			return fmt.Errorf("can't parse version %q for dependancy %q: %v", v, k, err)
		}
	}
	for src := range ps.Files {
		if filepath.IsAbs(src) {
			return fmt.Errorf("%q is an absolute path, expected relative", src)
		}
	}
	if filepath.IsAbs(ps.Install.Path) {
		return fmt.Errorf("%q is an absolute path, expected relative", ps.Install.Path)
	}
	if filepath.IsAbs(ps.Uninstall.Path) {
		return fmt.Errorf("%q is an absolute path, expected relative", ps.Uninstall.Path)
	}
	return nil
}

func (ps *PkgSpec) normalize() {
	for _, str := range []*string{&ps.Install.Path, &ps.Uninstall.Path} {
		if filepath.IsAbs(*str) {
			continue
		}
		*str = filepath.Clean("/" + *str)[1:]
	}
	if runtime.GOOS == "windows" {
		for src := range ps.Files {
			newSrc := strings.ReplaceAll(src, "\\", "/")
			if newSrc != src {
				ps.Files[newSrc] = ps.Files[src]
				delete(ps.Files, src)
			}
		}
	}
}

// MarshalPackageSpec encodes the given PkgSpec.
func MarshalPackageSpec(spec *PkgSpec) ([]byte, error) {
	if err := spec.verify(); err != nil {
		return nil, err
	}

	return json.MarshalIndent(spec, "", "  ")
}

// UnmarshalPackageSpec parses data and returns a PkgSpec, if it finds
// one.
func UnmarshalPackageSpec(data []byte) (*PkgSpec, error) {
	var p PkgSpec
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, err
	}
	p.normalize()
	if err := p.verify(); err != nil {
		return nil, err
	}
	return &p, nil
}

// PrettyPrint writes a formatted description of the PkgSpec to w.
// fromRepo indicates which repo the package was sourced from.
func (ps PkgSpec) PrettyPrint(w io.Writer, fromRepo string) {
	pkgInfo := []struct {
		name, value string
	}{
		{"Name", ps.Name},
		{"Arch", ps.Arch},
		{"Version", ps.Version},
		{"Repo", path.Base(fromRepo)},
		{"Authors", ps.Authors},
		{"Owners", ps.Owners},
		{"Source", ps.Source},
		{"Description", ps.Description},
		{"InstallPath", ps.InstallPath},
		{"Dependencies", ""},
		{"ReleaseNotes", ""},
	}
	var width int
	for _, pi := range pkgInfo {
		if len(pi.name) > width {
			width = len(pi.name)
		}
	}

	write := func(label, value string) {
		fmt.Fprintf(w, "%-*s: %s\n", width+1, label, value)
	}

	writeLines := func(label string, lines []string) {
		if len(lines) == 0 {
			write(label, "")
			return
		}
		for _, line := range lines {
			sl, _ := twwarp.WrapString(line, 76-width)
			for _, l := range sl {
				write(label, l)
				label = ""
			}
		}
	}

	for _, pi := range pkgInfo {
		switch pi.name {
		case "Dependencies":
			if len(ps.PkgDependencies) == 0 {
				write(pi.name, "None")
				continue
			}
			var deps []string
			for p, v := range ps.PkgDependencies {
				deps = append(deps, p+" "+v)
			}
			slices.Sort(deps)
			write(pi.name, deps[0])
			for _, l := range deps[1:] {
				write("", l)
			}
		case "ReleaseNotes":
			writeLines(pi.name, ps.ReleaseNotes)
		default:
			writeLines(pi.name, strings.Split(strings.TrimSpace(pi.value), "\n"))
		}
	}
}

// ErrRelationSyntax marks a RelationProblems error for fatal name or relation
// syntax that build and upload tools must reject.
var ErrRelationSyntax = errors.New("relation syntax")

// hasForbiddenRelationChar reports whether s contains a version operator rune
// ('<', '>' or '=', as relsyntax.IsOpRune defines them) or any Unicode
// whitespace character.
func hasForbiddenRelationChar(s string) bool {
	for _, r := range s {
		if relsyntax.IsOpRune(r) || unicode.IsSpace(r) {
			return true
		}
	}
	return false
}

// checkDotRelationEmpty checks whether trimmed (the TrimSpace form of raw in
// field) is empty or contains an empty dot-delimited name, architecture, or
// version segment.
func checkDotRelationEmpty(field, raw, trimmed string) error {
	if trimmed == "" {
		return fmt.Errorf("%s %q is empty", field, raw)
	}
	parts := strings.SplitN(trimmed, ".", 3)
	if parts[0] == "" {
		return fmt.Errorf("%s %q has an empty package name", field, raw)
	}
	if len(parts) == 2 && parts[1] == "" {
		return fmt.Errorf("%s %q has an empty architecture segment", field, raw)
	}
	if len(parts) == 3 && (parts[1] == "" || parts[2] == "") {
		return fmt.Errorf("%s %q has an empty dot-delimited segment", field, raw)
	}
	return nil
}

// checkRelationEntry validates the common dot-delimited structure of raw for
// field, returning the split PackageInfo, any syntax or architecture problems,
// and whether field-specific checks should continue.
func checkRelationEntry(field, raw string) (pi PackageInfo, probs []error, ok bool) {
	trimmed := strings.TrimSpace(raw)
	if hasForbiddenRelationChar(trimmed) {
		return pi, []error{fmt.Errorf("%w: %s %q must not contain '<', '>', '=' or whitespace", ErrRelationSyntax, field, raw)}, false
	}
	if err := checkDotRelationEmpty(field, raw, trimmed); err != nil {
		return pi, []error{err}, false
	}
	pi = PkgNameSplit(raw)
	if pi.Arch != "" && !slices.Contains(validArch, pi.Arch) {
		probs = append(probs, fmt.Errorf("%s %q has unknown architecture %q (often a dotted package name); matches only a package with that literal architecture", field, raw, pi.Arch))
	}
	return pi, probs, true
}

// checkRelationVersion checks that pi.Ver, when present on raw in field, parses
// as a valid GooGet version.
func checkRelationVersion(field, raw string, pi PackageInfo) error {
	if pi.Ver == "" {
		return nil
	}
	if _, err := ParseVersion(pi.Ver); err != nil {
		return fmt.Errorf("%s %q has invalid version %q (%v): install fails whenever %s is installed", field, raw, pi.Ver, err, pi.Name)
	}
	return nil
}

// malformedRelationError returns the fatal syntax error for a Conflicts or
// Replaces entry that contains an operator rune without matching the operator
// grammar.
func malformedRelationError(field, raw string) error {
	return fmt.Errorf("%w: %s %q is not a valid relation: use name[.arch], or name[.arch]OPversion with OP one of <, <=, =, >=, >", ErrRelationSyntax, field, raw)
}

// spacedOperatorError returns the fatal syntax error for a Conflicts or
// Replaces operator entry that contains whitespace around its operator.
func spacedOperatorError(field, raw string, op relsyntax.Op) error {
	return fmt.Errorf("%w: %s %q must not contain whitespace around operator %q", ErrRelationSyntax, field, raw, op)
}

// invalidOperatorVersionError returns the fatal syntax error for a Conflicts
// or Replaces operator entry whose version cannot be parsed.
func invalidOperatorVersionError(field, raw, ver string, err error) error {
	return fmt.Errorf("%w: %s %q has invalid version %q (%v)", ErrRelationSyntax, field, raw, ver, err)
}

// checkOperatorConflict validates a well-formed operator entry in Conflicts.
func checkOperatorConflict(raw string, p relsyntax.Parts, unversionedProvides map[string]bool) []error {
	if p.Spaced {
		return []error{spacedOperatorError("Conflicts entry", raw, p.Op)}
	}
	if p.Arch != "" {
		return []error{fmt.Errorf("%w: Conflicts entry %q specifies architecture or dotted segment %q with operator %q, which blocks installation on googet before 3.5.0 whenever any installed package provides %q; write %s%s%s", ErrRelationSyntax, raw, p.Arch, p.Op, p.Name, p.Name, p.Op, p.Ver)}
	}
	if _, err := ParseVersion(p.Ver); err != nil {
		return []error{invalidOperatorVersionError("Conflicts entry", raw, p.Ver, err)}
	}
	if unversionedProvides[p.Name] {
		return []error{fmt.Errorf("Conflicts entry %q bounds %q while Provides lists unversioned %q, which satisfies every version constraint; use Provides %q", raw, p.Name, p.Name, p.Name+"=<version>")}
	}
	return nil
}

// checkOperatorReplace validates a well-formed operator entry in Replaces,
// returning any entry-level problems and whether spec-level cross-checks
// should continue.
func checkOperatorReplace(raw string, p relsyntax.Parts) (probs []error, ok bool) {
	if p.Spaced {
		return []error{spacedOperatorError("Replaces entry", raw, p.Op)}, false
	}
	if _, err := ParseVersion(p.Ver); err != nil {
		return []error{invalidOperatorVersionError("Replaces entry", raw, p.Ver, err)}, false
	}
	if p.Arch != "" && !slices.Contains(validArch, p.Arch) {
		probs = append(probs, fmt.Errorf("Replaces entry %q has unknown architecture %q (often a dotted package name); matches only a package with that literal architecture", raw, p.Arch))
	}
	return probs, true
}

// unversionedProvideNames returns the set of valid package names that ps lists
// in Provides without a version.
func unversionedProvideNames(ps *PkgSpec) map[string]bool {
	var out map[string]bool
	for _, raw := range ps.Provides {
		name, _, hasEq := strings.Cut(raw, "=")
		if !hasEq && name != "" && !hasForbiddenRelationChar(name) {
			if out == nil {
				out = make(map[string]bool)
			}
			out[name] = true
		}
	}
	return out
}

// RelationProblems reports problems in ps's name and relation fields that
// verify does not check. Errors that wrap ErrRelationSyntax should fail a
// build or upload; the rest are warnings. Order: Name, Conflicts, Replaces,
// Provides, then PkgDependencies keys sorted.
func (ps *PkgSpec) RelationProblems() []error {
	if ps == nil {
		return nil
	}
	var probs []error

	if hasForbiddenRelationChar(ps.Name) {
		probs = append(probs, fmt.Errorf("%w: package name %q must not contain '<', '>', '=' or whitespace", ErrRelationSyntax, ps.Name))
	}

	unversionedProvides := unversionedProvideNames(ps)
	legacyConflictNames := make(map[string]bool)
	for _, raw := range ps.Conflicts {
		switch p := relsyntax.Split(raw); p.Form {
		case relsyntax.Legacy:
			pi, entryProbs, ok := checkRelationEntry("Conflicts entry", raw)
			probs = append(probs, entryProbs...)
			if !ok {
				continue
			}
			legacyConflictNames[pi.Name] = true
			if err := checkRelationVersion("Conflicts entry", raw, pi); err != nil {
				probs = append(probs, err)
			}
		case relsyntax.Malformed:
			probs = append(probs, malformedRelationError("Conflicts entry", raw))
		case relsyntax.Operator:
			probs = append(probs, checkOperatorConflict(raw, p, unversionedProvides)...)
		}
	}

	warnedBoth := make(map[string]bool)
	for _, raw := range ps.Replaces {
		var name, arch string
		p := relsyntax.Split(raw)
		switch p.Form {
		case relsyntax.Legacy:
			pi, entryProbs, ok := checkRelationEntry("Replaces entry", raw)
			probs = append(probs, entryProbs...)
			if !ok {
				continue
			}
			if err := checkRelationVersion("Replaces entry", raw, pi); err != nil {
				probs = append(probs, err)
			}
			name, arch = pi.Name, pi.Arch
		case relsyntax.Malformed:
			probs = append(probs, malformedRelationError("Replaces entry", raw))
			continue
		case relsyntax.Operator:
			entryProbs, ok := checkOperatorReplace(raw, p)
			probs = append(probs, entryProbs...)
			if !ok {
				continue
			}
			name, arch = p.Name, p.Arch
		}
		if legacyConflictNames[name] && !warnedBoth[name] {
			warnedBoth[name] = true
			probs = append(probs, fmt.Errorf("Replaces and Conflicts both name %q: googet before 3.5.0 refuses to install this package while %s is installed", name, name))
		}
		if ps.Name != "" && name == ps.Name && (arch == "" || arch == ps.Arch) {
			probs = append(probs, fmt.Errorf("Replaces entry %q names this package itself: upgrading removes the installed version and its dependents first", raw))
		}
		if p.Form == relsyntax.Operator && p.Op == relsyntax.OpGE && p.Arch != "" {
			probs = append(probs, fmt.Errorf("Replaces entry %q uses \">=\" with architecture %q, which googet before 3.5.0 ignores; write %s.%s.%s so older clients honour it", raw, p.Arch, p.Name, p.Arch, p.Ver))
		}
	}

	for _, raw := range ps.Provides {
		name, ver, hasEq := strings.Cut(raw, "=")
		if hasForbiddenRelationChar(name) || (hasEq && hasForbiddenRelationChar(ver)) {
			probs = append(probs, fmt.Errorf("%w: Provides entry %q must not contain '<', '>', extra '=', or whitespace", ErrRelationSyntax, raw))
			continue
		}
		if name == "" || (hasEq && ver == "") {
			probs = append(probs, fmt.Errorf("Provides entry %q has an empty name or version", raw))
			continue
		}
		if hasEq {
			if _, err := ParseVersion(ver); err != nil {
				probs = append(probs, fmt.Errorf("Provides entry %q has invalid version %q (%v): never satisfies a versioned requirement", raw, ver, err))
			}
		}
	}

	for _, key := range slices.Sorted(maps.Keys(ps.PkgDependencies)) {
		pi, entryProbs, ok := checkRelationEntry("PkgDependencies key", key)
		probs = append(probs, entryProbs...)
		if !ok {
			continue
		}
		if pi.Ver != "" {
			probs = append(probs, fmt.Errorf("PkgDependencies key %q embeds version %q, which is ignored in favour of map value %q", key, pi.Ver, ps.PkgDependencies[key]))
		}
	}

	return probs
}
