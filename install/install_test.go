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

package install

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"io/ioutil"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/googet/v2/client"
	"github.com/google/googet/v2/googetdb"
	"github.com/google/googet/v2/goolib"
	"github.com/google/googet/v2/internal/ownership"
	"github.com/google/googet/v2/internal/relation"
	"github.com/google/googet/v2/oswrap"
	"github.com/google/googet/v2/priority"
	"github.com/google/googet/v2/settings"
	"github.com/google/googet/v2/testutil"
	"github.com/google/logger"
)

// captureStdout returns what f writes to os.Stdout.
func captureStdout(t *testing.T, f func()) (out string) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan string)
	go func() {
		b, _ := io.ReadAll(r)
		r.Close()
		done <- string(b)
	}()
	orig := os.Stdout
	os.Stdout = w
	// The deferred call also restores os.Stdout if f calls t.Fatal.
	defer func() {
		os.Stdout = orig
		w.Close()
		out = <-done
	}()
	f()
	return ""
}

func TestMinInstalled(t *testing.T) {
	settings.Initialize(t.TempDir(), false)
	state := []client.PackageState{
		{
			PackageSpec: &goolib.PkgSpec{
				Name:    "foo_pkg",
				Version: "1.2.3@4",
				Arch:    "noarch",
			},
		},
		{
			PackageSpec: &goolib.PkgSpec{
				Name:    "bar_pkg",
				Version: "0.1.0@1",
				Arch:    "noarch",
			},
		},
	}
	db, err := googetdb.NewDB(settings.DBFile())
	if err != nil {
		t.Fatalf("googetdb.NewDB: %v", err)
	}
	defer db.Close()
	db.WriteStateToDB(state)
	table := []struct {
		pkg, arch string
		ins       bool
	}{
		{"foo_pkg", "noarch", true},
		{"foo_pkg", "", true},
		{"foo_pkg", "x86_64", false},
		{"foo_pkg", "arm64", false},
		{"bar_pkg", "noarch", false},
		{"baz_pkg", "noarch", false},
	}
	for _, tt := range table {
		ma, err := minInstalled(relation.Requirement{Name: tt.pkg, Arch: tt.arch, Ver: "1.0.0@1"}, db)
		if err != nil {
			t.Fatalf("error checking minAvailable: %v", err)
		}
		if ma != tt.ins {
			t.Errorf("minInstalled returned %v for %q when it should return %v", ma, tt.pkg, tt.ins)
		}
	}
}

func TestNeedsInstallation(t *testing.T) {
	settings.Initialize(t.TempDir(), false)
	state := []client.PackageState{
		{
			PackageSpec: &goolib.PkgSpec{
				Name:    "foo_pkg",
				Version: "1.0.0@1",
				Arch:    "noarch",
			},
		},
		{
			PackageSpec: &goolib.PkgSpec{
				Name:    "bar_pkg",
				Version: "1.0.0@1",
				Arch:    "noarch",
			},
		},
		{
			PackageSpec: &goolib.PkgSpec{
				Name:    "baz_pkg",
				Version: "1.0.0@1",
				Arch:    "noarch",
			},
		},
	}
	db, err := googetdb.NewDB(settings.DBFile())
	if err != nil {
		t.Fatalf("googetdb.NewDB: %v", err)
	}
	defer db.Close()
	db.WriteStateToDB(state)
	table := []struct {
		pkg string
		ver string
		ins bool
	}{
		{"foo_pkg", "1.0.0@1", false}, // equal
		{"bar_pkg", "2.0.0@1", true},  // higher
		{"baz_pkg", "0.1.0@1", false}, // lower
		{"pkg", "1.0.0@1", true},      // not installed
	}
	for _, tt := range table {
		ins, err := NeedsInstallation(goolib.PackageInfo{Name: tt.pkg, Arch: "noarch", Ver: tt.ver}, db)
		if err != nil {
			t.Fatalf("Error checking NeedsInstallation: %v", err)
		}
		if ins != tt.ins {
			t.Errorf("NeedsInstallation returned %v for %q when it should return %v", ins, tt.pkg, tt.ins)
		}
	}
}

func TestInstallPkg(t *testing.T) {
	src, err := ioutil.TempDir("", "")
	if err != nil {
		t.Fatalf("Failed to create temp directory: %v", err)
	}
	defer oswrap.RemoveAll(src)

	settings.Initialize(t.TempDir(), false)
	db, err := googetdb.NewDB(settings.DBFile())
	if err != nil {
		t.Fatalf("googetdb.NewDB: %v", err)
	}
	defer db.Close()

	dstRoot, err := ioutil.TempDir("", "")
	if err != nil {
		t.Fatalf("Failed to create temp directory: %v", err)
	}
	defer oswrap.RemoveAll(dstRoot)

	dst := dstRoot + ("/this/is/an/extremely/long/filename/you/wouldnt/expect/to/see/it/" +
		"in/the/wild/but/you/would/actually/be/surprised/at/some/of/the/" +
		"stuff/that/pops/up/and/seriously/two/hundred/and/fify/five/chars" +
		"is/quite/a/large/number/but/somehow/there/were/real/goo/packages" +
		"which/exceeded/this/limit/hence/this/absurdly/long/string/in/" +
		"this/unit/test")
	dst = filepath.FromSlash(dst)

	f, err := os.Create(filepath.Join(src, "test.goo"))
	if err != nil {
		log.Fatal(err)
	}
	defer oswrap.Remove(f.Name())

	gw := gzip.NewWriter(f)
	tw := tar.NewWriter(gw)

	files := []string{"test1", "test2", "test3"}
	want := make(map[string]string)
	for d := dst; d != dstRoot && filepath.Dir(d) != d; d = filepath.Dir(d) {
		want[d] = ""
	}
	for _, n := range files {
		f, err := oswrap.Create(filepath.Join(src, n))
		if err != nil {
			t.Fatalf("Failed to create test file: %v", err)
		}

		fi, err := f.Stat()
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
		if _, err := io.Copy(tw, f); err != nil {
			t.Fatal(err)
		}

		want[filepath.Join(dst, n)] = goolib.Checksum(f)
		if err := f.Close(); err != nil {
			t.Fatalf("Failed to close test file: %v", err)
		}
	}

	tw.Close()
	gw.Close()
	if err := f.Close(); err != nil {
		log.Fatal(err)
	}

	ps := goolib.PkgSpec{Files: map[string]string{"./": dst}}
	got, err := installPkg(f.Name(), &ps, Options{}, db, nil)
	if err != nil {
		t.Fatalf("Error running installPkg: %v", err)
	}

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("installPkg did not return expected file list, got: %+v, want: %+v", got, want)
	}

	for _, n := range files {
		want := filepath.Join(dst, n)
		if _, err := oswrap.Stat(want); err != nil {
			t.Errorf("Expected test file %s does not exist", want)
		}
	}
}

