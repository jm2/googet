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

// Package install handles the installation of packages.
package install

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/google/googet/v2/client"
	"github.com/google/googet/v2/download"
	"github.com/google/googet/v2/googetdb"
	"github.com/google/googet/v2/goolib"
	"github.com/google/googet/v2/internal/relation"
	"github.com/google/googet/v2/oswrap"
	"github.com/google/googet/v2/progress"
	"github.com/google/googet/v2/remove"
	"github.com/google/googet/v2/settings"
	"github.com/google/googet/v2/system"
	"github.com/google/logger"
)

// minInstalledRow returns the installed row db.FetchPkg picks for r if it
// satisfies r's minimum version. Keep its selection and version gate in sync
// with simulateReplacements (covered by
// TestSimulateReplacementsMatchesResolveReplacements).
func minInstalledRow(r relation.Requirement, db *googetdb.GooDB) (client.PackageState, bool, error) {
	p, err := db.FetchPkg(r.PackageInfo())
	if err != nil {
		return client.PackageState{}, false, err
	}
	if p.PackageSpec == nil {
		return client.PackageState{}, false, nil
	}
	ok, err := r.MinInstalled(p.PackageSpec.Name, p.PackageSpec.Arch, p.PackageSpec.Version)
	if err != nil {
		return client.PackageState{}, false, err
	}
	if !ok {
		return client.PackageState{}, false, nil
	}
	return p, true, nil
}

// replaceVictim returns the installed package that Replaces entry r removes:
// the single row minInstalledRow selects for r.
func replaceVictim(r relation.Requirement, db *googetdb.GooDB) (client.PackageState, bool, error) {
	return minInstalledRow(r, db)
}

// minInstalled reports whether the package r names is installed at r's
// minimum version or later. It checks the single installed row that
// db.FetchPkg picks for r.
func minInstalled(r relation.Requirement, db *googetdb.GooDB) (bool, error) {
	_, ok, err := minInstalledRow(r, db)
	return ok, err
}

// isSatisfied reports whether the package dependency is satisfied by an installed package
// or a package that provides it.
func isSatisfied(r relation.Requirement, db *googetdb.GooDB) (bool, error) {
	// First check if the package itself is installed.
	ok, err := minInstalled(r, db)
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
			if client.SatisfiesProvider(prov, r.Name, r.Ver) {
				return true, nil
			}
		}
	}
	return false, nil
}

// pkgKey returns the database identity key (name.arch) for ps.
func pkgKey(ps *goolib.PkgSpec) string {
	return ps.Name + "." + ps.Arch
}

// exclusions identifies installed packages, keyed as name.arch, that a forward
// conflict check skips because installing the incoming package removes them.
type exclusions struct {
	// self is the incoming package's own name.arch key.
	self string
	// victims holds the name.arch keys of rows that Replaces entries remove
	// during a repository install.
	victims map[string]bool
}

// selfExclusion returns an exclusion set containing only ps itself.
func selfExclusion(ps *goolib.PkgSpec) exclusions {
	return exclusions{self: pkgKey(ps)}
}

// repoExclusions returns the initial exclusion set for installing ps from a
// repository, covering ps itself and the single row each Replaces entry picks.
func repoExclusions(ps *goolib.PkgSpec, db *googetdb.GooDB) (exclusions, error) {
	ex := selfExclusion(ps)
	for _, pkg := range ps.Replaces {
		v, ok, err := replaceVictim(relation.ParseRelation(pkg), db)
		if err != nil {
			return exclusions{}, err
		}
		if !ok {
			continue
		}
		if ex.victims == nil {
			ex.victims = make(map[string]bool)
		}
		ex.victims[pkgKey(v.PackageSpec)] = true
	}
	return ex, nil
}

// fetchPick mirrors googetdb.GooDB.FetchPkg over an in-memory state snapshot:
// among entries with PackageSpec.Name == pi.Name ordered bytewise by
// (Arch, Version) to match SQLite's default BINARY collation, it returns the
// first entry matching pi, or the last entry of that name if none matches, or
// false if no entry has pi.Name.
func fetchPick(pi goolib.PackageInfo, installed client.GooGetState) (client.PackageState, bool) {
	var sameName []client.PackageState
	for _, st := range installed {
		if st.PackageSpec != nil && st.PackageSpec.Name == pi.Name {
			sameName = append(sameName, st)
		}
	}
	if len(sameName) == 0 {
		return client.PackageState{}, false
	}
	slices.SortFunc(sameName, func(a, b client.PackageState) int {
		return cmp.Or(
			strings.Compare(a.PackageSpec.Arch, b.PackageSpec.Arch),
			strings.Compare(a.PackageSpec.Version, b.PackageSpec.Version),
		)
	})
	var last client.PackageState
	for _, st := range sameName {
		if st.Match(pi) {
			return st, true
		}
		last = st
	}
	return last, true
}

