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
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/googet/v2/client"
	"github.com/google/googet/v2/goolib"
)

func seedPackageWithExtraFiles(t *testing.T, f *relFixture, spec goolib.PkgSpec, extraContents map[string]string) {
	t.Helper()
	files := map[string]string{spec.Name: filepath.Join(f.dst, spec.Name)}
	for rel := range extraContents {
		files[rel] = filepath.Join(f.dst, rel)
	}
	spec.Files = files

	var repoURL string
	for u := range f.rm {
		repoURL = u
	}

	contents := map[string]string{spec.Name: spec.String()}
	for rel, body := range extraContents {
		contents[rel] = body
	}
	rs := buildRepoArchiveWithFiles(t, f.gooDir, spec, contents)

	cachedPath := filepath.Join(f.cache, rs.Source)
	copyFile(t, filepath.Join(f.gooDir, rs.Source), cachedPath)

	installed := make(map[string]string, len(files))
	for rel, dstPath := range files {
		body := contents[rel]
		if err := os.MkdirAll(filepath.Dir(dstPath), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dstPath, []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
		installed[dstPath] = checksumOf(t, dstPath)
	}

	if err := f.db.AddPkg(client.PackageState{
		SourceRepo:     repoURL,
		DownloadURL:    repoURL + "/" + rs.Source,
		Checksum:       rs.Checksum,
		LocalPath:      cachedPath,
		PackageSpec:    &spec,
		InstalledFiles: installed,
	}); err != nil {
		t.Fatalf("AddPkg(%s): %v", spec, err)
	}
}

// TestReplaceMultiVictimCoListedFilesAndRecovery verifies that during a
// multi-victim replacement, a path co-listed by V1 and V2 stays on disk after
// the first victim is removed and is deleted after the second victim is
// removed, a path co-listed with an unrelated package Q stays throughout, and
// fault-injected recovery restores V1 and V2 with their rows and files.
func TestReplaceMultiVictimCoListedFilesAndRecovery(t *testing.T) {
	t.Run("fault_injected_recovery_restores_victims_and_preserves_colisters", func(t *testing.T) {
		f := newRelFixture(t, nil, []goolib.PkgSpec{
			{
				Name:     "R",
				Arch:     "noarch",
				Version:  "1",
				Replaces: []string{"V1", "V2"},
				Install:  failingScript,
			},
		})

		seedPackageWithExtraFiles(t, f, goolib.PkgSpec{Name: "V1", Arch: "noarch", Version: "1"}, map[string]string{
			"shared-victims": "from V1",
			"shared-with-q":  "from V1",
		})
		seedPackageWithExtraFiles(t, f, goolib.PkgSpec{Name: "V2", Arch: "noarch", Version: "1"}, map[string]string{
			"shared-victims": "from V2",
			"shared-with-q":  "from V2",
		})
		seedPackageWithExtraFiles(t, f, goolib.PkgSpec{Name: "Q", Arch: "noarch", Version: "1"}, map[string]string{
			"shared-with-q": "from Q",
		})

		sharedVictims := filepath.Join(f.dst, "shared-victims")
		sharedWithQ := filepath.Join(f.dst, "shared-with-q")
		before := f.rows(t)

		uninstallSeen := 0
		var sawAfterFirst, sawAfterReplace bool
		err := f.runWithHook(t, t.Context(), Options{}, "R", func(stage applyStage, step string) error {
			switch stage {
			case stageAfterUninstall:
				uninstallSeen++
				if uninstallSeen == 1 {
					sawAfterFirst = true
					victimName := strings.Split(step, ".")[0]
					if f.fileExists(victimName) {
						t.Errorf("%s exclusive file still exists after its uninstall", victimName)
					}
					if _, statErr := os.Stat(sharedVictims); statErr != nil {
						t.Errorf("Stat(sharedVictims) after first victim %s = %v, want kept while second victim remains", step, statErr)
					}
					if got, readErr := os.ReadFile(sharedWithQ); readErr != nil || string(got) != "from Q" {
						t.Errorf("sharedWithQ after first victim = %q, %v; want kept with %q", got, readErr, "from Q")
					}
				}
			case stageAfterReplace:
				sawAfterReplace = true
				if _, statErr := os.Stat(sharedVictims); !os.IsNotExist(statErr) {
					t.Errorf("Stat(sharedVictims) after both victims removed = %v, want deleted", statErr)
				}
				if got, readErr := os.ReadFile(sharedWithQ); readErr != nil || string(got) != "from Q" {
					t.Errorf("sharedWithQ after both victims removed = %q, %v; want kept with %q", got, readErr, "from Q")
				}
			}
			return nil
		})
		if err == nil || !strings.Contains(err.Error(), "missing-script.exe") {
			t.Fatalf("install R = %v, want failingScript error", err)
		}
		if !sawAfterFirst || !sawAfterReplace {
			t.Fatalf("hook observation flags = (afterFirst=%t, afterReplace=%t), want both true", sawAfterFirst, sawAfterReplace)
		}

		if diff := cmp.Diff([]string{"Q.noarch.1", "V1.noarch.1", "V2.noarch.1"}, f.installed(t)); diff != "" {
			t.Errorf("installed packages after recovery (-want +got):\n%s", diff)
		}
		after := f.rows(t)
		for _, name := range []string{"V1", "V2", "Q"} {
			if diff := cmp.Diff(before[name].InstalledFiles, after[name].InstalledFiles); diff != "" {
				t.Errorf("%s InstalledFiles after recovery (-before +after):\n%s", name, diff)
			}
			if !f.fileExists(name) {
				t.Errorf("%s exclusive file missing after recovery", name)
			}
		}
		if _, statErr := os.Stat(sharedVictims); statErr != nil {
			t.Errorf("Stat(sharedVictims) after recovery = %v, want restored", statErr)
		}
		if _, statErr := os.Stat(sharedWithQ); statErr != nil {
			t.Errorf("Stat(sharedWithQ) after recovery = %v, want present", statErr)
		}
	})

	t.Run("successful_replace_deletes_victim_shared_and_keeps_unrelated_shared", func(t *testing.T) {
		f := newRelFixture(t, nil, []goolib.PkgSpec{
			{
				Name:     "R",
				Arch:     "noarch",
				Version:  "1",
				Replaces: []string{"V1", "V2"},
			},
		})
		seedPackageWithExtraFiles(t, f, goolib.PkgSpec{Name: "V1", Arch: "noarch", Version: "1"}, map[string]string{
			"shared-victims": "from V1",
			"shared-with-q":  "from V1",
		})
		seedPackageWithExtraFiles(t, f, goolib.PkgSpec{Name: "V2", Arch: "noarch", Version: "1"}, map[string]string{
			"shared-victims": "from V2",
			"shared-with-q":  "from V2",
		})
		seedPackageWithExtraFiles(t, f, goolib.PkgSpec{Name: "Q", Arch: "noarch", Version: "1"}, map[string]string{
			"shared-with-q": "from Q",
		})

		sharedVictims := filepath.Join(f.dst, "shared-victims")
		sharedWithQ := filepath.Join(f.dst, "shared-with-q")

		if err := f.install(t, "R"); err != nil {
			t.Fatalf("install R: %v", err)
		}
		if diff := cmp.Diff([]string{"Q.noarch.1", "R.noarch.1"}, f.installed(t)); diff != "" {
			t.Errorf("installed packages after replace (-want +got):\n%s", diff)
		}
		if _, statErr := os.Stat(sharedVictims); !os.IsNotExist(statErr) {
			t.Errorf("Stat(sharedVictims) after replace = %v, want deleted", statErr)
		}
		if got, readErr := os.ReadFile(sharedWithQ); readErr != nil || string(got) != "from Q" {
			t.Errorf("sharedWithQ after replace = %q, %v; want kept with %q", got, readErr, "from Q")
		}
	})
}
