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
	"github.com/google/googet/v2/oswrap"
	"github.com/google/googet/v2/system"
	"github.com/google/logger"
)

func uninstallPkg(ctx context.Context, pi goolib.PackageInfo, dbOnly bool, downloader *client.Downloader, db *googetdb.GooDB) error {
	logger.Infof("Executing removal of package %q", pi.Name)
	ps, err := db.FetchPkg(pi)
	if err != nil {
		return fmt.Errorf("package not found in state file: %v", err)
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
// depends on it. Each package is visited once, so a dependency cycle ends,
// and each dependent is listed once per package even if it names the
// package by more than one dependency key.
func (deps DepMap) build(name, arch string, state client.GooGetState) {
	key := name + "." + arch
	if _, ok := deps[key]; ok {
		return
	}
	logger.Infof("Building dependency map for %q", name)
	deps[key] = nil
	for _, p := range state {
		if p.PackageSpec.Name == name && p.PackageSpec.Arch == arch {
			continue
		}
		for d := range p.PackageSpec.PkgDependencies {
			di := goolib.PkgNameSplit(d)
			if di.Name == name && (di.Arch == arch || di.Arch == "") {
				n, a := p.PackageSpec.Name, p.PackageSpec.Arch
				if dep := n + "." + a; !slices.Contains(deps[key], dep) {
					deps[key] = append(deps[key], dep)
				}
				deps.build(n, a, state)
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

// EnumerateDeps returns a DepMap and list of dependencies for a package.
func EnumerateDeps(pi goolib.PackageInfo, db *googetdb.GooDB) (DepMap, []string, error) {
	dm := make(DepMap)
	state, err := db.FetchPkgs("")
	if err != nil {
		return nil, nil, fmt.Errorf("Error connecting to state database while building dependency map: %v", err)
	}
	dm.build(pi.Name, pi.Arch, state)
	var dl []string
	for k := range dm {
		di := goolib.PkgNameSplit(k)
		ps, err := state.GetPackageState(di)
		if err != nil {
			return nil, nil, fmt.Errorf("error finding package in state file, even though the dependancy map was just built: %v", err)
		}
		dl = append(dl, k+" "+ps.PackageSpec.Version)
	}
	return dm, dl, nil
}

// All removes a package and all dependent packages. Packages with no dependent packages
// will be removed first. The removal order is worked out before anything is
// removed, so a dependency cycle that prevents it fails without removing
// anything. deps is not modified.
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
