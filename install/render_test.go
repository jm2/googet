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
	"errors"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/googet/v2/goolib"
	"github.com/google/googet/v2/testutil"
)

// TestPlanZeroValue checks that a nil or zero Plan describes an install that
// does nothing.
func TestPlanZeroValue(t *testing.T) {
	for _, p := range []*Plan{nil, {}} {
		if got := p.Installs(); got != nil {
			t.Errorf("Installs() = %v, want nil", got)
		}
		if got := p.Removals(); got != nil {
			t.Errorf("Removals() = %v, want nil", got)
		}
		if got := p.String(); got != "" {
			t.Errorf("String() = %q, want empty", got)
		}
	}
}

// TestInstallerPlan checks what Installer.Plan reports for an install with a
// dependency, an upgrade and a replacement, that it matches what FromRepo
// then does, and that planning writes nothing to the log or the console.
func TestInstallerPlan(t *testing.T) {
	c := corpusCase{
		Target:  "A",
		Orphans: OrphanRemove,
		Installed: []goolib.PkgSpec{
			{Name: "A", Arch: "noarch", Version: "1"},
			{Name: "P", Arch: "noarch", Version: "1"},
			{Name: "D", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"P": "1"}},
			{Name: "X", Arch: "noarch", Version: "1", Conflicts: []string{"B"}},
		},
		Available: []goolib.PkgSpec{
			{Name: "A", Arch: "noarch", Version: "2", PkgDependencies: map[string]string{"B": "1"}, Replaces: []string{"P"}},
			{Name: "B", Arch: "noarch", Version: "1"},
		},
	}
	f := newRelFixture(t, c.Installed, c.Available)
	if _, failErr := New(f.cache, f.db, Options{}).Plan(goolib.PackageInfo{Name: "A"}, "", f.rm, corpusArchs); !errors.Is(failErr, ErrOrphanedDependents) {
		t.Fatalf("Plan (default OrphanFail) = %v, want ErrOrphanedDependents", failErr)
	}
	logs := testutil.CaptureLogs(t)
	var p *Plan
	var err error
	stdout := captureStdout(t, func() {
		p, err = New(f.cache, f.db, Options{OrphanedDependents: OrphanRemove}).Plan(goolib.PackageInfo{Name: "A"}, "", f.rm, corpusArchs)
	})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if logs.Len() != 0 || stdout != "" {
		t.Errorf("Plan wrote log lines %q and console output %q, want none", logs.String(), stdout)
	}
	want := strings.Join([]string{
		"The following packages will be installed:",
		"  B.noarch.1",
		"  A.noarch.2 (upgrade from 1)",
		"The following packages will be removed:",
		"  D.noarch.1 (depends on a package A.noarch.2 replaces)",
		"  P.noarch.1 (replaced by A.noarch.2)",
		`Warning: installed package X.noarch.1 conflicts with B.noarch.1 (entry "B"); installing anyway (package_conflicts=warn)`,
		"",
	}, "\n")
	if diff := cmp.Diff(want, p.String()); diff != "" {
		t.Errorf("String() (-want +got):\n%s", diff)
	}

	live := runLive(t, c, f)
	var installs, removals []string
	for _, pi := range p.Installs() {
		installs = append(installs, pi.String())
	}
	for _, pi := range p.Removals() {
		removals = append(removals, `"`+pi.Name+`"`)
	}
	if diff := cmp.Diff(live.Installs, installs); diff != "" {
		t.Errorf("Installs() differs from what FromRepo installed (-FromRepo +Plan):\n%s", diff)
	}
	if diff := cmp.Diff(live.Removed, removals); diff != "" {
		t.Errorf("Removals() differs from what FromRepo removed (-FromRepo +Plan):\n%s", diff)
	}
}

// TestInstallerPlanReinstall checks that, like FromRepo, Plan doesn't skip a
// package that is already installed at the requested version.
func TestInstallerPlanReinstall(t *testing.T) {
	spec := goolib.PkgSpec{Name: "A", Arch: "noarch", Version: "1"}
	f := newRelFixture(t, []goolib.PkgSpec{spec}, []goolib.PkgSpec{spec})
	p, err := New(f.cache, f.db, Options{}).Plan(goolib.PackageInfo{Name: "A"}, "", f.rm, corpusArchs)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if want := "The following packages will be installed:\n  A.noarch.1 (reinstall)\n"; p.String() != want {
		t.Errorf("String() = %q, want %q", p.String(), want)
	}
}

// TestInstallerPlanError checks that Plan returns the error FromRepo would
// return and no plan.
func TestInstallerPlanError(t *testing.T) {
	f := newRelFixture(t, nil, []goolib.PkgSpec{{Name: "A", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"missing": "1"}}})
	p, err := New(f.cache, f.db, Options{}).Plan(goolib.PackageInfo{Name: "A"}, "", f.rm, corpusArchs)
	if want := "no package found satisfying missing in any repo"; err == nil || err.Error() != want || p != nil {
		t.Errorf("Plan = %v, %v; want nil, %q", p, err, want)
	}
}
