package settings_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/googet/v2/client"
	"github.com/google/googet/v2/settings"
	"github.com/google/googet/v2/testutil"
)

func TestInitialize(t *testing.T) {
	rootDir := t.TempDir()
	f, err := os.Create(filepath.Join(rootDir, "googet.conf"))
	if err != nil {
		t.Fatalf("error creating conf file: %v", err)
	}
	content := []byte("archs: [noarch, x86_64, arm64]\ncachelife: 10m\nlockfilemaxage: 1x\nallowunsafeurl: true\nprogress: false")
	if _, err := f.Write(content); err != nil {
		t.Fatalf("error writing conf file: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("error closing conf file: %v", err)
	}

	settings.Initialize(rootDir, true)

	if got, want := settings.Confirm, true; got != want {
		t.Errorf("settings.Confirm got: %v, want: %v", got, want)
	}

	t.Run("Parsing Architectures", func(t *testing.T) {
		wantArches := []string{"noarch", "x86_64", "arm64"}
		if diff := cmp.Diff(wantArches, settings.Archs); diff != "" {
			t.Errorf("settings.Archs unexpected diff (-want +got):\n%v", diff)
		}
	})

	t.Run("Parsing CacheLife", func(t *testing.T) {
		wantCacheLife := 10 * time.Minute
		if got := settings.CacheLife; got != wantCacheLife {
			t.Errorf("settings.CacheLife got: %v, want: %v", got, wantCacheLife)
		}
	})

	t.Run("Parsing LockFileMaxAge", func(t *testing.T) {
		wantLockFileMaxAge := 24 * time.Hour
		if got := settings.LockFileMaxAge; got != wantLockFileMaxAge {
			t.Errorf("settings.LockFileMaxAge got: %v, want: %v", got, wantLockFileMaxAge)
		}
	})

	t.Run("Parsing AllowUnsafeURL", func(t *testing.T) {
		wantAllowUnsafeURL := true
		if got := settings.AllowUnsafeURL; got != wantAllowUnsafeURL {
			t.Errorf("settings.AllowUnsafeURL got: %v, want: %v", got, wantAllowUnsafeURL)
		}
	})

	t.Run("Parsing Progress", func(t *testing.T) {
		if got, want := settings.Progress, false; got != want {
			t.Errorf("settings.Progress got: %v, want: %v", got, want)
		}
	})
}

func TestProgressDefault(t *testing.T) {
	rootDir := t.TempDir()
	conf := filepath.Join(rootDir, "googet.conf")
	if err := os.WriteFile(conf, []byte("progress: false"), 0644); err != nil {
		t.Fatalf("error writing conf file: %v", err)
	}
	settings.Initialize(rootDir, true)
	if settings.Progress {
		t.Fatalf("settings.Progress with progress: false = true, want false")
	}

	// An absent key restores the default rather than keeping the prior value.
	if err := os.WriteFile(conf, []byte("cachelife: 10m"), 0644); err != nil {
		t.Fatalf("error writing conf file: %v", err)
	}
	settings.Initialize(rootDir, true)
	if !settings.Progress {
		t.Errorf("settings.Progress with no progress key = false, want true")
	}
}

func TestStallTimeout(t *testing.T) {
	oldStall := client.StallTimeout
	oldRoot, oldConfirm, oldArchs := settings.RootDir, settings.Confirm, settings.Archs
	oldCacheLife, oldUnsafe, oldStrict, oldProgress := settings.CacheLife, settings.AllowUnsafeURL, settings.StrictConflicts, settings.Progress
	t.Cleanup(func() {
		client.StallTimeout = oldStall
		settings.RootDir, settings.Confirm, settings.Archs = oldRoot, oldConfirm, oldArchs
		settings.CacheLife, settings.AllowUnsafeURL, settings.StrictConflicts, settings.Progress = oldCacheLife, oldUnsafe, oldStrict, oldProgress
	})
	rootDir := t.TempDir()
	// Each case starts from the previous one's value, so a bad value must
	// restore the default rather than keep the prior setting.
	for _, tc := range []struct {
		conf string
		want time.Duration
	}{
		{"stalltimeout: 5m", 5 * time.Minute},
		{"stalltimeout: 5x", client.DefaultStallTimeout},
		{"stalltimeout: 30s", 30 * time.Second},
		{"stalltimeout: -1s", client.DefaultStallTimeout},
		{"stalltimeout: 0s", client.DefaultStallTimeout},
		{"cachelife: 10m", client.DefaultStallTimeout},
	} {
		if err := os.WriteFile(filepath.Join(rootDir, "googet.conf"), []byte(tc.conf), 0644); err != nil {
			t.Fatalf("error writing conf file: %v", err)
		}
		settings.Initialize(rootDir, true)
		if got := client.StallTimeout; got != tc.want {
			t.Errorf("client.StallTimeout with %q = %v, want %v", tc.conf, got, tc.want)
		}
	}
}

func TestPackageConflictsParsingAndReset(t *testing.T) {
	t.Cleanup(settings.Snapshot())
	for _, tc := range []struct {
		name      string
		yaml      string
		want      string
		wantErrCt int
	}{
		{name: "absent key", yaml: "cachelife: 10m\n", want: ""},
		{name: "lowercase warn", yaml: "packageconflicts: warn\n", want: "warn"},
		{name: "mixed case and whitespace error", yaml: "packageconflicts: \"  ErRoR \"\n", want: "error"},
		{name: "ignore", yaml: "packageconflicts: IGNORE\n", want: "ignore"},
		{name: "PascalCase key ignored", yaml: "PackageConflicts: error\n", want: ""},
		{name: "unknown extra key ignored", yaml: "futurekey: 123\npackageconflicts: warn\n", want: "warn"},
		{name: "invalid value logs error once and falls back to empty", yaml: "packageconflicts: bogus\n", want: "", wantErrCt: 1},
		{name: "whitespace-only value logs error once and falls back to empty", yaml: "packageconflicts: \"   \"\n", want: "", wantErrCt: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs := testutil.CaptureLogs(t)
			rootDir := t.TempDir()
			if err := os.WriteFile(filepath.Join(rootDir, "googet.conf"), []byte(tc.yaml), 0644); err != nil {
				t.Fatal(err)
			}
			settings.Initialize(rootDir, false)
			if settings.PackageConflicts != tc.want {
				t.Errorf("settings.PackageConflicts = %q, want %q", settings.PackageConflicts, tc.want)
			}
			if got := strings.Count(logs.String(), "ERROR:"); got != tc.wantErrCt {
				t.Errorf("ERROR log count = %d, want %d; logs:\n%s", got, tc.wantErrCt, logs.String())
			}
		})
	}

	t.Run("sequential Initialize resets prior state", func(t *testing.T) {
		rootDir := t.TempDir()
		conf := filepath.Join(rootDir, "googet.conf")

		if err := os.WriteFile(conf, []byte("packageconflicts: error\n"), 0644); err != nil {
			t.Fatal(err)
		}
		settings.Initialize(rootDir, false)
		if settings.PackageConflicts != "error" {
			t.Fatalf("settings.PackageConflicts = %q, want error", settings.PackageConflicts)
		}

		// Transition to an absent key must reset PackageConflicts to empty.
		if err := os.WriteFile(conf, []byte("cachelife: 5m\n"), 0644); err != nil {
			t.Fatal(err)
		}
		settings.Initialize(rootDir, false)
		if settings.PackageConflicts != "" {
			t.Errorf("after absent key, settings.PackageConflicts = %q, want empty", settings.PackageConflicts)
		}

		// Set error again, then transition to an invalid value.
		if err := os.WriteFile(conf, []byte("packageconflicts: error\n"), 0644); err != nil {
			t.Fatal(err)
		}
		settings.Initialize(rootDir, false)
		if err := os.WriteFile(conf, []byte("packageconflicts: invalid\n"), 0644); err != nil {
			t.Fatal(err)
		}
		settings.Initialize(rootDir, false)
		if settings.PackageConflicts != "" {
			t.Errorf("after invalid key, settings.PackageConflicts = %q, want empty", settings.PackageConflicts)
		}

		// Transition to a missing googet.conf must also stay empty.
		settings.Initialize(t.TempDir(), false)
		if settings.PackageConflicts != "" {
			t.Errorf("after missing conf, settings.PackageConflicts = %q, want empty", settings.PackageConflicts)
		}
	})
}

