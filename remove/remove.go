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

// Package remove handles the removal of packages.
package remove

import (
	"context"
	"fmt"
	"maps"
	"os"
	"slices"
	"sort"
	"strings"

	"github.com/google/googet/v2/client"
	"github.com/google/googet/v2/download"
	"github.com/google/googet/v2/googetdb"
	"github.com/google/googet/v2/goolib"
	"github.com/google/googet/v2/internal/relation"
	"github.com/google/googet/v2/oswrap"
	"github.com/google/googet/v2/system"
	"github.com/google/logger"
)

// uninstallPkg removes the single installed row db.FetchPkg picks for pi
// (unlike UninstallRow, which removes an exact name.arch.version row).
func uninstallPkg(ctx context.Context, pi goolib.PackageInfo, dbOnly bool, downloader *client.Downloader, db *googetdb.GooDB) error {
	logger.Infof("Executing removal of package %q", pi.Name)
	ps, err := db.FetchPkg(pi)
	if err != nil {
		return fmt.Errorf("package not found in state file: %v", err)
	}
	if ps.PackageSpec == nil {
		return fmt.Errorf("package %q not found in state database", pi.Name)
	}
	return uninstallRow(ctx, pi, ps, dbOnly, false, downloader, db)
}

// Options configures UninstallRow.
type Options struct {
	// DBOnly deletes only the package's database row, without running its
	// uninstall script or removing its files.
	DBOnly bool
	// KeepCache leaves the package's cached archive in place, so that the
	// package can be installed again from it.
	KeepCache bool
	// Downloader downloads the package's archive again if it isn't cached.
	Downloader *client.Downloader
}

// UninstallRow uninstalls exactly the installed package row describes: the
// package with its name, arch and version. It fails, changing nothing, if
// that package is no longer installed. Unless opts.DBOnly is set, it runs the
// package's uninstall script from its cached archive, which LocalArchive
// finds or downloads again, removes the files the package installed and,
// unless opts.KeepCache is set, deletes the archive. Then it deletes the
// package's database row.
func UninstallRow(ctx context.Context, row client.PackageState, opts Options, db *googetdb.GooDB) error {
	spec := row.PackageSpec
	if spec == nil {
		return fmt.Errorf("cannot uninstall package: missing package spec")
	}
	logger.Infof("Executing removal of package %q", spec.Name)
	ps, err := db.FetchPkg(goolib.PackageInfo{Name: spec.Name, Arch: spec.Arch})
	if err != nil {
		return fmt.Errorf("package not found in state file: %v", err)
	}
	if ps.PackageSpec == nil || ps.PackageSpec.Name != spec.Name || ps.PackageSpec.Arch != spec.Arch || ps.PackageSpec.Version != spec.Version {
		return fmt.Errorf("package %s is no longer installed", spec)
	}
	return uninstallRow(ctx, goolib.PackageInfo{Name: spec.Name, Arch: spec.Arch, Ver: spec.Version}, ps, opts.DBOnly, opts.KeepCache, opts.Downloader, db)
}

// uninstallRow uninstalls the installed package ps and deletes its database
// row by name and arch. Unless dbOnly is set, it runs the package's uninstall
// script from its cached archive, downloading the archive again if needed,
// removes the files the package installed, and, unless keepCache is set,
// deletes the archive. label names the package in messages.
func uninstallRow(ctx context.Context, label goolib.PackageInfo, ps client.PackageState, dbOnly, keepCache bool, downloader *client.Downloader, db *googetdb.GooDB) error {
	if !dbOnly {
		archive, err := localArchive(ctx, label, ps, downloader)
		if err != nil {
			return err
		}
		eDir, err := download.ExtractPkg(archive)
		if err != nil {
			return err
		}
		if err := system.Uninstall(eDir, &ps); err != nil {
			return err
		}

		if err := oswrap.RemoveAll(eDir); err != nil {
			logger.Error(err)
		}

		if len(ps.InstalledFiles) > 0 {
			var dirs []string
			for file, chksum := range ps.InstalledFiles {
				if chksum == "" {
					dirs = append(dirs, file)
					continue
				}
				logger.Infof("Removing %q", file)
				if _, err := client.RemoveOrRename(file); err != nil {
					logger.Error(err)
				}
			}
			sort.Sort(sort.Reverse(sort.StringSlice(dirs)))
			for _, dir := range dirs {
				logger.Infof("Removing %q", dir)
				if _, err := client.RemoveOrRename(dir); err != nil {
					logger.Info(err)
				}
			}
		}
		if !keepCache {
			if err := oswrap.RemoveAll(archive); err != nil {
				logger.Errorf("error removing package data from cache directory: %v", err)
			}
		}
	}
	return db.RemovePkg(ps.PackageSpec.Name, ps.PackageSpec.Arch)
}

