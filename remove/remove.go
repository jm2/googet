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

// uninstallPkg removes the single installed row db.FetchPkg picks for pi. Keep
// its row selection and RemovePkg(name, arch) deletion in sync with
// install.simulateReplacements (covered by
// TestSimulateReplacementsMatchesResolveReplacements in package install).
func uninstallPkg(ctx context.Context, pi goolib.PackageInfo, dbOnly bool, downloader *client.Downloader, db *googetdb.GooDB) error {
	logger.Infof("Executing removal of package %q", pi.Name)
	ps, err := db.FetchPkg(pi)
	if err != nil {
		return fmt.Errorf("package not found in state file: %v", err)
	}
	if ps.PackageSpec == nil {
		return fmt.Errorf("package %q not found in state database", pi.Name)
	}

	if !dbOnly {
		// Fix for package install by older versions of GooGet.
		if ps.LocalPath == "" && ps.UnpackDir != "" {
			ps.LocalPath = ps.UnpackDir + ".goo"
		}
		if ps.LocalPath == "" {
			return fmt.Errorf("no local path available for package %q", pi.Name)
		}

		f, err := os.Open(ps.LocalPath)
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		var rd bool
		if os.IsNotExist(err) {
			logger.Infof("Local package does not exist for %s.%s.%s, redownloading...", pi.Name, pi.Arch, pi.Ver)
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
				return fmt.Errorf("can not redownload %s.%s.%s, DownloadURL not saved", pi.Name, pi.Arch, pi.Ver)
			}
			if err := download.Package(ctx, ps.DownloadURL, ps.LocalPath, ps.Checksum, downloader); err != nil {
				return fmt.Errorf("error redownloading %s.%s.%s, package may no longer exist in the repo, you can use the '-db_only' flag to remove it from the database: %v", pi.Name, pi.Arch, pi.Ver, err)
			}
		}

		eDir, err := download.ExtractPkg(ps.LocalPath)
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
		if err := oswrap.RemoveAll(ps.LocalPath); err != nil {
			logger.Errorf("error removing package data from cache directory: %v", err)
		}
	}
	return db.RemovePkg(ps.PackageSpec.Name, ps.PackageSpec.Arch)
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
	if _, ok := pending[root]; ok {
		for _, pkg := range slices.Sorted(maps.Keys(pending)) {
			if slices.Contains(pending[pkg], root) {
				rootCycle = append(rootCycle, pkg)
			}
		}
		pending.remove(root)
	}
	for len(pending) > 0 {
		var ready []string
		for pkg, dependents := range pending {
			if len(dependents) == 0 {
				ready = append(ready, pkg)
			}
		}
		if len(ready) == 0 {
			remaining := slices.Sorted(maps.Keys(pending))
			return nil, nil, fmt.Errorf("cannot remove %s: these packages depend on each other in a cycle: %s; remove one of them first", root, strings.Join(remaining, ", "))
		}
		slices.Sort(ready)
		for _, pkg := range ready {
			pending.remove(pkg)
		}
		order = append(order, ready...)
	}
	return order, rootCycle, nil
}

// depVersions resolves every key in dm against state via PkgNameSplit and
// returns the "name.arch version" summary list built by EnumerateDeps.
func depVersions(dm DepMap, state client.GooGetState) ([]string, error) {
	var dl []string
	for k := range dm {
		di := goolib.PkgNameSplit(k)
		ps, err := state.GetPackageState(di)
		if err != nil {
			return nil, fmt.Errorf("error finding package in state file, even though the dependancy map was just built: %v", err)
		}
		dl = append(dl, k+" "+ps.PackageSpec.Version)
	}
	return dl, nil
}

// checkResolvable verifies that every key in dm resolves in state under the
// same PkgNameSplit lookup EnumerateDeps performs.
func checkResolvable(dm DepMap, state client.GooGetState) error {
	_, err := depVersions(dm, state)
	return err
}

// Plan returns the dependent packages in state that All uninstalls before
// uninstalling pi, in All's exact removal order and excluding pi itself. Each
// element is a "name.arch" key as passed to goolib.PkgNameSplit by All; pi's
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
// Keep the removal order and root-last execution in sync with Plan and
// install.simulateReplacements (covered by
// TestSimulateReplacementsMatchesResolveReplacements in package install).
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
		if err := uninstallPkg(ctx, goolib.PkgNameSplit(dep), dbOnly, downloader, db); err != nil {
			return err
		}
	}
	return uninstallPkg(ctx, pi, dbOnly, downloader, db)
}