// tree returns the files and directories under root, mapping each slash-
// separated relative path to its contents, or to "/" for a directory.
func tree(t *testing.T, root string) map[string]string {
	t.Helper()
	got := make(map[string]string)
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || path == root {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if d.IsDir() {
			got[filepath.ToSlash(rel)] = "/"
			return nil
		}
		b, err := os.ReadFile(path)
		got[filepath.ToSlash(rel)] = string(b)
		return err
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}
	return got
}

// writeGoo writes a package archive at path holding files, which maps
// slash-separated names to contents. Names ending in .sh are executable.
func writeGoo(t *testing.T, path string, files map[string]string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	gw := gzip.NewWriter(f)
	tw := tar.NewWriter(gw)
	for name, body := range files {
		mode := int64(0644)
		if strings.HasSuffix(name, ".sh") {
			mode = 0755
		}
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: mode, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []io.Closer{tw, gw, f} {
		if err := c.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestInstallPkgRollback(t *testing.T) {
	pkgFiles := map[string]string{"keep": "new", "sub/added": "added"}
	before := map[string]string{"keep": "old"}
	for _, tc := range []struct {
		desc      string
		files     map[string]string // Package Files, with destinations relative to dst.
		installer string            // A missing installer makes the install fail.
		want      map[string]string // The tree of dst after the install; nil means before.
	}{
		{
			desc:  "success",
			files: map[string]string{"./": "."},
			want:  map[string]string{"keep": "new", "sub": "/", "sub/added": "added"},
		},
		{
			desc:  "success with new parent directories",
			files: map[string]string{"keep": "p/q/keep"},
			want:  map[string]string{"keep": "old", "p": "/", "p/q": "/", "p/q/keep": "new"},
		},
		{
			desc:      "installer fails",
			files:     map[string]string{"./": "."},
			installer: "missing-installer",
		},
		{
			// The second mapping writes sub/added again after the first
			// created it; its original state is still "missing".
			desc:      "installer fails after a path is written twice",
			files:     map[string]string{"sub": "sub", "sub/added": "sub/added"},
			installer: "missing-installer",
		},
		{
			desc:      "installer fails after creating parent directories",
			files:     map[string]string{"./": "a/b", "keep": "p/q/keep"},
			installer: "missing-installer",
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			settings.Initialize(t.TempDir(), false)
			db, err := googetdb.NewDB(settings.DBFile())
			if err != nil {
				t.Fatalf("googetdb.NewDB: %v", err)
			}
			defer db.Close()
			dst := t.TempDir()
			for name, body := range before {
				if err := os.WriteFile(filepath.Join(dst, name), []byte(body), 0644); err != nil {
					t.Fatal(err)
				}
			}
			pkg := filepath.Join(t.TempDir(), "test.goo")
			writeGoo(t, pkg, pkgFiles)
			ps := goolib.PkgSpec{Files: map[string]string{}, Install: goolib.ExecFile{Path: tc.installer}}
			for src, rel := range tc.files {
				ps.Files[src] = filepath.Join(dst, rel)
			}

			_, err = installPkg(pkg, &ps, Options{}, db, nil)
			want := tc.want
			if want == nil {
				want = before
			}
			if gotErr, wantErr := err != nil, tc.installer != ""; gotErr != wantErr {
				t.Errorf("installPkg(%q) = %v, want error %t", pkg, err, wantErr)
			}
			if tc.installer != "" {
				// system.Install creates its log (googet_install.log, or
				// <installer>.log on Windows) in the extraction directory
				// before it runs the installer, which happens only after
				// every file is copied.
				extracted := strings.TrimSuffix(pkg, filepath.Ext(pkg))
				if logs, _ := filepath.Glob(filepath.Join(extracted, "*.log")); len(logs) == 0 {
					t.Errorf("installPkg(%q) = %v before running the installer, want installer failure", pkg, err)
				}
			}
			if diff := cmp.Diff(want, tree(t, dst)); diff != "" {
				t.Errorf("installPkg(%q) left an unexpected tree in dst (-want +got):\n%s", pkg, diff)
			}
			if tc.installer == "" && runtime.GOOS != "windows" {
				// The umask applies to ref as it does to the directories
				// that the install creates.
				ref := filepath.Join(t.TempDir(), "ref")
				if err := os.Mkdir(ref, 0755); err != nil {
					t.Fatal(err)
				}
				for rel, body := range want {
					if body != "/" {
						continue
					}
					if got, wantPerm := perm(t, filepath.Join(dst, rel)), perm(t, ref); got != wantPerm {
						t.Errorf("installPkg(%q) created %q with mode %v, want %v", pkg, rel, got, wantPerm)
					}
				}
			}
		})
	}
}

// errAddPkg is the error that the addPkg stub returns or panics with.
var errAddPkg = errors.New("injected AddPkg failure")

// addPkgFault is how the addPkg stub fails, if at all.
type addPkgFault int

const (
	addPkgOK addPkgFault = iota
	addPkgError
	addPkgPanic
)

// TestInstallAddPkg checks that FromDisk and FromRepo roll back the
// files of an upgrade when recording it in the database fails or panics, and
// that on success they record it and FromRepo cleans up the old version. A
// dbOnly install changes no installed files either way. The completion
// message is printed only after the install is recorded.
func TestInstallAddPkg(t *testing.T) {
	before := map[string]string{"keep": "old", "oldonly": "old"}
	// The tree when the new files are placed, the backup of the overwritten
	// file is not yet deleted and the old version is not yet cleaned up. The
	// random suffix of the backup name is dropped.
	placedTree := map[string]string{"keep": "new", "keep" + backupInfix: "old", "oldonly": "old", "sub": "/", "sub/added": "added"}
	for _, tc := range []struct {
		desc      string
		fromRepo  bool
		dbOnly    bool
		installer bool // The package has an install script that succeeds.
		fault     addPkgFault
		want      map[string]string
	}{
		{desc: "FromDisk success", want: map[string]string{"keep": "new", "oldonly": "old", "sub": "/", "sub/added": "added"}},
		{desc: "FromDisk error", fault: addPkgError, want: before},
		{desc: "FromDisk error after the installer ran", installer: true, fault: addPkgError, want: before},
		{desc: "FromDisk panic", fault: addPkgPanic, want: before},
		{desc: "FromRepo success", fromRepo: true, want: map[string]string{"keep": "new", "sub": "/", "sub/added": "added"}},
		{desc: "FromRepo error", fromRepo: true, fault: addPkgError, want: before},
		{desc: "FromRepo panic", fromRepo: true, fault: addPkgPanic, want: before},
		{desc: "FromDisk dbOnly success", dbOnly: true, want: before},
		{desc: "FromDisk dbOnly error", dbOnly: true, fault: addPkgError, want: before},
		{desc: "FromRepo dbOnly success", fromRepo: true, dbOnly: true, want: before},
		{desc: "FromRepo dbOnly error", fromRepo: true, dbOnly: true, fault: addPkgError, want: before},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			if tc.installer && runtime.GOOS != "linux" {
				// goolib.Exec runs shell scripts only on Linux. On Windows,
				// a successful installer also adds an uninstall entry under
				// HKLM.
				t.Skip("install scripts run only on Linux")
			}
			settings.Initialize(t.TempDir(), false)
			db, err := googetdb.NewDB(settings.DBFile())
			if err != nil {
				t.Fatalf("googetdb.NewDB: %v", err)
			}
			defer db.Close()
			dst, cache := t.TempDir(), t.TempDir()
			for name, body := range before {
				if err := os.WriteFile(filepath.Join(dst, name), []byte(body), 0644); err != nil {
					t.Fatal(err)
				}
			}
			oldGoo := filepath.Join(cache, "pkg.noarch.1.0.0@1.goo")
			if err := os.WriteFile(oldGoo, []byte("old package"), 0644); err != nil {
				t.Fatal(err)
			}
			if err := db.AddPkg(client.PackageState{
				LocalPath:   oldGoo,
				PackageSpec: &goolib.PkgSpec{Name: "pkg", Arch: "noarch", Version: "1.0.0@1"},
				InstalledFiles: map[string]string{
					filepath.Join(dst, "keep"):    goolib.Checksum(strings.NewReader("old")),
					filepath.Join(dst, "oldonly"): goolib.Checksum(strings.NewReader("old")),
				},
			}); err != nil {
				t.Fatalf("db.AddPkg: %v", err)
			}

			spec := &goolib.PkgSpec{
				Name:    "pkg",
				Arch:    "noarch",
				Version: "2.0.0@1",
				Files: map[string]string{
					"keep":      filepath.Join(dst, "keep"),
					"sub/added": filepath.Join(dst, "sub", "added"),
				},
			}
			files := map[string]string{"keep": "new", "sub/added": "added"}
			if tc.installer {
				spec.Install = goolib.ExecFile{Path: "install.sh"}
				files["install.sh"] = "#!/bin/sh\n"
			}
			b, err := json.Marshal(spec)
			if err != nil {
				t.Fatal(err)
			}
			files["pkg.pkgspec"] = string(b)
			pkg := filepath.Join(t.TempDir(), "pkg.goo")
			writeGoo(t, pkg, files)

			var placed map[string]string
			orig := addPkg
			t.Cleanup(func() { addPkg = orig })
			addPkg = func(db *googetdb.GooDB, ps client.PackageState, transfers []googetdb.FileTransfer) error {
				placed = make(map[string]string)
				for rel, body := range tree(t, dst) {
					if i := strings.Index(rel, backupInfix); i >= 0 {
						rel = rel[:i+len(backupInfix)]
					}
					placed[rel] = body
				}
				switch tc.fault {
				case addPkgError:
					return errAddPkg
				case addPkgPanic:
					panic(errAddPkg)
				}
				return orig(db, ps, transfers)
			}

			var panicked any
			var install func() error
			if tc.fromRepo {
				goo, err := os.ReadFile(pkg)
				if err != nil {
					t.Fatal(err)
				}
				rs := goolib.RepoSpec{Checksum: goolib.Checksum(bytes.NewReader(goo)), Source: "pkg.goo", PackageSpec: spec}
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					http.ServeFile(w, r, pkg)
				}))
				defer srv.Close()
				repo := srv.URL + "/repo"
				downloader, err := client.NewDownloader("")
				if err != nil {
					t.Fatal(err)
				}
				install = func() error {
					return FromRepo(t.Context(),
						goolib.PackageInfo{Name: "pkg", Arch: "noarch", Ver: "2.0.0@1"},
						repo, cache,
						client.RepoMap{repo: {Packages: []goolib.RepoSpec{rs}}},
						[]string{"noarch"}, tc.dbOnly, false, downloader, db)
				}
			} else {
				install = func() error {
					return FromDisk(pkg, cache, tc.dbOnly, false, false, db)
				}
			}
			logBuf := testutil.CaptureLogs(t)
			out := captureStdout(t, func() {
				err = func() error {
					defer func() { panicked = recover() }()
					return install()
				}()
			})
			logs := logBuf.String()

			// The files are placed, the backup is kept and the old version is
			// not yet cleaned up by the time the install is recorded. A dbOnly
			// install places no files.
			wantPlaced := placedTree
			if tc.dbOnly {
				wantPlaced = before
			}
			if diff := cmp.Diff(wantPlaced, placed); diff != "" {
				t.Errorf("tree when recording the install (-want +got):\n%s", diff)
			}
			switch tc.fault {
			case addPkgError:
				if !errors.Is(err, errAddPkg) {
					t.Errorf("install = %v, want %v", err, errAddPkg)
				}
			case addPkgPanic:
				if panicked != errAddPkg {
					t.Errorf("install panicked with %v, want %v", panicked, errAddPkg)
				}
			default:
				if err != nil || panicked != nil {
					t.Errorf("install = %v, panic %v, want success", err, panicked)
				}
			}
			if got, want := strings.Contains(out, "completed"), tc.fault == addPkgOK; got != want {
				t.Errorf("install printed a completion message = %t, want %t; output:\n%s", got, want, out)
			}
			// The log says that the installer's changes stay only if an
			// installer ran.
			wantLog := ""
			if tc.fault == addPkgError {
				wantLog = "Recording pkg.noarch.2.0.0@1 in the database failed: " + errAddPkg.Error()
				if tc.installer {
					wantLog = "Recording pkg.noarch.2.0.0@1 in the database failed after its installer ran; rolling back its files, but not the changes made by the installer: " + errAddPkg.Error()
				}
			}
			if got, want := strings.Contains(logs, "in the database failed"), wantLog != ""; got != want || !strings.Contains(logs, wantLog) {
				t.Errorf("install log mentions a database failure = %t, want %t with %q; log:\n%s", got, want, wantLog, logs)
			}
			if strings.Contains(logs, "Rollback:") {
				t.Errorf("install logged a rollback problem; log:\n%s", logs)
			}
			if diff := cmp.Diff(tc.want, tree(t, dst)); diff != "" {
				t.Errorf("tree after the install (-want +got):\n%s", diff)
			}
			_, err = os.Stat(oldGoo)
			if gotOld, wantOld := err == nil, !tc.fromRepo || tc.fault != addPkgOK; gotOld != wantOld {
				t.Errorf("old package %s exists = %t (%v), want %t", oldGoo, gotOld, err, wantOld)
			}
			st, err := db.FetchPkg(goolib.PackageInfo{Name: "pkg"})
			if err != nil {
				t.Fatalf("db.FetchPkg: %v", err)
			}
			wantVer := "2.0.0@1"
			if tc.fault != addPkgOK {
				wantVer = "1.0.0@1"
			}
			if st.PackageSpec.Version != wantVer {
				t.Errorf("installed version = %q, want %q", st.PackageSpec.Version, wantVer)
			}
			// The checksums that a dbOnly install records are not checked. The
			// parent directory the install created is recorded with an empty
			// checksum.
			if tc.fault == addPkgOK && !tc.dbOnly {
				wantFiles := map[string]string{
					filepath.Join(dst, "keep"):         goolib.Checksum(strings.NewReader("new")),
					filepath.Join(dst, "sub"):          "",
					filepath.Join(dst, "sub", "added"): goolib.Checksum(strings.NewReader("added")),
				}
				if diff := cmp.Diff(wantFiles, st.InstalledFiles); diff != "" {
					t.Errorf("recorded installed files (-want +got):\n%s", diff)
				}
			}
		})
	}
}