// simulateReplacements is an exact in-memory model of resolveReplacements over
// the database snapshot taken after dependencies are installed. It replays each
// ps.Replaces entry sequentially over a shrinking copy of installed using the
// same FetchPkg row selection, MinInstalled version gate, and remove.Plan
// cascade ordering as resolveReplacements and remove.All, returning the
// simulated post-replacement state and true on completion, or nil and false if
// any version comparison, dependent lookup, or cycle check fails.
func simulateReplacements(ps *goolib.PkgSpec, installed client.GooGetState) (client.GooGetState, bool) {
	snapshot := make(client.GooGetState, 0, len(installed))
	for _, st := range installed {
		if st.PackageSpec != nil {
			snapshot = append(snapshot, st)
		}
	}
	drop := func(pi goolib.PackageInfo) bool {
		pick, found := fetchPick(pi, snapshot)
		if !found {
			return false
		}
		name, arch := pick.PackageSpec.Name, pick.PackageSpec.Arch
		snapshot = slices.DeleteFunc(snapshot, func(st client.PackageState) bool {
			return st.PackageSpec.Name == name && st.PackageSpec.Arch == arch
		})
		return true
	}
	for _, raw := range ps.Replaces {
		r := relation.ParseRelation(raw)
		pi := r.PackageInfo()
		pick, found := fetchPick(pi, snapshot)
		if !found {
			continue
		}
		match, err := r.MinInstalled(pick.PackageSpec.Name, pick.PackageSpec.Arch, pick.PackageSpec.Version)
		if err != nil {
			return nil, false
		}
		if !match {
			continue
		}
		order, err := remove.Plan(pi, snapshot)
		if err != nil {
			return nil, false
		}
		for _, dep := range order {
			if !drop(goolib.PkgNameSplit(dep)) {
				return nil, false
			}
		}
		if !drop(pi) {
			return nil, false
		}
	}
	return snapshot, true
}

// has reports whether key is excluded from conflict checks.
func (e exclusions) has(key string) bool {
	return key != "" && (key == e.self || e.victims[key])
}

// specsFromState extracts the non-nil PkgSpec pointers from state.
func specsFromState(state client.GooGetState) []*goolib.PkgSpec {
	specs := make([]*goolib.PkgSpec, 0, len(state))
	for _, st := range state {
		if st.PackageSpec != nil {
			specs = append(specs, st.PackageSpec)
		}
	}
	return specs
}

// installedSpecs returns the non-nil package specs currently recorded in db.
func installedSpecs(db *googetdb.GooDB) ([]*goolib.PkgSpec, error) {
	state, err := db.FetchPkgs("")
	if err != nil {
		return nil, err
	}
	return specsFromState(state), nil
}

// nonExcluded returns the non-nil specs in installed whose name.arch key is
// not in ex.
func nonExcluded(installed []*goolib.PkgSpec, ex exclusions) []*goolib.PkgSpec {
	out := make([]*goolib.PkgSpec, 0, len(installed))
	for _, ps := range installed {
		if ps != nil && !ex.has(pkgKey(ps)) {
			out = append(out, ps)
		}
	}
	return out
}

// providesRequirement reports whether ps declares a Provides entry that
// satisfies r.
func providesRequirement(ps *goolib.PkgSpec, r relation.Requirement) bool {
	for _, prov := range ps.Provides {
		if client.SatisfiesProvider(prov, r.Name, r.Ver) {
			return true
		}
	}
	return false
}

// checkForwardConflict evaluates a single Conflicts requirement r against the
// primary FetchPkg candidate pick and the full installed package list under ex.
// It returns whether an unexcluded conflict exists and any excluded rows that
// matched r.
func checkForwardConflict(r relation.Requirement, pick *goolib.PkgSpec, installed []*goolib.PkgSpec, ex exclusions) (hit bool, exempted []*goolib.PkgSpec, err error) {
	if pick != nil {
		ok, err := r.MinInstalled(pick.Name, pick.Arch, pick.Version)
		if err != nil {
			return false, nil, err
		}
		if ok {
			if !ex.has(pkgKey(pick)) {
				return true, nil, nil
			}
			exempted = append(exempted, pick)
			others, err := relation.Resolve(r, nonExcluded(installed, ex))
			if err != nil {
				return false, nil, err
			}
			if len(others) > 0 {
				return true, exempted, nil
			}
		}
	}
	for _, inst := range installed {
		if inst == nil || !providesRequirement(inst, r) {
			continue
		}
		if !ex.has(pkgKey(inst)) {
			return true, exempted, nil
		}
		exempted = append(exempted, inst)
	}
	return false, exempted, nil
}

// resolveConflicts fails if any of ps.Conflicts matches an installed row not
// excluded by ex. It returns the entries that passed only because a Replaces
// victim was exempt; callers must recheck them after resolveReplacements runs.
func resolveConflicts(ps *goolib.PkgSpec, ex exclusions, db *googetdb.GooDB) ([]relation.Requirement, error) {
	if len(ps.Conflicts) == 0 {
		return nil, nil
	}
	// Check for any conflicting packages.
	// TODO(ajackura): Make sure no conflicting packages are listed as
	// dependencies or subdependancies.
	installed, err := installedSpecs(db)
	if err != nil {
		return nil, err
	}
	selfOnly := exclusions{self: ex.self}
	logged := make(map[string]bool)
	var deferred []relation.Requirement
	for _, pkg := range ps.Conflicts {
		r := relation.ParseRelation(pkg)
		pick, err := db.FetchPkg(r.PackageInfo())
		if err != nil {
			return nil, err
		}
		// Check if the conflicting package or a provider of it is installed.
		hit, exempted, err := checkForwardConflict(r, pick.PackageSpec, installed, ex)
		if err != nil {
			return nil, err
		}
		if hit {
			return nil, fmt.Errorf("cannot install, conflict with installed package or provider: %s", r.PackageInfo())
		}
		for _, exPkg := range exempted {
			k := pkgKey(exPkg)
			if !logged[k] {
				logged[k] = true
				logger.Infof("conflict %q with %s skipped: installing %s removes it", pkg, exPkg, ps)
			}
		}
		if len(ex.victims) > 0 {
			selfHit, _, err := checkForwardConflict(r, pick.PackageSpec, installed, selfOnly)
			if err != nil {
				return nil, err
			}
			if selfHit {
				deferred = append(deferred, r)
			}
		}
	}
	return deferred, nil
}

