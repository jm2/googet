package settings_test

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/googet/v2/client"
	"github.com/google/googet/v2/goolib"
	"github.com/google/googet/v2/settings"
	"github.com/google/googet/v2/system"
)

func TestInitialize(t *testing.T) {
	rootDir := t.TempDir()
	f, err := os.Create(filepath.Join(rootDir, "googet.conf"))
	if err != nil {
		t.Fatalf("error creating conf file: %v", err)
	}
	content := []byte("archs: [noarch, x86_64, arm64]\ncachelife: 10m\nlockfilemaxage: 1x\nallowunsafeurl: true\nprogress: false\ninstalltimeout: 90m\ninactivitytimeout: 2m\ninactivitymode: Enforce")
	if _, err := f.Write(content); err != nil {
		t.Fatalf("error writing conf file: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("error closing conf file: %v", err)
	}

	origTimeout, origInactivity, origMode := goolib.Timeout, goolib.InactivityTimeout, goolib.InactivityMode
	t.Cleanup(func() {
		goolib.Timeout, goolib.InactivityTimeout, goolib.InactivityMode = origTimeout, origInactivity, origMode
	})
	if err := settings.Initialize(rootDir, true); err != nil {
		t.Fatalf("settings.Initialize: %v", err)
	}

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

	t.Run("Parsing InstallTimeout", func(t *testing.T) {
		if got, want := goolib.Timeout, 90*time.Minute; got != want {
			t.Errorf("goolib.Timeout got: %v, want: %v", got, want)
		}
	})

	t.Run("Parsing InactivityTimeout", func(t *testing.T) {
		if got, want := goolib.InactivityTimeout, 2*time.Minute; got != want {
			t.Errorf("goolib.InactivityTimeout got: %v, want: %v", got, want)
		}
	})

	t.Run("Parsing InactivityMode", func(t *testing.T) {
		if got, want := goolib.InactivityMode, goolib.InactivityEnforce; got != want {
			t.Errorf("goolib.InactivityMode got: %q, want: %q", got, want)
		}
	})
}

func TestInactivitySettings(t *testing.T) {
	// Each row starts from values other than the defaults, so that a value
	// left unchanged is told apart from one reset to its default.
	for _, tt := range []struct {
		name     string
		conf     string
		wantTime time.Duration
		wantMode string
	}{
		{"zero timeout disables", "inactivitytimeout: 0", 0, goolib.InactivityEnforce},
		{"invalid timeout", "inactivitytimeout: 1x", 7 * time.Minute, goolib.InactivityEnforce},
		{"upper case mode", "inactivitymode: OFF", 7 * time.Minute, goolib.InactivityOff},
		// A typo must not leave enforce in effect.
		{"invalid mode", "inactivitymode: kill", 7 * time.Minute, goolib.InactivityMonitor},
	} {
		t.Run(tt.name, func(t *testing.T) {
			rootDir := t.TempDir()
			if err := os.WriteFile(filepath.Join(rootDir, "googet.conf"), []byte(tt.conf), 0644); err != nil {
				t.Fatalf("error writing conf file: %v", err)
			}
			origTime, origMode := goolib.InactivityTimeout, goolib.InactivityMode
			t.Cleanup(func() { goolib.InactivityTimeout, goolib.InactivityMode = origTime, origMode })
			goolib.InactivityTimeout, goolib.InactivityMode = 7*time.Minute, goolib.InactivityEnforce
			settings.Initialize(rootDir, true)
			if got := goolib.InactivityTimeout; got != tt.wantTime {
				t.Errorf("goolib.InactivityTimeout with %q got: %v, want: %v", tt.conf, got, tt.wantTime)
			}
			if got := goolib.InactivityMode; got != tt.wantMode {
				t.Errorf("goolib.InactivityMode with %q got: %q, want: %q", tt.conf, got, tt.wantMode)
			}
		})
	}
}

func TestInactivityModeDefault(t *testing.T) {
	// Without the key, settings leaves goolib.InactivityMode, and so its
	// default, alone. A sentinel tells that apart from setting the default.
	rootDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(rootDir, "googet.conf"), []byte("cachelife: 10m"), 0644); err != nil {
		t.Fatalf("error writing conf file: %v", err)
	}
	orig := goolib.InactivityMode
	t.Cleanup(func() { goolib.InactivityMode = orig })
	goolib.InactivityMode = "sentinel"
	settings.Initialize(rootDir, true)
	if got, want := goolib.InactivityMode, "sentinel"; got != want {
		t.Errorf("goolib.InactivityMode without inactivitymode = %q, want %q", got, want)
	}
}

