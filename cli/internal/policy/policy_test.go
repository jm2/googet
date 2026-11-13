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
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/googet/v2/goolib"
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

// TestOrphanedDependentsFlagRejectsEmptyAndInvalidValues checks that invalid,
// empty, or whitespace-only -orphaned_dependents values fail flag parsing
// without clobbering googet.conf.
func TestOrphanedDependentsFlagRejectsEmptyAndInvalidValues(t *testing.T) {
	t.Cleanup(settings.Snapshot())
	settings.OrphanedDependents = "remove"

	for _, bad := range []string{"bogus", "", "   ", "warn"} {
		var fl Flags
		fs := flag.NewFlagSet("test", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		fl.Register(fs)
		if err := fs.Parse([]string{"-orphaned_dependents=" + bad}); err == nil {
			t.Errorf("Parse(-orphaned_dependents=%q) succeeded, want error", bad)
		}
		if got := fl.Options(false, false).OrphanedDependents; got != install.OrphanRemove {
			t.Errorf("after rejected flag %q, Options().OrphanedDependents = %v, want OrphanRemove from settings", bad, got)
		}
	}
}

// TestOrphanedDependentsPrecedenceAndForceIsolation checks flag > conf >
// default precedence for -orphaned_dependents and verifies that -force never
// alters OrphanedDependents.
func TestOrphanedDependentsPrecedenceAndForceIsolation(t *testing.T) {
	t.Cleanup(settings.Snapshot())

	for _, confVal := range []struct {
		raw  string
		want install.OrphanMode
	}{
		{raw: "", want: install.OrphanFail},
		{raw: "fail", want: install.OrphanFail},
		{raw: "remove", want: install.OrphanRemove},
		{raw: "bogus", want: install.OrphanFail},
	} {
		for _, flagCase := range []struct {
			set  bool
			arg  string
			want install.OrphanMode
		}{
			{set: false, want: confVal.want},
			{set: true, arg: "fail", want: install.OrphanFail},
			{set: true, arg: "remove", want: install.OrphanRemove},
		} {
			for _, force := range []bool{false, true} {
				rootDir := t.TempDir()
				if confVal.raw != "" {
					if err := os.WriteFile(filepath.Join(rootDir, "googet.conf"), []byte("orphaneddependents: "+confVal.raw+"\n"), 0644); err != nil {
						t.Fatal(err)
					}
				}
				settings.Initialize(rootDir, false)

				var fl Flags
				fs := flag.NewFlagSet("test", flag.ContinueOnError)
				fs.SetOutput(io.Discard)
				fl.Register(fs)
				var args []string
				if flagCase.set {
					args = []string{"-orphaned_dependents=" + flagCase.arg}
				}
				if err := fs.Parse(args); err != nil {
					t.Fatalf("Parse(%v): %v", args, err)
				}
				got := fl.Options(false, force)
				if got.OrphanedDependents != flagCase.want {
					t.Errorf("conf=%q flag=%q(set=%t) force=%t: OrphanedDependents = %v, want %v",
						confVal.raw, flagCase.arg, flagCase.set, force, got.OrphanedDependents, flagCase.want)
				}
				if got.Force != force {
					t.Errorf("Force = %t, want %t", got.Force, force)
				}
			}
		}
	}
}

// TestSettingsAndParseOrphanModeVocabularyAgreement checks that settings.Initialize
// and install.ParseOrphanMode accept and reject the exact same vocabulary.
func TestSettingsAndParseOrphanModeVocabularyAgreement(t *testing.T) {
	t.Cleanup(settings.Snapshot())
	candidates := []string{
		"fail", "FAIL", "  Fail  ",
		"remove", "REMOVE", "\tremove\n",
		"", "   ", "warn", "error", "ignore", "cascade", "true", "false",
	}
	for _, raw := range candidates {
		rootDir := t.TempDir()
		confLine := fmt.Sprintf("orphaneddependents: %q\n", raw)
		if err := os.WriteFile(filepath.Join(rootDir, "googet.conf"), []byte(confLine), 0644); err != nil {
			t.Fatal(err)
		}
		settings.Initialize(rootDir, false)
		mode, parseErr := install.ParseOrphanMode(raw)
		if parseErr == nil {
			if settings.OrphanedDependents != mode.String() {
				t.Errorf("raw=%q: ParseOrphanMode accepted %q, but settings.OrphanedDependents = %q",
					raw, mode.String(), settings.OrphanedDependents)
			}
		} else {
			if settings.OrphanedDependents != "" {
				t.Errorf("raw=%q: ParseOrphanMode rejected (%v), but settings.OrphanedDependents = %q",
					raw, parseErr, settings.OrphanedDependents)
			}
		}
	}
}

// TestWrapErrorFixedHintOrderAndOrphanOnly checks that WrapError appends hints
// in deterministic order (conflict hint before orphan hint) while preserving
// error unwrapping, and that OrphanOnly matches single-chain orphan errors
// while rejecting multi-error joins.
func TestWrapErrorFixedHintOrderAndOrphanOnly(t *testing.T) {
	orphanTyped := &install.OrphanedDependentsError{
		Dependents: []install.OrphanedDependent{{
			Package:     goolib.PackageInfo{Name: "D", Arch: "noarch", Ver: "1"},
			Requirement: goolib.PackageInfo{Name: "P", Ver: "1"},
			Removed:     []goolib.PackageInfo{{Name: "P", Arch: "noarch", Ver: "1"}},
			ReplacedBy:  goolib.PackageInfo{Name: "R", Arch: "noarch", Ver: "1"},
		}},
	}
	nestedOrphan := WrapError(fmt.Errorf("installing A.noarch.2: %w", orphanTyped))
	if !errors.Is(nestedOrphan, install.ErrOrphanedDependents) {
		t.Errorf("errors.Is(nestedOrphan, ErrOrphanedDependents) = false, want true")
	}
	var gotTyped *install.OrphanedDependentsError
	if !errors.As(nestedOrphan, &gotTyped) || len(gotTyped.Dependents) != 1 {
		t.Errorf("errors.As(nestedOrphan, *OrphanedDependentsError) failed: %v", gotTyped)
	}
	if !strings.Contains(nestedOrphan.Error(), "(pass -orphaned_dependents=remove to remove them)") {
		t.Errorf("WrapError(orphan) = %q, want -orphaned_dependents=remove hint", nestedOrphan.Error())
	}
	if !OrphanOnly(nestedOrphan) {
		t.Errorf("OrphanOnly(nestedOrphan) = false, want true")
	}

	both := WrapError(errors.Join(
		fmt.Errorf("%w: conflict", install.ErrPackageConflict),
		orphanTyped,
	))
	conflictIdx := strings.Index(both.Error(), "(pass -package_conflicts=warn to install anyway)")
	orphanIdx := strings.Index(both.Error(), "(pass -orphaned_dependents=remove to remove them)")
	if conflictIdx < 0 || orphanIdx < 0 || conflictIdx >= orphanIdx {
		t.Errorf("WrapError(both) = %q, want conflict hint before orphan hint (idx %d vs %d)", both.Error(), conflictIdx, orphanIdx)
	}
	if OrphanOnly(both) {
		t.Errorf("OrphanOnly(joined conflict+orphan) = true, want false")
	}
	if OrphanOnly(fmt.Errorf("outer: %w", errors.Join(orphanTyped, errors.New("hard failure")))) {
		t.Errorf("OrphanOnly(outer wrapping joined error) = true, want false")
	}
	if OrphanOnly(nil) || OrphanOnly(errors.New("unrelated")) || OrphanOnly(install.ErrPackageConflict) {
		t.Errorf("OrphanOnly matched nil, unrelated, or conflict-only error")
	}
}

type customSingleWrap struct {
	msg   string
	inner error
}

func (c customSingleWrap) Error() string { return c.msg + ": " + c.inner.Error() }
func (c customSingleWrap) Unwrap() error { return c.inner }

// TestCrossFlagIndependenceAndEnumFlagEdgeCases checks that setting one CLI
// policy flag never clobbers the other policy's googet.conf value, and tests
// nil/zero enumFlag formatting and multi-unwrap OrphanOnly rejection.
func TestCrossFlagIndependenceAndEnumFlagEdgeCases(t *testing.T) {
	t.Cleanup(settings.Snapshot())
	rootDir := t.TempDir()
	conf := "packageconflicts: error\norphaneddependents: remove\nstrictconflicts: true\n"
	if err := os.WriteFile(filepath.Join(rootDir, "googet.conf"), []byte(conf), 0644); err != nil {
		t.Fatal(err)
	}
	settings.Initialize(rootDir, false)

	t.Run("package_conflicts_flag_preserves_orphaneddependents_from_conf", func(t *testing.T) {
		var fl Flags
		fs := flag.NewFlagSet("test", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		fl.Register(fs)
		if err := fs.Parse([]string{"-package_conflicts=warn"}); err != nil {
			t.Fatalf("Parse: %v", err)
		}
		got := fl.Options(false, false)
		want := install.Options{
			StrictConflicts:    true,
			PackageConflicts:   install.ConflictWarn,
			OrphanedDependents: install.OrphanRemove,
		}
		if got != want {
			t.Errorf("Options = %+v, want %+v", got, want)
		}
	})

	t.Run("orphaned_dependents_flag_preserves_packageconflicts_from_conf", func(t *testing.T) {
		var fl Flags
		fs := flag.NewFlagSet("test", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		fl.Register(fs)
		if err := fs.Parse([]string{"-orphaned_dependents=fail"}); err != nil {
			t.Fatalf("Parse: %v", err)
		}
		got := fl.Options(true, false)
		want := install.Options{
			DBOnly:             true,
			StrictConflicts:    true,
			PackageConflicts:   install.ConflictError,
			OrphanedDependents: install.OrphanFail,
		}
		if got != want {
			t.Errorf("Options = %+v, want %+v", got, want)
		}
	})

	t.Run("nil_and_zero_enum_flag_string_and_multi_wrap_orphan_only", func(t *testing.T) {
		var nilOrphan *enumFlag[install.OrphanMode]
		if got := nilOrphan.String(); got != "fail" {
			t.Errorf("nilOrphan.String() = %q, want fail", got)
		}
		var nilConflict *enumFlag[install.ConflictMode]
		if got := nilConflict.String(); got != "warn" {
			t.Errorf("nilConflict.String() = %q, want warn", got)
		}
		var zeroOrphan enumFlag[install.OrphanMode]
		if got := zeroOrphan.String(); got != "fail" {
			t.Errorf("zeroOrphan.String() = %q, want fail", got)
		}

		wrappedCustom := customSingleWrap{
			msg:   "context",
			inner: &install.OrphanedDependentsError{},
		}
		if !OrphanOnly(wrappedCustom) {
			t.Errorf("OrphanOnly(customSingleWrap) = false, want true")
		}
		multiFmt := fmt.Errorf("combined: %w and %w", &install.OrphanedDependentsError{}, errors.New("disk full"))
		if OrphanOnly(multiFmt) {
			t.Errorf("OrphanOnly(multi %%w fmt.Errorf) = true, want false")
		}
	})
}