// deferredChecks holds conflict checks that passed before dependency
// installation only because a Replaces victim was exempted, so callers can
// verify after replacements run that no conflicting row remains installed.
type deferredChecks struct {
	// fwd holds parsed ps.Conflicts requirements that passed only via a victim exemption.
	fwd []relation.Requirement
}

// checkConflicts evaluates ps's conflicts under ex and returns any checks that
// were deferred onto Replaces removal.
func checkConflicts(ps *goolib.PkgSpec, ex exclusions, db *googetdb.GooDB) (deferredChecks, error) {
	fwd, err := resolveConflicts(ps, ex, db)
	if err != nil {
		return deferredChecks{}, err
	}
	return deferredChecks{fwd: fwd}, nil
}

// recheckBeforeReplacements simulates resolveReplacements on the
// post-dependency database snapshot and re-evaluates deferred forward conflict
// entries against the simulated post-replacement state with self-only
// exclusions, failing before resolveReplacements removes any package whenever
// the post-replacement recheck would fail. If simulation does not complete
// cleanly, it logs at info level and defers to resolveReplacements and recheck.
func (d deferredChecks) recheckBeforeReplacements(ps *goolib.PkgSpec, db *googetdb.GooDB) error {
	if len(d.fwd) == 0 {
		return nil
	}
	state, err := db.FetchPkgs("")
	if err != nil {
		return err
	}
	post, ok := simulateReplacements(ps, state)
	if !ok {
		logger.Infof("Replacement simulation for %s did not complete; deferring conflict checks until after replacements", ps)
		return nil
	}
	pickFromPost := func(pi goolib.PackageInfo) (*goolib.PkgSpec, error) {
		st, _ := fetchPick(pi, post)
		return st.PackageSpec, nil
	}
	return recheckForwardEntries(d.fwd, ps, specsFromState(post), pickFromPost, "")
}

// recheck re-evaluates deferred conflict entries with self-only exclusions
// after resolveReplacements completes, ensuring no exempted victim survived.
func (d deferredChecks) recheck(ps *goolib.PkgSpec, db *googetdb.GooDB) error {
	if len(d.fwd) == 0 {
		return nil
	}
	installed, err := installedSpecs(db)
	if err != nil {
		return err
	}
	pickFromDB := func(pi goolib.PackageInfo) (*goolib.PkgSpec, error) {
		st, err := db.FetchPkg(pi)
		return st.PackageSpec, err
	}
	return recheckForwardEntries(d.fwd, ps, installed, pickFromDB, " (still installed after replacing)")
}

// recheckForwardEntries checks each requirement in reqs against installed
// under selfExclusion(ps) using pickFor to resolve each requirement's primary
// row without repeating exemption logs, appending suffix to the conflict error
// on a hit.
func recheckForwardEntries(reqs []relation.Requirement, ps *goolib.PkgSpec, installed []*goolib.PkgSpec, pickFor func(goolib.PackageInfo) (*goolib.PkgSpec, error), suffix string) error {
	selfOnly := selfExclusion(ps)
	for _, r := range reqs {
		pick, err := pickFor(r.PackageInfo())
		if err != nil {
			return err
		}
		hit, _, err := checkForwardConflict(r, pick, installed, selfOnly)
		if err != nil {
			return err
		}
		if hit {
			return fmt.Errorf("cannot install, conflict with installed package or provider: %s%s", r.PackageInfo(), suffix)
		}
	}
	return nil
}

// Options holds the per-run configuration for an Installer.
type Options struct {
	// DBOnly updates only the package database without modifying system files
	// or running install scripts.
	DBOnly bool
	// Force overwrites files owned by another package when StrictConflicts is
	// set.
	Force bool
	// StrictConflicts makes overlapping files owned by another package an
	// error unless Force is set.
	StrictConflicts bool
}

// OptionsFromSettings builds Options from the global settings package for
// callers that use googet.conf. It is the only place in package install that
// reads settings.
func OptionsFromSettings(dbOnly, force bool) Options {
	return Options{
		DBOnly:          dbOnly,
		Force:           force,
		StrictConflicts: settings.StrictConflicts,
	}
}

// Installer installs packages from repositories or local archives using an
// explicit Options value. Neither New nor Installer methods read the global
// settings package; callers that want googet.conf values should pass
// OptionsFromSettings or use the package-level FromRepo, FromDisk, and
// Reinstall functions.
type Installer struct {
	cache string
	db    *googetdb.GooDB
	opts  Options
}

// New returns an Installer configured with cache, db, and opts.
func New(cache string, db *googetdb.GooDB, opts Options) *Installer {
	return &Installer{cache: cache, db: db, opts: opts}
}

// repoInstall executes a repository installation run for an Installer.
type repoInstall struct {
	*Installer
	rm         client.RepoMap
	archs      []string
	downloader *client.Downloader
}

// resolveReplacements removes the installed packages that ps replaces, along
// with the packages that depend on them. Keep this loop in sync with
// simulateReplacements (covered by
// TestSimulateReplacementsMatchesResolveReplacements).
func (ri *repoInstall) resolveReplacements(ctx context.Context, ps *goolib.PkgSpec) error {
	db := ri.db
	// Check for and remove any package this replaces.
	// TODO(ajackura): Make sure no replacements are listed as
	// dependencies or subdependancies.
	for _, pkg := range ps.Replaces {
		r := relation.ParseRelation(pkg)
		_, ins, err := replaceVictim(r, db)
		if err != nil {
			return err
		}
		if !ins {
			continue
		}
		pi := r.PackageInfo()
		deps, _, err := remove.EnumerateDeps(pi, db)
		if err != nil {
			return err
		}
		logger.Infof("%s replaces %s, removing", ps, pi)
		if err := remove.All(ctx, pi, deps, ri.opts.DBOnly, ri.downloader, db); err != nil {
			return err
		}
	}
	return nil
}

