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

package googetdb

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/googet/v2/client"
	"github.com/google/googet/v2/goolib"
)

// TestAAAScratchTiming measures SQLite operation costs on the CI runner.
func TestAAAScratchTiming(t *testing.T) {
	for round := range 3 {
		dir, err := os.MkdirTemp("", "timing")
		if err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		db, err := NewDB(filepath.Join(dir, "googet.db"))
		if err != nil {
			t.Fatal(err)
		}
		tNew := time.Since(start)
		start = time.Now()
		for i := range 5 {
			if err := db.AddPkg(client.PackageState{PackageSpec: &goolib.PkgSpec{Name: string(rune('a' + i)), Arch: "noarch", Version: "1"}}); err != nil {
				t.Fatal(err)
			}
		}
		tAdd := time.Since(start) / 5
		start = time.Now()
		for range 5 {
			if _, err := db.FetchPkgs(""); err != nil {
				t.Fatal(err)
			}
		}
		tFetch := time.Since(start) / 5
		start = time.Now()
		if _, err := db.FetchPkg(goolib.PackageInfo{Name: "a"}); err != nil {
			t.Fatal(err)
		}
		tFetchOne := time.Since(start)
		start = time.Now()
		db.Close()
		tClose := time.Since(start)
		start = time.Now()
		rmErr := os.RemoveAll(dir)
		tRm := time.Since(start)
		t.Logf("round %d: NewDB=%v AddPkg=%v FetchPkgs=%v FetchPkg=%v Close=%v RemoveAll=%v (err %v)", round, tNew, tAdd, tFetch, tFetchOne, tClose, tRm, rmErr)
	}
	start := time.Now()
	for range 20 {
		db := openSnapshotTestDB(t)
		if err := db.AddPkg(client.PackageState{PackageSpec: &goolib.PkgSpec{Name: "a", Arch: "noarch", Version: "1"}}); err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("20 accumulated open DBs with one AddPkg each: %v", time.Since(start))
}
