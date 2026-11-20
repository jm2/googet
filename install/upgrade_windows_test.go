//go:build windows

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
	"os"
	"path/filepath"
	"testing"

	"github.com/google/googet/v2/client"
	"github.com/google/googet/v2/goolib"
)

// TestUpgradeCaseOnlyRenameKeepsFile verifies that on Windows, upgrading a
// package whose file path changes only in casing does not delete the newly
// installed file during upgrade cleanup.
func TestUpgradeCaseOnlyRenameKeepsFile(t *testing.T) {
	f := newRelFixture(t, nil, nil)
	var repoURL string
	for u := range f.rm {
		repoURL = u
	}

	v1Path := filepath.Join(f.dst, "App", "Foo.dll")
	v2Path := filepath.Join(f.dst, "app", "foo.dll")
	if err := os.MkdirAll(filepath.Dir(v1Path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(v1Path, []byte("from v1"), 0644); err != nil {
		t.Fatal(err)
	}
	v1Goo := filepath.Join(f.cache, "A.noarch.1.goo")
	writeGoo(t, v1Goo, map[string]string{"Foo.dll": "from v1"})
	specV1 := &goolib.PkgSpec{
		Name:    "A",
		Arch:    "noarch",
		Version: "1",
		Files:   map[string]string{"Foo.dll": v1Path},
	}
	if err := f.db.AddPkg(client.PackageState{
		SourceRepo:     repoURL,
		LocalPath:      v1Goo,
		PackageSpec:    specV1,
		InstalledFiles: map[string]string{v1Path: checksumOf(t, v1Path)},
	}); err != nil {
		t.Fatalf("AddPkg(v1): %v", err)
	}

	// Publish version 2 in the fixture repository with lowercase destination path.
	specV2 := goolib.PkgSpec{
		Name:    "A",
		Arch:    "noarch",
		Version: "2",
		Files:   map[string]string{"foo.dll": v2Path},
	}
	publishRepoSpec(f, buildRepoArchiveWithFiles(t, f.gooDir, specV2, map[string]string{
		"foo.dll": "from v2",
	}))

	if err := f.install(t, "A"); err != nil {
		t.Fatalf("upgrade install(A): %v", err)
	}
	if got, readErr := os.ReadFile(v2Path); readErr != nil || string(got) != "from v2" {
		t.Errorf("v2 file after case-only rename upgrade = %q, %v; want %q", got, readErr, "from v2")
	}
}