// installDeps installs the dependencies of ps and then removes the packages it
// replaces. chain lists the packages, as name.arch, whose dependencies are
// being installed further up the call stack.
func (ri *repoInstall) installDeps(ctx context.Context, ps *goolib.PkgSpec, chain []string) error {
	db := ri.db
	logger.Infof("Resolving conflicts and dependencies for %s %s version %s", ps.Arch, ps.Name, ps.Version)
	ex, err := repoExclusions(ps, db)
	if err != nil {
		return err
	}
	deferred, err := checkConflicts(ps, ex, db)
	if err != nil {
		return err
	}
	// Check for and install any dependencies.
	for p, ver := range ps.PkgDependencies {
		pi := relation.ParseDependency(p, ver)
		ok, err := isSatisfied(pi, db)
		if err != nil {
			return err
		} else if ok {
			logger.Infof("Dependency met: %s.%s with version greater than %s installed or provided", pi.Name, pi.Arch, ver)
			continue
		}

		installedArch, isLocked, _, _, err := db.InstalledLockState(pi.Name)
		if err != nil {
			logger.Infof("Error fetching installed package state: %v, proceeding without lock", err)
		}
		spec, repo, _, err := client.FindRepoLatest(goolib.PackageInfo{Name: pi.Name, Arch: pi.Arch, Ver: ver}, ri.rm, ri.archs, installedArch, isLocked)
		if err != nil {
			return err
		}
		if spec.Name == ps.Name && spec.Arch == ps.Arch {
			return fmt.Errorf("dependency %s of %s.%s resolves to the package itself", p, ps.Name, ps.Arch)
		}

		logger.Infof("Dependency found: %s.%s %s (provides %s) is available", spec.Name, spec.Arch, spec.Version, pi.Name)
		if err := ri.fromRepo(ctx, goolib.PackageInfo{Name: spec.Name, Arch: spec.Arch, Ver: spec.Version}, repo, chain); err != nil {
			return err
		}
	}
	if err := deferred.recheckBeforeReplacements(ps, db); err != nil {
		return err
	}
	if err := ri.resolveReplacements(ctx, ps); err != nil {
		return err
	}
	return deferred.recheck(ps, db)
}

// FromRepo installs pi and its dependencies from rm.
func (ins *Installer) FromRepo(ctx context.Context, pi goolib.PackageInfo, repo string, rm client.RepoMap, archs []string, downloader *client.Downloader) error {
	ri := &repoInstall{
		Installer:  ins,
		rm:         rm,
		archs:      archs,
		downloader: downloader,
	}
	return ri.fromRepo(ctx, pi, repo, nil)
}

// FromRepo installs a package and all dependencies from a repository.
func FromRepo(ctx context.Context, pi goolib.PackageInfo, repo, cache string, rm client.RepoMap, archs []string, dbOnly, force bool, downloader *client.Downloader, db *googetdb.GooDB) error {
	return New(cache, db, OptionsFromSettings(dbOnly, force)).FromRepo(ctx, pi, repo, rm, archs, downloader)
}

// fromRepo is FromRepo for a package that is a dependency of the packages in
// chain. A package that is already in chain depends on itself through a
// cycle, which is an error: installing it would recurse forever.
func (ri *repoInstall) fromRepo(ctx context.Context, pi goolib.PackageInfo, repo string, chain []string) error {
	db, rm := ri.db, ri.rm
	logger.Infof("Starting install of %s.%s.%s", pi.Name, pi.Arch, pi.Ver)
	fmt.Printf("Installing %s.%s.%s and dependencies...\n", pi.Name, pi.Arch, pi.Ver)
	// When a specific version is requested, look for an exact match in the repository.
	// Virtual package resolution is not currently supported for specific versions.
	// If no version is specified, resolve the latest version handling both
	// direct matches and providers.
	if pi.Ver == "" {
		installedArch, isLocked, _, _, err := db.InstalledLockState(pi.Name)
		if err != nil {
			logger.Infof("Error fetching installed package state: %v, proceeding without lock", err)
		}
		spec, repoURL, _, err := client.FindRepoLatest(pi, rm, ri.archs, installedArch, isLocked)
		if err != nil {
			return err
		}
		repo = repoURL
		// Use the resolved real package name and version.
		pi.Name = spec.Name
		pi.Arch = spec.Arch
		pi.Ver = spec.Version
	}

	rs, err := client.FindRepoSpec(pi, rm[repo])
	if err != nil {
		return err
	}
	key := rs.PackageSpec.Name + "." + rs.PackageSpec.Arch
	if at := slices.Index(chain, key); at >= 0 {
		return fmt.Errorf("cannot install %s: dependency cycle %s", chain[0], strings.Join(append(slices.Clone(chain[at:]), key), " -> "))
	}
	if err := ri.installDeps(ctx, rs.PackageSpec, append(slices.Clone(chain), key)); err != nil {
		return err
	}

	dst, pkgURL, err := download.FromRepo(ctx, rs, repo, ri.cache, ri.downloader)
	if err != nil {
		return err
	}

	// The state of the installed version, if any, is read before the new
	// version replaces it in the database.
	old, err := db.FetchPkg(pi)
	if err != nil {
		return err
	}
	insFiles, err := installPkg(dst, rs.PackageSpec, ri.opts, db, func(insFiles map[string]string) error {
		return addPkg(db, client.PackageState{
			SourceRepo:     repo,
			DownloadURL:    pkgURL,
			Checksum:       rs.Checksum,
			LocalPath:      dst,
			PackageSpec:    rs.PackageSpec,
			InstalledFiles: insFiles,
		})
	})
	if err != nil {
		return err
	}

	logger.Infof("Installation of %s.%s.%s completed", pi.Name, pi.Arch, pi.Ver)
	fmt.Printf("Installation of %s.%s.%s and all dependencies completed\n", pi.Name, pi.Arch, pi.Ver)
	// The old version is cleaned up only once the new one is recorded.
	cleanOld(old, insFiles, ri.opts.DBOnly)
	return nil
}

