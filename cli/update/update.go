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

package update

// The update subcommand handles bulk updating of packages.

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/googet/v2/cli"
	"github.com/google/googet/v2/cli/internal/policy"
	"github.com/google/googet/v2/client"
	"github.com/google/googet/v2/googetdb"
	"github.com/google/googet/v2/install"
	"github.com/google/googet/v2/internal/relation"
	"github.com/google/googet/v2/repo"
	"github.com/google/googet/v2/settings"
	"github.com/google/logger"
	"github.com/google/subcommands"
)

func init() { subcommands.Register(&updateCmd{}, "package management") }

type updateCmd struct {
	dbOnly      bool
	sources     string
	dryRun      bool
	force       bool
	policyFlags policy.Flags
}

func (*updateCmd) Name() string     { return "update" }
func (*updateCmd) Synopsis() string { return "update all packages to the latest version available" }
func (*updateCmd) Usage() string {
	return fmt.Sprintf("%s update [-sources repo1,repo2...] [-dry_run] [-force] [-package_conflicts warn|error|ignore] [-orphaned_dependents fail|remove] [-follow_replaces]\n", filepath.Base(os.Args[0]))
}

func (cmd *updateCmd) SetFlags(f *flag.FlagSet) {
	f.BoolVar(&cmd.dbOnly, "db_only", false, "only make changes to DB, don't perform install system actions")
	f.StringVar(&cmd.sources, "sources", "", "comma separated list of sources, setting this overrides local .repo files")
	f.BoolVar(&cmd.dryRun, "dry_run", false, "check for updates and print them, but do not prompt to install")
	f.BoolVar(&cmd.force, "force", false, "force overwrite of conflicting files (only required if StrictConflicts is enabled in config; doesn't affect -package_conflicts or -orphaned_dependents)")
	cmd.policyFlags.Register(f)
	cmd.policyFlags.RegisterFollowReplaces(f)
}

func (cmd *updateCmd) Execute(ctx context.Context, _ *flag.FlagSet, _ ...interface{}) subcommands.ExitStatus {
	db, err := googetdb.NewDB(settings.DBFile())
	if err != nil {
		logger.Errorf("Failed to open database: %v", err)
		return subcommands.ExitFailure
	}
	defer db.Close()
	cache := settings.CacheDir()
	state, err := db.FetchPkgs("")
	if err != nil {
		logger.Errorf("Failed fetching installed packages: %v", err)
		return subcommands.ExitFailure
	}

	if len(state) == 0 {
		fmt.Println("No packages installed.")
		return subcommands.ExitSuccess
	}

	repos, err := repo.BuildSources(cmd.sources)
	if err != nil {
		logger.Errorf("Failed to initialize repos: %v", err)
		return subcommands.ExitFailure
	}
	if repos == nil {
		logger.Error("No repos defined, create a .repo file or pass using the -sources flag.")
		return subcommands.ExitFailure
	}

	downloader, err := client.NewDownloader(settings.ProxyServer)
	if err != nil {
		logger.Errorf("Failed to initialize downloader: %v", err)
		return subcommands.ExitFailure
	}

	rm := downloader.AvailableVersions(ctx, repos, cache, settings.CacheLife)
	// Plan and print everything, replacements and their skip reasons
	// included, before -dry_run stops, so that a dry run shows exactly what a
	// live run would do.
	plan := planUpdates(state, rm, updateConfig{
		archs:          settings.Archs,
		opts:           cmd.policyFlags.Options(cmd.dbOnly, cmd.force),
		followReplaces: cmd.policyFlags.FollowReplaces(),
	})
	renderUpdates(os.Stdout, plan)
	actions := plan.actions()
	if len(actions) == 0 {
		fmt.Println("No updates available for any installed packages.")
		return subcommands.ExitSuccess
	}

	if cmd.dryRun {
		fmt.Println("Dry run mode active, exiting without updating.")
		return subcommands.ExitSuccess
	}

	if settings.Confirm {
		if !cli.Confirmation("Perform update?") {
			fmt.Println("Not updating.")
			return subcommands.ExitSuccess
		}
	}

	ins := install.New(cache, db, cmd.policyFlags.Options(cmd.dbOnly, cmd.force))
	exitCode := subcommands.ExitSuccess
	for _, a := range actions {
		// Install from the repository the plan printed, so that what update
		// shows and what it installs never disagree.
		pi := a.info()
		if err := ins.FromRepo(ctx, pi, a.repo, rm, settings.Archs, downloader); err != nil {
			if policy.OrphanOnly(err) {
				logSkippedOrphans(err)
				continue
			}
			logger.Errorf("Error updating %s %s %s: %v", pi.Arch, pi.Name, pi.Ver, policy.WrapError(err))
			exitCode = subcommands.ExitFailure
			continue
		}
	}

	return exitCode
}

// logSkippedOrphans emits one structured warning log line per orphaned
// dependent when update skips an orphaning replacement under OrphanFail mode.
func logSkippedOrphans(err error) {
	var oe *install.OrphanedDependentsError
	if !errors.As(err, &oe) || len(oe.Dependents) == 0 {
		logger.Warningf("dependency_unsatisfied op=replace mode=fail result=skipped err=%v", err)
		return
	}
	for _, d := range oe.Dependents {
		removed := make([]string, 0, len(d.Removed))
		for _, r := range d.Removed {
			removed = append(removed, r.String())
		}
		logger.Warningf("dependency_unsatisfied op=replace mode=fail result=skipped dependent=%s requirement=%q removed=%s replacer=%s",
			d.Package, relation.FormatPackageRequirement(d.Requirement), strings.Join(removed, ","), d.ReplacedBy)
	}
}
