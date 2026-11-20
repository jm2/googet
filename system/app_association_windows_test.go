//go:build windows
// +build windows

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
	"testing"

	"github.com/google/googet/v2/goolib"
)

func TestAppAssociationPortablePackage(t *testing.T) {
	ps := &goolib.PkgSpec{
		Name:    "Google Chrome",
		Authors: "Google LLC",
		Files:   map[string]string{"chrome.exe": "<ProgramFiles>/chrome.exe"},
	}
	if name, reg := AppAssociation(ps, ""); name != "" || reg != "" {
		t.Errorf("AppAssociation(package with Files) = (%q, %q), want empty", name, reg)
	}
}

func TestAppAssociationRepeatable(t *testing.T) {
	ps := &goolib.PkgSpec{
		Name:    "googet-app-association-test",
		Authors: "GooGet Test Authors Inc.",
	}
	name1, reg1 := AppAssociation(ps, "")
	name2, reg2 := AppAssociation(ps, "")
	if name1 != name2 || reg1 != reg2 {
		t.Errorf("AppAssociation(%q) = (%q, %q), then (%q, %q); want the same result", ps.Name, name1, reg1, name2, reg2)
	}
}