// FromDisk installs a local .goo file.
func FromDisk(pkgPath, cache string, dbOnly, force, shouldReinstall bool, db *googetdb.GooDB) error {
	return New(cache, db, OptionsFromSettings(dbOnly, force)).FromDisk(pkgPath, shouldReinstall)
}

// FromDisk installs a local .goo archive at pkgPath.
func (ins *Installer) FromDisk(pkgPath string, reinstall bool) error {
	db := ins.db
	if _, err := oswrap.Stat(pkgPath); err != nil {
		return err
	}
	zs, err := extractSpec(pkgPath)
	if err != nil {
		return fmt.Errorf("error extracting spec file: %v", err)
	}
	if !reinstall {
		ni, err := NeedsInstallation(goolib.PackageInfo{Name: zs.Name, Arch: zs.Arch, Ver: zs.Version}, db)
		if err != nil {
			return err
		}
		if !ni {
			fmt.Printf("%s.%s.%s or a newer version is already installed on the system\n", zs.Name, zs.Arch, zs.Version)
			return nil
		}
	}

	logger.Infof("Starting install of %q, version %q from %q", zs.Name, zs.Version, pkgPath)
	fmt.Printf("Installing %s %s...\n", zs.Name, zs.Version)

	if _, err := checkConflicts(zs, selfExclusion(zs), db); err != nil {
		return err
	}
	for p, ver := range zs.PkgDependencies {
		pi := relation.ParseDependency(p, ver)
		if ok, err := isSatisfied(pi, db); err != nil {
			return err
		} else if ok {
			logger.Infof("Dependency met: %s.%s with version greater than %s installed or provided", pi.Name, pi.Arch, ver)
			continue
		}
		return fmt.Errorf("package dependency %s %s (min version %s) not installed and not provided", pi.Name, pi.Arch, ver)
	}
	if len(zs.Replaces) > 0 {
		installed, err := installedSpecs(db)
		if err != nil {
			return err
		}
		// Refuse if any installed arch of a replaced package qualifies, not
		// just the row that minInstalled would check.
		for _, pkg := range zs.Replaces {
			r := relation.ParseRelation(pkg)
			rows, err := relation.Resolve(r, installed)
			if err != nil {
				return err
			}
			if len(rows) > 0 {
				return fmt.Errorf("cannot install, replaces installed package, remove first then try installation again: %s (installed: %s)", r.PackageInfo(), rows[0])
			}
		}
	}

	dst := filepath.Join(ins.cache, goolib.PackageInfo{Name: zs.Name, Arch: zs.Arch, Ver: zs.Version}.PkgName())
	if err := copyPkg(pkgPath, dst); err != nil {
		return err
	}

	if _, err := installPkg(dst, zs, ins.opts, db, func(insFiles map[string]string) error {
		return addPkg(db, client.PackageState{
			LocalPath:      dst,
			PackageSpec:    zs,
			InstalledFiles: insFiles,
		})
	}); err != nil {
		return err
	}

	if reinstall {
		logger.Infof("Reinstallation of %q, version %q completed", zs.Name, zs.Version)
		fmt.Printf("Reinstallation of %s completed\n", zs.Name)
	} else {
		logger.Infof("Installation of %q, version %q completed", zs.Name, zs.Version)
		fmt.Printf("Installation of %s completed\n", zs.Name)
	}
	return nil
}

// Reinstall reinstalls and optionally redownloads, a package.
func Reinstall(ctx context.Context, ps client.PackageState, rd, force bool, downloader *client.Downloader, db *googetdb.GooDB) error {
	spec := ps.PackageSpec
	logger.Infof("Starting reinstall of %s.%s, version %s", spec.Name, spec.Arch, spec.Version)
	fmt.Printf("Reinstalling %s.%s %s and dependencies...\n", spec.Name, spec.Arch, spec.Version)

	// Fix for package install by older versions of GooGet.
	if ps.LocalPath == "" && ps.UnpackDir != "" {
		ps.LocalPath = ps.UnpackDir + ".goo"
	}

	if ps.LocalPath == "" {
		return fmt.Errorf("local path not referenced in state file for %s.%s.%s. Cannot redownload", spec.Name, spec.Arch, spec.Version)
	}

	f, err := os.Open(ps.LocalPath)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if os.IsNotExist(err) {
		logger.Infof("Local package does not exist for %s.%s.%s, redownloading...", spec.Name, spec.Arch, spec.Version)
		rd = true
	}
	// Force redownload if checksum does not match.
	// If checksum is empty this was a local install so ignore.
	if !rd && ps.Checksum != "" && goolib.Checksum(f) != ps.Checksum {
		logger.Info("Local package checksum does not match, redownloading...")
		rd = true
	}
	f.Close()

	if rd {
		if ps.DownloadURL == "" {
			return fmt.Errorf("can not redownload %s.%s.%s, DownloadURL not saved", spec.Name, spec.Arch, spec.Version)
		}
		if err := download.Package(ctx, ps.DownloadURL, ps.LocalPath, ps.Checksum, downloader); err != nil {
			return fmt.Errorf("error redownloading package: %v", err)
		}
	}

	if _, err := installPkg(ps.LocalPath, ps.PackageSpec, OptionsFromSettings(false, force), db, nil); err != nil {
		return fmt.Errorf("error reinstalling package: %v", err)
	}

	logger.Infof("Reinstallation of %s.%s, version %s completed", spec.Name, spec.Arch, spec.Version)
	fmt.Printf("Reinstallation of %s.%s %s completed\n", spec.Name, spec.Arch, spec.Version)
	return nil
}

