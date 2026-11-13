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

package remove

// The remove subcommand handles the uninstallation of a package.

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/google/googet/v2/cli"
	"github.com/google/googet/v2/client"
	"github.com/google/googet/v2/googetdb"
	"github.com/google/googet/v2/goolib"
	"github.com/google/googet/v2/internal/relation"
	"github.com/google/googet/v2/remove"
	"github.com/google/googet/v2/settings"
	"github.com/google/logger"
	"github.com/google/subcommands"
)

func init() { subcommands.Register(&removeCmd{}, "package management") }

type removeCmd struct {
	dbOnly bool
	dryRun bool
}

func (cmd *removeCmd) Name() string     { return "remove" }
func (cmd *removeCmd) Synopsis() string { return "uninstall a package" }
func (cmd *removeCmd) Usage() string {
	return fmt.Sprintf("%s remove [-dry_run] <name>...\n", os.Args[0])
}

func (cmd *removeCmd) SetFlags(f *flag.FlagSet) {
	f.BoolVar(&cmd.dbOnly, "db_only", false, "only make changes to DB, don't perform uninstall system actions")
	f.BoolVar(&cmd.dryRun, "dry_run", false, "show what would be removed but do not remove")
}

func (cmd *removeCmd) Execute(ctx context.Context, flags *flag.FlagSet, _ ...interface{}) subcommands.ExitStatus {
	if flags.NArg() == 0 {
		fmt.Printf("%s\nUsage: %s\n", cmd.Synopsis(), cmd.Usage())
		return subcommands.ExitFailure
	}

	db, err := googetdb.NewDB(settings.DBFile())
	if err != nil {
		logger.Error(err)
		return subcommands.ExitFailure
	}
	defer db.Close()
	downloader, err := client.NewDownloader(settings.ProxyServer)
	if err != nil {
		logger.Error(err)
		return subcommands.ExitFailure
	}
	status := subcommands.ExitSuccess
	for _, arg := range flags.Args() {
		if err := cmd.removeOne(ctx, arg, downloader, db); err != nil {
			logger.Errorf("error removing %v: %v", arg, err)
			status = subcommands.ExitFailure
			continue
		}
		if !cmd.dryRun {
			logger.Infof("Removal of %q and dependent packages completed", arg)
			fmt.Printf("Removal of %s completed\n", arg)
		}
	}
	return status
}

func (cmd *removeCmd) removeOne(ctx context.Context, pkgName string, downloader *client.Downloader, db *googetdb.GooDB) error {
	ps, err := lookupInstalledPkg(pkgName, db)
	if err != nil {
		return err
	}
	if ps.PackageSpec == nil {
		logger.Errorf("Package %q not installed, cannot remove.", pkgName)
		return nil
	}
	pi := goolib.PackageInfo{Name: ps.PackageSpec.Name, Arch: ps.PackageSpec.Arch}
	deps, dl, err := remove.EnumerateDeps(pi, db)
	if err != nil {
		return err
	}
	orphans, err := providesOrphans(deps, db)
	if err != nil {
		logger.Warningf("Checking unsatisfied dependencies for %q: %v", pkgName, err)
	}

	if cmd.dryRun {
		fmt.Println("Dry run: The following packages would be removed:")
		for _, d := range dl {
			fmt.Println("  " + d)
		}
		writeUnsatisfied(os.Stdout, orphans)
		fmt.Printf("Dry run: Would remove %s and all dependencies.\n", pi.Name)
		return nil
	}

	if settings.Confirm {
		var b bytes.Buffer
		fmt.Fprintln(&b, "The following packages will be removed:")
		for _, d := range dl {
			fmt.Fprintln(&b, "  "+d)
		}
		writeUnsatisfied(&b, orphans)
		fmt.Fprintf(&b, "Do you wish to remove %s and all dependencies?", pi.Name)
		if !cli.Confirmation(b.String()) {
			fmt.Println("canceling removal...")
			return nil
		}
	} else {
		writeUnsatisfied(os.Stdout, orphans)
	}
	for _, o := range orphans {
		logger.Warningf("dependency_unsatisfied op=remove result=listed dependent=%s requirement=%q removed=%s",
			o.Dependent, o.Requirement.String(), relation.JoinSpecs(o.Removed, ","))
	}
	fmt.Printf("Removing %s and all dependencies...\n", pi.Name)
	return remove.All(ctx, pi, deps, cmd.dbOnly, downloader, db)
}

// lookupInstalledPkg resolves pkgName as an exact package name, a dotted-safe
// name.arch key when the suffix is a known architecture, or a legacy
// name.arch.version tuple. Because db.FetchPkg falls back to the last installed
// row sharing pi.Name when Arch or Ver does not match, non-bare probes verify
// the returned spec before accepting it.
func lookupInstalledPkg(pkgName string, db *googetdb.GooDB) (client.PackageState, error) {
	trimmed := strings.TrimSpace(pkgName)
	if ps, err := db.FetchPkg(goolib.PackageInfo{Name: trimmed}); err != nil || ps.PackageSpec != nil {
		return ps, err
	}
	if sk := relation.SplitKey(trimmed); sk.Arch != "" && slices.Contains(goolib.ValidArchs(), sk.Arch) {
		ps, err := db.FetchPkg(sk)
		if err != nil || (ps.PackageSpec != nil && ps.PackageSpec.Arch == sk.Arch) {
			return ps, err
		}
		return client.PackageState{}, nil
	}
	pi := goolib.PkgNameSplit(trimmed)
	ps, err := db.FetchPkg(pi)
	if err != nil || (ps.PackageSpec != nil && ps.Match(pi)) {
		return ps, err
	}
	return client.PackageState{}, nil
}

// providesOrphans returns the Provides-only dependents left with an
// unsatisfied dependency once the packages in deps are removed from db.
func providesOrphans(deps remove.DepMap, db *googetdb.GooDB) ([]relation.Orphan, error) {
	state, err := db.FetchPkgs("")
	if err != nil {
		return nil, err
	}
	var removing, remaining []*goolib.PkgSpec
	for _, st := range state {
		if st.PackageSpec == nil {
			continue
		}
		key := st.PackageSpec.Name + "." + st.PackageSpec.Arch
		if _, ok := deps[key]; ok {
			removing = append(removing, st.PackageSpec)
		} else {
			remaining = append(remaining, st.PackageSpec)
		}
	}
	orphans, err := relation.Unsatisfied(removing, remaining)
	if err != nil {
		return nil, err
	}
	var out []relation.Orphan
	for _, o := range orphans {
		if !o.ByName {
			out = append(out, o)
		}
	}
	return out, nil
}

// writeUnsatisfied formats orphans for the removal prompt, dry run, or
// unattended console output. It writes nothing when orphans is empty.
func writeUnsatisfied(w io.Writer, orphans []relation.Orphan) {
	if len(orphans) == 0 {
		return
	}
	fmt.Fprintln(w, "The following packages will have an unsatisfied dependency (not removed):")
	for _, o := range orphans {
		fmt.Fprintf(w, "  %s (requires %s, provided by %s)\n", o.Dependent, o.Requirement.String(), relation.JoinSpecs(o.Removed, ", "))
	}
}
