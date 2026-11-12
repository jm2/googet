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

package policy

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/google/googet/v2/install"
	"github.com/google/googet/v2/settings"
)

// TestFlagsParseRejectsEmptyAndInvalidValues checks that invalid, empty, or
// whitespace-only -package_conflicts values fail flag parsing without
// clobbering googet.conf.
func TestFlagsParseRejectsEmptyAndInvalidValues(t *testing.T) {
	old := settings.PackageConflicts
	settings.PackageConflicts = "error"
	t.Cleanup(func() { settings.PackageConflicts = old })

	for _, bad := range []string{"bogus", "", "   "} {
		var fl Flags
		fs := flag.NewFlagSet("test", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		fl.Register(fs)
		if err := fs.Parse([]string{"-package_conflicts=" + bad}); err == nil {
			t.Errorf("Parse(-package_conflicts=%q) succeeded, want error", bad)
		}
		if got := fl.Options(false, false).PackageConflicts; got != install.ConflictError {
			t.Errorf("after rejected flag %q, Options().PackageConflicts = %v, want ConflictError from settings", bad, got)
		}
	}
}

// TestFlagsOptionsPrecedence checks zero-value delegation to
// install.OptionsFromSettings, explicit flag precedence over googet.conf,
// StrictConflicts propagation, and -force isolation.
func TestFlagsOptionsPrecedence(t *testing.T) {
	oldStrict := settings.StrictConflicts
	oldPC := settings.PackageConflicts
	t.Cleanup(func() {
		settings.StrictConflicts = oldStrict
		settings.PackageConflicts = oldPC
	})

	settings.StrictConflicts = true
	settings.PackageConflicts = "error"

	// Zero-value Flags delegates directly to install.OptionsFromSettings.
	var zero Flags
	if got, want := zero.Options(true, true), (install.Options{
		DBOnly:           true,
		Force:            true,
		StrictConflicts:  true,
		PackageConflicts: install.ConflictError,
	}); got != want {
		t.Errorf("zero.Options(true, true) = %+v, want %+v", got, want)
	}

	// Explicit -package_conflicts overrides conf across all modes while
	// preserving StrictConflicts and never mapping -force to ConflictIgnore.
	for _, flagVal := range []struct {
		arg  string
		want install.ConflictMode
	}{
		{arg: "warn", want: install.ConflictWarn},
		{arg: "error", want: install.ConflictError},
		{arg: "ignore", want: install.ConflictIgnore},
	} {
		for _, strict := range []bool{false, true} {
			for _, force := range []bool{false, true} {
				settings.StrictConflicts = strict
				settings.PackageConflicts = "error"
				if flagVal.want == install.ConflictError {
					settings.PackageConflicts = "warn"
				}

				var fl Flags
				fs := flag.NewFlagSet("test", flag.ContinueOnError)
				fl.Register(fs)
				if err := fs.Parse([]string{"-package_conflicts=" + flagVal.arg}); err != nil {
					t.Fatalf("Parse(%q): %v", flagVal.arg, err)
				}
				got := fl.Options(false, force)
				want := install.Options{
					DBOnly:           false,
					Force:            force,
					StrictConflicts:  strict,
					PackageConflicts: flagVal.want,
				}
				if got != want {
					t.Errorf("flag=%q, strict=%t, force=%t: Options = %+v, want %+v", flagVal.arg, strict, force, got, want)
				}
			}
		}
	}
}

// TestWrapError checks that only install.ErrPackageConflict receives the CLI
// -package_conflicts=warn remediation hint.
func TestWrapError(t *testing.T) {
	if got := WrapError(nil); got != nil {
		t.Errorf("WrapError(nil) = %v, want nil", got)
	}
	other := errors.New("network timeout")
	if got := WrapError(other); got != other {
		t.Errorf("WrapError(other) = %v, want %v", got, other)
	}
	conflictErr := fmt.Errorf("%w: cannot install A: conflict with B", install.ErrPackageConflict)
	wrapped := WrapError(conflictErr)
	if !errors.Is(wrapped, install.ErrPackageConflict) {
		t.Errorf("errors.Is(wrapped, ErrPackageConflict) = false, want true")
	}
	if !strings.Contains(wrapped.Error(), "pass -package_conflicts=warn to install anyway") {
		t.Errorf("WrapError = %q, want '-package_conflicts=warn' hint", wrapped.Error())
	}
}