func copyPkg(src, dst string) (retErr error) {
	r, err := oswrap.Open(src)
	if err != nil {
		return err
	}
	defer r.Close()

	f, err := oswrap.Create(dst)
	if err != nil {
		return err
	}
	defer func() {
		if err := f.Close(); err != nil && retErr == nil {
			retErr = err
		}
	}()

	if _, err := io.Copy(f, r); err != nil {
		return err
	}
	return retErr
}

// NeedsInstallation checks if a package version needs installation.
func NeedsInstallation(pi goolib.PackageInfo, db *googetdb.GooDB) (bool, error) {
	p, err := db.FetchPkg(pi)
	if err != nil {
		return true, err
	}
	if p.PackageSpec == nil {
		return true, nil
	}
	if p.PackageSpec.Name == pi.Name && p.PackageSpec.Arch == pi.Arch {
		c, err := goolib.Compare(p.PackageSpec.Version, pi.Ver)
		if err != nil {
			return true, err
		}
		switch c {
		case 0:
			logger.Infof("%s.%s %s is already installed.\n", pi.Name, pi.Arch, pi.Ver)
			return false, nil
		case 1:
			logger.Infof("A newer version of %s.%s is already installed.\n", pi.Name, pi.Arch)
			return false, nil
		}
	}
	return true, nil
}