// LocalArchive returns the path of the installed package ps's cached archive,
// which uninstalling ps needs. Packages installed by older versions of GooGet
// record only an unpack directory, whose archive is that path plus ".goo".
// If the archive is missing, or ps records a checksum that the archive
// doesn't match, LocalArchive downloads it again from ps's download URL to
// the same path.
func LocalArchive(ctx context.Context, ps client.PackageState, downloader *client.Downloader) (string, error) {
	if ps.PackageSpec == nil {
		return "", fmt.Errorf("cannot resolve local archive: missing package spec")
	}
	return localArchive(ctx, goolib.PackageInfo{Name: ps.PackageSpec.Name, Arch: ps.PackageSpec.Arch, Ver: ps.PackageSpec.Version}, ps, downloader)
}

// localArchive is LocalArchive with label naming the package in messages.
func localArchive(ctx context.Context, label goolib.PackageInfo, ps client.PackageState, downloader *client.Downloader) (string, error) {
	// Fix for package install by older versions of GooGet.
	path := ps.LocalPath
	if path == "" && ps.UnpackDir != "" {
		path = ps.UnpackDir + ".goo"
	}
	if path == "" {
		return "", fmt.Errorf("no local path available for package %q", label.Name)
	}

	f, err := os.Open(path)
	if err != nil && !os.IsNotExist(err) {
		return "", err
	}
	var rd bool
	if os.IsNotExist(err) {
		logger.Infof("Local package does not exist for %s.%s.%s, redownloading...", label.Name, label.Arch, label.Ver)
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
			return "", fmt.Errorf("can not redownload %s.%s.%s, DownloadURL not saved", label.Name, label.Arch, label.Ver)
		}
		if err := download.Package(ctx, ps.DownloadURL, path, ps.Checksum, downloader); err != nil {
			return "", fmt.Errorf("error redownloading %s.%s.%s, package may no longer exist in the repo, you can use the '-db_only' flag to remove it from the database: %v", label.Name, label.Arch, label.Ver, err)
		}
	}
	return path, nil
}

// DepMap is a map of packages to dependant packages.
type DepMap map[string][]string

// remove deletes name from the map and from every list of dependents.
func (deps DepMap) remove(name string) {
	for dep, s := range deps {
		deps[dep] = slices.DeleteFunc(s, func(d string) bool { return d == name })
	}
	delete(deps, name)
}

// build adds name.arch and, transitively, every installed package that
// depends on it, logging each visited package at info level.
func (deps DepMap) build(name, arch string, state client.GooGetState) {
	deps.buildWithLog(name, arch, state, logger.Infof)
}

// buildWithLog adds name.arch and, transitively, every installed package that
// depends on it, calling logf for each visited package when logf is non-nil.
// Each package is visited once, so a dependency cycle ends, and each dependent
// is listed once per package even if it names the package by more than one
// dependency key.
func (deps DepMap) buildWithLog(name, arch string, state client.GooGetState, logf func(string, ...any)) {
	key := name + "." + arch
	if _, ok := deps[key]; ok {
		return
	}
	if logf != nil {
		logf("Building dependency map for %q", name)
	}
	deps[key] = nil
	for _, p := range state {
		if p.PackageSpec == nil {
			continue
		}
		if p.PackageSpec.Name == name && p.PackageSpec.Arch == arch {
			continue
		}
		for d := range p.PackageSpec.PkgDependencies {
			if relation.ParseDependency(d, "").SameIdentity(name, arch) {
				n, a := p.PackageSpec.Name, p.PackageSpec.Arch
				if dep := n + "." + a; !slices.Contains(deps[key], dep) {
					deps[key] = append(deps[key], dep)
				}
				deps.buildWithLog(n, a, state, logf)
			}
		}
	}
}

// removalOrder returns the packages in deps other than root, ordered so that
// each one comes after every package that depends on it. Root is removed last
// by contract, so root's own dependencies are removed before it even when
// they depend on root in turn; rootCycle lists those that do. If the
// remaining packages depend on each other in a cycle that doesn't include
// root, removalOrder returns an error naming them, and nothing should be
// removed. deps is not modified.
func removalOrder(deps DepMap, root string) (order, rootCycle []string, err error) {
	order, rootCycle, err = removalOrderSet(deps, []string{root})
	if err != nil {
		return nil, nil, err
	}
	// All removes the root separately, last.
	order = slices.DeleteFunc(order, func(pkg string) bool { return pkg == root })
	if len(order) == 0 {
		order = nil
	}
	return order, rootCycle, nil
}

