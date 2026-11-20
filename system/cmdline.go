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
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/google/googet/v2/client"
	"github.com/google/googet/v2/goolib"
	"github.com/google/logger"
)

var (
	msiSuccessCodes = []int{1641, 3010}
	uninstallArgRE  = regexp.MustCompile(`[^\s"]+|"([^"]*)"`)
)

// pathOps abstracts OS path and file-existence queries so Windows command-line
// derivation can be unit-tested hermetically on any host OS.
type pathOps struct {
	ext    func(string) string
	base   func(string) string
	join   func(...string) string
	exists func(string) bool
}

func normalizePathOps(p pathOps) pathOps {
	if p.ext == nil {
		p.ext = filepath.Ext
	}
	if p.base == nil {
		p.base = filepath.Base
	}
	if p.join == nil {
		p.join = filepath.Join
	}
	if p.exists == nil {
		p.exists = func(string) bool { return false }
	}
	return p
}

// cmdline describes a Windows installer or uninstaller command prior to
// exec.Cmd construction.
type cmdline struct {
	name      string
	args      []string
	script    bool
	exitCodes []int
	logPath   string
	rawPath   string
}

func msiExitCodes(extra []int) []int {
	ec := make([]int, 0, len(msiSuccessCodes)+len(extra))
	ec = append(ec, msiSuccessCodes...)
	ec = append(ec, extra...)
	return ec
}

// arpUninstallString selects the registry uninstall command string for
// extension using getValue to read named values from an opened Uninstall key.
// MSI entries always read UninstallString and log lookup errors, whereas other
// entries prefer QuietUninstallString and fall back to UninstallString.
func arpUninstallString(extension string, getValue func(string) (string, error)) string {
	switch extension {
	case "msi":
		un, err := getValue("UninstallString")
		if err != nil {
			logger.Error(err)
			return ""
		}
		return un
	default:
		un, err := getValue("QuietUninstallString")
		if err != nil {
			un, err = getValue("UninstallString")
			if err != nil {
				return ""
			}
		}
		return un
	}
}

// installCmdline derives the Windows installer command line, exit codes, and
// log path for ps, or returns false when ps specifies no install command.
func installCmdline(dir string, ps *goolib.PkgSpec, p pathOps) (cmdline, bool) {
	in := ps.Install
	if in.Path == "" {
		return cmdline{}, false
	}
	p = normalizePathOps(p)
	logPath := p.join(dir, in.Path+".log")
	s := p.join(dir, in.Path)
	msiLog := p.join(dir, "msi_install.log")

	switch p.ext(s) {
	case ".msi":
		args := append([]string{"/i", s, "/qn", "/norestart", "/log", msiLog}, in.Args...)
		return cmdline{
			name:      "msiexec",
			args:      args,
			exitCodes: msiExitCodes(in.ExitCodes),
			logPath:   logPath,
			rawPath:   in.Path,
		}, true
	case ".msp":
		args := append([]string{"/update", s, "/qn", "/norestart", "/log", msiLog}, in.Args...)
		return cmdline{
			name:      "msiexec",
			args:      args,
			exitCodes: msiExitCodes(in.ExitCodes),
			logPath:   logPath,
			rawPath:   in.Path,
		}, true
	case ".msu":
		args := append([]string{s, "/quiet", "/norestart"}, in.Args...)
		return cmdline{
			name:      "wusa",
			args:      args,
			exitCodes: msiExitCodes(in.ExitCodes),
			logPath:   logPath,
			rawPath:   in.Path,
		}, true
	case ".exe":
		return cmdline{
			name:      s,
			args:      slices.Clone(in.Args),
			exitCodes: msiExitCodes(in.ExitCodes),
			logPath:   logPath,
			rawPath:   in.Path,
		}, true
	case ".msix", ".msixbundle":
		// Add-AppxProvisionedPackage installs the package for all users.
		installCmd := fmt.Sprintf("Add-AppxProvisionedPackage -online -PackagePath %v -SkipLicense", s)
		args := append([]string{installCmd}, in.Args...)
		return cmdline{
			name:      "powershell",
			args:      args,
			exitCodes: msiExitCodes(in.ExitCodes),
			logPath:   logPath,
			rawPath:   in.Path,
		}, true
	default:
		return cmdline{
			name:      s,
			args:      slices.Clone(in.Args),
			script:    true,
			exitCodes: slices.Clone(in.ExitCodes),
			logPath:   logPath,
			rawPath:   in.Path,
		}, true
	}
}

