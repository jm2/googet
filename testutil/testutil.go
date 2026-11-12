// Package testutil provides common helper functions for tests.
package testutil

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/googet/v2/goolib"
	"github.com/google/logger"
)

// logSink is a concurrency-safe io.Writer registered with logger.Init so
// individual tests can capture package-level log lines.
type logSink struct {
	mu sync.Mutex
	w  io.Writer
}

func (s *logSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.w == nil {
		return len(p), nil
	}
	return s.w.Write(p)
}

func (s *logSink) set(w io.Writer) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.w = w
}

var sharedLogSink logSink

func init() {
	logger.Init("test", false, false, &sharedLogSink)
}

// CaptureLogs redirects package-level logger output to a buffer for the
// duration of t and restores silent logging on cleanup.
//
// CaptureLogs relies on testutil's init registering sharedLogSink before any
// importing test package runs its own init, because logger.Init configures the
// default logger only on its first call per process. CaptureLogs swaps a
// process-wide log sink and must not be used in tests that call t.Parallel.
func CaptureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	sharedLogSink.set(&buf)
	t.Cleanup(func() { sharedLogSink.set(nil) })
	return &buf
}

// ScaledIterations returns n, or a tenth of n (at least 1) on Windows, for
// randomized tests whose iterations each write a SQLite database. A SQLite
// write transaction takes about a hundred times longer on Windows CI runners
// than on Linux ones, and the tested logic doesn't depend on the platform.
func ScaledIterations(n int) int {
	if runtime.GOOS != "windows" {
		return n
	}
	return max(n/10, 1)
}

// GenGoo creates a name.noarch.version.goo package file in directory dir for
// the package with given pkgspec. When installed name.goo writes a file having
// same name as the package to the dst directory. The contents of this file is
// "name.noarch.version". Returns a RepoSpec for the goo package.
func GenGoo(t *testing.T, dir, dst string, ps goolib.PkgSpec) goolib.RepoSpec {
	t.Helper()
	ps.Files = map[string]string{ps.Name: filepath.Join(dst, ps.Name)}
	b, err := json.Marshal(ps)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(filepath.Join(dir, ps.String()+".goo"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	h := sha256.New()
	gw := gzip.NewWriter(io.MultiWriter(h, f))
	tw := tar.NewWriter(gw)
	modTime := time.Now()
	for _, x := range []struct {
		name    string
		content []byte
	}{
		{ps.Name, []byte(ps.String())},
		{ps.Name + ".pkgspec", b},
	} {
		if err := tw.WriteHeader(&tar.Header{Name: x.name, Mode: 0644, Size: int64(len(x.content)), ModTime: modTime}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(x.content); err != nil {
			t.Fatal(err)
		}
	}
	tw.Close()
	gw.Close()
	return goolib.RepoSpec{
		Checksum:    fmt.Sprintf("%x", h.Sum(nil)),
		Source:      filepath.Base(f.Name()),
		PackageSpec: &ps,
	}
}

// ServeGoo returns an HTTP server that serves files from dir.
func ServeGoo(t *testing.T, dir string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		absDir, err := filepath.Abs(dir)
		if err != nil {
			t.Fatal(err)
		}
		absPath, err := filepath.Abs(filepath.Join(absDir, r.URL.Path))
		if err != nil || !strings.HasPrefix(absPath, absDir) {
			http.Error(w, "Invalid file name", http.StatusBadRequest)
			return
		}
		f, err := os.Open(absPath)
		if err != nil {
			t.Logf("couldn't find file: %v", r.URL.Path)
			http.Error(w, "couldn't find requested file", http.StatusNotFound)
		} else {
			io.Copy(w, f)
			f.Close()
		}
	}))
}
