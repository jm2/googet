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
	"bytes"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/googet/v2/goolib"
	"github.com/google/googet/v2/priority"
)

// logSink is the log file the package's tests initialize the logger with. It
// discards what is logged unless a test captures it.
type logSink struct {
	mu  sync.Mutex
	buf *bytes.Buffer
}

// Write implements io.Writer.
func (s *logSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.buf == nil {
		return len(p), nil
	}
	return s.buf.Write(p)
}

// capture makes s record what is logged until t ends, and returns a function
// that reports what was recorded so far.
func (s *logSink) capture(t *testing.T) func() string {
	t.Helper()
	s.mu.Lock()
	s.buf = new(bytes.Buffer)
	s.mu.Unlock()
	t.Cleanup(func() {
		s.mu.Lock()
		s.buf = nil
		s.mu.Unlock()
	})
	return func() string {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.buf.String()
	}
}

// testLogs receives everything the package's tests log.
var testLogs logSink

// errorLine matches an error log line written from client.go and captures
// its message.
var errorLine = regexp.MustCompile(`^ERROR: \S+ \S+ client\.go:\d+: (.*)$`)

// TestFindRepoLatestLogsComparisonErrors checks that FindRepoLatest logs each
// version comparison that fails as one error line whose message is, byte for
// byte, the one it has always logged.
func TestFindRepoLatestLogsComparisonErrors(t *testing.T) {
	repo := func(specs ...*goolib.PkgSpec) RepoMap {
		r := Repo{Priority: priority.Default}
		for _, ps := range specs {
			r.Packages = append(r.Packages, goolib.RepoSpec{PackageSpec: ps})
		}
		return RepoMap{"https://a.example/repo": r}
	}
	for _, tc := range []struct {
		name    string
		rm      RepoMap
		pi      goolib.PackageInfo
		wantVer string
		want    []string
	}{
		{
			name:    "unparseable version against a minimum version",
			rm:      repo(&goolib.PkgSpec{Name: "B", Arch: "noarch", Version: "bad"}, &goolib.PkgSpec{Name: "B", Arch: "noarch", Version: "2.0.0"}),
			pi:      goolib.PackageInfo{Name: "B", Ver: "1.0.0"},
			wantVer: "2.0.0",
			want:    []string{"Error comparing versions bad vs 1.0.0: Invalid character(s) found in major number \"bad\""},
		},
		{
			name:    "unparseable provided version against a minimum version",
			rm:      repo(&goolib.PkgSpec{Name: "Q", Arch: "noarch", Version: "1.0.0", Provides: []string{"B=bad"}}, &goolib.PkgSpec{Name: "R", Arch: "noarch", Version: "1.0.0", Provides: []string{"B=2.0.0"}}),
			pi:      goolib.PackageInfo{Name: "B", Ver: "1.0.0"},
			wantVer: "1.0.0",
			want:    []string{"Error comparing versions bad vs 1.0.0: Invalid character(s) found in major number \"bad\""},
		},
		{
			name:    "priority and version comparison",
			rm:      repo(&goolib.PkgSpec{Name: "B", Arch: "noarch", Version: "bad"}, &goolib.PkgSpec{Name: "B", Arch: "noarch", Version: "2.0.0"}),
			pi:      goolib.PackageInfo{Name: "B"},
			wantVer: "2.0.0",
			want:    []string{"Error comparing priority/version: Invalid character(s) found in major number \"bad\""},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs := testLogs.capture(t)
			spec, _, _, err := FindRepoLatest(tc.pi, tc.rm, []string{"noarch"}, "", false)
			if err != nil {
				t.Fatalf("FindRepoLatest(%v): %v", tc.pi, err)
			}
			if spec.Version != tc.wantVer {
				t.Errorf("FindRepoLatest(%v) = %s, want version %s", tc.pi, spec, tc.wantVer)
			}
			var got []string
			for _, line := range strings.Split(strings.TrimSuffix(logs(), "\n"), "\n") {
				if m := errorLine.FindStringSubmatch(line); m != nil {
					got = append(got, m[1])
				}
			}
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("FindRepoLatest error log messages (-want +got):\n%s\nlogs:\n%s", diff, logs())
			}
		})
	}
}
