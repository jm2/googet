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

package verify

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/googet/v2/client"
	"github.com/google/googet/v2/googetdb"
	"github.com/google/googet/v2/goolib"
	"github.com/google/googet/v2/settings"
	"github.com/google/subcommands"
)

func writeVerifyTestGoo(t *testing.T, cacheDir string, spec goolib.PkgSpec, contents map[string]string) (string, string) {
	t.Helper()
	gooPath := filepath.Join(cacheDir, spec.String()+".goo")
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
	for name, body := range contents {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0644, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []io.Closer{tw, gw, fh} {
		if err := c.Close(); err != nil {
			t.Fatal(err)
		}
	}
	return gooPath, fmt.Sprintf("%x", h.Sum(nil))
}

func fileSHA256(t *testing.T, p string) string {
	t.Helper()
	f, err := os.Open(p)
	if err != nil {
		t.Fatalf("Open(%q): %v", p, err)
	}
	defer f.Close()
	return goolib.Checksum(f)
}

// TestVerifyReinstallConvergesAcrossOverwrittenSharedFile verifies that running
// `googet verify -reinstall A B` when B previously overwrote a file co-listed
// by A causes A's reinstall to reclaim the file and transfer ownership away
// from B, so B verifies cleanly in the same run and both packages verify
// cleanly on the next run without ping-ponging.
func TestVerifyReinstallConvergesAcrossOverwrittenSharedFile(t *testing.T) {
	root := t.TempDir()
	settings.Initialize(root, false)

	cacheDir := t.TempDir()
	dstDir := t.TempDir()
	shared := filepath.Join(dstDir, "shared.txt")
	onlyA := filepath.Join(dstDir, "only-a.txt")
	onlyB := filepath.Join(dstDir, "only-b.txt")

	specA := goolib.PkgSpec{
		Name:    "A",
		Arch:    "noarch",
		Version: "1.0.0",
		Files:   map[string]string{"shared.txt": shared, "only-a.txt": onlyA},
	}
	specB := goolib.PkgSpec{
		Name:    "B",
		Arch:    "noarch",
		Version: "1.0.0",
		Files:   map[string]string{"shared.txt": shared, "only-b.txt": onlyB},
	}
	gooA, sumGooA := writeVerifyTestGoo(t, cacheDir, specA, map[string]string{
		"shared.txt": "content from A",
		"only-a.txt": "exclusive A",
	})
	gooB, sumGooB := writeVerifyTestGoo(t, cacheDir, specB, map[string]string{
		"shared.txt": "content from B",
		"only-b.txt": "exclusive B",
	})

	// Seed disk in the state where B overwrote shared.txt while an older client
	// left shared.txt listed on both rows with each package's own checksum.
	if err := os.WriteFile(onlyA, []byte("exclusive A"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(onlyB, []byte("exclusive B"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(shared, []byte("content from A"), 0644); err != nil {
		t.Fatal(err)
	}
	sumSharedA := fileSHA256(t, shared)
	if err := os.WriteFile(shared, []byte("content from B"), 0644); err != nil {
		t.Fatal(err)
	}
	sumSharedB := fileSHA256(t, shared)

	db, err := googetdb.NewDB(settings.DBFile())
	if err != nil {
		t.Fatalf("googetdb.NewDB: %v", err)
	}
	if err := db.WriteStateToDB(client.GooGetState{
		{
			LocalPath:   gooA,
			Checksum:    sumGooA,
			PackageSpec: &specA,
			InstalledFiles: map[string]string{
				shared: sumSharedA,
				onlyA:  fileSHA256(t, onlyA),
			},
		},
		{
			LocalPath:   gooB,
			Checksum:    sumGooB,
			PackageSpec: &specB,
			InstalledFiles: map[string]string{
				shared: sumSharedB,
				onlyB:  fileSHA256(t, onlyB),
			},
		},
	}); err != nil {
		_ = db.Close()
		t.Fatalf("WriteStateToDB: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("db.Close: %v", err)
	}

	runVerify := func(reinstall bool, args ...string) subcommands.ExitStatus {
		t.Helper()
		cmd := &verifyCmd{}
		fs := flag.NewFlagSet("verify", flag.ContinueOnError)
		cmd.SetFlags(fs)
		var rawArgs []string
		if reinstall {
			rawArgs = append(rawArgs, "-reinstall")
		}
		rawArgs = append(rawArgs, args...)
		if err := fs.Parse(rawArgs); err != nil {
			t.Fatalf("fs.Parse(%v): %v", rawArgs, err)
		}
		return cmd.Execute(context.Background(), fs)
	}

	// First run: A fails verification and reinstalls, reclaiming shared.txt and
	// transferring shared.txt out of B's row; B is then re-fetched in the same
	// loop and passes verification without reinstalling over A.
	if status := runVerify(true, "A", "B"); status != subcommands.ExitSuccess {
		t.Fatalf("first verify -reinstall A B status = %v, want %v", status, subcommands.ExitSuccess)
	}
	if got, err := os.ReadFile(shared); err != nil || string(got) != "content from A" {
		t.Fatalf("shared.txt after first verify -reinstall A B = %q, %v; want %q", got, err, "content from A")
	}

	// Second run (without -reinstall and with -reinstall): both A and B pass
	// immediately and shared.txt remains A's content.
	if status := runVerify(false, "A", "B"); status != subcommands.ExitSuccess {
		t.Fatalf("follow-up verify A B (no -reinstall) status = %v, want %v", status, subcommands.ExitSuccess)
	}
	if status := runVerify(true, "A", "B"); status != subcommands.ExitSuccess {
		t.Fatalf("second verify -reinstall A B status = %v, want %v", status, subcommands.ExitSuccess)
	}

	dbCheck, err := googetdb.NewDB(settings.DBFile())
	if err != nil {
		t.Fatalf("reopening DB: %v", err)
	}
	defer dbCheck.Close()

	gotA, err := dbCheck.FetchPkg(goolib.PackageInfo{Name: "A", Arch: "noarch"})
	if err != nil {
		t.Fatalf("FetchPkg(A): %v", err)
	}
	gotB, err := dbCheck.FetchPkg(goolib.PackageInfo{Name: "B", Arch: "noarch"})
	if err != nil {
		t.Fatalf("FetchPkg(B): %v", err)
	}
	if diff := cmp.Diff(map[string]string{shared: sumSharedA, onlyA: fileSHA256(t, onlyA)}, gotA.InstalledFiles); diff != "" {
		t.Errorf("A InstalledFiles (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff(map[string]string{onlyB: fileSHA256(t, onlyB)}, gotB.InstalledFiles); diff != "" {
		t.Errorf("B InstalledFiles (-want +got):\n%s", diff)
	}
}
