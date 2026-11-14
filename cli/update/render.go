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

package update

import (
	"fmt"
	"io"

	"github.com/google/logger"
)

// renderUpdates writes plan for the user to w and emits its log lines, in the
// order planning produced them: one line on w and one informational log line
// per action, and each planning note at its level.
func renderUpdates(w io.Writer, plan updatePlan) {
	fmt.Fprintln(w, "Searching for available updates...")
	for _, e := range plan.entries {
		switch {
		case e.action != nil:
			renderAction(w, *e.action)
		case e.note != nil:
			renderNote(*e.note)
		}
	}
}

// renderAction writes a to w and logs it.
func renderAction(w io.Writer, a updateAction) {
	fmt.Fprintf(w, "  %s.%s, %s --> %s.%s, %s from %s\n", a.from.Name, a.from.Arch, a.from.Version, a.to.Name, a.to.Arch, a.to.Version, a.repo)
	logger.Infof("%s for package %s.%s, %s installed and %s.%s, %s available from %s.", a.op, a.from.Name, a.from.Arch, a.from.Version, a.to.Name, a.to.Arch, a.to.Version, a.repo)
}

// renderNote emits n at its log level.
func renderNote(n planNote) {
	if n.level == noteError {
		logger.Error(n.text)
		return
	}
	logger.Info(n.text)
}