// TestReinstallRollback checks that Reinstall records the reinstalled package
// and deletes the backups when it succeeds, and rolls the files back exactly
// once, recording nothing, when the installer or recording it fails.
func TestReinstallRollback(t *testing.T) {
	before := map[string]string{"keep": "old"}
	for _, tc := range []struct {
		desc        string
		installer   string // A missing installer makes the reinstall fail.
		recordFails bool   // A database trigger makes recording the reinstall fail.
		want        map[string]string
	}{
		{desc: "success", want: map[string]string{"keep": "new", "sub": "/", "sub/added": "added"}},
		{desc: "installer fails", installer: "missing-installer", want: before},
		{desc: "record fails", recordFails: true, want: before},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			settings.Initialize(t.TempDir(), false)
			db, err := googetdb.NewDB(settings.DBFile())
			if err != nil {
				t.Fatalf("googetdb.NewDB: %v", err)
			}
			defer db.Close()
			dst := t.TempDir()
			for name, body := range before {
				if err := os.WriteFile(filepath.Join(dst, name), []byte(body), 0644); err != nil {
					t.Fatal(err)
				}
			}
			pkg := filepath.Join(t.TempDir(), "pkg.goo")
			writeGoo(t, pkg, map[string]string{"keep": "new", "sub/added": "added"})
			ps := client.PackageState{
				LocalPath: pkg,
				PackageSpec: &goolib.PkgSpec{
					Name:    "pkg",
					Arch:    "noarch",
					Version: "1.0.0@1",
					Files: map[string]string{
						"keep":      filepath.Join(dst, "keep"),
						"sub/added": filepath.Join(dst, "sub", "added"),
					},
					Install: goolib.ExecFile{Path: tc.installer},
				},
			}

			if tc.recordFails {
				raw, err := sql.Open("sqlite", settings.DBFile())
				if err != nil {
					t.Fatalf("sql.Open(DBFile): %v", err)
				}
				if _, err := raw.Exec(`CREATE TRIGGER abort_insert BEFORE INSERT ON InstalledPackages BEGIN SELECT RAISE(ABORT, 'simulated record failure'); END;`); err != nil {
					raw.Close()
					t.Fatalf("creating trigger: %v", err)
				}
				if err := raw.Close(); err != nil {
					t.Fatalf("closing raw database: %v", err)
				}
			}

			logBuf := testutil.CaptureLogs(t)
			out := captureStdout(t, func() {
				err = Reinstall(t.Context(), ps, false, false, nil, db)
			})
			logs := logBuf.String()

			if gotErr, wantErr := err != nil, tc.installer != "" || tc.recordFails; gotErr != wantErr {
				t.Errorf("Reinstall() = %v, want error %t", err, wantErr)
			}
			if tc.installer != "" {
				// system.Install creates its log in the extraction directory
				// just before it runs the installer.
				extracted := strings.TrimSuffix(pkg, filepath.Ext(pkg))
				if installerLogs, _ := filepath.Glob(filepath.Join(extracted, "*.log")); len(installerLogs) == 0 {
					t.Errorf("Reinstall() = %v before running the installer, want installer failure", err)
				}
			}
			if got, want := strings.Contains(out, "completed"), tc.installer == "" && !tc.recordFails; got != want {
				t.Errorf("Reinstall() printed a completion message = %t, want %t; output:\n%s", got, want, out)
			}
			// A second rollback would remove the restored file and then fail
			// to restore it from the backup that is already gone.
			if strings.Contains(logs, "Rollback:") || (!tc.recordFails && strings.Contains(logs, "in the database failed")) {
				t.Errorf("Reinstall() logged an unexpected problem; log:\n%s", logs)
			}
			if diff := cmp.Diff(tc.want, tree(t, dst)); diff != "" {
				t.Errorf("tree after Reinstall() (-want +got):\n%s", diff)
			}
			pkgs, err := db.FetchPkgs("")
			if err != nil {
				t.Fatalf("db.FetchPkgs: %v", err)
			}
			// A successful reinstall records its row with the files it
			// installed; a failed one records nothing.
			if tc.installer != "" || tc.recordFails {
				if len(pkgs) != 0 {
					t.Errorf("Reinstall() recorded %v in the database, want nothing", pkgs)
				}
				return
			}
			if len(pkgs) != 1 || pkgs[0].PackageSpec.String() != "pkg.noarch.1.0.0@1" {
				t.Fatalf("Reinstall() recorded %v in the database, want only pkg.noarch.1.0.0@1", pkgs)
			}
			var gotFiles []string
			for f := range pkgs[0].InstalledFiles {
				gotFiles = append(gotFiles, f)
			}
			slices.Sort(gotFiles)
			wantFiles := []string{filepath.Join(dst, "keep"), filepath.Join(dst, "sub"), filepath.Join(dst, "sub", "added")}
			if diff := cmp.Diff(wantFiles, gotFiles); diff != "" {
				t.Errorf("recorded InstalledFiles (-want +got):\n%s", diff)
			}
		})
	}
}

