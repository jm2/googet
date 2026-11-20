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
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/googet/v2/client"
	"github.com/google/googet/v2/goolib"
	"github.com/google/googet/v2/testutil"
)

// TestStalledDownloadRetriedThroughExecutor checks that a repository install
// retries a download that stalls, both when it fetches archives before
// changing anything and when a step downloads an archive again because it
// went missing, and then installs and records the package.
func TestStalledDownloadRetriedThroughExecutor(t *testing.T) {
	for _, tc := range []struct {
		desc string
		// stall is the request for the archive that stalls, counting from 1.
		stall int
		// removeFetched removes the fetched archive before the step runs.
		removeFetched bool
	}{
		{desc: "fetch", stall: 1},
		{desc: "download again in the step", stall: 2, removeFetched: true},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			f := newRelFixture(t, nil, []goolib.PkgSpec{{Name: "foo", Arch: "noarch", Version: "1"}})
			src := f.archive(t, "foo")
			body, err := os.ReadFile(src)
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan struct{})
			var mu sync.Mutex
			gets := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/"+filepath.Base(src) {
					http.NotFound(w, r)
					return
				}
				w.Header().Set("Content-Length", strconv.Itoa(len(body)))
				if r.Method != http.MethodGet {
					return
				}
				mu.Lock()
				gets++
				n := gets
				mu.Unlock()
				if n != tc.stall {
					w.Write(body)
					return
				}
				// Send part of the archive, then nothing more.
				w.Write(body[:1])
				w.(http.Flusher).Flush()
				select {
				case <-r.Context().Done():
				case <-done:
				}
			}))
			t.Cleanup(srv.Close)
			t.Cleanup(func() { close(done) })
			var specs []goolib.RepoSpec
			for _, r := range f.rm {
				specs = append(specs, r.Packages...)
			}
			rm := client.RepoMap{srv.URL: client.Repo{Packages: specs}}
			f.downloader.StallTimeout = 200 * time.Millisecond

			logs := testutil.CaptureLogs(t)
			ri := &repoInstall{Installer: New(f.cache, f.db, Options{}), rm: rm, archs: []string{"noarch"}, downloader: f.downloader, stageHook: func(stage applyStage, _ string) error {
				if stage != stageFetched || !tc.removeFetched {
					return nil
				}
				fetched, err := filepath.Glob(filepath.Join(f.cache, "*.goo"))
				if err != nil {
					return err
				}
				for _, p := range fetched {
					if err := os.Remove(p); err != nil {
						return err
					}
				}
				return nil
			}}
			if err := ri.fromRepo(t.Context(), goolib.PackageInfo{Name: "foo"}, ""); err != nil {
				t.Fatalf("install foo = %v, want the stalled download retried", err)
			}
			if diff := cmp.Diff([]string{"foo.noarch.1"}, f.installed(t)); diff != "" {
				t.Errorf("installed packages (-want +got):\n%s", diff)
			}
			if !f.fileExists("foo") {
				t.Errorf("foo's file is missing")
			}
			mu.Lock()
			n := gets
			mu.Unlock()
			if n != tc.stall+1 {
				t.Errorf("archive requested %d times, want %d", n, tc.stall+1)
			}
			if c := strings.Count(logs.String(), "failed (attempt 1 of 3)"); c != 1 || !strings.Contains(logs.String(), client.ErrStalled.Error()) {
				t.Errorf("logged %d retries, want 1 for a stalled transfer; logs:\n%s", c, logs)
			}
		})
	}
}
