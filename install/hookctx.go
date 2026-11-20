/*
Copyright 2026 Google LLC

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

package install

import (
	"github.com/google/googet/v2/client"
	"github.com/google/googet/v2/goolib"
)

// installHookObserver, when non-nil in tests, receives the InstallerContext
// computed for each non-db_only install hook invocation in installPkgInner.
var installHookObserver func(goolib.InstallerContext)

// installContext returns the InstallerContext for installing s.spec over s.old.
func installContext(s planStep) goolib.InstallerContext {
	sv := classifyVersion(s.old, s.spec.PackageSpec)
	return goolib.InstallerContext{
		Action:          sv.action,
		Package:         s.spec.PackageSpec.String(),
		PreviousVersion: sv.from,
	}
}

// removalContext returns the InstallerContext for uninstalling r before
// installing s.spec.
func removalContext(s planStep, r removal) goolib.InstallerContext {
	if r.victim {
		return goolib.InstallerContext{
			Action:     goolib.ActionReplace,
			Package:    r.row.PackageSpec.String(),
			ReplacedBy: s.spec.PackageSpec.String(),
		}
	}
	return goolib.InstallerContext{
		Action:  goolib.ActionRemoveDependent,
		Package: r.row.PackageSpec.String(),
	}
}

// recoverContext returns the InstallerContext for reinstalling e during
// replacement recovery.
func recoverContext(e recordEntry) goolib.InstallerContext {
	return goolib.InstallerContext{
		Action:  goolib.ActionRecover,
		Package: e.row.PackageSpec.String(),
	}
}

// reinstallContext returns the InstallerContext for reinstalling an installed
// package row ps via Reinstall.
func reinstallContext(ps client.PackageState) goolib.InstallerContext {
	return goolib.InstallerContext{
		Action:          goolib.ActionReinstall,
		Package:         ps.PackageSpec.String(),
		PreviousVersion: ps.PackageSpec.Version,
	}
}