// uninstallCmdline derives the Windows uninstaller command line, exit codes,
// and log path for st (consulting arp when st's spec omits Uninstall.Path), or
// returns false when nothing should run.
func uninstallCmdline(dir string, st *client.PackageState, arp func(reg, ext string) string, p pathOps) (cmdline, bool) {
	p = normalizePathOps(p)
	var filePath string
	ps := st.PackageSpec
	un := ps.Uninstall
	if un.Path == "" {
		switch p.ext(ps.Install.Path) {
		case ".msi":
			u := arp(st.InstalledApp.Reg, "msi")
			if u == "" {
				return cmdline{}, false
			}
			u = strings.ReplaceAll(u, `/I`, `/X`)
			commands := uninstallArgRE.FindAllString(u, -1)
			un.Path = strings.ReplaceAll(commands[0], "\"", "")
			un.Args = []string{"/qn", "/norestart"}
			un.Args = append(commands[1:], un.Args...)
			filePath = un.Path
		case ".msix", ".msixbundle":
			un.Path = ps.Install.Path
			filePath = un.Path
		default:
			u := arp(st.InstalledApp.Reg, "")
			commands := uninstallArgRE.FindAllString(u, -1)
			if len(commands) > 0 {
				un.Path = strings.ReplaceAll(commands[0], "\"", "")
				un.Args = commands[1:]
				filePath = un.Path
			}
		}
		if un.Path == "" {
			return cmdline{}, false
		}
	}

	logPath := fmt.Sprintf("%s.log", un.Path)
	if !p.exists(un.Path) {
		logPath = p.join(dir, logPath)
	}
	if filePath == "" {
		filePath = p.join(dir, un.Path)
	}

	switch p.ext(filePath) {
	case ".msi":
		msiLog := p.join(dir, "msi_uninstall.log")
		args := append([]string{"/x", filePath, "/qn", "/norestart", "/log", msiLog}, un.Args...)
		return cmdline{
			name:      "msiexec",
			args:      args,
			exitCodes: msiExitCodes(un.ExitCodes),
			logPath:   logPath,
			rawPath:   un.Path,
		}, true
	case ".msu":
		args := append([]string{filePath, "/uninstall", "/quiet", "/norestart"}, un.Args...)
		return cmdline{
			name:      "wusa",
			args:      args,
			exitCodes: msiExitCodes(un.ExitCodes),
			logPath:   logPath,
			rawPath:   un.Path,
		}, true
	case ".exe":
		return cmdline{
			name:      filePath,
			args:      slices.Clone(un.Args),
			exitCodes: msiExitCodes(un.ExitCodes),
			logPath:   logPath,
			rawPath:   un.Path,
		}, true
	case ".msix", ".msixbundle":
		s := strings.Split(p.base(filePath), "_")[0]
		removeCmd := fmt.Sprintf(`Get-AppxProvisionedPackage -online | Where {$_.DisplayName -match "%v*"} | Remove-AppProvisionedPackage -online -AllUsers`, s)
		args := append([]string{removeCmd}, un.Args...)
		return cmdline{
			name:      "powershell",
			args:      args,
			exitCodes: msiExitCodes(un.ExitCodes),
			logPath:   logPath,
			rawPath:   un.Path,
		}, true
	default:
		// Script uninstallers always join dir with un.Path, even when the path
		// came from the registry, to keep the earlier
		// goolib.Exec(filepath.Join(dir, un.Path), ...) behavior.
		return cmdline{
			name:      p.join(dir, un.Path),
			args:      slices.Clone(un.Args),
			script:    true,
			exitCodes: slices.Clone(un.ExitCodes),
			logPath:   logPath,
			rawPath:   un.Path,
		}, true
	}
}

// buildCmd constructs the exec.Cmd for cl—using script when cl.script is true
// and exec.Command otherwise—and applies withContext last.
func buildCmd(cl cmdline, script func(string, []string) (*exec.Cmd, error), ic goolib.InstallerContext) (*exec.Cmd, error) {
	var (
		cmd *exec.Cmd
		err error
	)
	if cl.script {
		cmd, err = script(cl.name, cl.args)
		if err != nil {
			return nil, err
		}
	} else {
		cmd = exec.Command(cl.name, cl.args...)
	}
	return withContext(cmd, ic), nil
}