// removalOrderSet returns the packages in deps, including every package in
// roots that is a key of deps, ordered so that each one comes after every
// package that depends on it. Roots are being removed on purpose, so a root's
// own dependencies are removed before it even when they depend on it in turn;
// rootCycle lists, sorted, the packages that depend on a root in this way.
// Dependencies between roots are kept, so a root that depends on another root
// is removed first. Packages that are ready at the same time are taken in
// sorted order. If packages other than roots depend on each other in a cycle,
// removalOrderSet returns an error naming them, and nothing should be
// removed; if only roots remain in a cycle, the first of the cycle's roots in
// sorted order is removed next. deps is not modified.
func removalOrderSet(deps DepMap, roots []string) (order, rootCycle []string, err error) {
	isRoot := make(map[string]bool, len(roots))
	for _, r := range roots {
		isRoot[r] = true
	}
	pending := make(DepMap, len(deps))
	for pkg, dependents := range deps {
		pending[pkg] = slices.Clone(dependents)
		// A dependent that isn't a key would otherwise never be removed.
		for _, d := range dependents {
			if _, ok := deps[d]; !ok {
				pending[d] = nil
			}
		}
	}
	for _, pkg := range slices.Sorted(maps.Keys(pending)) {
		if isRoot[pkg] {
			continue
		}
		var cycle bool
		pending[pkg] = slices.DeleteFunc(pending[pkg], func(d string) bool {
			if isRoot[d] {
				cycle = true
				return true
			}
			return false
		})
		if cycle {
			rootCycle = append(rootCycle, pkg)
		}
	}
	for len(pending) > 0 {
		var ready []string
		for pkg, dependents := range pending {
			if len(dependents) == 0 {
				ready = append(ready, pkg)
			}
		}
		if len(ready) == 0 {
			var remaining []string
			for pkg := range pending {
				if !isRoot[pkg] {
					remaining = append(remaining, pkg)
				}
			}
			if len(remaining) > 0 {
				slices.Sort(remaining)
				return nil, nil, fmt.Errorf("cannot remove %s: these packages depend on each other in a cycle: %s; remove one of them first", strings.Join(roots, ", "), strings.Join(remaining, ", "))
			}
			ready = []string{cycleBreak(pending)}
		}
		slices.Sort(ready)
		for _, pkg := range ready {
			pending.remove(pkg)
		}
		order = append(order, ready...)
	}
	return order, rootCycle, nil
}

// cycleBreak returns the package removalOrderSet removes next when every
// package left in pending is a root that waits for another to be removed
// first. It returns the first, in sorted order, of the roots that lie on a
// cycle that waits for nothing outside it, so that a root that depends on
// another root, but isn't in the cycle itself, is still removed first.
func cycleBreak(pending DepMap) string {
	// reaches returns the packages that from waits for, directly or not.
	reaches := func(from string) map[string]bool {
		seen := map[string]bool{}
		stack := []string{from}
		for len(stack) > 0 {
			pkg := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			for _, d := range pending[pkg] {
				if !seen[d] {
					seen[d] = true
					stack = append(stack, d)
				}
			}
		}
		return seen
	}
	keys := slices.Sorted(maps.Keys(pending))
	for _, pkg := range keys {
		if !reaches(pkg)[pkg] {
			continue
		}
		closed := true
		for d := range reaches(pkg) {
			if !reaches(d)[pkg] {
				closed = false
				break
			}
		}
		if closed {
			return pkg
		}
	}
	return keys[0]
}

// depVersions resolves every key in dm against state via relation.SplitKey and
// returns the "name.arch version" summary list built by EnumerateDeps.
func depVersions(dm DepMap, state client.GooGetState) ([]string, error) {
	var dl []string
	for k := range dm {
		di := relation.SplitKey(k)
		ps, err := state.GetPackageState(di)
		if err != nil {
			return nil, fmt.Errorf("error finding package in state file, even though the dependancy map was just built: %v", err)
		}
		dl = append(dl, k+" "+ps.PackageSpec.Version)
	}
	return dl, nil
}

// checkResolvable verifies that every key in dm resolves in state under the
// same relation.SplitKey lookup EnumerateDeps performs.
func checkResolvable(dm DepMap, state client.GooGetState) error {
	_, err := depVersions(dm, state)
	return err
}

