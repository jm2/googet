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
	"fmt"

	"github.com/google/googet/v2/googetdb"
	"github.com/google/googet/v2/settings"
	"github.com/google/logger"
)

// ConflictMode controls how reverse package conflicts are handled.
type ConflictMode int

const (
	// ConflictWarn logs a warning and continues installation when an
	// installed package conflicts with the incoming package.
	ConflictWarn ConflictMode = iota
	// ConflictError fails the installation when an installed package
	// conflicts with the incoming package.
	ConflictError
	// ConflictIgnore logs at info level and continues installation without
	// user-facing output when an installed package conflicts with the
	// incoming package.
	ConflictIgnore
)

// String returns the canonical lowercase configuration name for m.
func (m ConflictMode) String() string {
	switch m {
	case ConflictWarn:
		return "warn"
	case ConflictError:
		return "error"
	case ConflictIgnore:
		return "ignore"
	default:
		return fmt.Sprintf("ConflictMode(%d)", int(m))
	}
}

// ParseConflictMode parses a case-insensitive conflict mode name ("warn",
// "error", or "ignore") after trimming surrounding whitespace.
func ParseConflictMode(s string) (ConflictMode, error) {
	norm, err := settings.NormalizeConflictMode(s)
	if err != nil {
		return ConflictWarn, err
	}
	switch norm {
	case "error":
		return ConflictError, nil
	case "ignore":
		return ConflictIgnore, nil
	default:
		return ConflictWarn, nil
	}
}

// OrphanMode controls how replacing an installed package handles installed
// packages that depend by package name on a package being replaced and are no
// longer satisfied afterwards.
type OrphanMode int

const (
	// OrphanFail fails the installation before changing anything when
	// replacing a package would leave an installed package with an unsatisfied
	// name-based dependency.
	OrphanFail OrphanMode = iota
	// OrphanRemove uninstalls installed packages whose name-based dependencies
	// would be left unsatisfied by a replacement, in reverse dependency order.
	OrphanRemove
)

// String returns the canonical lowercase configuration name for m.
func (m OrphanMode) String() string {
	switch m {
	case OrphanFail:
		return "fail"
	case OrphanRemove:
		return "remove"
	default:
		return fmt.Sprintf("OrphanMode(%d)", int(m))
	}
}

// ParseOrphanMode parses a case-insensitive orphan mode name ("fail" or
// "remove") after trimming surrounding whitespace.
func ParseOrphanMode(s string) (OrphanMode, error) {
	norm, err := settings.NormalizeOrphanMode(s)
	if err != nil {
		return OrphanFail, err
	}
	if norm == "remove" {
		return OrphanRemove, nil
	}
	return OrphanFail, nil
}

// Options holds the per-run configuration for an Installer.
type Options struct {
	// DBOnly updates only the package database without modifying system files
	// or running install scripts.
	DBOnly bool
	// Force overwrites files owned by another package when StrictConflicts is
	// set; it applies to file overlaps only and never affects PackageConflicts
	// or OrphanedDependents.
	Force bool
	// StrictConflicts makes overlapping files owned by another package an
	// error unless Force is set.
	StrictConflicts bool
	// PackageConflicts controls how reverse package conflicts from installed
	// packages are handled.
	PackageConflicts ConflictMode
	// OrphanedDependents controls how replacing a package handles installed
	// packages whose name-based dependencies would become unsatisfied.
	OrphanedDependents OrphanMode
}

// OptionsFromSettings builds Options from the global settings package for
// callers that use googet.conf. It is the only place in package install that
// reads settings. settings.Initialize guarantees settings.PackageConflicts and
// settings.OrphanedDependents are either "" or strings accepted by
// ParseConflictMode and ParseOrphanMode; if either holds any other non-empty
// value (for example from direct mutation), OptionsFromSettings logs an error
// once per call and falls back to its default.
func OptionsFromSettings(dbOnly, force bool) Options {
	return Options{
		DBOnly:             dbOnly,
		Force:              force,
		StrictConflicts:    settings.StrictConflicts,
		PackageConflicts:   modeFromSetting(settings.PackageConflicts, "PackageConflicts", ConflictWarn, ParseConflictMode),
		OrphanedDependents: modeFromSetting(settings.OrphanedDependents, "OrphanedDependents", OrphanFail, ParseOrphanMode),
	}
}

// modeFromSetting parses a non-empty settings string with parse, logging an
// error and returning def when parsing fails.
func modeFromSetting[T fmt.Stringer](raw, field string, def T, parse func(string) (T, error)) T {
	if raw == "" {
		return def
	}
	v, err := parse(raw)
	if err != nil {
		logger.Errorf("Invalid settings.%s: %v; falling back to %s", field, err, def)
		return def
	}
	return v
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
