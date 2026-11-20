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

package client

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/googet/v2/goolib"
)

// openHandles returns how many of this process's file descriptors refer to
// path, and false when /proc/self/fd isn't available to count them. It
// duplicates the helper in verify's tests because testutil imports client,
// so this package's tests can't share a helper through testutil.
func openHandles(path string) (int, bool) {
	fds, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return 0, false
	}
	n := 0
	for _, fd := range fds {
		target, err := os.Readlink(filepath.Join("/proc/self/fd", fd.Name()))
		if err == nil && target == path {
			n++
		}
	}
	return n, true
}

func TestUnmarshalRepoPackagesCacheReleasesFile(t *testing.T) {
	tempDir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	url := "http://localhost/test-repo"
	cf := filepath.Join(tempDir, fmt.Sprintf("%x.rs", sha256.Sum256([]byte(url))))
	want := []goolib.RepoSpec{{Source: "foo"}}
	j, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("json.Marshal(%v): %v", want, err)
	}
	if err := os.WriteFile(cf, j, 0644); err != nil {
		t.Fatal(err)
	}

	// No http server as this should use the cached content.
	d, err := NewDownloader(proxyServer)
	if err != nil {
		t.Fatalf("NewDownloader(%s): %v", proxyServer, err)
	}
	for i := range 2 {
		got, err := d.unmarshalRepoPackages(context.Background(), url, tempDir, cacheLife)
		if err != nil {
			t.Fatalf("unmarshalRepoPackages read %d: %v", i+1, err)
		}
		if diff := cmp.Diff(want, got); diff != "" {
			t.Fatalf("unmarshalRepoPackages read %d mismatch (-want +got):\n%s", i+1, diff)
		}
	}
	if n, ok := openHandles(cf); ok && n != 0 {
		t.Errorf("%d open handles to the cached repo index after reading it, want 0", n)
	}
	// Windows refuses to rename or remove a file while a handle to it is open.
	moved := cf + ".old"
	if err := os.Rename(cf, moved); err != nil {
		t.Fatalf("os.Rename(%q) after reading the cache: %v", cf, err)
	}
	if err := os.Remove(moved); err != nil {
		t.Errorf("os.Remove(%q) after reading the cache: %v", moved, err)
	}
}
