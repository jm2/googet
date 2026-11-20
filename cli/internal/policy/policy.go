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

// Package policy resolves package-conflict and orphaned-dependent CLI flags
// over install.OptionsFromSettings for GooGet subcommands that accept
// -package_conflicts and -orphaned_dependents.
package policy

import (
	"errors"
	"flag"
	"fmt"

	"github.com/google/googet/v2/install"
)

// enumFlag is a strict flag.Value backed by a parser function that records
// whether the flag was explicitly set on the command line.
type enumFlag[T fmt.Stringer] struct {
	set   bool
	val   T
	def   T
	parse func(string) (T, error)
}

// String returns the current enum value's string form, or def when unset.
func (e *enumFlag[T]) String() string {
	if e == nil || !e.set {
		var zero T
		if e != nil {
			return e.def.String()
		}
		return zero.String()
	}
	return e.val.String()
}

// Set parses s with e.parse, rejecting empty or unknown values before
// recording the flag as explicitly set.
func (e *enumFlag[T]) Set(s string) error {
	v, err := e.parse(s)
	if err != nil {
		return err
	}
	e.val = v
	e.set = true
	return nil
}

// Flags holds CLI flags that configure package installation conflict and
// orphaned-dependent policies. Its zero value is ready to use without calling
// Register.
type Flags struct {
	packageConflicts   enumFlag[install.ConflictMode]
	orphanedDependents enumFlag[install.OrphanMode]
}

// Register binds the policy flags onto f.
func (fl *Flags) Register(f *flag.FlagSet) {
	fl.packageConflicts.def = install.ConflictWarn
	fl.packageConflicts.parse = install.ParseConflictMode
	fl.orphanedDependents.def = install.OrphanFail
	fl.orphanedDependents.parse = install.ParseOrphanMode

	f.Var(&fl.packageConflicts, "package_conflicts", "how to handle reverse package conflicts from installed packages (warn, error, or ignore); overrides packageconflicts in googet.conf")
	f.Var(&fl.orphanedDependents, "orphaned_dependents", "how to handle installed packages whose dependencies would become unsatisfied when replacing a package (fail or remove); overrides orphaneddependents in googet.conf")
}

// Options resolves the complete install.Options for a subcommand invocation,
// starting from install.OptionsFromSettings and overriding policy fields only
// when the corresponding flags were explicitly passed on the command line.
func (fl *Flags) Options(dbOnly, force bool) install.Options {
	opts := install.OptionsFromSettings(dbOnly, force)
	if fl.packageConflicts.set {
		opts.PackageConflicts = fl.packageConflicts.val
	}
	if fl.orphanedDependents.set {
		opts.OrphanedDependents = fl.orphanedDependents.val
	}
	return opts
}

// WrapError appends CLI remediation hints in a fixed order when err matches
// install.ErrPackageConflict and/or install.ErrOrphanedDependents.
func WrapError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, install.ErrPackageConflict) {
		err = fmt.Errorf("%w (pass -package_conflicts=warn to install anyway)", err)
	}
	if errors.Is(err, install.ErrOrphanedDependents) {
		err = fmt.Errorf("%w (pass -orphaned_dependents=remove to remove them)", err)
	}
	return err
}

// OrphanOnly reports whether err is a single-chain error caused solely by
// install.ErrOrphanedDependents. It returns false immediately if any node in
// the error chain wraps multiple errors via Unwrap() []error, so a joined hard
// error is never mistaken for an orphan-only skip.
func OrphanOnly(err error) bool {
	for cur := err; cur != nil; {
		if _, multi := cur.(interface{ Unwrap() []error }); multi {
			return false
		}
		if cur == install.ErrOrphanedDependents {
			return true
		}
		u, ok := cur.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		cur = u.Unwrap()
	}
	return false
}