func TestUndo(t *testing.T) {
	for _, tc := range []struct {
		desc   string
		commit bool
		want   map[string]string
	}{
		{desc: "rollback", want: map[string]string{"old": "old"}},
		{desc: "commit", commit: true, want: map[string]string{"old": "new", "empty": "new", "a": "/", "a/b": "/", "a/b/new": "new"}},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			root := t.TempDir()
			if err := os.WriteFile(filepath.Join(root, "old"), []byte("old"), 0644); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(filepath.Join(root, "empty"), 0755); err != nil {
				t.Fatal(err)
			}
			var u undo
			write := func(rel string) {
				t.Helper()
				p := filepath.Join(root, rel)
				if err := u.ensureParents(filepath.Dir(p)); err != nil {
					t.Fatalf("ensureParents(%q) = %v", filepath.Dir(p), err)
				}
				if err := u.prepare(p); err != nil {
					t.Fatalf("prepare(%q) = %v", p, err)
				}
				if err := os.WriteFile(p, []byte("new"), 0644); err != nil {
					t.Fatal(err)
				}
			}
			// Each path is written twice; the second write must not replace
			// the record of its original state.
			for _, rel := range []string{"old", "a/b/new", "old", "a/b/new", "empty"} {
				write(rel)
			}
			if tc.commit {
				u.commit()
			} else {
				// The empty directory that a file replaced is not recreated.
				u.rollback()
			}
			if diff := cmp.Diff(tc.want, tree(t, root)); diff != "" {
				t.Errorf("tree after %s (-want +got):\n%s", tc.desc, diff)
			}
		})
	}
}

