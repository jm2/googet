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
)

// TestReplaceRemovesDottedDependent checks that replacing a package reports a
// dotted-name dependent under default OrphanFail, removes it under
// OrphanRemove while leaving alone an unrelated package named after the dotted
// name's first segment, and that the replacement simulation predicts the same
// result.
func TestReplaceRemovesDottedDependent(t *testing.T) {
	f := newRelFixture(t,
		[]goolib.PkgSpec{
			{Name: "P", Arch: "noarch", Version: "1"},
			{Name: "foo.bar", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"P": "1"}},
			{Name: "foo", Arch: "noarch", Version: "1"},
		},
		[]goolib.PkgSpec{{Name: "R", Arch: "noarch", Version: "1", Replaces: []string{"P"}}})
	before, err := f.db.FetchPkgs("")
	if err != nil {
		t.Fatal(err)
	}
	r := &goolib.PkgSpec{Name: "R", Arch: "noarch", Version: "1", Replaces: []string{"P"}}
	post, ok := simulateReplacements(r, before)
	if !ok {
		t.Fatalf("simulateReplacements(R) did not complete")
	}
	var simulated []string
	for _, st := range post {
		simulated = append(simulated, st.PackageSpec.String())
	}
	if diff := cmp.Diff([]string{"foo.noarch.1"}, simulated); diff != "" {
		t.Errorf("simulated state after replacing P (-want +got):\n%s", diff)
	}

	if err := f.install(t, "R"); !errors.Is(err, ErrOrphanedDependents) || !strings.Contains(err.Error(), "foo.bar.noarch.1") || strings.Contains(err.Error(), "foo.noarch.1") {
		t.Fatalf("install R (default OrphanFail) = %v, want ErrOrphanedDependents naming only foo.bar.noarch.1", err)
	}
	if err := f.installWith(t, Options{OrphanedDependents: OrphanRemove}, "R"); err != nil {
		t.Fatalf("install R (OrphanRemove): %v", err)
	}
	if diff := cmp.Diff([]string{"R.noarch.1", "foo.noarch.1"}, f.installed(t)); diff != "" {
		t.Errorf("installed packages after replace (-want +got):\n%s", diff)
	}
	if !f.fileExists("foo") {
		t.Errorf("foo's file was removed; only foo.bar depends on P")
	}
	if f.fileExists("foo.bar") {
		t.Errorf("foo.bar's file still exists after its dependency P was replaced")
	}
}