func TestInstallTimeoutSettings(t *testing.T) {
	origTimeout := goolib.Timeout
	t.Cleanup(func() { goolib.Timeout = origTimeout })
	for _, tt := range []struct {
		name string
		conf string
		want time.Duration
	}{
		{"invalid resets to default", "installtimeout: 1x", goolib.DefaultTimeout},
		{"absent resets to default", "cachelife: 10m", goolib.DefaultTimeout},
		{"zero disables", "installtimeout: 0", 0},
		{"negative disables", "installtimeout: -1s", -time.Second},
	} {
		t.Run(tt.name, func(t *testing.T) {
			rootDir := t.TempDir()
			if err := os.WriteFile(filepath.Join(rootDir, "googet.conf"), []byte(tt.conf), 0644); err != nil {
				t.Fatalf("error writing conf file: %v", err)
			}
			goolib.Timeout = 3 * time.Hour
			if err := settings.Initialize(rootDir, true); err != nil {
				t.Fatalf("settings.Initialize: %v", err)
			}
			if got := goolib.Timeout; got != tt.want {
				t.Errorf("goolib.Timeout with %q got: %v, want: %v", tt.conf, got, tt.want)
			}
		})
	}
}

func TestProgressDefault(t *testing.T) {
	rootDir := t.TempDir()
	conf := filepath.Join(rootDir, "googet.conf")
	if err := os.WriteFile(conf, []byte("progress: false"), 0644); err != nil {
		t.Fatalf("error writing conf file: %v", err)
	}
	if err := settings.Initialize(rootDir, true); err != nil {
		t.Fatalf("settings.Initialize: %v", err)
	}
	if settings.Progress {
		t.Fatalf("settings.Progress with progress: false = true, want false")
	}

	// An absent key restores the default rather than keeping the prior value.
	if err := os.WriteFile(conf, []byte("cachelife: 10m"), 0644); err != nil {
		t.Fatalf("error writing conf file: %v", err)
	}
	if err := settings.Initialize(rootDir, true); err != nil {
		t.Fatalf("settings.Initialize: %v", err)
	}
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
		if err := settings.Initialize(rootDir, true); err != nil {
			t.Fatalf("settings.Initialize: %v", err)
		}
		if got := client.StallTimeout; got != tc.want {
			t.Errorf("client.StallTimeout with %q = %v, want %v", tc.conf, got, tc.want)
		}
	}
}

func TestInitializeUnreadableConf(t *testing.T) {
	rootDir := t.TempDir()
	// A directory exists but can't be read as a file, like a conf that only
	// administrators may read.
	if err := os.Mkdir(filepath.Join(rootDir, "googet.conf"), 0755); err != nil {
		t.Fatalf("error creating conf directory: %v", err)
	}
	err := settings.Initialize(rootDir, false)
	if err == nil {
		t.Fatal("settings.Initialize with an unreadable googet.conf = nil, want an error")
	}
	if errors.Is(err, fs.ErrNotExist) {
		t.Errorf("settings.Initialize with an unreadable googet.conf = %v, want an error other than fs.ErrNotExist", err)
	}
}

func TestLoadConfUnreadableConf(t *testing.T) {
	rootDir := t.TempDir()
	// A directory exists but can't be read as a file, like a conf that only
	// administrators may read.
	if err := os.Mkdir(filepath.Join(rootDir, "googet.conf"), 0755); err != nil {
		t.Fatalf("error creating conf directory: %v", err)
	}
	settings.SetRoot(rootDir, false)
	err := settings.LoadConf()
	if err == nil {
		t.Fatal("settings.LoadConf with an unreadable googet.conf = nil, want an error")
	}
	if errors.Is(err, fs.ErrNotExist) {
		t.Errorf("settings.LoadConf with an unreadable googet.conf = %v, want an error other than fs.ErrNotExist", err)
	}
}

func TestInitializeMissingConf(t *testing.T) {
	settings.Archs = []string{"stale"}
	if err := settings.Initialize(t.TempDir(), false); err != nil {
		t.Fatalf("settings.Initialize without googet.conf = %v, want nil", err)
	}
	want, err := system.InstallableArchs()
	if err != nil {
		t.Fatalf("system.InstallableArchs: %v", err)
	}
	if diff := cmp.Diff(want, settings.Archs); diff != "" {
		t.Errorf("settings.Archs without googet.conf diff (-want +got):\n%s", diff)
	}
}

