/*
Copyright 2016 Google Inc. All Rights Reserved.
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

package remove

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"io"
	"io/ioutil"
	"log"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/google/googet/v2/client"
	"github.com/google/googet/v2/googetdb"
	"github.com/google/googet/v2/goolib"
	"github.com/google/googet/v2/oswrap"
	"github.com/google/googet/v2/settings"
	"github.com/google/googet/v2/testutil"
)

func TestUninstallPkg(t *testing.T) {
	settings.Initialize(t.TempDir(), false)
	src, err := ioutil.TempDir("", "")
	if err != nil {
		t.Fatalf("Failed to create temp directory: %v", err)
	}
	defer oswrap.RemoveAll(src)

	dst, err := ioutil.TempDir("", "")
	if err != nil {
		t.Fatalf("Failed to create temp directory: %v", err)
	}
	defer oswrap.RemoveAll(dst)

	testFolder := filepath.Join(dst, "and")
	testFolder2 := filepath.Join(testFolder, "another")
	testFolder3 := filepath.Join(testFolder2, "level")
	if err := oswrap.MkdirAll(testFolder3, 0755); err != nil {
		t.Fatalf("Failed to create test folder: %v", err)
	}

	f, err := oswrap.Create(filepath.Join(src, "test.goo"))
	if err != nil {
		log.Fatal(err)
	}
	defer oswrap.Remove(f.Name())

	gw := gzip.NewWriter(f)
	tw := tar.NewWriter(gw)
	testFile, err := oswrap.Create(filepath.Join(testFolder3, "foo"))
	if err != nil {
		t.Fatalf("Failed to create test file: %v", err)
	}

	fi, err := testFile.Stat()
	if err != nil {
		t.Fatal(err)
	}
	fih, err := tar.FileInfoHeader(fi, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := tw.WriteHeader(fih); err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(tw, testFile); err != nil {
		t.Fatal(err)
	}

	if err := testFile.Close(); err != nil {
		t.Fatalf("Failed to close test file: %v", err)
	}

	tw.Close()
	gw.Close()
	if err := f.Close(); err != nil {
		log.Fatal(err)
	}

	d, err := client.NewDownloader("")
	if err != nil {
		t.Fatalf("NewDownloader: %v", err)
	}
	db, err := googetdb.NewDB(settings.DBFile())
	if err != nil {
		t.Fatalf("googetdb.NewDB: %v", err)
	}
	defer db.Close()
	db.WriteStateToDB(client.GooGetState{
		client.PackageState{
			PackageSpec: &goolib.PkgSpec{
				Name: "foo",
			},
			InstalledFiles: map[string]string{
				testFile.Name(): "chksum",
				testFolder:      "",
				testFolder2:     "",
				testFolder3:     "",
				dst:             "",
			},
			LocalPath: f.Name(),
		},
	})
	if err := uninstallPkg(context.Background(), goolib.PackageInfo{Name: "foo"}, false, d, db); err != nil {
		t.Fatalf("Error running uninstallPkg: %v", err)
	}
	newState, err := db.FetchPkgs("")
	if newState != nil {
		t.Errorf("Package not removed from state: %v", newState)
	}
	for _, n := range []string{testFile.Name(), dst} {
		if _, err := oswrap.Stat(n); err == nil {
			t.Errorf("%s was not removed", n)
		}
	}
}

func TestBuild(t *testing.T) {
	pkg1 := "foo_pkg"
	pkg2 := "bar_pkg"
	pkg3 := "baz_pkg"
	as := ".noarch"
	state := []client.PackageState{
		{
			PackageSpec: &goolib.PkgSpec{
				Name:    pkg1,
				Version: "1.0.0@1",
				Arch:    "noarch",
				PkgDependencies: map[string]string{
					pkg3 + as: "1.0.0@1",
				},
			},
		},
		{
			PackageSpec: &goolib.PkgSpec{
				Name:    pkg2,
				Version: "1.0.0@1",
				Arch:    "noarch",
				PkgDependencies: map[string]string{
					pkg3 + as: "1.0.0@1",
					pkg1 + as: "1.0.0@1",
				},
			},
		},
		{
			PackageSpec: &goolib.PkgSpec{
				Name:    pkg3,
				Version: "1.0.0@1",
				Arch:    "noarch",
			},
		},
	}

	table := []struct {
		pkg  string
		want DepMap
	}{
		{pkg1, DepMap{pkg1 + as: []string{pkg2 + as}, pkg2 + as: nil}},
		{pkg2, DepMap{pkg2 + as: nil}},
		{pkg3, DepMap{pkg1 + as: []string{pkg2 + as}, pkg2 + as: nil, pkg3 + as: []string{pkg1 + as, pkg2 + as}}},
	}
	for _, tt := range table {
		deps := make(DepMap)
		deps.build(tt.pkg, "noarch", state)
		if !reflect.DeepEqual(deps, tt.want) {
			t.Errorf("returned dependancy map does not match expected one: got %v, want %v", deps, tt.want)
		}
	}
}

func TestRemoveDep(t *testing.T) {
	pkg1 := "foo_pkg"
	pkg2 := "bar_pkg"
	pkg3 := "baz_pkg"
	deps := DepMap{pkg1: []string{pkg2}, pkg2: nil, pkg3: []string{pkg1, pkg2}}
	want := DepMap{pkg1: []string{}, pkg3: []string{pkg1}}
	deps.remove(pkg2)

	if !reflect.DeepEqual(deps, want) {
		t.Errorf("returned dependancy map does not match expected one: got %v, want %v", deps, want)
	}
}

func TestPlan(t *testing.T) {
	state := client.GooGetState{
		{PackageSpec: &goolib.PkgSpec{Name: "P", Arch: "noarch", Version: "1"}},
		{PackageSpec: &goolib.PkgSpec{Name: "P", Arch: "x86_64", Version: "1"}},
		{PackageSpec: &goolib.PkgSpec{Name: "D", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"P": "1"}}},
		{PackageSpec: &goolib.PkgSpec{Name: "Top", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"D.noarch": "1"}}},
	}

	// Arch-less root matches dependents with arch-less or arch-qualified deps
	// via SameIdentity(name, ""), omits the root itself, removes leaf
	// dependents before intermediate ones, and emits no log output.
	logs := testutil.CaptureLogs(t)
	got, err := Plan(goolib.PackageInfo{Name: "P"}, state)
	if err != nil {
		t.Fatalf("Plan(P): %v", err)
	}
	want := []string{"Top.noarch", "D.noarch"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Plan(P) = %v, want %v", got, want)
	}
	if logs.Len() != 0 {
		t.Errorf("Plan emitted unexpected log output:\n%s", logs.String())
	}

	// Non-root dependency cycles fail without removing anything.
	cycleState := client.GooGetState{
		{PackageSpec: &goolib.PkgSpec{Name: "P", Arch: "noarch", Version: "1"}},
		{PackageSpec: &goolib.PkgSpec{Name: "A", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"P": "1", "B": "1"}}},
		{PackageSpec: &goolib.PkgSpec{Name: "B", Arch: "noarch", Version: "1", PkgDependencies: map[string]string{"A": "1"}}},
	}
	if _, err := Plan(goolib.PackageInfo{Name: "P", Arch: "noarch"}, cycleState); err == nil {
		t.Error("Plan on non-root cycle = nil, want error")
	}

	// Dotted package names fail state lookup via PkgNameSplit just like EnumerateDeps.
	dottedState := client.GooGetState{
		{PackageSpec: &goolib.PkgSpec{Name: "dotted.pkg", Arch: "noarch", Version: "1"}},
	}
	if _, err := Plan(goolib.PackageInfo{Name: "dotted.pkg", Arch: "noarch"}, dottedState); err == nil {
		t.Error("Plan on dotted package name = nil, want error")
	}
	if logs.Len() != 0 {
		t.Errorf("Plan error paths emitted unexpected log output:\n%s", logs.String())
	}
}

func TestUninstallPkgMissingFromDB(t *testing.T) {
	db, err := googetdb.NewDB(filepath.Join(t.TempDir(), "googet.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	for _, dbOnly := range []bool{false, true} {
		err := uninstallPkg(context.Background(), goolib.PackageInfo{Name: "missing"}, dbOnly, nil, db)
		if err == nil || !strings.Contains(err.Error(), "not found in state database") {
			t.Errorf("uninstallPkg(missing, dbOnly=%t) = %v, want error containing %q", dbOnly, err, "not found in state database")
		}
	}
}
