//go:build linux
// +build linux

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

package system

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"syscall"

	"github.com/google/googet/v2/client"
	"github.com/google/googet/v2/goolib"
	"github.com/google/googet/v2/oswrap"
	"github.com/google/logger"
)

// Install performs a system specific install given a package extraction directory and a PkgSpec struct.
func Install(dir string, ps *goolib.PkgSpec) error {
	return InstallWith(dir, ps, goolib.InstallerContext{})
}

// InstallWith performs a system specific install like Install, passing ic's
// environment variables to the install command when ic.Action is non-zero.
func InstallWith(dir string, ps *goolib.PkgSpec, ic goolib.InstallerContext) error {
	in := ps.Install
	if in.Path == "" {
		return nil
	}

	logger.Infof("Running install command: %q", in.Path)
	logInstallerContext("install", ic)
	out, err := oswrap.Create(filepath.Join(dir, "googet_install.log"))
	if err != nil {
		return err
	}
	defer func() {
		if err := out.Close(); err != nil {
			logger.Error(err)
		}
	}()
	cmd, err := goolib.Command(filepath.Join(dir, in.Path), in.Args)
	if err != nil {
		return fmt.Errorf("error running install: %v", err)
	}
	if err := goolib.Run(withContext(cmd, ic), in.ExitCodes, out); err != nil {
		return fmt.Errorf("error running install: %v", err)
	}
	return nil
}

// Uninstall performs a system specific uninstall given a package extraction directory and a PackageState struct.
func Uninstall(dir string, ps *client.PackageState) error {
	return UninstallWith(dir, ps, goolib.InstallerContext{})
}

// UninstallWith performs a system specific uninstall like Uninstall, passing
// ic's environment variables to the uninstall command when ic.Action is
// non-zero.
func UninstallWith(dir string, ps *client.PackageState, ic goolib.InstallerContext) error {
	un := ps.PackageSpec.Uninstall
	if un.Path == "" {
		return nil
	}

	logger.Infof("Running uninstall command: %q", un.Path)
	logInstallerContext("uninstall", ic)
	// Logging is only useful for failed uninstalls.
	out, err := oswrap.Create(filepath.Join(dir, "googet_remove.log"))
	if err != nil {
		return err
	}
	defer func() {
		if err := out.Close(); err != nil {
			logger.Error(err)
		}
	}()
	cmd, err := goolib.Command(filepath.Join(dir, un.Path), un.Args)
	if err != nil {
		return err
	}
	return goolib.Run(withContext(cmd, ic), un.ExitCodes, out)
}

// InstallableArchs returns a slice of archs supported by this machine.
func InstallableArchs() ([]string, error) {
	// Just return all archs as Linux builds are currently just used for testing.
	return []string{"noarch", "x86_64", "x86_32", "arm", "arm64"}, nil
}

// AppAssociation returns empty strings and is a stub of the Windows implementation.
func AppAssociation(ps *goolib.PkgSpec, installSource string) (string, string) {
	return "", ""
}

// IsAdmin returns nil and is a stub of the Windows implementation
func IsAdmin() error {
	return nil
}

// isGooGetRunning checks if the process with the given PID is running and is a googet process.
func isGooGetRunning(pid int) (bool, error) {
	// On Linux, we can check /proc/<pid>/exe
	exe, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid))
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil // Process does not exist
		}
		return false, err
	}
	// Check if the executable path contains "googet"
	return filepath.Base(exe) == "googet", nil
}

// lock attempts to obtain an exclusive lock on the provided file.
func lock(f *os.File) (func(), error) {
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return nil, err
	}
	cleanup := func() {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
		os.Remove(f.Name())
	}

	if err := f.Truncate(0); err != nil {
		cleanup()
		return nil, fmt.Errorf("failed to truncate lockfile: %v", err)
	}
	if _, err := f.WriteString(strconv.Itoa(os.Getpid())); err != nil {
		cleanup()
		return nil, fmt.Errorf("failed to write PID to lockfile: %v", err)
	}

	// Downgrade to shared lock so that other processes can read the PID.
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_SH); err != nil {
		cleanup()
		return nil, fmt.Errorf("failed to downgrade to shared lock: %v", err)
	}
	return cleanup, nil
}
