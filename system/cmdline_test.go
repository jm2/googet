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

package system

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/googet/v2/client"
	"github.com/google/googet/v2/goolib"
	"github.com/google/logger"
)

type testLogSink struct {
	mu  sync.Mutex
	buf *bytes.Buffer
}

func (s *testLogSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.buf == nil {
		return len(p), nil
	}
	return s.buf.Write(p)
}

func (s *testLogSink) capture(t *testing.T) func() string {
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

var systemTestLogs testLogSink

func init() {
	logger.Init("system_test", false, false, &systemTestLogs)
}

func slashPathOps(exists func(string) bool) pathOps {
	return pathOps{
		ext:    path.Ext,
		base:   path.Base,
		join:   path.Join,
		exists: exists,
	}
}

func fakeARP(entries map[string]map[string]string) func(reg, ext string) string {
	return func(reg, ext string) string {
		vals, ok := entries[reg]
		if !ok {
			return ""
		}
		return arpUninstallString(ext, func(name string) (string, error) {
			v, exists := vals[name]
			if !exists {
				return "", errors.New("registry value not found")
			}
			return v, nil
		})
	}
}

func unexpectedARP(t *testing.T) func(reg, ext string) string {
	t.Helper()
	return func(reg, ext string) string {
		t.Fatalf("unexpected ARP lookup for reg=%q ext=%q", reg, ext)
		return ""
	}
}

func TestNormalizePathOpsDefaults(t *testing.T) {
	p := normalizePathOps(pathOps{})
	if got, want := p.ext("archive.msi"), ".msi"; got != want {
		t.Errorf("normalizePathOps().ext(\"archive.msi\") = %q, want %q", got, want)
	}
	if got, want := p.base(filepath.Join("dir", "pkg_1.0.msix")), "pkg_1.0.msix"; got != want {
		t.Errorf("normalizePathOps().base(...) = %q, want %q", got, want)
	}
	if got, want := p.join("dir", "file.exe"), filepath.Join("dir", "file.exe"); got != want {
		t.Errorf("normalizePathOps().join(...) = %q, want %q", got, want)
	}
	if p.exists("nonexistent.exe") {
		t.Error("normalizePathOps().exists(\"nonexistent.exe\") = true, want false")
	}
}

func TestInstallCmdlineEmptyPath(t *testing.T) {
	ps := &goolib.PkgSpec{Name: "empty", Arch: "x86_64", Version: "1.0.0"}
	got, ok := installCmdline("pkgdir", ps, slashPathOps(nil))
	if ok {
		t.Fatalf("installCmdline() with empty Install.Path returned ok=true, cmdline=%+v; want ok=false", got)
	}
}

func TestInstallCmdlineBranches(t *testing.T) {
	tests := []struct {
		name string
		in   goolib.ExecFile
		want cmdline
	}{
		{
			name: "msi_installer",
			in:   goolib.ExecFile{Path: "sub/setup.msi", Args: []string{"PROP=1"}, ExitCodes: []int{42}},
			want: cmdline{
				name:      "msiexec",
				args:      []string{"/i", "pkgdir/sub/setup.msi", "/qn", "/norestart", "/log", "pkgdir/msi_install.log", "PROP=1"},
				script:    false,
				exitCodes: []int{1641, 3010, 42},
				logPath:   "pkgdir/sub/setup.msi.log",
				rawPath:   "sub/setup.msi",
			},
		},
		{
			name: "msp_patch",
			in:   goolib.ExecFile{Path: "patch.msp", Args: []string{"/passive"}},
			want: cmdline{
				name:      "msiexec",
				args:      []string{"/update", "pkgdir/patch.msp", "/qn", "/norestart", "/log", "pkgdir/msi_install.log", "/passive"},
				script:    false,
				exitCodes: []int{1641, 3010},
				logPath:   "pkgdir/patch.msp.log",
				rawPath:   "patch.msp",
			},
		},
		{
			name: "msu_update",
			in:   goolib.ExecFile{Path: "kb500.msu", Args: []string{"/warnrestart"}, ExitCodes: []int{2359302}},
			want: cmdline{
				name:      "wusa",
				args:      []string{"pkgdir/kb500.msu", "/quiet", "/norestart", "/warnrestart"},
				script:    false,
				exitCodes: []int{1641, 3010, 2359302},
				logPath:   "pkgdir/kb500.msu.log",
				rawPath:   "kb500.msu",
			},
		},
		{
			name: "exe_installer",
			in:   goolib.ExecFile{Path: "setup.exe", Args: []string{"/S", "/D=target"}, ExitCodes: []int{1}},
			want: cmdline{
				name:      "pkgdir/setup.exe",
				args:      []string{"/S", "/D=target"},
				script:    false,
				exitCodes: []int{1641, 3010, 1},
				logPath:   "pkgdir/setup.exe.log",
				rawPath:   "setup.exe",
			},
		},
		{
			name: "msix_package",
			in:   goolib.ExecFile{Path: "Contoso.App_1.0.msix", Args: []string{"-Verbose"}},
			want: cmdline{
				name:      "powershell",
				args:      []string{"Add-AppxProvisionedPackage -online -PackagePath pkgdir/Contoso.App_1.0.msix -SkipLicense", "-Verbose"},
				script:    false,
				exitCodes: []int{1641, 3010},
				logPath:   "pkgdir/Contoso.App_1.0.msix.log",
				rawPath:   "Contoso.App_1.0.msix",
			},
		},
		{
			name: "msixbundle_package",
			in:   goolib.ExecFile{Path: "Contoso.App_1.0.msixbundle"},
			want: cmdline{
				name:      "powershell",
				args:      []string{"Add-AppxProvisionedPackage -online -PackagePath pkgdir/Contoso.App_1.0.msixbundle -SkipLicense"},
				script:    false,
				exitCodes: []int{1641, 3010},
				logPath:   "pkgdir/Contoso.App_1.0.msixbundle.log",
				rawPath:   "Contoso.App_1.0.msixbundle",
			},
		},
		{
			name: "default_ps1_script_uses_spec_exit_codes_only",
			in:   goolib.ExecFile{Path: "install.ps1", Args: []string{"-Mode", "full"}, ExitCodes: []int{7}},
			want: cmdline{
				name:      "pkgdir/install.ps1",
				args:      []string{"-Mode", "full"},
				script:    true,
				exitCodes: []int{7},
				logPath:   "pkgdir/install.ps1.log",
				rawPath:   "install.ps1",
			},
		},
		{
			name: "default_unknown_extension_defers_to_script_builder",
			in:   goolib.ExecFile{Path: "install.unknown"},
			want: cmdline{
				name:      "pkgdir/install.unknown",
				args:      nil,
				script:    true,
				exitCodes: nil,
				logPath:   "pkgdir/install.unknown.log",
				rawPath:   "install.unknown",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ps := &goolib.PkgSpec{
				Name:    "pkg",
				Arch:    "x86_64",
				Version: "1.0.0",
				Install: tc.in,
			}
			got, ok := installCmdline("pkgdir", ps, slashPathOps(nil))
			if !ok {
				t.Fatalf("installCmdline() = _, false; want ok=true")
			}
			if diff := cmp.Diff(tc.want, got, cmp.AllowUnexported(cmdline{})); diff != "" {
				t.Errorf("installCmdline() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestUninstallCmdlineExplicitAndARPBranches(t *testing.T) {
	regEntries := map[string]map[string]string{
		"HKLM/MSI/Standard": {
			"QuietUninstallString": "wrong_quiet_for_msi.exe",
			"UninstallString":      "MsiExec.exe /I{11111111-2222-3333-4444-555555555555} /l*v msi.log",
		},
		"HKLM/MSI/PathContainsSlashI": {
			"UninstallString": "/opt/InstallDir/MsiExec.exe /I{AAAA-BBBB}",
		},
		"HKLM/EXE/BothQuietAndStandard": {
			"QuietUninstallString": "/vendor/bin/uninstall.exe /quiet /norestart",
			"UninstallString":      "/vendor/bin/uninstall.exe /interactive",
		},
		"HKLM/EXE/FallbackStandardQuoted": {
			"UninstallString": `"/vendor/bin/unins000.exe" /VERYSILENT`,
		},
		"HKLM/Script/RelativePS1": {
			"QuietUninstallString": "scripts/remove.ps1 -Force",
		},
	}

	tests := []struct {
		name      string
		state     client.PackageState
		useARP    bool
		existsSet map[string]bool
		want      cmdline
	}{
		{
			name: "spec_msi",
			state: client.PackageState{
				PackageSpec: &goolib.PkgSpec{
					Uninstall: goolib.ExecFile{Path: "setup.msi", Args: []string{"CLEAN=1"}, ExitCodes: []int{9}},
				},
			},
			want: cmdline{
				name:      "msiexec",
				args:      []string{"/x", "pkgdir/setup.msi", "/qn", "/norestart", "/log", "pkgdir/msi_uninstall.log", "CLEAN=1"},
				script:    false,
				exitCodes: []int{1641, 3010, 9},
				logPath:   "pkgdir/setup.msi.log",
				rawPath:   "setup.msi",
			},
		},
		{
			name: "spec_msu",
			state: client.PackageState{
				PackageSpec: &goolib.PkgSpec{
					Uninstall: goolib.ExecFile{Path: "kb500.msu"},
				},
			},
			want: cmdline{
				name:      "wusa",
				args:      []string{"pkgdir/kb500.msu", "/uninstall", "/quiet", "/norestart"},
				script:    false,
				exitCodes: []int{1641, 3010},
				logPath:   "pkgdir/kb500.msu.log",
				rawPath:   "kb500.msu",
			},
		},
		{
			name: "spec_exe_when_uninstall_path_missing_on_disk",
			state: client.PackageState{
				PackageSpec: &goolib.PkgSpec{
					Uninstall: goolib.ExecFile{Path: "uninstall.exe", Args: []string{"/S"}},
				},
			},
			want: cmdline{
				name:      "pkgdir/uninstall.exe",
				args:      []string{"/S"},
				script:    false,
				exitCodes: []int{1641, 3010},
				logPath:   "pkgdir/uninstall.exe.log",
				rawPath:   "uninstall.exe",
			},
		},
		{
			name: "spec_exe_when_uninstall_path_exists_on_disk",
			state: client.PackageState{
				PackageSpec: &goolib.PkgSpec{
					Uninstall: goolib.ExecFile{Path: "sub/uninstall.exe", Args: []string{"/S"}},
				},
			},
			existsSet: map[string]bool{"sub/uninstall.exe": true},
			want: cmdline{
				name:      "pkgdir/sub/uninstall.exe",
				args:      []string{"/S"},
				script:    false,
				exitCodes: []int{1641, 3010},
				logPath:   "sub/uninstall.exe.log",
				rawPath:   "sub/uninstall.exe",
			},
		},
		{
			name: "spec_msix",
			state: client.PackageState{
				PackageSpec: &goolib.PkgSpec{
					Uninstall: goolib.ExecFile{Path: "sub/Contoso.App_1.2.3_x64.msix"},
				},
			},
			want: cmdline{
				name:      "powershell",
				args:      []string{`Get-AppxProvisionedPackage -online | Where {$_.DisplayName -match "Contoso.App*"} | Remove-AppProvisionedPackage -online -AllUsers`},
				script:    false,
				exitCodes: []int{1641, 3010},
				logPath:   "pkgdir/sub/Contoso.App_1.2.3_x64.msix.log",
				rawPath:   "sub/Contoso.App_1.2.3_x64.msix",
			},
		},
		{
			name: "spec_msixbundle",
			state: client.PackageState{
				PackageSpec: &goolib.PkgSpec{
					Uninstall: goolib.ExecFile{Path: "Contoso.Bundle_2.0_x64.msixbundle", Args: []string{"-Verbose"}},
				},
			},
			want: cmdline{
				name: "powershell",
				args: []string{
					`Get-AppxProvisionedPackage -online | Where {$_.DisplayName -match "Contoso.Bundle*"} | Remove-AppProvisionedPackage -online -AllUsers`,
					"-Verbose",
				},
				script:    false,
				exitCodes: []int{1641, 3010},
				logPath:   "pkgdir/Contoso.Bundle_2.0_x64.msixbundle.log",
				rawPath:   "Contoso.Bundle_2.0_x64.msixbundle",
			},
		},
		{
			name: "spec_msp_falls_into_default_script_branch",
			state: client.PackageState{
				PackageSpec: &goolib.PkgSpec{
					Uninstall: goolib.ExecFile{Path: "patch.msp", ExitCodes: []int{5}},
				},
			},
			want: cmdline{
				name:      "pkgdir/patch.msp",
				args:      nil,
				script:    true,
				exitCodes: []int{5},
				logPath:   "pkgdir/patch.msp.log",
				rawPath:   "patch.msp",
			},
		},
		{
			name: "spec_ps1_script",
			state: client.PackageState{
				PackageSpec: &goolib.PkgSpec{
					Uninstall: goolib.ExecFile{Path: "remove.ps1", Args: []string{"-Clean"}, ExitCodes: []int{3}},
				},
			},
			want: cmdline{
				name:      "pkgdir/remove.ps1",
				args:      []string{"-Clean"},
				script:    true,
				exitCodes: []int{3},
				logPath:   "pkgdir/remove.ps1.log",
				rawPath:   "remove.ps1",
			},
		},
		{
			name: "arp_msi_rewrites_i_to_x_and_routes_through_exe",
			state: client.PackageState{
				PackageSpec: &goolib.PkgSpec{
					Install:   goolib.ExecFile{Path: "pkg.msi"},
					Uninstall: goolib.ExecFile{ExitCodes: []int{8}},
				},
				InstalledApp: client.InstalledApplication{Reg: "HKLM/MSI/Standard"},
			},
			useARP:    true,
			existsSet: map[string]bool{"MsiExec.exe": true},
			want: cmdline{
				name:      "MsiExec.exe",
				args:      []string{"/X{11111111-2222-3333-4444-555555555555}", "/l*v", "msi.log", "/qn", "/norestart"},
				script:    false,
				exitCodes: []int{1641, 3010, 8},
				logPath:   "MsiExec.exe.log",
				rawPath:   "MsiExec.exe",
			},
		},
		{
			name: "arp_msi_rewrites_slash_i_inside_executable_path",
			state: client.PackageState{
				PackageSpec: &goolib.PkgSpec{
					Install: goolib.ExecFile{Path: "pkg.msi"},
				},
				InstalledApp: client.InstalledApplication{Reg: "HKLM/MSI/PathContainsSlashI"},
			},
			useARP: true,
			want: cmdline{
				name:      "/opt/XnstallDir/MsiExec.exe",
				args:      []string{"/X{AAAA-BBBB}", "/qn", "/norestart"},
				script:    false,
				exitCodes: []int{1641, 3010},
				logPath:   "pkgdir/opt/XnstallDir/MsiExec.exe.log",
				rawPath:   "/opt/XnstallDir/MsiExec.exe",
			},
		},
		{
			name: "arp_msix_reuses_install_path_without_registry_lookup",
			state: client.PackageState{
				PackageSpec: &goolib.PkgSpec{
					Install: goolib.ExecFile{Path: "Contoso.Tool_1.0.0_x64.msix"},
				},
			},
			want: cmdline{
				name:      "powershell",
				args:      []string{`Get-AppxProvisionedPackage -online | Where {$_.DisplayName -match "Contoso.Tool*"} | Remove-AppProvisionedPackage -online -AllUsers`},
				script:    false,
				exitCodes: []int{1641, 3010},
				logPath:   "pkgdir/Contoso.Tool_1.0.0_x64.msix.log",
				rawPath:   "Contoso.Tool_1.0.0_x64.msix",
			},
		},
		{
			name: "arp_default_prefers_quiet_uninstall_string",
			state: client.PackageState{
				PackageSpec: &goolib.PkgSpec{
					Install: goolib.ExecFile{Path: "setup.exe"},
				},
				InstalledApp: client.InstalledApplication{Reg: "HKLM/EXE/BothQuietAndStandard"},
			},
			useARP:    true,
			existsSet: map[string]bool{"/vendor/bin/uninstall.exe": true},
			want: cmdline{
				name:      "/vendor/bin/uninstall.exe",
				args:      []string{"/quiet", "/norestart"},
				script:    false,
				exitCodes: []int{1641, 3010},
				logPath:   "/vendor/bin/uninstall.exe.log",
				rawPath:   "/vendor/bin/uninstall.exe",
			},
		},
		{
			name: "arp_default_falls_back_to_uninstall_string_and_strips_command_quotes",
			state: client.PackageState{
				PackageSpec: &goolib.PkgSpec{
					Install: goolib.ExecFile{Path: "setup.exe"},
				},
				InstalledApp: client.InstalledApplication{Reg: "HKLM/EXE/FallbackStandardQuoted"},
			},
			useARP:    true,
			existsSet: map[string]bool{"/vendor/bin/unins000.exe": true},
			want: cmdline{
				name:      "/vendor/bin/unins000.exe",
				args:      []string{"/VERYSILENT"},
				script:    false,
				exitCodes: []int{1641, 3010},
				logPath:   "/vendor/bin/unins000.exe.log",
				rawPath:   "/vendor/bin/unins000.exe",
			},
		},
		{
			name: "arp_default_script_joins_dir_with_derived_path",
			state: client.PackageState{
				PackageSpec: &goolib.PkgSpec{
					Install: goolib.ExecFile{Path: "install.ps1"},
				},
				InstalledApp: client.InstalledApplication{Reg: "HKLM/Script/RelativePS1"},
			},
			useARP: true,
			want: cmdline{
				name:      "pkgdir/scripts/remove.ps1",
				args:      []string{"-Force"},
				script:    true,
				exitCodes: nil,
				logPath:   "pkgdir/scripts/remove.ps1.log",
				rawPath:   "scripts/remove.ps1",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			uninstallBefore := tc.state.PackageSpec.Uninstall
			arp := unexpectedARP(t)
			if tc.useARP {
				arp = fakeARP(regEntries)
			}
			p := slashPathOps(func(s string) bool {
				return tc.existsSet[s]
			})

			got, ok := uninstallCmdline("pkgdir", &tc.state, arp, p)
			if !ok {
				t.Fatalf("uninstallCmdline() = _, false; want ok=true")
			}
			if diff := cmp.Diff(tc.want, got, cmp.AllowUnexported(cmdline{})); diff != "" {
				t.Errorf("uninstallCmdline() mismatch (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(uninstallBefore, tc.state.PackageSpec.Uninstall); diff != "" {
				t.Errorf("uninstallCmdline() mutated PackageSpec.Uninstall (-want +got):\n%s", diff)
			}
		})
	}
}

func TestUninstallCmdlineEmptyARPReturnsFalse(t *testing.T) {
	regEntries := map[string]map[string]string{
		"HKLM/MSI/MissingUninstallString": {
			"QuietUninstallString": "ignored_for_msi.exe",
		},
		"HKLM/MSI/EmptyQuotes": {
			"UninstallString": `""`,
		},
		"HKLM/EXE/Empty": {},
		"HKLM/EXE/EmptyQuotes": {
			"QuietUninstallString": `""`,
		},
	}

	tests := []struct {
		name        string
		installPath string
		reg         string
		wantErrLog  bool
	}{
		{
			name:        "msi_missing_uninstall_string_logs_error_and_skips",
			installPath: "pkg.msi",
			reg:         "HKLM/MSI/MissingUninstallString",
			wantErrLog:  true,
		},
		{
			name:        "msi_empty_quoted_command_skips",
			installPath: "pkg.msi",
			reg:         "HKLM/MSI/EmptyQuotes",
		},
		{
			name:        "exe_missing_arp_values_skips",
			installPath: "setup.exe",
			reg:         "HKLM/EXE/Empty",
		},
		{
			name:        "exe_empty_quoted_command_skips",
			installPath: "setup.exe",
			reg:         "HKLM/EXE/EmptyQuotes",
		},
		{
			name:        "portable_package_without_install_or_uninstall_skips",
			installPath: "",
			reg:         "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			logs := systemTestLogs.capture(t)
			st := &client.PackageState{
				PackageSpec: &goolib.PkgSpec{
					Install: goolib.ExecFile{Path: tc.installPath},
				},
				InstalledApp: client.InstalledApplication{Reg: tc.reg},
			}
			got, ok := uninstallCmdline("pkgdir", st, fakeARP(regEntries), slashPathOps(nil))
			if ok {
				t.Fatalf("uninstallCmdline() = %+v, true; want false", got)
			}
			gotLogs := logs()
			if tc.wantErrLog && !strings.Contains(gotLogs, "registry value not found") {
				t.Errorf("logs = %q, want error log containing %q", gotLogs, "registry value not found")
			}
			if !tc.wantErrLog && gotLogs != "" {
				t.Errorf("logs = %q, want empty", gotLogs)
			}
		})
	}
}

func TestUninstallCmdlineWhitespaceOnlyMSIARPPanics(t *testing.T) {
	st := &client.PackageState{
		PackageSpec: &goolib.PkgSpec{
			Install: goolib.ExecFile{Path: "pkg.msi"},
		},
		InstalledApp: client.InstalledApplication{Reg: "HKLM/MSI/Whitespace"},
	}
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("uninstallCmdline() with whitespace-only MSI UninstallString did not panic, want index-out-of-range panic")
		}
	}()
	uninstallCmdline("pkgdir", st, func(reg, ext string) string {
		return "   \t  "
	}, slashPathOps(nil))
}

func TestCmdlineSlicesDoNotAliasPackageOrSpecSlices(t *testing.T) {
	inArgs := []string{"/S", "/D=path"}
	inCodes := []int{42}
	ps := &goolib.PkgSpec{
		Install:   goolib.ExecFile{Path: "setup.exe", Args: inArgs, ExitCodes: inCodes},
		Uninstall: goolib.ExecFile{Path: "remove.ps1", Args: []string{"-Clean"}, ExitCodes: []int{99}},
	}

	clExe, ok := installCmdline("pkgdir", ps, slashPathOps(nil))
	if !ok {
		t.Fatal("installCmdline(setup.exe) returned ok=false")
	}
	clExe.exitCodes[0] = -1
	clExe.args[0] = "mutated"

	if diff := cmp.Diff([]int{1641, 3010}, msiSuccessCodes); diff != "" {
		t.Errorf("mutating cmdline.exitCodes modified msiSuccessCodes (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff([]string{"/S", "/D=path"}, ps.Install.Args); diff != "" {
		t.Errorf("mutating cmdline.args modified PkgSpec.Install.Args (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff([]int{42}, ps.Install.ExitCodes); diff != "" {
		t.Errorf("mutating cmdline.exitCodes modified PkgSpec.Install.ExitCodes (-want +got):\n%s", diff)
	}

	clAgain, _ := installCmdline("pkgdir", ps, slashPathOps(nil))
	if diff := cmp.Diff([]int{1641, 3010, 42}, clAgain.exitCodes); diff != "" {
		t.Errorf("subsequent installCmdline() exitCodes mismatch (-want +got):\n%s", diff)
	}

	clScript, ok := uninstallCmdline("pkgdir", &client.PackageState{PackageSpec: ps}, unexpectedARP(t), slashPathOps(nil))
	if !ok {
		t.Fatal("uninstallCmdline(remove.ps1) returned ok=false")
	}
	clScript.exitCodes[0] = -2
	clScript.args[0] = "mutated"

	if diff := cmp.Diff([]string{"-Clean"}, ps.Uninstall.Args); diff != "" {
		t.Errorf("mutating script cmdline.args modified PkgSpec.Uninstall.Args (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff([]int{99}, ps.Uninstall.ExitCodes); diff != "" {
		t.Errorf("mutating script cmdline.exitCodes modified PkgSpec.Uninstall.ExitCodes (-want +got):\n%s", diff)
	}
}

func TestBuildCmdAppliesContextAcrossAllBranches(t *testing.T) {
	// Intentionally omit t.Parallel because t.Setenv mutates the process environment.
	t.Setenv("GOOGET_ACTION", "stale")
	t.Setenv("GooGetRoot", "/var/lib/googet")

	activeContext := goolib.InstallerContext{
		Action:          goolib.ActionInstall,
		Package:         "foo.x86_64.1.0.0",
		PreviousVersion: "",
	}

	cases := []struct {
		name     string
		cl       cmdline
		wantArgs []string
	}{
		{
			name:     "msiexec_install",
			cl:       cmdline{name: "msiexec", args: []string{"/i", "pkgdir/pkg.msi", "/qn"}},
			wantArgs: []string{"msiexec", "/i", "pkgdir/pkg.msi", "/qn"},
		},
		{
			name:     "arp_msi_exe_uninstall",
			cl:       cmdline{name: "MsiExec.exe", args: []string{"/X{GUID}", "/qn", "/norestart"}},
			wantArgs: []string{"MsiExec.exe", "/X{GUID}", "/qn", "/norestart"},
		},
		{
			name:     "wusa_install",
			cl:       cmdline{name: "wusa", args: []string{"pkgdir/kb.msu", "/quiet", "/norestart"}},
			wantArgs: []string{"wusa", "pkgdir/kb.msu", "/quiet", "/norestart"},
		},
		{
			name:     "direct_exe",
			cl:       cmdline{name: "pkgdir/setup.exe", args: []string{"/S"}},
			wantArgs: []string{"pkgdir/setup.exe", "/S"},
		},
		{
			name:     "powershell_appx",
			cl:       cmdline{name: "powershell", args: []string{"Add-AppxProvisionedPackage -online -PackagePath pkgdir/app.msix -SkipLicense"}},
			wantArgs: []string{"powershell", "Add-AppxProvisionedPackage -online -PackagePath pkgdir/app.msix -SkipLicense"},
		},
		{
			name:     "script_branch",
			cl:       cmdline{name: "pkgdir/install.ps1", args: []string{"-Flag"}, script: true},
			wantArgs: []string{"powershell", "-ExecutionPolicy", "Bypass", "-NonInteractive", "-NoProfile", "-Command", "pkgdir/install.ps1", "-Flag"},
		},
	}

	scriptBuilder := func(s string, args []string) (*exec.Cmd, error) {
		full := append([]string{"-ExecutionPolicy", "Bypass", "-NonInteractive", "-NoProfile", "-Command", s}, args...)
		return exec.Command("powershell", full...), nil
	}

	for _, tc := range cases {
		t.Run(tc.name+"_zero_context", func(t *testing.T) {
			calledScript := false
			sb := func(s string, args []string) (*exec.Cmd, error) {
				calledScript = true
				return scriptBuilder(s, args)
			}
			cmd, err := buildCmd(tc.cl, sb, goolib.InstallerContext{})
			if err != nil {
				t.Fatalf("buildCmd() unexpected error: %v", err)
			}
			if calledScript != tc.cl.script {
				t.Errorf("script builder called = %v, want %v", calledScript, tc.cl.script)
			}
			if diff := cmp.Diff(tc.wantArgs, cmd.Args); diff != "" {
				t.Errorf("buildCmd() Args mismatch (-want +got):\n%s", diff)
			}
			if cmd.Env != nil {
				t.Errorf("buildCmd() with zero context set Env = %v, want nil", cmd.Env)
			}
		})

		t.Run(tc.name+"_active_context", func(t *testing.T) {
			cmd, err := buildCmd(tc.cl, scriptBuilder, activeContext)
			if err != nil {
				t.Fatalf("buildCmd() unexpected error: %v", err)
			}
			if diff := cmp.Diff(tc.wantArgs, cmd.Args); diff != "" {
				t.Errorf("buildCmd() Args mismatch (-want +got):\n%s", diff)
			}
			if cmd.Env == nil {
				t.Fatal("buildCmd() with active context left Env nil")
			}
			if slices.Contains(cmd.Env, "GOOGET_ACTION=stale") {
				t.Errorf("buildCmd() retained stale GOOGET_ACTION=stale: %v", cmd.Env)
			}
			for _, wantEntry := range []string{
				"GooGetRoot=/var/lib/googet",
				"GOOGET_ACTION=install",
				"GOOGET_PACKAGE=foo.x86_64.1.0.0",
				"GOOGET_PREVIOUS_VERSION=",
			} {
				if !slices.Contains(cmd.Env, wantEntry) {
					t.Errorf("buildCmd() Env = %v, missing %q", cmd.Env, wantEntry)
				}
			}
		})
	}

	t.Run("script_builder_error_propagates", func(t *testing.T) {
		wantErr := errors.New(`unknown extension ".msp"`)
		cmd, err := buildCmd(cmdline{name: "pkgdir/patch.msp", script: true}, func(string, []string) (*exec.Cmd, error) {
			return nil, wantErr
		}, activeContext)
		if !errors.Is(err, wantErr) {
			t.Errorf("buildCmd() error = %v, want %v", err, wantErr)
		}
		if cmd != nil {
			t.Errorf("buildCmd() cmd = %v, want nil on error", cmd)
		}
	})
}

func TestLogInstallerContextFormattingAndFiltering(t *testing.T) {
	t.Run("zero_and_out_of_range_actions_emit_nothing", func(t *testing.T) {
		logs := systemTestLogs.capture(t)
		logInstallerContext("install", goolib.InstallerContext{})
		logInstallerContext("uninstall", goolib.InstallerContext{Action: goolib.Action(99), Package: "pkg.noarch.1"})
		if got := logs(); got != "" {
			t.Errorf("logInstallerContext() emitted %q for inactive actions, want empty", got)
		}
	})

	t.Run("non_replace_clears_replaced_by_and_sanitizes_nuls", func(t *testing.T) {
		logs := systemTestLogs.capture(t)
		logInstallerContext("install", goolib.InstallerContext{
			Action:          goolib.ActionInstall,
			Package:         "pkg\x00.x86_64.1.0.0",
			PreviousVersion: "",
			ReplacedBy:      "ignored.x86_64.2.0.0",
		})
		const want = "installer_context hook=install action=install package=pkg.x86_64.1.0.0 previous= replaced_by="
		if got := logs(); !strings.Contains(got, want) {
			t.Errorf("logInstallerContext() = %q, want substring %q", got, want)
		}
	})

	t.Run("replace_logs_sanitized_replaced_by", func(t *testing.T) {
		logs := systemTestLogs.capture(t)
		logInstallerContext("uninstall", goolib.InstallerContext{
			Action:          goolib.ActionReplace,
			Package:         "old\x00.noarch.1",
			PreviousVersion: "0.\x009",
			ReplacedBy:      "new\x00.noarch.2",
		})
		const want = "installer_context hook=uninstall action=replace package=old.noarch.1 previous=0.9 replaced_by=new.noarch.2"
		if got := logs(); !strings.Contains(got, want) {
			t.Errorf("logInstallerContext() = %q, want substring %q", got, want)
		}
	})
}

func writeEnvDumpScript(t *testing.T, dir, scriptName, outPath string) {
	t.Helper()
	body := "#!/bin/sh\n" +
		"echo \"ACTION=${GOOGET_ACTION-<unset>}\" > " + outPath + "\n" +
		"echo \"PACKAGE=${GOOGET_PACKAGE-<unset>}\" >> " + outPath + "\n" +
		"echo \"PREVIOUS=${GOOGET_PREVIOUS_VERSION-<unset>}\" >> " + outPath + "\n" +
		"echo \"REPLACED_BY=${GOOGET_REPLACED_BY-<unset>}\" >> " + outPath + "\n" +
		"echo \"SOURCES=${GOOGET_SOURCES-<unset>}\" >> " + outPath + "\n" +
		"echo \"ROOT=${GooGetRoot-<unset>}\" >> " + outPath + "\n" +
		"echo \"hook-ran\"\n"
	fullPath := filepath.Join(dir, scriptName)
	if err := os.WriteFile(fullPath, []byte(body), 0755); err != nil {
		t.Fatalf("os.WriteFile(%q): %v", fullPath, err)
	}
}

func TestInstallUninstallAndVerifyLinuxExec(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("requires Linux /bin/sh execution")
	}

	t.Setenv("GOOGET_ACTION", "stale_inherited")
	t.Setenv("GOOGET_SOURCES", "https://repo.example")
	t.Setenv("GooGetRoot", "/var/lib/googet")

	t.Run("empty_paths_noop_without_context_log", func(t *testing.T) {
		dir := t.TempDir()
		logs := systemTestLogs.capture(t)
		ps := &goolib.PkgSpec{Name: "noop", Arch: "noarch", Version: "1.0.0"}
		st := &client.PackageState{PackageSpec: ps}
		ic := goolib.InstallerContext{Action: goolib.ActionInstall, Package: "noop.noarch.1.0.0"}

		if err := InstallWith(dir, ps, ic); err != nil {
			t.Fatalf("InstallWith() with empty path failed: %v", err)
		}
		if err := UninstallWith(dir, st, goolib.InstallerContext{Action: goolib.ActionRemove, Package: "noop.noarch.1.0.0"}); err != nil {
			t.Fatalf("UninstallWith() with empty path failed: %v", err)
		}
		if got := logs(); got != "" {
			t.Errorf("logs = %q, want empty when no hook runs", got)
		}
	})

	t.Run("zero_context_install_uninstall_and_verify_inherit_caller_env", func(t *testing.T) {
		dir := t.TempDir()
		installEnvOut := filepath.Join(dir, "install_env.txt")
		uninstallEnvOut := filepath.Join(dir, "uninstall_env.txt")
		verifyEnvOut := filepath.Join(dir, "verify_env.txt")
		writeEnvDumpScript(t, dir, "install.sh", installEnvOut)
		writeEnvDumpScript(t, dir, "uninstall.sh", uninstallEnvOut)
		writeEnvDumpScript(t, dir, "verify.sh", verifyEnvOut)

		ps := &goolib.PkgSpec{
			Name:      "legacy",
			Arch:      "x86_64",
			Version:   "1.0.0",
			Install:   goolib.ExecFile{Path: "install.sh"},
			Uninstall: goolib.ExecFile{Path: "uninstall.sh"},
			Verify:    goolib.ExecFile{Path: "verify.sh"},
		}
		st := &client.PackageState{PackageSpec: ps}

		logs := systemTestLogs.capture(t)
		if err := Install(dir, ps); err != nil {
			t.Fatalf("Install() failed: %v", err)
		}
		if err := Uninstall(dir, st); err != nil {
			t.Fatalf("Uninstall() failed: %v", err)
		}
		if err := Verify(dir, ps); err != nil {
			t.Fatalf("Verify() failed: %v", err)
		}

		wantInherited := strings.Join([]string{
			"ACTION=stale_inherited",
			"PACKAGE=<unset>",
			"PREVIOUS=<unset>",
			"REPLACED_BY=<unset>",
			"SOURCES=https://repo.example",
			"ROOT=/var/lib/googet",
			"",
		}, "\n")

		for label, path := range map[string]string{
			"Install":   installEnvOut,
			"Uninstall": uninstallEnvOut,
			"Verify":    verifyEnvOut,
		} {
			gotBytes, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("reading %s env dump: %v", label, err)
			}
			if diff := cmp.Diff(wantInherited, string(gotBytes)); diff != "" {
				t.Errorf("%s child env mismatch (-want +got):\n%s", label, diff)
			}
		}

		if gotLogs := logs(); strings.Contains(gotLogs, "installer_context") {
			t.Errorf("zero-context operations logged installer_context unexpectedly:\n%s", gotLogs)
		}
		for _, logFile := range []string{"googet_install.log", "googet_remove.log", "googet_verify.log"} {
			b, err := os.ReadFile(filepath.Join(dir, logFile))
			if err != nil {
				t.Fatalf("expected %s to exist: %v", logFile, err)
			}
			if !strings.Contains(string(b), "hook-ran") {
				t.Errorf("%s = %q, want %q", logFile, string(b), "hook-ran")
			}
		}
	})

	t.Run("active_context_strips_inherited_googet_vars_and_logs_context", func(t *testing.T) {
		dir := t.TempDir()
		installEnvOut := filepath.Join(dir, "install_with_env.txt")
		uninstallEnvOut := filepath.Join(dir, "uninstall_with_env.txt")
		writeEnvDumpScript(t, dir, "install.sh", installEnvOut)
		writeEnvDumpScript(t, dir, "uninstall.sh", uninstallEnvOut)

		ps := &goolib.PkgSpec{
			Name:      "modern",
			Arch:      "x86_64",
			Version:   "2.0.0",
			Install:   goolib.ExecFile{Path: "install.sh"},
			Uninstall: goolib.ExecFile{Path: "uninstall.sh"},
		}
		st := &client.PackageState{PackageSpec: ps}

		logs := systemTestLogs.capture(t)
		if err := InstallWith(dir, ps, goolib.InstallerContext{
			Action:          goolib.ActionUpgrade,
			Package:         "modern.x86_64.2.0.0",
			PreviousVersion: "1.5.0",
			ReplacedBy:      "ignored.x86_64.9.9.9",
		}); err != nil {
			t.Fatalf("InstallWith() failed: %v", err)
		}
		if err := UninstallWith(dir, st, goolib.InstallerContext{
			Action:     goolib.ActionReplace,
			Package:    "legacy.x86_64.1.0.0",
			ReplacedBy: "modern.x86_64.2.0.0",
		}); err != nil {
			t.Fatalf("UninstallWith() failed: %v", err)
		}

		gotInstall, err := os.ReadFile(installEnvOut)
		if err != nil {
			t.Fatalf("reading InstallWith env dump: %v", err)
		}
		wantInstall := strings.Join([]string{
			"ACTION=upgrade",
			"PACKAGE=modern.x86_64.2.0.0",
			"PREVIOUS=1.5.0",
			"REPLACED_BY=<unset>",
			"SOURCES=<unset>",
			"ROOT=/var/lib/googet",
			"",
		}, "\n")
		if diff := cmp.Diff(wantInstall, string(gotInstall)); diff != "" {
			t.Errorf("InstallWith() child env mismatch (-want +got):\n%s", diff)
		}

		gotUninstall, err := os.ReadFile(uninstallEnvOut)
		if err != nil {
			t.Fatalf("reading UninstallWith env dump: %v", err)
		}
		wantUninstall := strings.Join([]string{
			"ACTION=replace",
			"PACKAGE=legacy.x86_64.1.0.0",
			"PREVIOUS=",
			"REPLACED_BY=modern.x86_64.2.0.0",
			"SOURCES=<unset>",
			"ROOT=/var/lib/googet",
			"",
		}, "\n")
		if diff := cmp.Diff(wantUninstall, string(gotUninstall)); diff != "" {
			t.Errorf("UninstallWith() child env mismatch (-want +got):\n%s", diff)
		}

		gotLogs := logs()
		for _, wantLine := range []string{
			"installer_context hook=install action=upgrade package=modern.x86_64.2.0.0 previous=1.5.0 replaced_by=",
			"installer_context hook=uninstall action=replace package=legacy.x86_64.1.0.0 previous= replaced_by=modern.x86_64.2.0.0",
		} {
			if !strings.Contains(gotLogs, wantLine) {
				t.Errorf("logs missing %q; got:\n%s", wantLine, gotLogs)
			}
		}
	})

	t.Run("log_file_created_before_command_start_failure", func(t *testing.T) {
		dir := t.TempDir()
		ps := &goolib.PkgSpec{
			Name:    "broken",
			Arch:    "x86_64",
			Version: "1.0.0",
			Install: goolib.ExecFile{Path: "missing_script.unknown"},
		}
		if err := InstallWith(dir, ps, goolib.InstallerContext{
			Action:  goolib.ActionInstall,
			Package: "broken.x86_64.1.0.0",
		}); err == nil {
			t.Fatal("InstallWith() with missing script succeeded, want error")
		}
		if _, err := os.Stat(filepath.Join(dir, "googet_install.log")); err != nil {
			t.Errorf("googet_install.log not created before command error: %v", err)
		}
	})
}