// perm returns the permission bits of path.
func perm(t *testing.T, path string) os.FileMode {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Mode().Perm()
}

func TestUndoMkdirsModes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not report Unix permission bits")
	}
	root := t.TempDir()
	// The umask applies to the reference directories as it does to the
	// directories that ensurePackageDir creates.
	refParent, refDir := filepath.Join(root, "ref-parent"), filepath.Join(root, "ref-dir")
	for ref, mode := range map[string]os.FileMode{refParent: 0755, refDir: 0500} {
		if err := os.Mkdir(ref, mode); err != nil {
			t.Fatal(err)
		}
	}
	parent := filepath.Join(root, "a")
	dir := filepath.Join(parent, "b")
	var u undo
	if err := u.ensurePackageDir(dir, 0500); err != nil {
		t.Fatalf("ensurePackageDir(%q, 0500) = %v", dir, err)
	}
	for path, ref := range map[string]string{parent: refParent, dir: refDir} {
		if got, want := perm(t, path), perm(t, ref); got != want {
			t.Errorf("ensurePackageDir(%q, 0500) created %q with mode %v, want %v", dir, path, got, want)
		}
	}
}

func TestUndoMkdirsExisting(t *testing.T) {
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0644); err != nil {
		t.Fatal(err)
	}
	// top is the ancestor of file just below the volume root, such as /tmp or
	// C:\Users. On Windows, oswrap.MkdirAll panics on a volume root, so
	// the directory helpers must not call it on an existing directory or its
	// parent.
	top := file
	for filepath.Dir(filepath.Dir(top)) != filepath.Dir(top) {
		top = filepath.Dir(top)
	}
	for _, tc := range []struct {
		dir     string
		wantErr bool
	}{
		{dir: filepath.Dir(top)},
		{dir: top},
		{dir: filepath.Dir(file)},
		{dir: file, wantErr: true},
	} {
		for name, mkdirs := range map[string]func(*undo, string) error{
			"ensureParents":    (*undo).ensureParents,
			"ensurePackageDir": func(u *undo, dir string) error { return u.ensurePackageDir(dir, 0755) },
		} {
			var u undo
			if err := mkdirs(&u, tc.dir); (err != nil) != tc.wantErr {
				t.Errorf("%s(%q) = %v, want error %t", name, tc.dir, err, tc.wantErr)
			}
			if len(u.changes) != 0 {
				t.Errorf("%s(%q) recorded %v, want no changes", name, tc.dir, u.changes)
			}
		}
	}
}

func TestCleanOldFiles(t *testing.T) {
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

	for _, n := range []string{filepath.Join(src, "test1"), filepath.Join(src, "test2")} {
		if err := ioutil.WriteFile(n, []byte{}, 0666); err != nil {
			t.Fatalf("Failed to create test file: %v", err)
		}
	}

	want := filepath.Join(dst, "test1")
	notWant := filepath.Join(dst, "test2")
	dontCare := filepath.Join(dst, "test3")
	for _, n := range []string{want, notWant, dontCare} {
		if err := ioutil.WriteFile(n, []byte{}, 0666); err != nil {
			t.Fatalf("Failed to create test file: %v", err)
		}
	}

	st := client.PackageState{
		PackageSpec: &goolib.PkgSpec{
			Files: map[string]string{filepath.Base(src): dst},
		},
		InstalledFiles: map[string]string{
			want:    "chksum",
			notWant: "chksum",
			dst:     "",
		},
	}

	cleanOldFiles(st, map[string]string{want: "", dst: ""}, ownership.NewIndex(nil, ownership.RowID{}), client.RemoveOrRename, ownership.Logger{
		Infof:  logger.Infof,
		Errorf: logger.Errorf,
	})

	for _, n := range []string{want, dontCare} {
		if _, err := oswrap.Stat(n); err != nil {
			t.Errorf("Expected test file %s does not exist", want)
		}
	}

	if _, err := oswrap.Stat(notWant); err == nil {
		t.Errorf("Deprecated file %s not removed", notWant)
	}
}

func TestResolveDst(t *testing.T) {
	if err := os.Setenv("foo", "bar"); err != nil {
		t.Errorf("error setting environment variable: %v", err)
	}

	table := []struct {
		dst, want string
	}{
		{"<foo>/some/place", "bar/some/place"},
		{"<foo/some/place", "/<foo/some/place"},
		{"something/<foo>/some/place", "/something/<foo>/some/place"},
	}
	for _, tt := range table {
		got := resolveDst(tt.dst)
		if got != tt.want {
			t.Errorf("resolveDst returned %s, want %s", got, tt.want)
		}
	}
}

