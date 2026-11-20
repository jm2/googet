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

// Package policy resolves conflict-handling CLI flags over install.OptionsFromSettings
// for GooGet subcommands that accept -package_conflicts.
package policy

import (
	"errors"
	"flag"
	"fmt"

	"github.com/google/googet/v2/install"
)

// modeFlag is a flag.Value that parses an install.ConflictMode and records
// whether the flag was explicitly set on the command line.
type modeFlag struct {
	set bool
	val install.ConflictMode
}

// String returns the current mode name, defaulting to "warn".
func (m *modeFlag) String() string {
	if m == nil || !m.set {
		return install.ConflictWarn.String()
	}
	return m.val.String()
}

// Set parses s with install.ParseConflictMode, rejecting empty or unknown
// values before recording the flag as explicitly set.
func (m *modeFlag) Set(s string) error {
	mode, err := install.ParseConflictMode(s)
	if err != nil {
		return err
	}
	m.val = mode
	m.set = true
	return nil
}

// Flags holds CLI flags that configure package installation conflict policy.
// Its zero value is ready to use without calling Register.
type Flags struct {
	packageConflicts modeFlag
}

// Register binds the conflict-policy flags onto f.
func (fl *Flags) Register(f *flag.FlagSet) {
	f.Var(&fl.packageConflicts, "package_conflicts", "how to handle reverse package conflicts from installed packages (warn, error, or ignore); overrides packageconflicts in googet.conf")
}

// Options resolves the complete install.Options for a subcommand invocation,
// starting from install.OptionsFromSettings and overriding PackageConflicts
// only when -package_conflicts was explicitly passed on the command line.
func (fl *Flags) Options(dbOnly, force bool) install.Options {
	opts := install.OptionsFromSettings(dbOnly, force)
	if fl.packageConflicts.set {
		opts.PackageConflicts = fl.packageConflicts.val
	}
	return opts
}

// WrapError appends a CLI remediation hint when err is a reverse package
// conflict failure from package install.
func WrapError(err error) error {
	if errors.Is(err, install.ErrPackageConflict) {
		return fmt.Errorf("%w (pass -package_conflicts=warn to install anyway)", err)
	}
	return err
}
