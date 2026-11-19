//go:build windows
// +build windows

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
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/googet/v2/client"
	"github.com/google/googet/v2/goolib"
)

func TestUninstallCmdlineWindowsBackslashPaths(t *testing.T) {
	regEntries := map[string]map[string]string{
		`HKLM\Script\DrivePath`: {
			"QuietUninstallString": `C:\Vendor\uninstall.ps1 -Silent`,
		},
		`HKLM\Script\UnquotedSpaces`: {
			"QuietUninstallString": `C:\Program Files\Vendor\uninstall.ps1 -Silent`,
		},
		`HKLM\EXE\QuotedSpacesAndDottedDir`: {
			"QuietUninstallString": `"C:\Program Files\Vendor.v2\Uninstall Tool\unins000.exe" /SILENT "/LOG=C:\Temp\unins log.txt"`,
		},
		`HKLM\MSI\QuotedSystemPath`: {
			"UninstallString": `"C:\Windows\System32\MsiExec.exe" /I{12345678-ABCD-EF01-2345-6789ABCDEF01}`,
		},
	}

	tests := []struct {
		name      string
		dir       string
		state     client.PackageState
		existsSet map[string]bool
		want      cmdline
	}{
		{
			name: "arp_script_joins_extract_dir_with_drive_letter_path",
			dir:  `C:\ProgramData\GooGet\extract`,
			state: client.PackageState{
				PackageSpec: &goolib.PkgSpec{
					Install: goolib.ExecFile{Path: `scripts\install.ps1`},
				},
				InstalledApp: client.InstalledApplication{Reg: `HKLM\Script\DrivePath`},
			},
			want: cmdline{
				name:      `C:\ProgramData\GooGet\extract\C:\Vendor\uninstall.ps1`,
				args:      []string{"-Silent"},
				script:    true,
				exitCodes: nil,
				logPath:   `C:\ProgramData\GooGet\extract\C:\Vendor\uninstall.ps1.log`,
				rawPath:   `C:\Vendor\uninstall.ps1`,
			},
		},
		{
			// An unquoted registry path is split at whitespace, so a path
			// containing spaces is cut at its first space and the rest becomes
			// arguments, as in the earlier implementation.
			name: "arp_unquoted_path_with_spaces_splits_at_first_space",
			dir:  `C:\ProgramData\GooGet\extract`,
			state: client.PackageState{
				PackageSpec: &goolib.PkgSpec{
					Install: goolib.ExecFile{Path: `scripts\install.ps1`},
				},
				InstalledApp: client.InstalledApplication{Reg: `HKLM\Script\UnquotedSpaces`},
			},
			want: cmdline{
				name:      `C:\ProgramData\GooGet\extract\C:\Program`,
				args:      []string{`Files\Vendor\uninstall.ps1`, "-Silent"},
				script:    true,
				exitCodes: nil,
				logPath:   `C:\ProgramData\GooGet\extract\C:\Program.log`,
				rawPath:   `C:\Program`,
			},
		},
		{
			name: "msix_base_extracts_package_prefix_after_backslash",
			dir:  `C:\ProgramData\GooGet\extract`,
			state: client.PackageState{
				PackageSpec: &goolib.PkgSpec{
					Uninstall: goolib.ExecFile{Path: `sub\Contoso.Widget_3.4.0_x64__8wekyb3d8bbwe.msix`},
				},
			},
			want: cmdline{
				name:      "powershell",
				args:      []string{`Get-AppxProvisionedPackage -online | Where {$_.DisplayName -match "Contoso.Widget*"} | Remove-AppProvisionedPackage -online -AllUsers`},
				script:    false,
				exitCodes: []int{1641, 3010},
				logPath:   `C:\ProgramData\GooGet\extract\sub\Contoso.Widget_3.4.0_x64__8wekyb3d8bbwe.msix.log`,
				rawPath:   `sub\Contoso.Widget_3.4.0_x64__8wekyb3d8bbwe.msix`,
			},
		},
		{
			name: "quoted_arp_exe_with_spaces_and_dotted_directory_exists",
			dir:  `C:\ProgramData\GooGet\extract`,
			state: client.PackageState{
				PackageSpec: &goolib.PkgSpec{
					Install: goolib.ExecFile{Path: `sub\setup.exe`},
				},
				InstalledApp: client.InstalledApplication{Reg: `HKLM\EXE\QuotedSpacesAndDottedDir`},
			},
			existsSet: map[string]bool{
				`C:\Program Files\Vendor.v2\Uninstall Tool\unins000.exe`: true,
			},
			want: cmdline{
				name:      `C:\Program Files\Vendor.v2\Uninstall Tool\unins000.exe`,
				args:      []string{"/SILENT", `"/LOG=C:\Temp\unins log.txt"`},
				script:    false,
				exitCodes: []int{1641, 3010},
				logPath:   `C:\Program Files\Vendor.v2\Uninstall Tool\unins000.exe.log`,
				rawPath:   `C:\Program Files\Vendor.v2\Uninstall Tool\unins000.exe`,
			},
		},
		{
			name: "quoted_arp_msi_with_spaces",
			dir:  `C:\ProgramData\GooGet\extract`,
			state: client.PackageState{
				PackageSpec: &goolib.PkgSpec{
					Install: goolib.ExecFile{Path: `sub\pkg.msi`},
				},
				InstalledApp: client.InstalledApplication{Reg: `HKLM\MSI\QuotedSystemPath`},
			},
			existsSet: map[string]bool{
				`C:\Windows\System32\MsiExec.exe`: true,
			},
			want: cmdline{
				name:      `C:\Windows\System32\MsiExec.exe`,
				args:      []string{"/X{12345678-ABCD-EF01-2345-6789ABCDEF01}", "/qn", "/norestart"},
				script:    false,
				exitCodes: []int{1641, 3010},
				logPath:   `C:\Windows\System32\MsiExec.exe.log`,
				rawPath:   `C:\Windows\System32\MsiExec.exe`,
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := windowsPaths
			p.exists = func(s string) bool {
				return tc.existsSet[s]
			}
			got, ok := uninstallCmdline(tc.dir, &tc.state, fakeARP(regEntries), p)
			if !ok {
				t.Fatalf("uninstallCmdline() = _, false; want ok=true")
			}
			if diff := cmp.Diff(tc.want, got, cmp.AllowUnexported(cmdline{})); diff != "" {
				t.Errorf("uninstallCmdline() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}