func TestIsSatisfied(t *testing.T) {
	settings.Initialize(t.TempDir(), false)
	state := []client.PackageState{
		{
			PackageSpec: &goolib.PkgSpec{
				Name:     "provider_pkg",
				Version:  "1.0.0@1",
				Arch:     "noarch",
				Provides: []string{"libfoo", "libbar=1.5.0"},
			},
		},
		{
			PackageSpec: &goolib.PkgSpec{
				Name:    "real_pkg",
				Version: "2.0.0@1",
				Arch:    "noarch",
			},
		},
	}
	db, err := googetdb.NewDB(settings.DBFile())
	if err != nil {
		t.Fatalf("googetdb.NewDB: %v", err)
	}
	defer db.Close()
	if err := db.WriteStateToDB(state); err != nil {
		t.Fatalf("WriteStateToDB: %v", err)
	}

	tests := []struct {
		name string
		pi   goolib.PackageInfo
		want bool
	}{
		{
			name: "Directly installed package",
			pi:   goolib.PackageInfo{Name: "real_pkg", Arch: "noarch", Ver: "1.0.0"},
			want: true,
		},
		{
			name: "Provided package without version",
			pi:   goolib.PackageInfo{Name: "libfoo", Arch: "noarch", Ver: "1.0.0"},
			want: true,
		},
		{
			name: "Provided package with satisfied version",
			pi:   goolib.PackageInfo{Name: "libbar", Arch: "noarch", Ver: "1.0.0"},
			want: true,
		},
		{
			name: "Provided package with unsatisfied version",
			pi:   goolib.PackageInfo{Name: "libbar", Arch: "noarch", Ver: "2.0.0"},
			want: false,
		},
		{
			name: "Not installed and not provided",
			pi:   goolib.PackageInfo{Name: "missing_pkg", Arch: "noarch", Ver: "1.0.0"},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := isSatisfied(relation.Requirement{Name: tt.pi.Name, Arch: tt.pi.Arch, Ver: tt.pi.Ver}, db)
			if err != nil {
				t.Fatalf("isSatisfied error: %v", err)
			}
			if got != tt.want {
				t.Errorf("isSatisfied(%v) = %v, want %v", tt.pi, got, tt.want)
			}
		})
	}
}

func TestResolveConflicts_Provides(t *testing.T) {
	settings.Initialize(t.TempDir(), false)
	state := []client.PackageState{
		{
			PackageSpec: &goolib.PkgSpec{
				Name:     "provider_pkg",
				Version:  "1.0.0@1",
				Arch:     "noarch",
				Provides: []string{"libconflict"},
			},
		},
	}
	db, err := googetdb.NewDB(settings.DBFile())
	if err != nil {
		t.Fatalf("googetdb.NewDB: %v", err)
	}
	defer db.Close()
	if err := db.WriteStateToDB(state); err != nil {
		t.Fatalf("WriteStateToDB: %v", err)
	}

	ps := &goolib.PkgSpec{
		Name:      "conflicting_pkg",
		Version:   "1.0.0@1",
		Arch:      "noarch",
		Conflicts: []string{"libconflict"},
	}

	_, err = resolveConflicts(ps, exclusions{}, db)
	if err == nil {
		t.Error("resolveConflicts expected error, got nil")
	} else {
		expectedErr := "cannot install, conflict with installed package or provider: libconflict"
		if err.Error() != expectedErr {
			t.Errorf("resolveConflicts error = %q, want %q", err.Error(), expectedErr)
		}
	}
}

func TestFromRepo_SatisfiedByProvider(t *testing.T) {
	// A dependency that an installed package provides is not installed
	// again, even though no repository offers it.
	f := newRelFixture(t,
		[]goolib.PkgSpec{{Name: "provider_pkg", Version: "1.0.0@1", Arch: "noarch", Provides: []string{"libvirt"}}},
		[]goolib.PkgSpec{{Name: "consumer_pkg", Version: "1.0.0@1", Arch: "noarch", PkgDependencies: map[string]string{"libvirt": "1.0.0"}}})
	if err := f.install(t, "consumer_pkg"); err != nil {
		t.Errorf("install consumer_pkg failed: %v", err)
	}
	if diff := cmp.Diff([]string{"consumer_pkg.noarch.1.0.0@1", "provider_pkg.noarch.1.0.0@1"}, f.installed(t)); diff != "" {
		t.Errorf("installed packages (-want +got):\n%s", diff)
	}
}

func TestFromRepo_SatisfiedByUninstalledProvider(t *testing.T) {
	// A dependency that no installed package satisfies resolves to a
	// repository package that provides it.
	f := newRelFixture(t, nil, []goolib.PkgSpec{
		{Name: "provider_pkg", Version: "1.0.0@1", Arch: "noarch", Provides: []string{"libvirt"}},
		{Name: "consumer_pkg", Version: "1.0.0@1", Arch: "noarch", PkgDependencies: map[string]string{"libvirt": "1.0.0"}},
	})
	if err := f.install(t, "consumer_pkg"); err != nil {
		t.Errorf("install consumer_pkg failed to resolve provider: %v", err)
	}
	if diff := cmp.Diff([]string{"consumer_pkg.noarch.1.0.0@1", "provider_pkg.noarch.1.0.0@1"}, f.installed(t)); diff != "" {
		t.Errorf("installed packages (-want +got):\n%s", diff)
	}
}

