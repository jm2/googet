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
	"os/exec"
	"slices"
	"strings"

	"github.com/google/googet/v2/goolib"
	"github.com/google/logger"
)

const googetEnvPrefix = "GOOGET_"

func hasGooGetEnvPrefix(entry string) bool {
	// Leading '=' characters belong to Windows cmd.exe pseudo-keys such as
	// "=C:=C:\..." or "=ExitCode=0" and are never GOOGET_ variables.
	if strings.HasPrefix(entry, "=") {
		return false
	}
	eq := strings.IndexByte(entry, '=')
	if eq < 0 {
		return false
	}
	key := entry[:eq]
	return len(key) >= len(googetEnvPrefix) && strings.EqualFold(key[:len(googetEnvPrefix)], googetEnvPrefix)
}

func sanitizeEnvValue(v string) string {
	return strings.ReplaceAll(v, "\x00", "")
}

// installerEnv builds the child environment for ic from base without mutating
// base: it strips every inherited entry whose key starts with "GOOGET_" (in any
// case, keeping '='-prefixed pseudo-keys, entries without '=', and keys such as
// GooGetRoot), then appends GOOGET_ACTION, GOOGET_PACKAGE,
// GOOGET_PREVIOUS_VERSION (always present, possibly empty), and—only when
// ic.Action is ActionReplace—GOOGET_REPLACED_BY, with NUL bytes removed from
// appended values. If ic.Action.String() is empty, it returns a copy of base.
func installerEnv(base []string, ic goolib.InstallerContext) []string {
	action := ic.Action.String()
	if action == "" {
		return slices.Clone(base)
	}
	out := make([]string, 0, len(base)+4)
	for _, entry := range base {
		if !hasGooGetEnvPrefix(entry) {
			out = append(out, entry)
		}
	}
	out = append(out,
		"GOOGET_ACTION="+action,
		"GOOGET_PACKAGE="+sanitizeEnvValue(ic.Package),
		"GOOGET_PREVIOUS_VERSION="+sanitizeEnvValue(ic.PreviousVersion),
	)
	if ic.Action == goolib.ActionReplace {
		out = append(out, "GOOGET_REPLACED_BY="+sanitizeEnvValue(ic.ReplacedBy))
	}
	return out
}

// withContext configures c.Env for ic after all other exec.Cmd fields (such as
// Dir or SysProcAttr) are set. When ic.Action.String() is empty, c.Env is left
// unchanged (nil when unset).
func withContext(c *exec.Cmd, ic goolib.InstallerContext) *exec.Cmd {
	if c == nil || ic.Action.String() == "" {
		return c
	}
	c.Env = installerEnv(c.Environ(), ic)
	return c
}

func logInstallerContext(hook string, ic goolib.InstallerContext) {
	action := ic.Action.String()
	if action == "" {
		return
	}
	replacedBy := ic.ReplacedBy
	if ic.Action != goolib.ActionReplace {
		replacedBy = ""
	}
	logger.Infof("installer_context hook=%s action=%s package=%s previous=%s replaced_by=%s",
		hook, action, sanitizeEnvValue(ic.Package), sanitizeEnvValue(ic.PreviousVersion), sanitizeEnvValue(replacedBy))
}