func TestInitializeMalformedConf(t *testing.T) {
	defaultArchs, err := system.InstallableArchs()
	if err != nil {
		t.Fatalf("system.InstallableArchs: %v", err)
	}
	for _, tc := range []struct {
		desc      string
		conf      string
		wantArchs []string
	}{
		{
			desc:      "syntax error uses defaults",
			conf:      "archs: [noarch\n",
			wantArchs: defaultArchs,
		},
		{
			desc:      "type error keeps the fields that decoded",
			conf:      "archs: [arm64]\nprogress: sometimes\n",
			wantArchs: []string{"arm64"},
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			rootDir := t.TempDir()
			if err := os.WriteFile(filepath.Join(rootDir, "googet.conf"), []byte(tc.conf), 0644); err != nil {
				t.Fatalf("error writing conf file: %v", err)
			}
			settings.Archs = []string{"stale"}
			if err := settings.Initialize(rootDir, false); err != nil {
				t.Fatalf("settings.Initialize with %q = %v, want nil", tc.conf, err)
			}
			if diff := cmp.Diff(tc.wantArchs, settings.Archs); diff != "" {
				t.Errorf("settings.Archs with %q diff (-want +got):\n%s", tc.conf, diff)
			}
		})
	}
}

func TestSetRootAndLoadConf(t *testing.T) {
	oldStall := client.StallTimeout
	oldRoot, oldConfirm, oldArchs := settings.RootDir, settings.Confirm, settings.Archs
	oldCacheLife, oldLockAge, oldProxy := settings.CacheLife, settings.LockFileMaxAge, settings.ProxyServer
	oldUnsafe, oldStrict, oldProgress := settings.AllowUnsafeURL, settings.StrictConflicts, settings.Progress
	t.Cleanup(func() {
		client.StallTimeout = oldStall
		settings.RootDir, settings.Confirm, settings.Archs = oldRoot, oldConfirm, oldArchs
		settings.CacheLife, settings.LockFileMaxAge, settings.ProxyServer = oldCacheLife, oldLockAge, oldProxy
		settings.AllowUnsafeURL, settings.StrictConflicts, settings.Progress = oldUnsafe, oldStrict, oldProgress
	})

	settings.CacheLife = 3 * time.Minute
	settings.LockFileMaxAge = 24 * time.Hour
	settings.ProxyServer = ""
	settings.AllowUnsafeURL = false
	settings.StrictConflicts = false
	settings.Progress = true

	rootDir := t.TempDir()
	conf := "cachelife: 7m\nlockfilemaxage: 5m\nproxyserver: http://proxy.example:8080\nallowunsafeurl: true\nstrictconflicts: true\nprogress: false\n"
	if err := os.WriteFile(filepath.Join(rootDir, "googet.conf"), []byte(conf), 0644); err != nil {
		t.Fatalf("error writing conf file: %v", err)
	}

	// SetRoot must set paths and Confirm without reading googet.conf.
	settings.SetRoot(rootDir, false)
	if settings.RootDir != rootDir || settings.Confirm {
		t.Fatalf("after SetRoot: RootDir=%q Confirm=%v, want %q false", settings.RootDir, settings.Confirm, rootDir)
	}
	for name, got := range map[string]string{
		"LockFile":  settings.LockFile(),
		"StateFile": settings.StateFile(),
		"DBFile":    settings.DBFile(),
		"ConfFile":  settings.ConfFile(),
		"LogFile":   settings.LogFile(),
		"CacheDir":  settings.CacheDir(),
		"RepoDir":   settings.RepoDir(),
	} {
		if filepath.Dir(got) != rootDir {
			t.Errorf("%s() = %q, want path under %q", name, got, rootDir)
		}
	}
	if settings.CacheLife != 3*time.Minute || settings.LockFileMaxAge != 24*time.Hour || settings.ProxyServer != "" || settings.AllowUnsafeURL || settings.StrictConflicts || !settings.Progress {
		t.Fatalf("SetRoot read googet.conf unexpectedly: CacheLife=%v LockFileMaxAge=%v ProxyServer=%q AllowUnsafeURL=%v StrictConflicts=%v Progress=%v",
			settings.CacheLife, settings.LockFileMaxAge, settings.ProxyServer, settings.AllowUnsafeURL, settings.StrictConflicts, settings.Progress)
	}

	// LoadConf reads googet.conf from the directory set by SetRoot.
	if err := settings.LoadConf(); err != nil {
		t.Fatalf("settings.LoadConf() = %v, want nil", err)
	}
	if settings.CacheLife != 7*time.Minute {
		t.Errorf("CacheLife = %v, want 7m", settings.CacheLife)
	}
	if settings.LockFileMaxAge != 5*time.Minute {
		t.Errorf("LockFileMaxAge = %v, want 5m", settings.LockFileMaxAge)
	}
	if settings.ProxyServer != "http://proxy.example:8080" {
		t.Errorf("ProxyServer = %q, want http://proxy.example:8080", settings.ProxyServer)
	}
	if !settings.AllowUnsafeURL {
		t.Errorf("AllowUnsafeURL = false, want true")
	}
	if !settings.StrictConflicts {
		t.Errorf("StrictConflicts = false, want true")
	}
	if settings.Progress {
		t.Errorf("Progress = true, want false")
	}
}