func TestSnapshotRestoresAllGlobals(t *testing.T) {
	t.Cleanup(settings.Snapshot())

	settings.RootDir = "/orig/root"
	settings.Confirm = true
	settings.CacheLife = 7 * time.Minute
	settings.LockFileMaxAge = 12 * time.Hour
	settings.Archs = []string{"noarch"}
	settings.ProxyServer = "http://proxy:8080"
	settings.AllowUnsafeURL = true
	settings.StrictConflicts = true
	settings.PackageConflicts = "error"
	settings.Progress = false

	restore := settings.Snapshot()
	settings.Initialize(t.TempDir(), false)
	settings.Archs = append(settings.Archs, "arm64")
	restore()

	if got, want := settings.RootDir, "/orig/root"; got != want {
		t.Errorf("settings.RootDir = %q, want %q", got, want)
	}
	if got, want := settings.Confirm, true; got != want {
		t.Errorf("settings.Confirm = %t, want %t", got, want)
	}
	if got, want := settings.CacheLife, 7*time.Minute; got != want {
		t.Errorf("settings.CacheLife = %v, want %v", got, want)
	}
	if got, want := settings.LockFileMaxAge, 12*time.Hour; got != want {
		t.Errorf("settings.LockFileMaxAge = %v, want %v", got, want)
	}
	if diff := cmp.Diff([]string{"noarch"}, settings.Archs); diff != "" {
		t.Errorf("settings.Archs (-want +got):\n%s", diff)
	}
	if got, want := settings.ProxyServer, "http://proxy:8080"; got != want {
		t.Errorf("settings.ProxyServer = %q, want %q", got, want)
	}
	if got, want := settings.AllowUnsafeURL, true; got != want {
		t.Errorf("settings.AllowUnsafeURL = %t, want %t", got, want)
	}
	if got, want := settings.StrictConflicts, true; got != want {
		t.Errorf("settings.StrictConflicts = %t, want %t", got, want)
	}
	if got, want := settings.PackageConflicts, "error"; got != want {
		t.Errorf("settings.PackageConflicts = %q, want %q", got, want)
	}
	if got, want := settings.Progress, false; got != want {
		t.Errorf("settings.Progress = %t, want %t", got, want)
	}
}
