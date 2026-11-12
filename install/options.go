/*
Copyright 2026 Google Inc. All Rights Reserved.
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

package install

import (
	"github.com/google/googet/v2/googetdb"
	"github.com/google/googet/v2/settings"
	"github.com/google/logger"
)

// Options holds the per-run configuration for an Installer.
type Options struct {
	// DBOnly updates only the package database without modifying system files
	// or running install scripts.
	DBOnly bool
	// Force overwrites files owned by another package when StrictConflicts is
	// set; it applies to file overlaps only and never affects PackageConflicts.
	Force bool
	// StrictConflicts makes overlapping files owned by another package an
	// error unless Force is set.
	StrictConflicts bool
	// PackageConflicts controls how reverse package conflicts from installed
	// packages are handled.
	PackageConflicts ConflictMode
}

// OptionsFromSettings builds Options from the global settings package for
// callers that use googet.conf. It is the only place in package install that
// reads settings. settings.Initialize guarantees settings.PackageConflicts is
// either "" or a string accepted by ParseConflictMode; if it holds any other
// non-empty value (for example from direct mutation), OptionsFromSettings logs
// an error once per call and falls back to ConflictWarn.
func OptionsFromSettings(dbOnly, force bool) Options {
	mode := ConflictWarn
	if settings.PackageConflicts != "" {
		m, err := ParseConflictMode(settings.PackageConflicts)
		if err != nil {
			logger.Errorf("Invalid settings.PackageConflicts value %q: %v; falling back to warn", settings.PackageConflicts, err)
		} else {
			mode = m
		}
	}
	return Options{
		DBOnly:           dbOnly,
		Force:            force,
		StrictConflicts:  settings.StrictConflicts,
		PackageConflicts: mode,
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
