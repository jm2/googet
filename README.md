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
```

Configuration keys in `googet.conf` are lowercase (for example, `packageconflicts`
and `strictconflicts`; PascalCase keys are ignored).

- `stalltimeout` (a Go duration such as `5m` or `30s`, default `2m`; zero or
  negative values keep the default) is how long googet waits for a server to
  respond, or for more data during a download, before it treats the transfer as
  stalled. Raise it if your proxy scans large files before responding.
- `packageconflicts` controls what happens when an already-installed package
  declares `Conflicts` matching a package being installed:
  - `warn` (default): log a warning and continue installing.
  - `error`: fail the installation (normally before that package's dependencies
    are installed).
  - `ignore`: continue installing without printing a warning (still recorded at
    info level in the log).
  Reinstalling an already-installed package (`googet install -reinstall` and
  `googet verify`) does not check package conflicts.

On `googet install` and `googet update`, `-package_conflicts` overrides
`packageconflicts` in `googet.conf`:

| Policy | CLI flag | `googet.conf` key | Default | Notes |
| --- | --- | --- | --- | --- |
| Reverse package conflicts | `-package_conflicts=warn\|error\|ignore` | `packageconflicts: warn\|error\|ignore` | `warn` | Precedence: CLI flag > `googet.conf` > default. `-force` applies only to file overlaps (`strictconflicts`) and never affects `-package_conflicts`. |

Compatibility note: do not pass `-package_conflicts` to `googet` binaries older
than 3.5.0, because older binaries exit with an unknown-flag usage error
(including when an older `googet` binary runs a self-upgrade). Setting
`packageconflicts` in `googet.conf` is ignored by older binaries.

## How an install proceeds

`googet install` and `googet update` install a package in three phases:

1. **Plan.** GooGet works out every package it will install, in order, and
   every installed package that a `Replaces` entry removes, and checks
   conflicts and dependencies. If anything fails here, nothing changes. The
   resolution messages for all packages are printed in this phase.
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

Archives fetched for an install that then fails stay in the cache directory,
so a later attempt doesn't download them again. While an install runs, the
cache holds the archives of every package it installs, plus those of the
installed packages it removes, plus the previous versions of upgraded
packages until each upgrade completes. `googet clean` deletes cached archives
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
