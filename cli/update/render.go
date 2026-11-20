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
	"strings"

	"github.com/google/googet/v2/goolib"
	"github.com/google/googet/v2/internal/relation"
	"github.com/google/logger"
)

// renderUpdates writes plan for the user to w and emits its log lines, in the
// order planning produced them: one line on w and one log line per action and
// per skipped replacement, one informational log line per diagnostic, and
// each planning note at its level. A dry run renders the same plan as a live
// run.
func renderUpdates(w io.Writer, plan updatePlan) {
	fmt.Fprintln(w, "Searching for available updates...")
	for _, e := range plan.entries {
		switch {
		case e.action != nil:
			renderAction(w, *e.action)
		case e.skip != nil:
			renderSkip(w, *e.skip)
		case e.diag != nil:
			renderDiag(*e.diag)
		case e.note != nil:
			renderNote(*e.note)
		}
	}
}

// renderAction writes a to w and logs it. A replacement also names the
// packages its Replaces entries remove and, separately, the dependents
// removed with them.
func renderAction(w io.Writer, a updateAction) {
	suffix := ""
	if a.op == opReplace {
		suffix = replaceDetail(a)
	}
	fmt.Fprintf(w, "  %s.%s, %s --> %s.%s, %s from %s%s\n", a.from.Name, a.from.Arch, a.from.Version, a.to.Name, a.to.Arch, a.to.Version, a.repo, suffix)
	logger.Infof("%s for package %s.%s, %s installed and %s.%s, %s available from %s%s.", a.op, a.from.Name, a.from.Arch, a.from.Version, a.to.Name, a.to.Arch, a.to.Version, a.repo, suffix)
}

// replaceDetail describes what the replacement a removes, partitioning
// a.removals into victims of a.to ("replaces ..."), victims of any dependency
// steps ("<dep> replaces ..."), and cascaded dependents ("removes orphaned ...").
func replaceDetail(a updateAction) string {
	winner := specInfo(a.to)
	var (
		direct    []goolib.PackageInfo
		depOrder  []goolib.PackageInfo
		depByStep = make(map[goolib.PackageInfo][]goolib.PackageInfo)
		cascaded  []goolib.PackageInfo
	)
	for _, r := range a.removals {
		switch {
		case r.ReplacedBy == nil:
			cascaded = append(cascaded, r.Package)
		case *r.ReplacedBy == winner:
			direct = append(direct, r.Package)
		default:
			if _, seen := depByStep[*r.ReplacedBy]; !seen {
				depOrder = append(depOrder, *r.ReplacedBy)
			}
			depByStep[*r.ReplacedBy] = append(depByStep[*r.ReplacedBy], r.Package)
		}
	}
	var parts []string
	if len(direct) > 0 {
		parts = append(parts, "replaces "+joinInfos(direct))
	}
	for _, dep := range depOrder {
		parts = append(parts, dep.Name+"."+dep.Arch+"."+dep.Ver+" replaces "+joinInfos(depByStep[dep]))
	}
	if len(cascaded) > 0 {
		parts = append(parts, "removes orphaned "+joinInfos(cascaded))
	}
	if len(parts) == 0 {
		return ""
	}
	return " (" + strings.Join(parts, "; ") + ")"
}

// renderSkip writes s to w and logs it as a structured warning line. A skip
// for orphaned dependents also logs one line per dependent, as update does
// when it skips an orphaning install.
func renderSkip(w io.Writer, s replaceSkip) {
	replacer := "none"
	if s.replacer != nil {
		replacer = s.replacer.String()
	}
	detail := ""
	switch {
	case len(s.candidates) > 0:
		detail = relation.JoinSpecs(s.candidates, ",")
		fmt.Fprintf(w, "  %s.%s, %s: not replaced (%s: %s)\n", s.pkg.Name, s.pkg.Arch, s.pkg.Version, s.reason, relation.JoinSpecs(s.candidates, ", "))
	case s.err != nil:
		fmt.Fprintf(w, "  %s.%s, %s: not replaced by %s (%s: %v)\n", s.pkg.Name, s.pkg.Arch, s.pkg.Version, replacer, s.reason, s.err)
	default:
		fmt.Fprintf(w, "  %s.%s, %s: not replaced by %s (%s)\n", s.pkg.Name, s.pkg.Arch, s.pkg.Version, replacer, s.reason)
	}
	line := fmt.Sprintf("update_replace_skipped package=%s replacer=%s reason=%s", s.pkg, replacer, s.reason)
	if detail != "" {
		line += " candidates=" + detail
	}
	if s.err != nil {
		line += fmt.Sprintf(" err=%q", s.err.Error())
	}
	logger.Warning(line)
	if s.reason == reasonOrphans {
		logSkippedOrphans(s.err)
	}
}

// renderDiag logs d as a structured informational line.
func renderDiag(d replaceDiag) {
	line := fmt.Sprintf("%s package=%s replacer=%s", d.kind, d.pkg, d.replacer)
	if len(d.others) > 0 {
		line += " ignored=" + relation.JoinSpecs(d.others, ",")
	}
	logger.Info(line)
}

// renderNote emits n at its log level.
func renderNote(n planNote) {
	if n.level == noteError {
		logger.Error(n.text)
		return
	}
	logger.Info(n.text)
}

// joinInfos formats infos as comma-separated name.arch.version entries.
func joinInfos(infos []goolib.PackageInfo) string {
	parts := make([]string, 0, len(infos))
	for _, pi := range infos {
		parts = append(parts, pi.Name+"."+pi.Arch+"."+pi.Ver)
	}
	return strings.Join(parts, ", ")
}