// Plan returns the dependent packages in state that All uninstalls before
// uninstalling pi, in All's exact removal order and excluding pi itself. Each
// element is a "name.arch" key as passed to relation.SplitKey by All; pi's
// own root key (pi.Name + "." + pi.Arch, with an empty arch suffix when
// pi.Arch is empty) is seeded into the dependency map and then omitted from
// order because All uninstalls pi last. Plan is pure, does not modify state,
// and emits no log output; it returns an error if any entry in the dependency
// map cannot be resolved in state or if a dependency cycle prevents removal.
func Plan(pi goolib.PackageInfo, state client.GooGetState) (order []string, err error) {
	dm := make(DepMap)
	dm.buildWithLog(pi.Name, pi.Arch, state, nil)
	if err := checkResolvable(dm, state); err != nil {
		return nil, err
	}
	order, _, err = removalOrder(dm, pi.Name+"."+pi.Arch)
	return order, err
}

// OrderSet returns the packages in pkgs (each formatted as "name.arch"),
// ordered so that every package comes after every other package in pkgs that
// depends on it in state. Packages in roots are being removed intentionally,
// so a root's own dependencies on non-roots are ignored when breaking cycles,
// while dependencies between roots are preserved so that a root depending on
// another root is removed first. rootCycle lists, sorted, non-root packages in
// pkgs that form a dependency cycle with a root. OrderSet is pure, does not
// modify state, and returns an error if any key in pkgs is absent from state
// or if non-root packages depend on each other in a cycle.
func OrderSet(pkgs, roots []string, state client.GooGetState) (order, rootCycle []string, err error) {
	if len(pkgs) == 0 {
		return nil, nil, nil
	}
	byKey := make(map[string]*goolib.PkgSpec, len(state))
	for _, st := range state {
		if st.PackageSpec != nil {
			byKey[st.PackageSpec.Name+"."+st.PackageSpec.Arch] = st.PackageSpec
		}
	}
	inSet := make(map[string]bool, len(pkgs))
	for _, k := range pkgs {
		if _, ok := byKey[k]; !ok {
			return nil, nil, fmt.Errorf("package %q not found in state database", relation.SplitKey(k).Name)
		}
		inSet[k] = true
	}
	sortedPkgs := slices.Sorted(maps.Keys(inSet))
	deps := make(DepMap, len(sortedPkgs))
	for _, k := range sortedPkgs {
		ps := byKey[k]
		deps[k] = nil
		for _, candKey := range sortedPkgs {
			if candKey == k {
				continue
			}
			cand := byKey[candKey]
			for depKey := range cand.PkgDependencies {
				if relation.ParseDependency(depKey, "").SameIdentity(ps.Name, ps.Arch) {
					deps[k] = append(deps[k], candKey)
					break
				}
			}
		}
	}
	return removalOrderSet(deps, roots)
}

// EnumerateDeps returns a DepMap and list of dependencies for a package.
func EnumerateDeps(pi goolib.PackageInfo, db *googetdb.GooDB) (DepMap, []string, error) {
	state, err := db.FetchPkgs("")
	if err != nil {
		return nil, nil, fmt.Errorf("Error connecting to state database while building dependency map: %v", err)
	}
	dm := make(DepMap)
	dm.build(pi.Name, pi.Arch, state)
	dl, err := depVersions(dm, state)
	if err != nil {
		return nil, nil, err
	}
	return dm, dl, nil
}

// All removes a package and all dependent packages. Packages with no dependent packages
// will be removed first. The removal order is worked out before anything is
// removed, so a dependency cycle that prevents it fails without removing
// anything. deps is not modified.
//
// Keep the removal order and root-last execution in sync with Plan via
// removalOrder.
func All(ctx context.Context, pi goolib.PackageInfo, deps DepMap, dbOnly bool, downloader *client.Downloader, db *googetdb.GooDB) error {
	// deps includes an entry for pi itself, which removalOrder leaves out
	// because pi is always uninstalled at the end.
	root := pi.Name + "." + pi.Arch
	order, rootCycle, err := removalOrder(deps, root)
	if err != nil {
		return err
	}
	for _, pkg := range rootCycle {
		logger.Infof("%s is in a dependency cycle with %s, which is being removed; removing %s first", pkg, root, pkg)
	}
	for _, dep := range order {
		if err := uninstallPkg(ctx, relation.SplitKey(dep), dbOnly, downloader, db); err != nil {
			return err
		}
	}
	return uninstallPkg(ctx, pi, dbOnly, downloader, db)
}
