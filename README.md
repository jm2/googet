# GooGet

[![Go](https://github.com/google/googet/actions/workflows/go.yml/badge.svg)](https://github.com/google/googet/actions/workflows/go.yml)

GooGet (Googet's Obviously Only a Goofy Experimental Title) is a modular
package repository solution primarily designed for Windows.

GooGet is shipped with the official Google Cloud Platform Windows images and is
used to maintain the guest environment.

## Build

To build and package googet run

```
go run goopack/goopack.go googet.goospec
```

This will result in googet.exe and googet.x86_64.VERSION.goo which can be installed on a
machine with the `googet install` command (assuming googet is already
installed).

To install on a fresh machine copy both googet.exe and the googet package
over and run:

```
googet -root 'c:/ProgramData/GooGet' install googet googet.x86_64.VERSION.goo
```

## Conf file

GooGet has the ability to use a conf file to change a few of the default settings.
Place a file named googet.conf in the googet root, which by default is
`C:\ProgramData\GooGet` and configurable by the `-root` flag.


```
proxyserver: http://address_to_proxy:port
archs: [noarch, x86_64]
cachelife: 10m
stalltimeout: 5m
strictconflicts: false
packageconflicts: warn
orphaneddependents: fail
followreplaces: false
```

Configuration keys in `googet.conf` are lowercase (for example, `packageconflicts`,
`orphaneddependents`, `followreplaces`, and `strictconflicts`; PascalCase keys
are ignored).

- `stalltimeout` (a Go duration such as `5m` or `30s`, default `2m`; zero or
  negative values keep the default) is how long googet waits for a server to
  respond, or for more data during a download, before it treats the transfer as
  stalled. Raise it if your proxy scans large files before responding.
- `packageconflicts` controls what happens when an already-installed package
  declares `Conflicts` matching a package being installed, or declares
  `Replaces` naming it (so installing it would put back a package that the
  installed one replaced):
  - `warn` (default): log a warning and continue installing.
  - `error`: fail the installation (normally before that package's dependencies
    are installed).
  - `ignore`: continue installing without printing a warning (still recorded at
    info level in the log).
  Every case is logged as a structured `package_conflict` line tagged with the
  mode: `direction=reverse` for `Conflicts` and `direction=reverse_replaces`
  for `Replaces`. A `Replaces` entry matches only the package's own name, arch
  and version, never its `Provides` entries.
  Reinstalling an already-installed package (`googet install -reinstall` and
  `googet verify`) does not check package conflicts.
- `orphaneddependents` controls what happens when a package's `Replaces`
  entries remove an installed package and leave another installed package's
  name-based dependency unsatisfied:
  - `fail` (default): fail `googet install` before changing anything and list
    the affected dependents; during `googet update`, log a warning, skip that
    update, and continue updating other packages.
  - `remove`: uninstall the affected dependents before removing the replaced
    package.
  Dependents satisfied by the replacing package's `Provides` or by another
  remaining provider stay installed. Dependents that relied only on a
  `Provides` entry of the replaced package are listed and never removed.
- `followreplaces` (default `false`) controls whether `googet update` replaces
  an installed package with another package that declares both `Replaces` and
  `Provides` for it, as when a package is renamed. When it is `false`,
  `googet update` upgrades installed packages in place, and moves to a package
  that `Provides` an installed package's name only once no repository offers
  the installed package any more.

On `googet install` and `googet update`, CLI policy flags override
`googet.conf` (`-follow_replaces` exists only on `googet update`):

| Policy | CLI flag | `googet.conf` key | Default | Notes |
| --- | --- | --- | --- | --- |
| Reverse package conflicts and `Replaces` | `-package_conflicts=warn\|error\|ignore` | `packageconflicts: warn\|error\|ignore` | `warn` | Precedence: CLI flag > `googet.conf` > default. `-force` applies only to file overlaps (`strictconflicts`) and never affects `-package_conflicts`. |
| Orphaned dependents on replace | `-orphaned_dependents=fail\|remove` | `orphaneddependents: fail\|remove` | `fail` | Precedence: CLI flag > `googet.conf` > default. `-force` never affects `-orphaned_dependents`. During `googet update`, `fail` skips the orphaning update instead of failing the run. |
| Update follows `Replaces` | `-follow_replaces`, `-follow_replaces=true\|false` | `followreplaces: true\|false` | `false` | `googet update` only. Precedence: CLI flag > `googet.conf` > default, so `-follow_replaces=false` overrides `followreplaces: true`. `-force` never affects `-follow_replaces`. |

Compatibility note: do not pass `-package_conflicts` to `googet` binaries older
than 3.5.0, `-orphaned_dependents` to `googet` binaries older than 3.6.0, or
`-follow_replaces` to `googet` binaries older than 3.7.0, because older
binaries exit with an unknown-flag usage error (including when an older
`googet` binary runs a self-upgrade). Setting `packageconflicts`,
`orphaneddependents` or `followreplaces` in `googet.conf` is ignored by older
binaries.

## How an install proceeds

`googet install` and `googet update` install a package in three phases:

1. **Plan.** GooGet works out every package it will install, in order, and
   every installed package removed by a `Replaces` entry or cascaded dependent
   removal, and checks conflicts and dependencies. If anything fails here,
   nothing changes. The resolution messages for all packages are printed in
   this phase.
2. **Fetch.** GooGet downloads and verifies the archive of every package it
   will install. Unless `-db_only` is set, it also makes sure the cached
   archive of every package it will remove is present, because uninstalling
   a package runs scripts from its archive; a missing archive is downloaded
   again from where it was installed from. If any archive can't be fetched,
   nothing changes. If a replaced package's archive is gone from its
   repository, the error names it: restore the archive, or remove that
   package first with `googet remove -db_only <name>`, which doesn't run its
   uninstall script.
3. **Apply.** GooGet installs the packages in order, removing each package's
   replaced packages just before installing it.

The confirmation prompt and `googet install -dry_run` show the plan: the
packages that will be installed, in order, the installed packages that will be
removed and why, any `Provides`-only dependents left with an unsatisfied
dependency (not removed), and any conflict warnings. `-dry_run` exits with an
error if the install would fail while it is planned, for example on a conflict
or orphaned dependent. `googet update -dry_run` lists available updates without
planning them.

### Replacing packages is best-effort

When a package's `Replaces` entries remove installed packages, GooGet first
prints and logs a `replace_record` line naming them and their cached archives,
and keeps those archives while the package is installed. If the install fails
before the package is recorded in the database (an uninstall fails, its
install script fails, its files can't be copied, or the database can't be
written, even after a second attempt), GooGet removes the files the package
placed and installs the removed packages again from the kept archives, in the
reverse of the order it removed them, and reports both the failure and what
it reinstalled. Recovery isn't a transaction: uninstall and install scripts,
and installer side effects such as MSI changes, aren't undone, so a package
whose install script ran before its database write failed keeps those
changes, and recovery runs the replaced packages' install scripts again. A
removed package that can't be installed again stays removed: GooGet prints and
logs a `replace_record ... result=not_restored` line naming it and, when one
was kept, its archive. If the database keeps failing, recovery can't record the
replaced packages either, so the replacing package and the replaced packages
all end up removed, and any archives they had are kept.

The record lives only in memory. If GooGet is killed or interrupted (for
example with Ctrl-C, or by a shutdown) while it replaces packages, the
removed packages stay removed. Install them again from the archives the
`replace_record` line names, before `googet clean` deletes those archives. A
kill while a package's files are being placed can also leave them in place,
unrecorded, with the files they overwrote kept as `name.googet-bak*` next to
them.

Archives fetched for an install that then fails stay in the cache directory,
so a later attempt doesn't download them again. While an install runs, the
cache holds the archives of every package it installs, plus those of the
installed packages it removes, which are deleted once the package that
replaces them is recorded, plus the previous versions of upgraded packages
until each upgrade completes. `googet clean` deletes cached archives
that don't belong to an installed package.

## Repo file

GooGet has the ability to use a repo file to change some repo specific settings.
The file is named your-repo-name.repo by default. It can be used to set the
source URL and indicate that the client should pass authorization headers in 
requests to the source URL.

```
URL: https://foo.com/googet/bar
  useoauth: true
```

## Google Cloud Storage as a back-end

Googet supports using Google Cloud Storage as its server.

```
set GOOREPO=%TEMP%\googet-repo
set REPONAME=my_repo
mkdir %GOOREPO%\%REPONAME%
mkdir %GOOREPO%\packages
go run goopack/goopack.go googet.goospec
copy *.goo %GOOREPO%\packages
go run server\gooserve.go -root %GOOREPO% -save_index %GOOREPO%\%REPONAME%\index
gcloud storage buckets create --project my-project gs://my-googet-server
gcloud storage rsync --recursive %GOOREPO% gs://my-googet-server
googet addrepo gcs gs://my-googet-server

rem This command should print 'gcs: gs://my-googet-server'
googet listrepos

rem This command should list the googet package and any other packages in your repo
googet available -sources gs://my-googet-server/

```

Note that you must regenerate the index and re-upload it to your bucket each time
you add or change packges.
