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
	"github.com/google/googet/v2/internal/ownership"
	"github.com/google/googet/v2/internal/relation"
	"github.com/google/googet/v2/oswrap"
	"github.com/google/googet/v2/progress"
	"github.com/google/googet/v2/system"
	"github.com/google/logger"
)

// repoInstall executes a repository installation run for an Installer.
type repoInstall struct {
	*Installer
	rm         client.RepoMap
	archs      []string
	downloader *client.Downloader
	// stageHook, if set, is called at each applyStage and can fail the
	// install there. Only tests set it.
	stageHook func(stage applyStage, step string) error
}

// FromRepo installs pi and its dependencies from rm.
func (ins *Installer) FromRepo(ctx context.Context, pi goolib.PackageInfo, repo string, rm client.RepoMap, archs []string, downloader *client.Downloader) error {
	ri := &repoInstall{
		Installer:  ins,
		rm:         rm,
		archs:      archs,
		downloader: downloader,
	}
	return ri.fromRepo(ctx, pi, repo)
}

// FromRepo installs a package and all dependencies from a repository.
func FromRepo(ctx context.Context, pi goolib.PackageInfo, repo, cache string, rm client.RepoMap, archs []string, dbOnly, force bool, downloader *client.Downloader, db *googetdb.GooDB) error {
	return New(cache, db, OptionsFromSettings(dbOnly, force)).FromRepo(ctx, pi, repo, rm, archs, downloader)
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

	state, err := db.FetchPkgs("")
	if err != nil {
		return err
	}
	if _, err := buildPlan(planTarget{spec: zs}, nil, state, nil, planPolicy{conflicts: ins.opts.PackageConflicts}, liveSink{}); err != nil {
		return err
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

	// Installing directly from an archive already in the cache (for example,
	// recovering from a kept archive named by replace_record) must not
	// truncate the archive onto itself.
	if srcInfo, err := r.Stat(); err == nil {
		if dstInfo, err := oswrap.Stat(dst); err == nil && os.SameFile(srcInfo, dstInfo) {
			return nil
		}
	}

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

func makeInstallFunction(src, dst string, insFiles map[string]string, opts Options, cm conflictMap, u *undo) func(string, os.FileInfo, error) error {
	return func(path string, fi os.FileInfo, err error) (outerr error) {
		if err != nil {
			return err
		}
		outPath := filepath.Join(dst, strings.TrimPrefix(path, src))

		if owner, ok := cm.owner(outPath); ok && !fi.IsDir() {
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

// conflictMap looks up file-overlap owners for an incoming package against an
// ownership.Index, ignoring rows with currentPkg's name and directory entries
// with empty checksums.
type conflictMap struct {
	ix         *ownership.Index
	currentPkg string
}

func newConflictMap(ix *ownership.Index, currentPkg string) conflictMap {
	return conflictMap{ix: ix, currentPkg: currentPkg}
}

func (cm conflictMap) owner(path string) (string, bool) {
	if cm.ix == nil {
		return "", false
	}
	var owner string
	var found bool
	for _, o := range cm.ix.Owners(path) {
		if o.Row.Name == cm.currentPkg || o.Checksum == "" {
			continue
		}
		owner = o.Row.Name
		found = true
	}
	return owner, found
}

func buildConflictMap(db *googetdb.GooDB, currentPkg string) (conflictMap, error) {
	pkgs, err := db.FetchPkgs("")
	if err != nil {
		return conflictMap{}, err
	}
	return newConflictMap(ownership.NewIndex(pkgs, ownership.RowID{}), currentPkg), nil
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
		// Provides entry, as an install does.
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
//
// Deprecated: ListDeps lists every dependency the repositories resolve,
// whether or not it is already installed, and fails if a dependency that an
// installed package satisfies isn't in any repository. Use Installer.Plan,
// which lists what an install actually does.
func ListDeps(pi goolib.PackageInfo, rm client.RepoMap, repo string, archs []string, db *googetdb.GooDB) ([]goolib.PackageInfo, error) {
	logger.Infof("Building dependency list for %s.%s.%s", pi.Name, pi.Arch, pi.Ver)
	return listDeps(pi, rm, repo, nil, archs, db)
}
