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

package testutil

import (
	"runtime"
	"testing"
)

func TestScaledIterations(t *testing.T) {
	for _, tc := range []struct {
		n, wantWindows int
	}{
		{0, 1},
		{1, 1},
		{9, 1},
		{10, 1},
		{19, 1},
		{20, 2},
		{200, 20},
		{250, 25},
		{300, 30},
	} {
		want := tc.n
		if runtime.GOOS == "windows" {
			want = tc.wantWindows
		}
		if got := ScaledIterations(tc.n); got != want {
			t.Errorf("ScaledIterations(%d) = %d on %s, want %d", tc.n, got, runtime.GOOS, want)
		}
	}
}