func TestListDeps_SatisfiedByProvider(t *testing.T) {
	settings.Initialize(t.TempDir(), false)
	db, err := googetdb.NewDB(settings.DBFile())
	if err != nil {
		t.Fatalf("googetdb.NewDB: %v", err)
	}
	defer db.Close()
	rm := client.RepoMap{
		"repo1": client.Repo{
			Priority: priority.Value(500),
			Packages: []goolib.RepoSpec{
				{PackageSpec: &goolib.PkgSpec{Name: "consumer_pkg", Version: "1.0.0@1", Arch: "x86_64",
					PkgDependencies: map[string]string{"libvirt": "0.0.0"}}},
				{PackageSpec: &goolib.PkgSpec{Name: "provider_pkg", Version: "2.0.0@1", Arch: "noarch",
					Provides: []string{"libvirt"}}},
			},
		},
	}
	pi := goolib.PackageInfo{Name: "consumer_pkg", Arch: "x86_64", Ver: "1.0.0@1"}
	got, err := ListDeps(pi, rm, "repo1", []string{"x86_64", "noarch"}, db)
	if err != nil {
		t.Fatalf("ListDeps: %v", err)
	}
	want := []goolib.PackageInfo{pi, {Name: "provider_pkg", Arch: "noarch", Ver: "2.0.0@1"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ListDeps = %v, want %v", got, want)
	}
}

func TestListDeps_SelfDependency(t *testing.T) {
	for _, tc := range []struct {
		desc    string
		pkgs    []*goolib.PkgSpec // pkgs[0] is passed to ListDeps
		want    []goolib.PackageInfo
		wantErr string
	}{
		{
			desc: "depends on a name it provides",
			pkgs: []*goolib.PkgSpec{
				{Name: "self_pkg", Version: "1.0.0", Arch: "noarch", Provides: []string{"virt"},
					PkgDependencies: map[string]string{"virt": "0.0.0"}},
			},
			wantErr: "resolves to the package itself",
		},
		{
			desc: "depends on itself",
			pkgs: []*goolib.PkgSpec{
				{Name: "self_pkg", Version: "1.0.0", Arch: "noarch",
					PkgDependencies: map[string]string{"self_pkg": "0.0.0"}},
			},
			wantErr: "resolves to the package itself",
		},
		{
			desc: "depends on another arch of itself",
			pkgs: []*goolib.PkgSpec{
				{Name: "self_pkg", Version: "1.0.0", Arch: "x86_64",
					PkgDependencies: map[string]string{"self_pkg.x86_32": "0.0.0"}},
				{Name: "self_pkg", Version: "1.0.0", Arch: "x86_32"},
			},
			want: []goolib.PackageInfo{{Name: "self_pkg", Arch: "x86_32", Ver: "1.0.0"}},
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			settings.Initialize(t.TempDir(), false)
			db, err := googetdb.NewDB(settings.DBFile())
			if err != nil {
				t.Fatalf("googetdb.NewDB: %v", err)
			}
			defer db.Close()
			var specs []goolib.RepoSpec
			for _, ps := range tc.pkgs {
				specs = append(specs, goolib.RepoSpec{PackageSpec: ps})
			}
			rm := client.RepoMap{"repo1": client.Repo{Priority: priority.Value(500), Packages: specs}}
			pi := goolib.PackageInfo{Name: tc.pkgs[0].Name, Arch: tc.pkgs[0].Arch, Ver: tc.pkgs[0].Version}
			got, err := ListDeps(pi, rm, "repo1", []string{"x86_64", "x86_32", "noarch"}, db)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("ListDeps = %v, %v; want error containing %q", got, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ListDeps: %v", err)
			}
			want := append([]goolib.PackageInfo{pi}, tc.want...)
			if diff := cmp.Diff(want, got); diff != "" {
				t.Errorf("ListDeps mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestFromRepo_SelfDependency(t *testing.T) {
	for _, tc := range []struct {
		desc     string
		pkgs     []*goolib.PkgSpec // pkgs[0] is the package FromRepo installs.
		wantSelf bool              // wantSelf reports whether to expect the "resolves to the package itself" error.
	}{
		{
			desc: "depends on a name it provides",
			pkgs: []*goolib.PkgSpec{
				{Name: "self_pkg", Version: "1.0.0", Arch: "noarch", Provides: []string{"virt"},
					PkgDependencies: map[string]string{"virt": "0.0.0"}},
			},
			wantSelf: true,
		},
		{
			desc: "depends on itself",
			pkgs: []*goolib.PkgSpec{
				{Name: "self_pkg", Version: "1.0.0", Arch: "noarch",
					PkgDependencies: map[string]string{"self_pkg": "0.0.0"}},
			},
			wantSelf: true,
		},
		{
			// Resolution succeeds and the install then fails at the download,
			// because the test repo has no real package files.
			desc: "depends on another arch of itself",
			pkgs: []*goolib.PkgSpec{
				{Name: "self_pkg", Version: "1.0.0", Arch: "x86_64",
					PkgDependencies: map[string]string{"self_pkg.x86_32": "0.0.0"}},
				{Name: "self_pkg", Version: "1.0.0", Arch: "x86_32"},
			},
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			settings.Initialize(t.TempDir(), false)
			db, err := googetdb.NewDB(settings.DBFile())
			if err != nil {
				t.Fatalf("googetdb.NewDB: %v", err)
			}
			defer db.Close()
			var specs []goolib.RepoSpec
			for _, ps := range tc.pkgs {
				specs = append(specs, goolib.RepoSpec{PackageSpec: ps})
			}
			rm := client.RepoMap{"repo1": client.Repo{Priority: priority.Value(500), Packages: specs}}
			downloader, err := client.NewDownloader("")
			if err != nil {
				t.Fatalf("NewDownloader: %v", err)
			}
			pi := goolib.PackageInfo{Name: tc.pkgs[0].Name, Arch: tc.pkgs[0].Arch}
			err = New(t.TempDir(), db, Options{}).FromRepo(t.Context(), pi, "", rm, []string{"x86_64", "x86_32", "noarch"}, downloader)
			const selfErr = "resolves to the package itself"
			if gotSelf := err != nil && strings.Contains(err.Error(), selfErr); gotSelf != tc.wantSelf {
				t.Errorf("FromRepo = %v; want an error containing %q: %v", err, selfErr, tc.wantSelf)
			}
		})
	}
}

func TestListDeps_VersionRequirement(t *testing.T) {
	for _, tc := range []struct {
		desc    string
		dep     map[string]string
		pkgs    []*goolib.PkgSpec
		want    []goolib.PackageInfo // excluding the consumer itself
		wantErr string               // substring of the expected error, if any
	}{
		{
			desc: "versioned provides meets requirement, provider package version lower",
			dep:  map[string]string{"virt": "3.0.0"},
			pkgs: []*goolib.PkgSpec{
				{Name: "provider_pkg", Version: "1.0.0", Arch: "noarch", Provides: []string{"virt=5.0.0"}},
			},
			want: []goolib.PackageInfo{{Name: "provider_pkg", Arch: "noarch", Ver: "1.0.0"}},
		},
		{
			desc: "unversioned provides meets any required version",
			dep:  map[string]string{"virt": "3.0.0"},
			pkgs: []*goolib.PkgSpec{
				{Name: "provider_pkg", Version: "1.0.0", Arch: "noarch", Provides: []string{"virt"}},
			},
			want: []goolib.PackageInfo{{Name: "provider_pkg", Arch: "noarch", Ver: "1.0.0"}},
		},
		{
			desc: "versioned provides below requirement",
			dep:  map[string]string{"virt": "3.0.0"},
			pkgs: []*goolib.PkgSpec{
				{Name: "provider_pkg", Version: "4.0.0", Arch: "noarch", Provides: []string{"virt=2.0.0"}},
			},
			wantErr: "cannot resolve dependency virt version 3.0.0 or greater",
		},
		{
			desc: "direct dependency meets requirement",
			dep:  map[string]string{"dep_pkg": "3.0.0"},
			pkgs: []*goolib.PkgSpec{
				{Name: "dep_pkg", Version: "3.1.0", Arch: "noarch"},
			},
			want: []goolib.PackageInfo{{Name: "dep_pkg", Arch: "noarch", Ver: "3.1.0"}},
		},
		{
			desc: "direct dependency below requirement",
			dep:  map[string]string{"dep_pkg": "3.0.0"},
			pkgs: []*goolib.PkgSpec{
				{Name: "dep_pkg", Version: "1.0.0", Arch: "noarch"},
			},
			wantErr: "cannot resolve dependency dep_pkg version 3.0.0 or greater",
		},
		{
			desc: "direct dependency below requirement, provider meets it",
			dep:  map[string]string{"dep_pkg": "3.0.0"},
			pkgs: []*goolib.PkgSpec{
				{Name: "dep_pkg", Version: "1.0.0", Arch: "noarch"},
				{Name: "provider_pkg", Version: "1.0.0", Arch: "noarch", Provides: []string{"dep_pkg=3.0.0"}},
			},
			want: []goolib.PackageInfo{{Name: "provider_pkg", Arch: "noarch", Ver: "1.0.0"}},
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			settings.Initialize(t.TempDir(), false)
			db, err := googetdb.NewDB(settings.DBFile())
			if err != nil {
				t.Fatalf("googetdb.NewDB: %v", err)
			}
			defer db.Close()
			consumer := &goolib.PkgSpec{Name: "consumer_pkg", Version: "1.0.0", Arch: "noarch", PkgDependencies: tc.dep}
			specs := []goolib.RepoSpec{{PackageSpec: consumer}}
			for _, ps := range tc.pkgs {
				specs = append(specs, goolib.RepoSpec{PackageSpec: ps})
			}
			rm := client.RepoMap{"repo1": client.Repo{Priority: priority.Value(500), Packages: specs}}
			pi := goolib.PackageInfo{Name: "consumer_pkg", Arch: "noarch", Ver: "1.0.0"}
			got, err := ListDeps(pi, rm, "repo1", []string{"noarch"}, db)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("ListDeps = %v, %v; want error containing %q", got, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ListDeps: %v", err)
			}
			want := append([]goolib.PackageInfo{pi}, tc.want...)
			if diff := cmp.Diff(want, got); diff != "" {
				t.Errorf("ListDeps mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestBuildConflictMap(t *testing.T) {
	settings.Initialize(t.TempDir(), false)
	state := []client.PackageState{
		{
			PackageSpec: &goolib.PkgSpec{Name: "pkgA", Version: "1.0.0@1", Arch: "noarch"},
			InstalledFiles: map[string]string{
				"/path/to/file1": "chksum1",
				"/path/to/dir1":  "",
			},
		},
		{
			PackageSpec: &goolib.PkgSpec{Name: "pkgB", Version: "1.0.0@1", Arch: "noarch"},
			InstalledFiles: map[string]string{
				"/path/to/file2": "chksum2",
			},
		},
	}
	db, err := googetdb.NewDB(settings.DBFile())
	if err != nil {
		t.Fatalf("googetdb.NewDB: %v", err)
	}
	defer db.Close()
	if err := db.WriteStateToDB(state); err != nil {
		t.Fatalf("WriteStateToDB: %v", err)
	}

	cm, err := buildConflictMap(db, "pkgB")
	if err != nil {
		t.Fatalf("buildConflictMap: %v", err)
	}

	if _, ok := cm.owner("/path/to/dir1"); ok {
		t.Errorf("buildConflictMap included directory /path/to/dir1")
	}
	if owner, ok := cm.owner("/path/to/file1"); !ok || owner != "pkgA" {
		t.Errorf("expected /path/to/file1 to map to pkgA, got %v", owner)
	}
	if _, ok := cm.owner("/path/to/file2"); ok {
		t.Errorf("buildConflictMap included file from excluded package pkgB")
	}
}

func TestMakeInstallFunction(t *testing.T) {
	dstDir, err := ioutil.TempDir("", "")
	if err != nil {
		t.Fatal(err)
	}
	defer oswrap.RemoveAll(dstDir)

	srcDir, err := ioutil.TempDir("", "")
	if err != nil {
		t.Fatal(err)
	}
	srcDir = filepath.Join(srcDir, "foo") // append subdirectory to properly test TrimPrefix
	oswrap.MkdirAll(srcDir, 0755)
	defer oswrap.RemoveAll(srcDir)

	conflictPath := filepath.Join(dstDir, "conflicting_file")
	cm := newConflictMap(ownership.NewIndex(client.GooGetState{
		{
			PackageSpec:    &goolib.PkgSpec{Name: "pkgOwner", Arch: "noarch", Version: "1.0.0"},
			InstalledFiles: map[string]string{conflictPath: "chksum"},
		},
	}, ownership.RowID{}), "currentPkg")

	f, err := oswrap.Create(filepath.Join(srcDir, "conflicting_file"))
	if err != nil {
		t.Fatal(err)
	}
	fi, _ := f.Stat()
	f.Close()

	// Test 1: Conflict without force -> Success by default
	fnBlock := makeInstallFunction(srcDir, dstDir, make(map[string]string), Options{}, cm, &undo{})
	errBlock := fnBlock(filepath.Join(srcDir, "conflicting_file"), fi, nil)
	if errBlock != nil {
		t.Errorf("expected no conflict error by default, got %v", errBlock)
	}

	// Test 2: Conflict with force -> Success
	fnForce := makeInstallFunction(srcDir, dstDir, make(map[string]string), Options{Force: true}, cm, &undo{})
	errForce := fnForce(filepath.Join(srcDir, "conflicting_file"), fi, nil)
	if errForce != nil {
		t.Errorf("expected no error with force, got %v", errForce)
	}

	// Test 3: Conflict without force in strict mode -> Error
	fnStrict := makeInstallFunction(srcDir, dstDir, make(map[string]string), Options{StrictConflicts: true}, cm, &undo{})
	errStrict := fnStrict(filepath.Join(srcDir, "conflicting_file"), fi, nil)
	if errStrict == nil {
		t.Errorf("expected conflict error in strict mode, got nil")
	}

	// Test 4: Conflict with force in strict mode -> Success
	fnStrictForce := makeInstallFunction(srcDir, dstDir, make(map[string]string), Options{StrictConflicts: true, Force: true}, cm, &undo{})
	errStrictForce := fnStrictForce(filepath.Join(srcDir, "conflicting_file"), fi, nil)
	if errStrictForce != nil {
		t.Errorf("expected no error with force in strict mode, got %v", errStrictForce)
	}
}