func extractSpec(pkgPath string) (*goolib.PkgSpec, error) {
	f, err := oswrap.Open(pkgPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	return goolib.ExtractPkgSpec(f)
}

func makeInstallFunction(src, dst string, insFiles map[string]string, opts Options, conflictMap map[string]string, u *undo) func(string, os.FileInfo, error) error {
	return func(path string, fi os.FileInfo, err error) (outerr error) {
		if err != nil {
			return err
		}
		outPath := filepath.Join(dst, strings.TrimPrefix(path, src))

		if owner, ok := conflictMap[outPath]; ok && !fi.IsDir() {
			if opts.StrictConflicts && !opts.Force {
				return fmt.Errorf("file conflict: %s is already owned by package %s", outPath, owner)
			}
			if opts.Force {
				logger.Infof("Warning: file conflict: %s is already owned by package %s, overwriting due to force flag", outPath, owner)
			} else {
				logger.Infof("Warning: file conflict: %s is already owned by package %s, overwriting because `StrictConflicts` is not set", outPath, owner)
			}
			progress.Printf("Warning: file conflict: %s is already owned by package %s, overwriting...\n", outPath, owner)
		}

		if opts.DBOnly {
			// Record what a real install would: the payload's checksum for
			// files, and an empty hash for directories.
			if fi.IsDir() {
				insFiles[outPath] = ""
				return nil
			}
			f, err := oswrap.Open(path)
			if err != nil {
				return err
			}
			defer f.Close()
			insFiles[outPath] = goolib.Checksum(f)
			return nil
		}
		if fi.IsDir() {
			logger.Infof("Creating folder %q", outPath)
			// We designate directories by an empty hash.
			insFiles[outPath] = ""
			return u.mkdirAll(outPath, fi.Mode())
		}
		if err := u.mkdirAll(filepath.Dir(outPath), dirPerm); err != nil {
			return err
		}
		if err := u.prepare(outPath); err != nil {
			return err
		}
		logger.Infof("Copying file %q", outPath)
		oFile, err := oswrap.Create(outPath)
		if err != nil {
			return err
		}
		defer func() {
			if err := oFile.Close(); err != nil && outerr == nil {
				outerr = err
			}
		}()
		iFile, err := oswrap.Open(path)
		if err != nil {
			return err
		}
		defer iFile.Close()

		hash := sha256.New()
		mw := io.MultiWriter(oFile, hash)
		if _, err := io.Copy(mw, iFile); err != nil {
			return err
		}
		insFiles[outPath] = hex.EncodeToString(hash.Sum(nil))
		return nil
	}
}

func resolveDst(dst string) string {
	if !filepath.IsAbs(dst) {
		if strings.HasPrefix(dst, "<") {
			if i := strings.LastIndex(dst, ">"); i != -1 {
				return os.Getenv(dst[1:i]) + dst[i+1:]
			}
		}
		return "/" + dst
	}
	return dst
}

func cleanOld(st client.PackageState, insFiles map[string]string, dbOnly bool) {
	if !dbOnly {
		cleanOldFiles(st, insFiles)
	}
	if st.LocalPath != "" && oswrap.RemoveAll(st.LocalPath) != nil {
		logger.Error("Unable to remove old local path")
	}
	if st.UnpackDir != "" && oswrap.RemoveAll(st.UnpackDir) != nil {
		logger.Error("Unable to remove old unpack dir")
	}
}

func cleanOldFiles(oldState client.PackageState, insFiles map[string]string) {
	if len(oldState.InstalledFiles) == 0 {
		return
	}
	var files []string
	for file := range oldState.InstalledFiles {
		if chksum, ok := insFiles[file]; !ok {
			if chksum == "" {
				files = append(files, file)
				continue
			}
			logger.Infof("Cleaning up old file %q", file)
			if _, err := client.RemoveOrRename(file); err != nil {
				logger.Error(err)
			}
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(files)))
	for _, dir := range files {
		if _, err := client.RemoveOrRename(dir); err != nil {
			logger.Info(err)
		}
	}
}

func buildConflictMap(db *googetdb.GooDB, currentPkg string) (map[string]string, error) {
	conflictMap := make(map[string]string)
	pkgs, err := db.FetchPkgs("")
	if err != nil {
		return nil, err
	}
	for _, p := range pkgs {
		if p.PackageSpec == nil || p.PackageSpec.Name == currentPkg {
			continue
		}
		for f, hash := range p.InstalledFiles {
			if hash != "" {
				conflictMap[f] = p.PackageSpec.Name
			}
		}
	}
	return conflictMap, nil
}

// errInstallInterrupted is the spinner status error used when an install
// unwinds without returning, such as on a panic.
var errInstallInterrupted = errors.New("install interrupted")

// addPkg records an installed package in the database. Tests replace it to
// make the update fail.
var addPkg = (*googetdb.GooDB).AddPkg

// installPkg extracts and installs a package, rendering a spinner on
// interactive terminals for the duration of the install.
//
// If record is not nil, it must persist the install, such as by recording it
// in the database. It is called with the installed files after the files are
// placed and the installer has succeeded. If it fails or panics, the placed
// files are rolled back, but changes made by the installer are not. The
// backups of overwritten files are kept until record succeeds.
func installPkg(pkg string, ps *goolib.PkgSpec, opts Options, db *googetdb.GooDB, record func(insFiles map[string]string) error) (insFiles map[string]string, err error) {
	sp := progress.NewSpinner(fmt.Sprintf("Installing %s.%s.%s", ps.Name, ps.Arch, ps.Version))
	// The spinner is stopped by a deferred call so that no exit path leaves it
	// redrawing. A normal return overwrites err before the deferred call runs;
	// a panic leaves errInstallInterrupted in place, so it renders "failed".
	err = errInstallInterrupted
	defer func() { sp.Stop(err) }()
	return installPkgInner(pkg, ps, opts, db, record)
}

// backupInfix marks the backup of a file that an install overwrites.
const backupInfix = ".googet-bak"

// change is one filesystem change made by an install.
type change struct {
	path string
	// backup is the path to the backup of path's old content, or empty if
	// path was created.
	backup string
}

// undo records the changes an install makes to the filesystem, in order, so
// that they can be reverted if the install fails. Reverting them newest first
// restores each path to its original state even if it was written more than
// once. The zero value is ready to use.
type undo struct {
	changes []change
}

// dirPerm is the mode of the parent directories an install creates. It
// matches download.ExtractPkg.
const dirPerm os.FileMode = 0755

// mkdirAll creates dir with the given mode and any missing parents with mode
// dirPerm, recording each directory it creates. It creates only the missing
// directories, so it never calls oswrap.MkdirAll on an existing path such as
// a Windows volume root, which panics.
func (u *undo) mkdirAll(dir string, mode os.FileMode) error {
	var missing []string
	for d := dir; ; d = filepath.Dir(d) {
		if _, err := oswrap.Lstat(d); !os.IsNotExist(err) || filepath.Dir(d) == d {
			break
		}
		missing = append(missing, d)
	}
	if len(missing) == 0 {
		fi, err := oswrap.Stat(dir)
		if err != nil {
			return err
		}
		if !fi.IsDir() {
			return fmt.Errorf("%s exists and is not a directory", dir)
		}
		return nil
	}
	for i := len(missing) - 1; i >= 0; i-- {
		u.changes = append(u.changes, change{path: missing[i]})
	}
	if len(missing) > 1 {
		if err := oswrap.MkdirAll(missing[1], dirPerm); err != nil {
			return err
		}
	}
	return oswrap.MkdirAll(dir, mode)
}

// prepare makes path ready to be written. An existing file is renamed to a
// backup in the same directory, which works on Windows even for running
// executables; a missing path is recorded as created. If googet is killed
// before commit or rollback runs, the backup is left behind as
// name.googet-bak*.
func (u *undo) prepare(path string) error {
	fi, err := oswrap.Lstat(path)
	switch {
	case os.IsNotExist(err):
		u.changes = append(u.changes, change{path: path})
		return nil
	case err != nil:
		return err
	case fi.IsDir():
		// An empty directory in the way of the file is removed, and is not
		// recreated on rollback; client.RemoveOrRename fails if it is not
		// empty.
		if _, err := client.RemoveOrRename(path); err != nil {
			return err
		}
		u.changes = append(u.changes, change{path: path})
		return nil
	}
	// oswrap has no CreateTemp; os handles long absolute Windows paths itself.
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+backupInfix+"*")
	if err != nil {
		return err
	}
	f.Close()
	if err := oswrap.Rename(path, f.Name()); err != nil {
		oswrap.Remove(f.Name())
		return err
	}
	u.changes = append(u.changes, change{path: path, backup: f.Name()})
	return nil
}

