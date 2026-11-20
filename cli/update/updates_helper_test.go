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
	"os"

	"github.com/google/googet/v2/client"
	"github.com/google/googet/v2/goolib"
	"github.com/google/googet/v2/install"
	"github.com/google/googet/v2/settings"
)

// updates plans the updates of the installed packages in state from rm with
// the archs, install policies and followreplaces setting in settings, prints
// and logs the plan, and returns the packages to install, in order.
func updates(state client.GooGetState, rm client.RepoMap) []goolib.PackageInfo {
	plan := planUpdates(state, rm, updateConfig{
		archs:          settings.Archs,
		opts:           install.OptionsFromSettings(false, false),
		followReplaces: settings.FollowReplaces,
	})
	renderUpdates(os.Stdout, plan)
	var out []goolib.PackageInfo
	for _, a := range plan.actions() {
		out = append(out, a.info())
	}
	return out
}
