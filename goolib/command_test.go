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

package goolib

import (
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestCommandSuccess(t *testing.T) {
	ps1Dirty := filepath.Join("pkgdir", "sub", "..", "install.ps1")
	ps1Clean := filepath.Clean(ps1Dirty)
	cmdDirty := filepath.Join("pkgdir", "sub", "..", "install.cmd")
	cmdClean := filepath.Clean(cmdDirty)
	batPath := filepath.Join("pkgdir", "install.bat")
	exePath := filepath.Join("pkgdir", "setup.exe")

	tests := []struct {
		name     string
		goos     string
		script   string
		args     []string
		wantArgs []string
	}{
		{
			name:   "windows_ps1_with_args",
			goos:   "windows",
			script: ps1Dirty,
			args:   []string{"-Flag", "value"},
			wantArgs: []string{
				"powershell",
				"-ExecutionPolicy", "Bypass",
				"-NonInteractive",
				"-NoProfile",
				"-Command", ps1Clean,
				"-Flag", "value",
			},
		},
		{
			name:   "windows_ps1_without_args",
			goos:   "windows",
			script: ps1Clean,
			args:   nil,
			wantArgs: []string{
				"powershell",
				"-ExecutionPolicy", "Bypass",
				"-NonInteractive",
				"-NoProfile",
				"-Command", ps1Clean,
			},
		},
		{
			name:     "windows_cmd",
			goos:     "windows",
			script:   cmdDirty,
			args:     []string{"/c", "echo"},
			wantArgs: []string{cmdClean, "/c", "echo"},
		},
		{
			name:     "windows_bat",
			goos:     "windows",
			script:   batPath,
			args:     []string{"arg1", "arg2"},
			wantArgs: []string{batPath, "arg1", "arg2"},
		},
		{
			name:     "windows_exe",
			goos:     "windows",
			script:   exePath,
			args:     []string{"/S"},
			wantArgs: []string{exePath, "/S"},
		},
		{
			name:     "linux_direct",
			goos:     "linux",
			script:   "/bin/sh",
			args:     []string{"-c", "true"},
			wantArgs: []string{"/bin/sh", "-c", "true"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Allocate extra capacity to verify command never mutates the caller's backing array.
			callerArgs := make([]string, len(tc.args), len(tc.args)+4)
			copy(callerArgs, tc.args)
			before := slices.Clone(callerArgs)

			got, err := command(tc.goos, tc.script, callerArgs)
			if err != nil {
				t.Fatalf("command(%q, %q, %v) unexpected error: %v", tc.goos, tc.script, tc.args, err)
			}
			if diff := cmp.Diff(tc.wantArgs, got.Args); diff != "" {
				t.Errorf("command(%q, %q, %v) Args mismatch (-want +got):\n%s", tc.goos, tc.script, tc.args, diff)
			}
			if got.Env != nil {
				t.Errorf("command(%q, %q, %v) Env = %v, want nil", tc.goos, tc.script, tc.args, got.Env)
			}
			if diff := cmp.Diff(before, callerArgs); diff != "" {
				t.Errorf("command(%q, %q, %v) mutated caller args (-want +got):\n%s", tc.goos, tc.script, tc.args, diff)
			}
			// Mutating got.Args must not affect callerArgs either.
			got.Args[len(got.Args)-1] = "mutated"
			if diff := cmp.Diff(before, callerArgs); diff != "" {
				t.Errorf("mutating returned Cmd.Args affected caller args (-want +got):\n%s", diff)
			}
		})
	}
}

func TestCommandErrors(t *testing.T) {
	tests := []struct {
		name    string
		goos    string
		script  string
		args    []string
		wantErr string
	}{
		{
			name:    "windows_unknown_extension",
			goos:    "windows",
			script:  filepath.Join("pkgdir", "setup.msi"),
			args:    []string{"/qn"},
			wantErr: `unknown extension ".msi"`,
		},
		{
			name:    "windows_empty_extension",
			goos:    "windows",
			script:  filepath.Join("pkgdir", "setup"),
			args:    nil,
			wantErr: `unknown extension ""`,
		},
		{
			name:    "darwin_unsupported_os",
			goos:    "darwin",
			script:  "/bin/sh",
			args:    []string{"-c", "true"},
			wantErr: `OS "darwin" is not Windows or Linux`,
		},
		{
			name:    "freebsd_unsupported_os",
			goos:    "freebsd",
			script:  "/bin/sh",
			args:    nil,
			wantErr: `OS "freebsd" is not Windows or Linux`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := command(tc.goos, tc.script, tc.args)
			if err == nil {
				t.Fatalf("command(%q, %q, %v) = %v, want error %q", tc.goos, tc.script, tc.args, got, tc.wantErr)
			}
			if got != nil {
				t.Errorf("command(%q, %q, %v) returned non-nil Cmd %v alongside error %v", tc.goos, tc.script, tc.args, got, err)
			}
			if err.Error() != tc.wantErr {
				t.Errorf("command(%q, %q, %v) error = %q, want %q", tc.goos, tc.script, tc.args, err.Error(), tc.wantErr)
			}
		})
	}
}

func TestCommandUnknownInterpreter(t *testing.T) {
	interpreter[".custom"] = "python"
	t.Cleanup(func() {
		delete(interpreter, ".custom")
	})

	got, err := command("windows", filepath.Join("pkgdir", "run.custom"), nil)
	const wantErr = `unknown interpreter: "python"`
	if err == nil {
		t.Fatalf("command(\"windows\", \"run.custom\", nil) = %v, want error %q", got, wantErr)
	}
	if err.Error() != wantErr {
		t.Errorf("command(\"windows\", \"run.custom\", nil) error = %q, want %q", err.Error(), wantErr)
	}
}

func TestCommandAndExecOnLinux(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("requires Linux /bin/sh")
	}

	cmd, err := Command("/bin/sh", []string{"-c", "printf hello"})
	if err != nil {
		t.Fatalf("Command(\"/bin/sh\", ...) unexpected error: %v", err)
	}
	wantArgs := []string{"/bin/sh", "-c", "printf hello"}
	if diff := cmp.Diff(wantArgs, cmd.Args); diff != "" {
		t.Errorf("Command(\"/bin/sh\", ...) Args mismatch (-want +got):\n%s", diff)
	}
	if cmd.Env != nil {
		t.Errorf("Command(\"/bin/sh\", ...) Env = %v, want nil", cmd.Env)
	}

	var out syncBuffer
	captureStdio(t, func() {
		err = Exec("/bin/sh", []string{"-c", "echo ok; exit 7"}, []int{7}, &out)
	})
	if err != nil {
		t.Fatalf("Exec with accepted exit code 7 failed: %v", err)
	}
	if !strings.Contains(out.String(), "ok\n") {
		t.Errorf("Exec output = %q, want substring %q", out.String(), "ok\n")
	}

	var failOut syncBuffer
	captureStdio(t, func() {
		err = Exec("/bin/sh", []string{"-c", "exit 9"}, []int{7}, &failOut)
	})
	const wantExitErr = "command exited with error code 9"
	if err == nil || err.Error() != wantExitErr {
		t.Errorf("Exec with rejected exit code 9 error = %v, want %q", err, wantExitErr)
	}
}
