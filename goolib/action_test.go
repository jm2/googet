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
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestActionString(t *testing.T) {
	tests := []struct {
		name   string
		action Action
		want   string
	}{
		{name: "unspecified", action: ActionUnspecified, want: ""},
		{name: "install", action: ActionInstall, want: "install"},
		{name: "upgrade", action: ActionUpgrade, want: "upgrade"},
		{name: "downgrade", action: ActionDowngrade, want: "downgrade"},
		{name: "reinstall", action: ActionReinstall, want: "reinstall"},
		{name: "remove", action: ActionRemove, want: "remove"},
		{name: "replace", action: ActionReplace, want: "replace"},
		{name: "remove_dependent", action: ActionRemoveDependent, want: "remove-dependent"},
		{name: "recover", action: ActionRecover, want: "recover"},
		{name: "negative_one", action: Action(-1), want: ""},
		{name: "negative_large", action: Action(-100), want: ""},
		{name: "above_max", action: ActionRecover + 1, want: ""},
		{name: "large_out_of_range", action: Action(99), want: ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.action.String(); got != tc.want {
				t.Errorf("Action(%d).String() = %q, want %q", tc.action, got, tc.want)
			}
		})
	}
}

func TestActionStringClosedTokenSet(t *testing.T) {
	var nonEmpty []string
	for a := Action(-16); a <= Action(64); a++ {
		s := a.String()
		if s == "rollback" {
			t.Fatalf("Action(%d).String() = %q, want rollback excluded from defined actions", a, s)
		}
		if s != "" {
			nonEmpty = append(nonEmpty, s)
		}
	}

	want := []string{
		"install",
		"upgrade",
		"downgrade",
		"reinstall",
		"remove",
		"replace",
		"remove-dependent",
		"recover",
	}
	if diff := cmp.Diff(want, nonEmpty); diff != "" {
		t.Errorf("defined Action.String() token set mismatch (-want +got):\n%s", diff)
	}
}

func TestInstallerContextZeroValue(t *testing.T) {
	var ic InstallerContext
	if ic.Action != ActionUnspecified {
		t.Errorf("zero InstallerContext.Action = %v, want %v", ic.Action, ActionUnspecified)
	}
	if got := ic.Action.String(); got != "" {
		t.Errorf("zero InstallerContext.Action.String() = %q, want empty string", got)
	}
}

// TestActionStringMatchesREADMETokens verifies that the non-empty Action.String
// values and the GOOGET_ACTION tokens documented in README.md are the same set.
func TestActionStringMatchesREADMETokens(t *testing.T) {
	readmeBytes, err := os.ReadFile(filepath.Join("..", "README.md"))
	if err != nil {
		t.Fatalf("ReadFile(../README.md): %v", err)
	}
	readme := string(readmeBytes)
	const heading = "## Installer environment variables"
	start := strings.Index(readme, heading)
	if start < 0 {
		t.Fatalf("README.md missing %q section", heading)
	}
	section := readme[start+len(heading):]
	if next := strings.Index(section, "\n## "); next >= 0 {
		section = section[:next]
	}

	var actionRow string
	for _, line := range strings.Split(section, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "| `GOOGET_ACTION` |") {
			actionRow = line
			break
		}
	}
	if actionRow == "" {
		t.Fatalf("README.md section %q missing GOOGET_ACTION table row", heading)
	}

	codeRe := regexp.MustCompile("`([^`]+)`")
	var readmeTokens []string
	for _, m := range codeRe.FindAllStringSubmatch(actionRow, -1) {
		tok := m[1]
		if tok == "GOOGET_ACTION" {
			continue
		}
		if !slices.Contains(readmeTokens, tok) {
			readmeTokens = append(readmeTokens, tok)
		}
	}
	slices.Sort(readmeTokens)

	var codeTokens []string
	for a := Action(-16); a <= Action(64); a++ {
		if s := a.String(); s != "" && !slices.Contains(codeTokens, s) {
			codeTokens = append(codeTokens, s)
		}
	}
	slices.Sort(codeTokens)

	if diff := cmp.Diff(codeTokens, readmeTokens); diff != "" {
		t.Errorf("README GOOGET_ACTION token set mismatch against Action.String() (-code +readme):\n%s", diff)
	}
}