// rollback reverts the changes, newest first: created paths are removed and
// backups are restored. Directories that are not empty are left in place.
func (u *undo) rollback() {
	for i := len(u.changes) - 1; i >= 0; i-- {
		c := u.changes[i]
		// RemoveOrRename moves a file that is in use out of the way. If it
		// returns a name, the file was moved there, even if scheduling its
		// removal on reboot then failed.
		fn, err := client.RemoveOrRename(c.path)
		switch {
		case err != nil && fn == "":
			logger.Warningf("Rollback: leaving %q: %v", c.path, err)
			continue
		case err != nil:
			logger.Warningf("Rollback: moved %q to %q, but scheduling its removal on reboot failed: %v", c.path, fn, err)
		}
		if c.backup == "" {
			continue
		}
		if err := oswrap.Rename(c.backup, c.path); err != nil {
			logger.Errorf("Rollback: failed to restore %q from %q: %v", c.path, c.backup, err)
		}
	}
}

// commit deletes the backups, scheduling any that are in use for removal on
// reboot.
func (u *undo) commit() {
	for _, c := range u.changes {
		if c.backup == "" {
			continue
		}
		if err := oswrap.Remove(c.backup); err != nil {
			// RemoveOnReboot does nothing off Windows, so log the failure.
			logger.Warningf("Failed to remove backup %q, falling back to removal on reboot (Windows only): %v", c.backup, err)
			if err := oswrap.RemoveOnReboot(c.backup); err != nil {
				logger.Errorf("Failed to schedule removal of backup %q on reboot: %v", c.backup, err)
			}
		}
	}
}

// installPkgInner implements installPkg. It owns the journal of the
// filesystem changes: if any step, including record, fails or panics, the
// files it placed are removed and the files it overwrote are restored.
func installPkgInner(pkg string, ps *goolib.PkgSpec, opts Options, db *googetdb.GooDB, record func(insFiles map[string]string) error) (map[string]string, error) {
	dir, err := download.ExtractPkg(pkg)
	if err != nil {
		return nil, err
	}

	logger.Infof("Executing install of package %q", filepath.Base(dir))

	u := &undo{}
	// done is set only on the final return so that errors and panics both
	// roll back.
	done := false
	defer func() {
		if done {
			u.commit()
		} else {
			u.rollback()
		}
	}()

	conflictMap, err := buildConflictMap(db, ps.Name)
	if err != nil {
		return nil, err
	}

	insFiles := make(map[string]string)
	for src, dst := range ps.Files {
		dst = resolveDst(dst)
		src = filepath.Join(dir, src)
		if err := oswrap.Walk(src, makeInstallFunction(src, dst, insFiles, opts, conflictMap, u)); err != nil {
			return nil, err
		}
	}

	if !opts.DBOnly {
		if err := system.Install(dir, ps); err != nil {
			return nil, err
		}
	}

	if record != nil {
		if err := record(insFiles); err != nil {
			if !opts.DBOnly && ps.Install.Path != "" {
				logger.Errorf("Recording %s.%s.%s in the database failed after its installer ran; rolling back its files, but not the changes made by the installer: %v", ps.Name, ps.Arch, ps.Version, err)
			} else {
				logger.Errorf("Recording %s.%s.%s in the database failed: %v", ps.Name, ps.Arch, ps.Version, err)
			}
			return nil, err
		}
	}

	done = true
	if err := oswrap.RemoveAll(dir); err != nil {
		logger.Error(err)
	}

	return insFiles, nil
}

func listDeps(pi goolib.PackageInfo, rm client.RepoMap, repo string, dl []goolib.PackageInfo, archs []string, db *googetdb.GooDB) ([]goolib.PackageInfo, error) {
	// List each package once, which also ends a dependency cycle.
	if slices.ContainsFunc(dl, func(d goolib.PackageInfo) bool { return d.Name == pi.Name && d.Arch == pi.Arch }) {
		return dl, nil
	}
	rs, err := client.FindRepoSpec(pi, rm[repo])
	if err != nil {
		return nil, err
	}
	dl = append(dl, pi)
	for d, v := range rs.PackageSpec.PkgDependencies {
		// Keep the required version so that FindRepoLatest only returns a
		// package that meets it, either directly or through a versioned
		// Provides entry, as installDeps does.
		di := relation.ParseDependency(d, v).PackageInfo()
		installedArch, isLocked, _, _, err := db.InstalledLockState(di.Name)
		if err != nil {
			logger.Infof("Error fetching installed package state: %v, proceeding without lock", err)
		}
		spec, repo, arch, err := client.FindRepoLatest(di, rm, archs, installedArch, isLocked)
		if err != nil {
			return nil, fmt.Errorf("cannot resolve dependency %s version %s or greater: %v", d, v, err)
		}
		if spec.Name == pi.Name && arch == pi.Arch {
			return nil, fmt.Errorf("dependency %s of %s.%s resolves to the package itself", d, pi.Name, pi.Arch)
		}
		// The dependency may be satisfied by a package that provides it, so
		// continue with that package's own name and arch.
		di.Name = spec.Name
		di.Arch = arch
		di.Ver = spec.Version
		dl, err = listDeps(di, rm, repo, dl, archs, db)
		if err != nil {
			return nil, err
		}
	}
	return dl, nil
}

// ListDeps returns a list of dependencies and subdependancies for a package.
func ListDeps(pi goolib.PackageInfo, rm client.RepoMap, repo string, archs []string, db *googetdb.GooDB) ([]goolib.PackageInfo, error) {
	logger.Infof("Building dependency list for %s.%s.%s", pi.Name, pi.Arch, pi.Ver)
	return listDeps(pi, rm, repo, nil, archs, db)
}
