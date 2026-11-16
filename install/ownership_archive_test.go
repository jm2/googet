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
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/google/googet/v2/goolib"
)

type archiveEntry struct {
	name string
	mode int64
	body string
}

// writeGooArchiveWithEntries writes a .goo archive containing spec's .pkgspec
// followed by entries to gooPath and returns its lowercase hex SHA-256 digest.
func writeGooArchiveWithEntries(t *testing.T, gooPath string, spec goolib.PkgSpec, entries []archiveEntry) string {
	t.Helper()
	fh, err := os.Create(gooPath)
	if err != nil {
		t.Fatalf("Create(%q): %v", gooPath, err)
	}
	h := sha256.New()
	gw := gzip.NewWriter(io.MultiWriter(h, fh))
	tw := tar.NewWriter(gw)

	specBytes, err := json.Marshal(spec)
	if err != nil {
		t.Fatalf("json.Marshal(spec): %v", err)
	}
	if err := tw.WriteHeader(&tar.Header{Name: spec.Name + ".pkgspec", Mode: 0644, Size: int64(len(specBytes))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(specBytes); err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		mode := entry.mode
		if mode == 0 {
			mode = 0644
		}
		if err := tw.WriteHeader(&tar.Header{Name: entry.name, Mode: mode, Size: int64(len(entry.body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(entry.body)); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []io.Closer{tw, gw, fh} {
		if err := c.Close(); err != nil {
			t.Fatal(err)
		}
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}

func buildRepoArchiveWithEntries(t *testing.T, gooDir string, spec goolib.PkgSpec, entries []archiveEntry) goolib.RepoSpec {
	t.Helper()
	gooName := spec.String() + ".goo"
	gooPath := filepath.Join(gooDir, gooName)
	sum := writeGooArchiveWithEntries(t, gooPath, spec, entries)
	specCopy := spec
	return goolib.RepoSpec{
		Checksum:    sum,
		Source:      gooName,
		PackageSpec: &specCopy,
	}
}

func buildRepoArchiveWithFiles(t *testing.T, gooDir string, spec goolib.PkgSpec, contents map[string]string) goolib.RepoSpec {
	t.Helper()
	names := make([]string, 0, len(contents))
	for name := range contents {
		names = append(names, name)
	}
	slices.Sort(names)
	entries := make([]archiveEntry, 0, len(names))
	for _, name := range names {
		entries = append(entries, archiveEntry{name: name, mode: 0644, body: contents[name]})
	}
	return buildRepoArchiveWithEntries(t, gooDir, spec, entries)
}

func publishRepoSpec(f *relFixture, rs goolib.RepoSpec) {
	for repoURL, repo := range f.rm {
		repo.Packages = append(repo.Packages, rs)
		f.rm[repoURL] = repo
		return
	}
}
